package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests probe the durability boundary that Process.Kill cannot reach.
// A real power cut is hardware-dependent; here we inject a failed fsync and we
// simulate the OS dropping unflushed pages by truncating a SyncNever log.

func TestFailedFsyncIsReportedAsAnError(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(Options{Dir: dir, Sync: SyncAlways})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	old := syncFile
	t.Cleanup(func() { syncFile = old })
	syncFile = func(*os.File) error { return errors.New("injected fsync failure") }

	err = s.Put("k", []byte("v"))
	if err == nil {
		t.Fatal("Put returned success even though fsync failed")
	}
	if !strings.Contains(err.Error(), "injected fsync failure") {
		t.Fatalf("error = %v, want the injected failure", err)
	}
}

func TestFailedFsyncDoesNotBreakTheNextAppend(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(Options{Dir: dir, Sync: SyncAlways})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	old := syncFile
	syncFile = func(*os.File) error { return errors.New("injected fsync failure") }
	if err := s.Put("k", []byte("v")); err == nil {
		t.Fatal("Put should have failed")
	}
	syncFile = old

	// The failed write still landed in the file. Advancing end before the sync
	// means this append must go after it, not overwrite it.
	if err := s.Put("k2", []byte("v2")); err != nil {
		t.Fatalf("next Put: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	re, err := Open(Options{Dir: dir, Sync: SyncAlways})
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()
	got, err := re.Get("k2")
	if err != nil || string(got) != "v2" {
		t.Fatalf("Get(k2) = %q, %v; the record after the failed sync was lost", got, err)
	}
}

func TestSyncNeverLosesUnflushedWritesInSimulation(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(Options{Dir: dir, Sync: SyncNever})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := s.Put(keyFor(i), []byte(valFor(i))); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	path := filepath.Join(dir, activeLogName)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the OS losing pages that were never fsynced: keep only the
	// first half of the file. This is a simulation, not a real power cut.
	if err := os.Truncate(path, st.Size()/2); err != nil {
		t.Fatal(err)
	}

	re, err := Open(Options{Dir: dir, Sync: SyncNever})
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()

	if re.Len() == 10 {
		t.Fatal("all writes survived a simulated loss of unflushed pages")
	}
	if _, err := re.Get(keyFor(0)); err != nil {
		t.Fatalf("the oldest write was lost too: %v", err)
	}
}
