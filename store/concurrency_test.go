package store

import (
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"
)

func concurrentWindow(t *testing.T) time.Duration {
	t.Helper()
	if testing.Short() {
		return time.Second
	}
	return 10 * time.Second
}

func TestConcurrentMixedWorkload(t *testing.T) {
	s := openSegmented(t, t.TempDir(), 16<<10)
	const keys = 100
	for i := 0; i < keys; i++ {
		if err := s.Put(keyFor(i), []byte(valFor(i))); err != nil {
			t.Fatal(err)
		}
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 50; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(g)))
			for {
				select {
				case <-stop:
					return
				default:
				}
				i := r.Intn(keys)
				switch r.Intn(3) {
				case 0:
					if err := s.Put(keyFor(i), []byte(valFor(i))); err != nil {
						t.Errorf("Put: %v", err)
						return
					}
				case 1:
					_, err := s.Get(keyFor(i))
					// A key can legitimately be deleted by another goroutine;
					// anything else is a bug.
					if err != nil && !errors.Is(err, ErrNotFound) {
						t.Errorf("Get: %v", err)
						return
					}
				case 2:
					if err := s.Delete(keyFor(i)); err != nil {
						t.Errorf("Delete: %v", err)
						return
					}
				}
			}
		}(g)
	}

	time.Sleep(concurrentWindow(t))
	close(stop)
	wg.Wait()

	// The store still works afterwards, and recovery agrees with it.
	if err := s.Put("after", []byte("storm")); err != nil {
		t.Fatal(err)
	}
	dir := s.opts.Dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	re := openSegmented(t, dir, 16<<10)
	if got, err := re.Get("after"); err != nil || string(got) != "storm" {
		t.Fatalf("Get(after) = %q, %v", got, err)
	}
}

func TestConcurrentReadersNeverLoseAStableKey(t *testing.T) {
	s := openSegmented(t, t.TempDir(), 16<<10)
	const keys = 50
	for i := 0; i < keys; i++ {
		if err := s.Put(keyFor(i), []byte(valFor(i))); err != nil {
			t.Fatal(err)
		}
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Writers only rewrite keys; nothing in this test deletes.
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(100 + g)))
			for {
				select {
				case <-stop:
					return
				default:
				}
				i := r.Intn(keys)
				if err := s.Put(keyFor(i), []byte(valFor(i))); err != nil {
					t.Errorf("Put: %v", err)
					return
				}
			}
		}(g)
	}
	for g := 0; g < 40; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(200 + g)))
			for {
				select {
				case <-stop:
					return
				default:
				}
				i := r.Intn(keys)
				got, err := s.Get(keyFor(i))
				if err != nil {
					t.Errorf("Get(%s) failed for a key that was never deleted: %v", keyFor(i), err)
					return
				}
				if string(got) != valFor(i) {
					t.Errorf("Get(%s) = %q, want %q", keyFor(i), got, valFor(i))
					return
				}
			}
		}(g)
	}

	time.Sleep(concurrentWindow(t))
	close(stop)
	wg.Wait()
}

func TestCompactionRunsWhileTheStoreIsBusy(t *testing.T) {
	dir := t.TempDir()
	s := openSegmented(t, dir, 8<<10)

	const stable = 40
	for i := 0; i < stable; i++ {
		if err := s.Put(keyFor(i), []byte(valFor(i))); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if err := s.Compact(); err != nil {
					t.Errorf("Compact: %v", err)
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
	}()

	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				// Writers touch their own key space, so every key stays
				// present and every value is deterministic.
				key := fmt.Sprintf("w%d-%04d", g, n%20)
				val := fmt.Sprintf("value-%d-%d", g, n%20)
				if err := s.Put(key, []byte(val)); err != nil {
					t.Errorf("Put: %v", err)
					return
				}
				got, err := s.Get(key)
				if err != nil {
					t.Errorf("Get(%s) during compaction: %v", key, err)
					return
				}
				if string(got) != val {
					t.Errorf("Get(%s) = %q, want %q", key, got, val)
					return
				}
			}
		}(g)
	}

	time.Sleep(concurrentWindow(t) / 2)
	close(stop)
	wg.Wait()

	for i := 0; i < stable; i++ {
		got, err := s.Get(keyFor(i))
		if err != nil {
			t.Fatalf("Get(%s) after concurrent compaction: %v", keyFor(i), err)
		}
		if string(got) != valFor(i) {
			t.Fatalf("Get(%s) = %q, want %q", keyFor(i), got, valFor(i))
		}
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	re := openSegmented(t, dir, 8<<10)
	for i := 0; i < stable; i++ {
		if got, err := re.Get(keyFor(i)); err != nil || string(got) != valFor(i) {
			t.Fatalf("after reopen Get(%s) = %q, %v", keyFor(i), got, err)
		}
	}
}
