package store

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(Options{Dir: t.TempDir(), Sync: SyncNever})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestPutGet(t *testing.T) {
	s := openTestStore(t)

	cases := []struct{ key, val string }{
		{"a", "1"},
		{"", "empty key is legal"},
		{"unicode-ключ", "значение"},
		{"long", string(bytes.Repeat([]byte("x"), 1<<20))},
		{"empty-value", ""},
	}
	for _, tc := range cases {
		if err := s.Put(tc.key, []byte(tc.val)); err != nil {
			t.Fatalf("Put(%q): %v", tc.key, err)
		}
	}
	for _, tc := range cases {
		got, err := s.Get(tc.key)
		if err != nil {
			t.Fatalf("Get(%q): %v", tc.key, err)
		}
		if string(got) != tc.val {
			t.Fatalf("Get(%q) = %q, want %q", tc.key, got, tc.val)
		}
	}
}

func TestGetMissingKey(t *testing.T) {
	s := openTestStore(t)

	_, err := s.Get("nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get err = %v, want ErrNotFound", err)
	}
}

func TestGetEmptyValueIsNotAMissingKey(t *testing.T) {
	s := openTestStore(t)
	if err := s.Put("k", []byte{}); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get("k")
	if err != nil {
		t.Fatalf("Get err = %v, want nil for a stored empty value", err)
	}
	if len(got) != 0 {
		t.Fatalf("Get = %q, want empty", got)
	}
}

func TestOverwriteReturnsLatestValue(t *testing.T) {
	s := openTestStore(t)

	for i := 0; i < 1000; i++ {
		if err := s.Put("counter", []byte(fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get("counter")
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != fmt.Sprint(i) {
			t.Fatalf("after write %d, Get = %q", i, got)
		}
	}
}

func TestDelete(t *testing.T) {
	s := openTestStore(t)
	if err := s.Put("k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("k"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Get("k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete err = %v, want ErrNotFound", err)
	}
	// Deleting a missing key is a no-op, not an error.
	if err := s.Delete("k"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if err := s.Delete("never-existed"); err != nil {
		t.Fatalf("Delete of unknown key: %v", err)
	}
}

func TestPutAfterDeleteBringsKeyBack(t *testing.T) {
	s := openTestStore(t)
	for _, step := range []struct {
		del bool
		val string
	}{
		{false, "one"},
		{true, ""},
		{false, "two"},
	} {
		if step.del {
			if err := s.Delete("k"); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := s.Put("k", []byte(step.val)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.Get("k")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "two" {
		t.Fatalf("Get = %q, want %q", got, "two")
	}
}

func TestGetReturnsACopy(t *testing.T) {
	s := openTestStore(t)
	if err := s.Put("k", []byte("original")); err != nil {
		t.Fatal(err)
	}

	first, err := s.Get("k")
	if err != nil {
		t.Fatal(err)
	}
	first[0] = 'X' // a caller scribbling on its result must not corrupt the store

	second, err := s.Get("k")
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != "original" {
		t.Fatalf("second Get = %q, want %q", second, "original")
	}
}

func TestPutRejectsOversizedRecord(t *testing.T) {
	s := openTestStore(t)
	huge := bytes.Repeat([]byte("x"), MaxRecordSize+1)

	err := s.Put("k", huge)
	if !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("Put err = %v, want ErrRecordTooLarge", err)
	}
	if _, err := s.Get("k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected Put still created the key: %v", err)
	}
}

func TestLoad100kKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 100k-key load test in -short mode")
	}
	s := openTestStore(t)

	const n = 100_000
	for i := 0; i < n; i++ {
		if err := s.Put(fmt.Sprintf("key-%06d", i), []byte(fmt.Sprintf("value-%d", i))); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("key-%06d", i)
		got, err := s.Get(key)
		if err != nil {
			t.Fatalf("Get %s: %v", key, err)
		}
		if want := fmt.Sprintf("value-%d", i); string(got) != want {
			t.Fatalf("Get %s = %q, want %q", key, got, want)
		}
	}
}

func TestOpenCreatesTheDirectory(t *testing.T) {
	dir := t.TempDir() + "/nested/deeper"
	s, err := Open(Options{Dir: dir, Sync: SyncNever})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if err := s.Put("k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get("k"); err != nil || string(got) != "v" {
		t.Fatalf("Get = %q, %v", got, err)
	}
}
