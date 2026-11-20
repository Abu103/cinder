package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func openTestLog(t *testing.T, policy SyncPolicy) (*Log, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "000001.log")
	l, err := OpenLog(path, policy)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, path
}

func TestAppendReturnsContiguousOffsets(t *testing.T) {
	l, _ := openTestLog(t, SyncNever)

	recs := []Record{
		{Key: "a", Value: []byte("1"), Timestamp: 1},
		{Key: "bb", Value: []byte("22"), Timestamp: 2},
		{Key: "ccc", Timestamp: 3, Tombstone: true},
	}

	var wantOffset int64
	for i, rec := range recs {
		off, size, err := l.Append(rec)
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		if off != wantOffset {
			t.Fatalf("Append %d offset = %d, want %d", i, off, wantOffset)
		}
		if want := len(Encode(rec)); size != want {
			t.Fatalf("Append %d size = %d, want %d", i, size, want)
		}
		wantOffset += int64(size)
	}

	if got := l.Size(); got != wantOffset {
		t.Fatalf("Size = %d, want %d", got, wantOffset)
	}
}

func TestReadAtReturnsTheRecordAppendedThere(t *testing.T) {
	l, path := openTestLog(t, SyncNever)

	recs := []Record{
		{Key: "a", Value: []byte("1"), Timestamp: 10},
		{Key: "bb", Value: bytes.Repeat([]byte("x"), 4096), Timestamp: 11},
		{Key: "", Value: []byte{}, Timestamp: 12},
		{Key: "gone", Timestamp: 13, Tombstone: true},
	}
	type at struct {
		off, size int64
	}
	var locs []at
	for _, rec := range recs {
		off, size, err := l.Append(rec)
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		locs = append(locs, at{off, int64(size)})
	}

	for i, loc := range locs {
		got, err := l.ReadAt(loc.off, loc.size)
		if err != nil {
			t.Fatalf("ReadAt %d: %v", i, err)
		}
		if got.Key != recs[i].Key || !bytes.Equal(got.Value, recs[i].Value) ||
			got.Timestamp != recs[i].Timestamp || got.Tombstone != recs[i].Tombstone {
			t.Fatalf("ReadAt %d = %+v, want %+v", i, got, recs[i])
		}
	}

	// The bytes on disk really are the encoded records, back to back.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want []byte
	for _, rec := range recs {
		want = append(want, Encode(rec)...)
	}
	if !bytes.Equal(raw, want) {
		t.Fatalf("file bytes differ from the concatenated records")
	}
}

func TestAppendAfterReopenContinuesAtEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "000001.log")

	l, err := OpenLog(path, SyncAlways)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, _, err := l.Append(Record{Key: "k", Value: []byte("v"), Timestamp: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	end := l.Size()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	re, err := OpenLog(path, SyncAlways)
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()
	if got := re.Size(); got != end {
		t.Fatalf("Size after reopen = %d, want %d", got, end)
	}
	off, _, err := re.Append(Record{Key: "last", Value: []byte("v"), Timestamp: 99})
	if err != nil {
		t.Fatal(err)
	}
	if off != end {
		t.Fatalf("first append after reopen at %d, want %d", off, end)
	}
	got, err := re.ReadAt(off, int64(len(Encode(Record{Key: "last", Value: []byte("v"), Timestamp: 99}))))
	if err != nil {
		t.Fatal(err)
	}
	if got.Key != "last" {
		t.Fatalf("re-read key = %q, want %q", got.Key, "last")
	}
}

func TestSyncPoliciesWriteIdenticalBytes(t *testing.T) {
	recs := []Record{
		{Key: "a", Value: []byte("1"), Timestamp: 1},
		{Key: "b", Value: []byte("2"), Timestamp: 2},
	}

	dir := t.TempDir()
	files := make(map[SyncPolicy][]byte)
	for _, policy := range []SyncPolicy{SyncAlways, SyncNever} {
		path := filepath.Join(dir, fmt.Sprintf("log-%d", policy))
		l, err := OpenLog(path, policy)
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range recs {
			if _, _, err := l.Append(rec); err != nil {
				t.Fatal(err)
			}
		}
		if err := l.Close(); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		files[policy] = b
	}

	if !bytes.Equal(files[SyncAlways], files[SyncNever]) {
		t.Fatalf("sync policy changed the bytes on disk")
	}
}

func TestReadAtWithShortRangeIsAnError(t *testing.T) {
	l, _ := openTestLog(t, SyncNever)
	off, size, err := l.Append(Record{Key: "a", Value: []byte("value"), Timestamp: 1})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := l.ReadAt(off, int64(size-1)); err == nil {
		t.Fatal("ReadAt with a range shorter than the record returned nil error")
	}
	if _, err := l.ReadAt(-1, 10); err == nil {
		t.Fatal("ReadAt with a negative offset returned nil error")
	}
}

func TestReadAtDetectsCorruptRecord(t *testing.T) {
	l, path := openTestLog(t, SyncNever)
	off, size, err := l.Append(Record{Key: "a", Value: []byte("v"), Timestamp: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xFF
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	re, err := OpenLog(path, SyncNever)
	if err != nil {
		t.Fatal(err)
	}
	defer re.Close()
	if _, err := re.ReadAt(off, int64(size)); !errors.Is(err, ErrChecksum) {
		t.Fatalf("ReadAt err = %v, want ErrChecksum", err)
	}
}

func BenchmarkAppend(b *testing.B) {
	rec := Record{Key: "bench-key", Value: bytes.Repeat([]byte("v"), 64), Timestamp: 1}
	for _, tc := range []struct {
		name   string
		policy SyncPolicy
	}{
		{"SyncAlways", SyncAlways},
		{"SyncNever", SyncNever},
	} {
		b.Run(tc.name, func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "bench.log")
			l, err := OpenLog(path, tc.policy)
			if err != nil {
				b.Fatal(err)
			}
			defer l.Close()

			b.ResetTimer()
			b.SetBytes(int64(len(Encode(rec))))
			for i := 0; i < b.N; i++ {
				if _, _, err := l.Append(rec); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
