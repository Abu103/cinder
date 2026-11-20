package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	segExt       = ".log"
	tmpExt       = ".tmp"
	manifestName = "MANIFEST"
	// manifestHeader versions the file so a future format change is detected
	// instead of misparsed.
	manifestHeader = "cinder-manifest-v1"
	// activeLogName is the first segment every store starts with.
	activeLogName = "000001.log"
)

func segName(id uint32) string { return fmt.Sprintf("%06d%s", id, segExt) }

func segPath(dir string, id uint32) string { return filepath.Join(dir, segName(id)) }

// parseSegName turns "000042.log" into 42. Anything else is an error.
func parseSegName(name string) (uint32, error) {
	if !strings.HasSuffix(name, segExt) {
		return 0, fmt.Errorf("cinder: %q is not a segment name", name)
	}
	base := strings.TrimSuffix(name, segExt)
	if len(base) != 6 {
		return 0, fmt.Errorf("cinder: %q is not a segment name", name)
	}
	id, err := strconv.ParseUint(base, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("cinder: %q is not a segment name: %w", name, err)
	}
	return uint32(id), nil
}

// rotateIfNeeded seals the active segment and starts a new one when recSize
// would push it past the cap. A record bigger than the cap still gets written:
// a segment always holds at least one record.
func (s *Store) rotateIfNeeded(recSize int64) error {
	active := s.logs[s.activeID]
	if active.Size() == 0 || active.Size()+recSize <= s.opts.SegmentSize {
		return nil
	}
	return s.startSegment(s.nextID)
}

func (s *Store) startSegment(id uint32) error {
	l, err := OpenLog(segPath(s.opts.Dir, id), s.opts.Sync)
	if err != nil {
		return err
	}

	prevActive, prevNext := s.activeID, s.nextID
	s.logs[id] = l
	s.activeID = id
	s.nextID = id + 1

	ids := make([]uint32, 0, len(s.logs))
	for sid := range s.logs {
		ids = append(ids, sid)
	}
	if err := writeManifest(s.opts.Dir, ids); err != nil {
		// The in-memory view must not point at a segment the manifest does not
		// know about.
		delete(s.logs, id)
		s.activeID, s.nextID = prevActive, prevNext
		_ = l.Close()
		return err
	}
	return nil
}

