package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openSegmented(t *testing.T, dir string, segSize int64) *Store {
	t.Helper()
	s, err := Open(Options{Dir: dir, Sync: SyncNever, SegmentSize: segSize})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func segmentFiles(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.log"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func diskUsage(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	for _, f := range segmentFiles(t, dir) {
		st, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		total += st.Size()
	}
	return total
}

func TestRotationCreatesMultipleSegments(t *testing.T) {
	dir := t.TempDir()
	s := openSegmented(t, dir, 256)

	for i := 0; i < 40; i++ {
		if err := s.Put(keyFor(i), []byte(valFor(i))); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}

	files := segmentFiles(t, dir)
	if len(files) < 3 {
		t.Fatalf("wrote %d segments, want at least 3 for 40 records at 256 bytes each", len(files))
	}
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() > 256+31 {
			t.Fatalf("%s is %d bytes, over the segment size", filepath.Base(f), st.Size())
		}
	}
	// Reads must follow the index into whichever segment holds the value.
	for i := 0; i < 40; i++ {
		got, err := s.Get(keyFor(i))
		if err != nil {
			t.Fatalf("Get %s: %v", keyFor(i), err)
		}
		if string(got) != valFor(i) {
			t.Fatalf("Get %s = %q, want %q", keyFor(i), got, valFor(i))
		}
	}
}

