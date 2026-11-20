package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeRecords fills a fresh store with n records of a fixed encoded size, so
// tests can compute exact byte offsets.
func writeRecords(t *testing.T, dir string, n int) int {
	t.Helper()
	s, err := Open(Options{Dir: dir, Sync: SyncNever})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	for i := 0; i < n; i++ {
		if err := s.Put(keyFor(i), []byte(valFor(i))); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	return len(Encode(Record{Key: keyFor(0), Value: []byte(valFor(0))}))
}

func keyFor(i int) string { return fmt.Sprintf("k%04d", i) }
func valFor(i int) string { return fmt.Sprintf("v%04d", i) }

func logPath(dir string) string { return filepath.Join(dir, activeLogName) }

func TestReopenReplaysEveryRecord(t *testing.T) {
	dir := t.TempDir()
	writeRecords(t, dir, 100)

	s, err := Open(Options{Dir: dir, Sync: SyncNever})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	if got := s.Len(); got != 100 {
		t.Fatalf("Len after reopen = %d, want 100", got)
	}
	if got := s.Stats().Records; got != 100 {
		t.Fatalf("replayed %d records, want 100", got)
	}
	if got := s.Stats().BytesCut; got != 0 {
		t.Fatalf("cut %d bytes, want 0", got)
	}
	for i := 0; i < 100; i++ {
		got, err := s.Get(keyFor(i))
		if err != nil {
			t.Fatalf("Get %s: %v", keyFor(i), err)
		}
		if string(got) != valFor(i) {
			t.Fatalf("Get %s = %q, want %q", keyFor(i), got, valFor(i))
		}
	}
}

func TestReopenCutsGarbageTail(t *testing.T) {
	dir := t.TempDir()
	writeRecords(t, dir, 100)
	path := logPath(dir)

	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("garbage")); err != nil { // exactly 7 bytes
		t.Fatal(err)
	}
	f.Close()

	s, err := Open(Options{Dir: dir, Sync: SyncNever})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	if got := s.Len(); got != 100 {
		t.Fatalf("Len = %d, want exactly 100 keys", got)
	}
	if got := s.Stats().BytesCut; got != 7 {
		t.Fatalf("BytesCut = %d, want 7", got)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("file size = %d, want %d after truncation", after.Size(), before.Size())
	}
}

func TestReopenCutsHalfWrittenRecord(t *testing.T) {
	dir := t.TempDir()
	recSize := writeRecords(t, dir, 100)
	path := logPath(dir)

	// Chop the last record in half, the way a kill mid-write would.
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, st.Size()-int64(recSize/2)); err != nil {
		t.Fatal(err)
	}

	s, err := Open(Options{Dir: dir, Sync: SyncNever})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	if got := s.Len(); got != 99 {
		t.Fatalf("Len = %d, want 99 (records 0-98 intact)", got)
	}
	if got := s.Stats().BytesCut; got != int64(recSize)-int64(recSize/2) {
		t.Fatalf("BytesCut = %d, want %d", got, int64(recSize)-int64(recSize/2))
	}
	for i := 0; i < 99; i++ {
		if _, err := s.Get(keyFor(i)); err != nil {
			t.Fatalf("Get %s after recovery: %v", keyFor(i), err)
		}
	}
	if _, err := s.Get(keyFor(99)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the half-written key survived: %v", err)
	}

	// The store must still accept writes after recovery.
	if err := s.Put("after", []byte("crash")); err != nil {
		t.Fatalf("Put after recovery: %v", err)
	}
}

func TestReopenRefusesMidFileCorruption(t *testing.T) {
	dir := t.TempDir()
	recSize := writeRecords(t, dir, 100)
	path := logPath(dir)

	// Flip a value byte inside record 50. Records 51-99 are still after it.
	const victim = 50
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[victim*recSize+HeaderSize] ^= 0xFF
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	before := int64(len(raw))

	_, err = Open(Options{Dir: dir, Sync: SyncNever})
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open err = %v, want ErrCorrupt", err)
	}

	after, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if after.Size() != before {
		t.Fatalf("corrupt store was truncated to %d, want %d: mid-file damage is not a torn tail",
			after.Size(), before)
	}
}

func TestReopenReplaysTombstones(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(Options{Dir: dir, Sync: SyncNever})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put("gone", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("kept", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("gone"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	re, err := Open(Options{Dir: dir, Sync: SyncNever})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer re.Close()

	if _, err := re.Get("gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted key came back: %v", err)
	}
	if got, err := re.Get("kept"); err != nil || string(got) != "v2" {
		t.Fatalf("Get(kept) = %q, %v", got, err)
	}
	if got := re.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1", got)
	}
}

func TestReopenReplaysNewestValue(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(Options{Dir: dir, Sync: SyncNever})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if err := s.Put("k", []byte(fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	re, err := Open(Options{Dir: dir, Sync: SyncNever})
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()
	if got, err := re.Get("k"); err != nil || string(got) != "49" {
		t.Fatalf("Get(k) = %q, %v; want the newest write", got, err)
	}
}

func TestReopenEmptyStore(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(Options{Dir: dir, Sync: SyncNever})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	re, err := Open(Options{Dir: dir, Sync: SyncNever})
	if err != nil {
		t.Fatalf("reopen empty store: %v", err)
	}
	defer re.Close()
	if got := re.Len(); got != 0 {
		t.Fatalf("Len = %d, want 0", got)
	}
	if got := re.Stats(); got.Records != 0 || got.BytesCut != 0 {
		t.Fatalf("stats = %+v, want zeroes", got)
	}
}
