package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cinder/internal/crashproto"
	"cinder/store"
)

func writeAcknowledged(t *testing.T, dir string, n int) []string {
	t.Helper()
	s, err := store.Open(store.Options{Dir: dir, Sync: store.SyncNever})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	keys := make([]string, 0, n)
	for i := 0; i < n; i++ {
		key := crashproto.Key(i)
		if err := s.Put(key, []byte(crashproto.Value(key))); err != nil {
			t.Fatalf("Put %s: %v", key, err)
		}
		keys = append(keys, key)
	}
	return keys
}

func TestVerifyPassesWhenEveryAcknowledgedKeySurvived(t *testing.T) {
	dir := t.TempDir()
	keys := writeAcknowledged(t, dir, 10)

	if err := verify(dir, keys); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestVerifyFailsWhenAnAcknowledgedKeyIsMissing(t *testing.T) {
	dir := t.TempDir()
	keys := writeAcknowledged(t, dir, 10)
	keys = append(keys, crashproto.Key(10)) // never written

	err := verify(dir, keys)
	if err == nil {
		t.Fatal("verify passed with a key that was never written")
	}
	if !strings.Contains(err.Error(), crashproto.Key(10)) {
		t.Fatalf("error %q does not name the missing key", err)
	}
}

func TestVerifyFailsWhenAValueIsWrong(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(store.Options{Dir: dir, Sync: store.SyncNever})
	if err != nil {
		t.Fatal(err)
	}
	key := crashproto.Key(0)
	if err := s.Put(key, []byte("corrupted")); err != nil {
		t.Fatal(err)
	}
	s.Close()

	if err := verify(dir, []string{key}); err == nil {
		t.Fatal("verify passed with the wrong value")
	}
}

func TestVerifyPassesOverATornTail(t *testing.T) {
	dir := t.TempDir()
	keys := writeAcknowledged(t, dir, 10)

	// A kill mid-write leaves a partial record in the newest segment.
	files, err := filepath.Glob(filepath.Join(dir, "*.log"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no segments: %v", err)
	}
	newest := files[len(files)-1]
	f, err := os.OpenFile(newest, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, 13)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := verify(dir, keys); err != nil {
		t.Fatalf("verify after a torn tail: %v", err)
	}
}
