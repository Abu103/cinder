package main

import (
	"errors"
	"fmt"

	"cinder/internal/crashproto"
	"cinder/store"
)

// verify reopens the store in dir and checks that every key the writer
// acknowledged is present with the value the protocol assigns to it. A torn
// tail from the kill is not a failure: recovery is expected to cut it.
func verify(dir string, acked []string) error {
	s, err := store.Open(store.Options{Dir: dir, Sync: store.SyncNever})
	if err != nil {
		return fmt.Errorf("reopen store: %w", err)
	}
	defer s.Close()

	for _, key := range acked {
		got, err := s.Get(key)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("acknowledged key %q is gone", key)
		}
		if err != nil {
			return fmt.Errorf("read %q: %w", key, err)
		}
		if want := crashproto.Value(key); string(got) != want {
			return fmt.Errorf("key %q = %q, want %q", key, got, want)
		}
	}
	return nil
}