// Compact rewrites every live record into a fresh set of segments, records
// them in the manifest, and only then deletes the old files. Tombstones are
// dropped because the records they killed are not copied.
//
// A crash at any point either leaves the old manifest in place (the new files
// are orphans and are cleaned up on the next Open) or leaves the new manifest
// in place (the old files are cleaned up on the next Open). There is no window
// where the manifest names a file that is not fully written.
func (s *Store) Compact() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := s.opts.Dir
	oldIDs := make([]uint32, 0, len(s.logs))
	for id := range s.logs {
		oldIDs = append(oldIDs, id)
	}

	var (
		newIDs   []uint32
		tmpLog   *Log
		tmpID    uint32
		tmpSize  int64
		next     = s.nextID
		newIndex = make(map[string]entry, len(s.index))
	)

	abort := func() {
		if tmpLog != nil {
			_ = tmpLog.Close()
			_ = os.Remove(tmpLog.Path())
			tmpLog = nil
		}
		for _, id := range newIDs {
			_ = os.Remove(segPath(dir, id))
		}
	}
	begin := func() error {
		tmpID = next
		next++
		l, err := OpenLog(segPath(dir, tmpID)+tmpExt, s.opts.Sync)
		if err != nil {
			return err
		}
		tmpLog, tmpSize = l, 0
		return nil
	}
	seal := func() error {
		if tmpLog == nil {
			return nil
		}
		tmp := tmpLog.Path()
		if err := tmpLog.Close(); err != nil {
			return err
		}
		tmpLog = nil
		if err := os.Rename(tmp, segPath(dir, tmpID)); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		newIDs = append(newIDs, tmpID)
		return nil
	}

	for key, e := range s.index {
		src, ok := s.logs[e.fileID]
		if !ok {
			abort()
			return fmt.Errorf("cinder: compact: key %q points at closed segment %d", key, e.fileID)
		}
		rec, err := src.ReadAt(e.offset, int64(e.size))
		if err != nil {
			abort()
			return fmt.Errorf("cinder: compact %q: %w", key, err)
		}

		size := int64(HeaderSize + len(rec.Key) + len(rec.Value))
		if tmpLog == nil || (tmpSize > 0 && tmpSize+size > s.opts.SegmentSize) {
			if err := seal(); err != nil {
				abort()
				return err
			}
			if err := begin(); err != nil {
				abort()
				return err
			}
		}

		off, n, err := tmpLog.Append(rec)
		if err != nil {
			abort()
			return fmt.Errorf("cinder: compact %q: %w", key, err)
		}
		newIndex[key] = entry{fileID: tmpID, offset: off, size: n, ts: rec.Timestamp}
		tmpSize += int64(n)
	}
	if err := seal(); err != nil {
		abort()
		return err
	}

	// A fresh active segment, so writes after compaction never share a file
	// with the compacted output.
	activeID := next
	next++
	activeLog, err := OpenLog(segPath(dir, activeID), s.opts.Sync)
	if err != nil {
		abort()
		return err
	}
	newIDs = append(newIDs, activeID)

	if err := writeManifest(dir, newIDs); err != nil {
		_ = activeLog.Close()
		abort()
		return err
	}

	logs := make(map[uint32]*Log, len(newIDs))
	logs[activeID] = activeLog
	for _, id := range newIDs {
		if id == activeID {
			continue
		}
		l, err := OpenLog(segPath(dir, id), s.opts.Sync)
		if err != nil {
			// The manifest already names these files, so a partial swap would
			// hand out a broken store. Close everything and report.
			_ = activeLog.Close()
			for _, opened := range logs {
				_ = opened.Close()
			}
			return fmt.Errorf("cinder: compact: reopen segment %d: %w", id, err)
		}
		logs[id] = l
	}

	old := s.logs
	s.logs, s.index, s.activeID, s.nextID = logs, newIndex, activeID, next
	for _, id := range oldIDs {
		if l, ok := old[id]; ok {
			_ = l.Close()
		}
		_ = os.Remove(segPath(dir, id))
	}
	return nil
}

func writeManifest(dir string, ids []uint32) error {
	sorted := append([]uint32(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var b strings.Builder
	b.WriteString(manifestHeader)
	b.WriteByte('\n')
	for _, id := range sorted {
		b.WriteString(segName(id))
		b.WriteByte('\n')
	}

	tmp := filepath.Join(dir, manifestName+tmpExt)
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("cinder: write manifest: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, manifestName)); err != nil {
		return fmt.Errorf("cinder: install manifest: %w", err)
	}
	return nil
}

// readManifest returns the live segment ids. found is false when the store
// predates the manifest, in which case the caller scans the directory.
func readManifest(dir string) (ids []uint32, found bool, err error) {
	b, err := os.ReadFile(filepath.Join(dir, manifestName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("cinder: read manifest: %w", err)
	}

	lines := strings.Split(strings.TrimRight(string(b), "\r\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != manifestHeader {
		return nil, false, fmt.Errorf("cinder: %s is not a cinder manifest", manifestName)
	}
	for i, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		id, err := parseSegName(line)
		if err != nil {
			return nil, false, fmt.Errorf("cinder: manifest line %d: %w", i+2, err)
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, false, fmt.Errorf("cinder: %s names no segments", manifestName)
	}
	return ids, true, nil
}

func scanSegmentIDs(dir string) ([]uint32, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*"+segExt))
	if err != nil {
		return nil, fmt.Errorf("cinder: scan segments: %w", err)
	}
	ids := make([]uint32, 0, len(names))
	for _, name := range names {
		id, err := parseSegName(filepath.Base(name))
		if err != nil {
			continue // not one of ours
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

// removeOrphans deletes files a crashed compaction left behind: segments the
// manifest does not name, and temporary manifest copies.
func removeOrphans(dir string, live []uint32) {
	keep := make(map[uint32]bool, len(live))
	for _, id := range live {
		keep[id] = true
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		if name == manifestName+tmpExt || strings.HasSuffix(name, segExt+tmpExt) {
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		if id, err := parseSegName(name); err == nil && !keep[id] {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}
