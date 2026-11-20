package store

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

var benchSizes = []int{10_000, 100_000, 1_000_000}

func sizeName(n int) string {
	switch n {
	case 10_000:
		return "10k"
	case 100_000:
		return "100k"
	case 1_000_000:
		return "1M"
	}
	return fmt.Sprint(n)
}

func benchValue() []byte { return bytes.Repeat([]byte("v"), 32) }

// BenchmarkLoad measures filling an empty store with n keys, sequentially.
func BenchmarkLoad(b *testing.B) {
	val := benchValue()
	for _, n := range benchSizes {
		b.Run(sizeName(n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				s, err := Open(Options{Dir: b.TempDir(), Sync: SyncNever})
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()

				for j := 0; j < n; j++ {
					if err := s.Put(fmt.Sprintf("key-%08d", j), val); err != nil {
						b.Fatal(err)
					}
				}

				b.StopTimer()
				_ = s.Close()
			}
		})
	}
}

// BenchmarkRandomRead measures one Get of a random existing key.
func BenchmarkRandomRead(b *testing.B) {
	val := benchValue()
	for _, n := range benchSizes {
		b.Run(sizeName(n), func(b *testing.B) {
			s, err := Open(Options{Dir: b.TempDir(), Sync: SyncNever})
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()

			keys := make([]string, n)
			for j := 0; j < n; j++ {
				keys[j] = fmt.Sprintf("key-%08d", j)
				if err := s.Put(keys[j], val); err != nil {
					b.Fatal(err)
				}
			}

			r := rand.New(rand.NewSource(1))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.Get(keys[r.Intn(n)]); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkReopen measures recovery time for a log of n records.
func BenchmarkReopen(b *testing.B) {
	val := benchValue()
	for _, n := range benchSizes {
		b.Run(sizeName(n), func(b *testing.B) {
			dir := b.TempDir()
			s, err := Open(Options{Dir: dir, Sync: SyncNever})
			if err != nil {
				b.Fatal(err)
			}
			for j := 0; j < n; j++ {
				if err := s.Put(fmt.Sprintf("key-%08d", j), val); err != nil {
					b.Fatal(err)
				}
			}
			if err := s.Close(); err != nil {
				b.Fatal(err)
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				re, err := Open(Options{Dir: dir, Sync: SyncNever})
				if err != nil {
					b.Fatal(err)
				}
				if got := re.Len(); got != n {
					b.Fatalf("replayed %d keys, want %d", got, n)
				}
				if err := re.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
