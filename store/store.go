package store

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// ErrNotFound is returned by Get for a key that was never written or has been
// deleted. Check it with errors.Is.
var ErrNotFound = errors.New("cinder: key not found")

// ErrCorrupt means a record in the middle of a sealed segment failed to
// decode. Open refuses instead of discarding the records after it.
var ErrCorrupt = errors.New("cinder: log corruption")

// RecoveryStats describes what Open found when it replayed the log.
type RecoveryStats struct {
	// Records is the number of records replayed into the index.
	Records int
	// BytesCut is the size of a torn tail that was truncated away. Zero when
	// the log ended cleanly.
	BytesCut int64
}

// entry is where the newest record for one key lives.
type entry struct {
	fileID uint32
	offset int64
	size   int
	ts     int64
}

// DefaultSegmentSize is the cap for one log file when Options.SegmentSize is
// unset.
const DefaultSegmentSize = 64 << 20

// Options configure a Store.
type Options struct {
	// Dir holds the log segments and the MANIFEST file. Created if missing.
	Dir string
	// Sync is the durability policy for every write.
	Sync SyncPolicy
	// SegmentSize caps one segment file in bytes. Zero means
	// DefaultSegmentSize.
	SegmentSize int64
}

// Store is a segmented, log-structured key-value database. It is safe for
// concurrent use.
type Store struct {
	// mu guards everything below. Readers take RLock; writes, rotation,
	// compaction and Close take Lock. Get holds RLock for the whole disk read,
	// so compaction can never close a segment out from under a reader.
	mu       sync.RWMutex
	opts     Options
	logs     map[uint32]*Log // open segment handles, by segment id
	activeID uint32          // segment new writes go to
	nextID   uint32          // id the next segment will get
	index    map[string]entry
	stats    RecoveryStats
}

// Open opens the store in opts.Dir, recovering the index from every segment
// listed in the manifest and cutting a torn tail if the newest one has one.
func Open(opts Options) (*Store, error) {
	if opts.Dir == "" {
		return nil, errors.New("cinder: Options.Dir is required")
	}
	if opts.SegmentSize <= 0 {
		opts.SegmentSize = DefaultSegmentSize
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("cinder: create data dir: %w", err)
	}

	ids, fromManifest, err := readManifest(opts.Dir)
	if err != nil {
		return nil, err
	}
	if !fromManifest {
		ids, err = scanSegmentIDs(opts.Dir)
		if err != nil {
			return nil, err
		}
	}
	if len(ids) == 0 {
		ids = []uint32{1}
	}

	s := &Store{opts: opts, logs: make(map[uint32]*Log, len(ids)), index: make(map[string]entry)}
	for i, id := range ids {
		l, err := OpenLog(segPath(opts.Dir, id), opts.Sync)
		if err != nil {
			_ = s.closeAll()
			return nil, err
		}
		s.logs[id] = l

		last := i == len(ids)-1
		if err := s.replaySegment(id, l, last); err != nil {
			_ = s.closeAll()
			return nil, err
		}
	}

	s.activeID = ids[len(ids)-1]
	s.nextID = s.activeID + 1

	// Every later rotate and compaction rewrites the manifest, so recovery knows
	// which files are live. Writing it here also covers a store created before
	// the manifest existed.
	if err := writeManifest(opts.Dir, ids); err != nil {
		_ = s.closeAll()
		return nil, err
	}
	if fromManifest {
		// Output of a compaction that died before recording itself.
		removeOrphans(opts.Dir, ids)
	}
	return s, nil
}

// Put appends value as the newest record for key.
func (s *Store) Put(key string, value []byte) error {
	if total := len(key) + len(value); total > MaxRecordSize {
		return fmt.Errorf("%w: key+value is %d bytes", ErrRecordTooLarge, total)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	recSize := int64(HeaderSize + len(key) + len(value))
	if err := s.rotateIfNeeded(recSize); err != nil {
		return err
	}

	rec := Record{Key: key, Value: value, Timestamp: time.Now().UnixNano()}
	off, size, err := s.logs[s.activeID].Append(rec)
	if err != nil {
		return err
	}
	s.index[key] = entry{fileID: s.activeID, offset: off, size: size, ts: rec.Timestamp}
	return nil
}

// Get returns the newest value for key, or ErrNotFound.
func (s *Store) Get(key string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	e, ok := s.index[key]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, key)
	}
	log, ok := s.logs[e.fileID]
	if !ok {
		return nil, fmt.Errorf("cinder: key %q points at closed segment %d", key, e.fileID)
	}
	rec, err := log.ReadAt(e.offset, int64(e.size))
	if err != nil {
		return nil, err
	}
	return rec.Value, nil
}

// Delete marks key as removed. Deleting a key that does not exist is not an
// error; the tombstone is still written so the delete survives a restart.
func (s *Store) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.rotateIfNeeded(int64(HeaderSize + len(key))); err != nil {
		return err
	}
	rec := Record{Key: key, Timestamp: time.Now().UnixNano(), Tombstone: true}
	if _, _, err := s.logs[s.activeID].Append(rec); err != nil {
		return err
	}
	delete(s.index, key)
	return nil
}

// Len is the number of live keys.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.index)
}

// Stats reports what the last Open found while replaying the log.
func (s *Store) Stats() RecoveryStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stats
}

// Close releases every open segment file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeAll()
}

func (s *Store) closeAll() error {
	var first error
	for id, l := range s.logs {
		if err := l.Close(); err != nil && first == nil {
			first = err
		}
		delete(s.logs, id)
	}
	return first
}