func TestReopenReadsEverySegment(t *testing.T) {
	dir := t.TempDir()
	s := openSegmented(t, dir, 256)
	for i := 0; i < 40; i++ {
		if err := s.Put(keyFor(i), []byte(valFor(i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	re := openSegmented(t, dir, 256)
	if got := re.Len(); got != 40 {
		t.Fatalf("Len = %d, want 40", got)
	}
	if got := re.Stats().Records; got != 40 {
		t.Fatalf("replayed %d records, want 40", got)
	}
	for i := 0; i < 40; i++ {
		got, err := re.Get(keyFor(i))
		if err != nil {
			t.Fatalf("Get %s: %v", keyFor(i), err)
		}
		if string(got) != valFor(i) {
			t.Fatalf("Get %s = %q, want %q", keyFor(i), got, valFor(i))
		}
	}
}

func TestCompactionShrinksDiskUsage(t *testing.T) {
	dir := t.TempDir()
	s := openSegmented(t, dir, 64<<10)

	const keys, rounds = 100, 100
	value := strings.Repeat("x", 64)
	for r := 0; r < rounds; r++ {
		for i := 0; i < keys; i++ {
			if err := s.Put(keyFor(i), []byte(value)); err != nil {
				t.Fatal(err)
			}
		}
	}

	before := diskUsage(t, dir)
	if err := s.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	after := diskUsage(t, dir)

	reduction := 100 * (1 - float64(after)/float64(before))
	t.Logf("disk usage before=%d bytes after=%d bytes reduction=%.1f%%", before, after, reduction)
	if reduction < 90 {
		t.Fatalf("compaction reduced disk usage by %.1f%%, want at least 90%%", reduction)
	}

	for i := 0; i < keys; i++ {
		got, err := s.Get(keyFor(i))
		if err != nil {
			t.Fatalf("Get %s after compaction: %v", keyFor(i), err)
		}
		if string(got) != value {
			t.Fatalf("Get %s = %q, want the compacted value", keyFor(i), got)
		}
	}
}

func TestCompactionDropsDeletedKeysForGood(t *testing.T) {
	dir := t.TempDir()
	s := openSegmented(t, dir, 32<<10)

	if err := s.Put("alive", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("dead", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("dead"); err != nil {
		t.Fatal(err)
	}
	if err := s.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// If compaction dropped the tombstone but kept the original put, replay
	// would bring the deleted key back.
	re := openSegmented(t, dir, 32<<10)
	if _, err := re.Get("dead"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted key came back after compaction + reopen: %v", err)
	}
	if got, err := re.Get("alive"); err != nil || string(got) != "v" {
		t.Fatalf("Get(alive) = %q, %v", got, err)
	}
	if got := re.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1", got)
	}
}

func TestCompactionSurvivesReopenWithEveryValue(t *testing.T) {
	dir := t.TempDir()
	s := openSegmented(t, dir, 512)
	for i := 0; i < 20; i++ {
		if err := s.Put(keyFor(i), []byte(valFor(i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	// Writes after compaction must still work, and land in a fresh segment.
	if err := s.Put("after", []byte("compact")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	re := openSegmented(t, dir, 512)
	for i := 0; i < 20; i++ {
		got, err := re.Get(keyFor(i))
		if err != nil {
			t.Fatalf("Get %s: %v", keyFor(i), err)
		}
		if string(got) != valFor(i) {
			t.Fatalf("Get %s = %q, want %q", keyFor(i), got, valFor(i))
		}
	}
	if got, err := re.Get("after"); err != nil || string(got) != "compact" {
		t.Fatalf("Get(after) = %q, %v", got, err)
	}
}

func TestOrphanSegmentsFromCrashedCompactionAreIgnored(t *testing.T) {
	dir := t.TempDir()
	s := openSegmented(t, dir, 32<<10)
	for i := 0; i < 5; i++ {
		if err := s.Put(keyFor(i), []byte(valFor(i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// A compaction that died after writing but before recording its output
	// leaves a high-numbered segment nobody references.
	orphan := filepath.Join(dir, "000999.log")
	if err := os.WriteFile(orphan, []byte("this is not a record"), 0o644); err != nil {
		t.Fatal(err)
	}

	re := openSegmented(t, dir, 32<<10)
	for i := 0; i < 5; i++ {
		got, err := re.Get(keyFor(i))
		if err != nil {
			t.Fatalf("Get %s: %v", keyFor(i), err)
		}
		if string(got) != valFor(i) {
			t.Fatalf("Get %s = %q, want %q", keyFor(i), got, valFor(i))
		}
	}
	if _, statErr := os.Stat(orphan); !os.IsNotExist(statErr) {
		t.Fatalf("orphan segment was not cleaned up: %v", statErr)
	}
}

func TestManifestIsWrittenAndRead(t *testing.T) {
	dir := t.TempDir()
	s := openSegmented(t, dir, 32<<10)
	if err := s.Put("k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	s.Close()

	if _, err := os.Stat(filepath.Join(dir, "MANIFEST")); err != nil {
		t.Fatalf("MANIFEST missing: %v", err)
	}

	re := openSegmented(t, dir, 32<<10)
	if got, err := re.Get("k"); err != nil || string(got) != "v" {
		t.Fatalf("Get(k) = %q, %v", got, err)
	}
}

func TestCorruptMiddleSegmentStillRefusesToOpen(t *testing.T) {
	dir := t.TempDir()
	s := openSegmented(t, dir, 256)
	for i := 0; i < 40; i++ {
		if err := s.Put(keyFor(i), []byte(valFor(i))); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	files := segmentFiles(t, dir)
	if len(files) < 3 {
		t.Fatalf("need at least 3 segments, got %d", len(files))
	}
	// Corrupt a byte in the first (sealed, non-active) segment.
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xFF
	if err := os.WriteFile(files[0], raw, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = Open(Options{Dir: dir, Sync: SyncNever, SegmentSize: 256})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open err = %v, want ErrCorrupt: a sealed segment is never a torn tail", err)
	}
}

func TestSegmentNaming(t *testing.T) {
	if got := segName(1); got != "000001.log" {
		t.Fatalf("segName(1) = %q", got)
	}
	if got := segName(42); got != "000042.log" {
		t.Fatalf("segName(42) = %q", got)
	}
	if _, err := parseSegName("MANIFEST"); err == nil {
		t.Fatal("parseSegName accepted MANIFEST")
	}
	if id, err := parseSegName("000007.log"); err != nil || id != 7 {
		t.Fatalf("parseSegName(000007.log) = %d, %v", id, err)
	}
}
