package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"math"
	"testing"
)

// castagnoli is the table spec'd for Cinder: CRC-32C, not the IEEE polynomial.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// makeHeader builds a record header with a *valid* CRC for the given fields.
// Tests use it to reach past the checksum and exercise the length guard.
func makeHeader(t *testing.T, keyLen, valLen uint32, flag byte, ts int64) []byte {
	t.Helper()
	body := make([]byte, HeaderSize-4)
	binary.BigEndian.PutUint64(body[0:8], uint64(ts))
	binary.BigEndian.PutUint32(body[8:12], keyLen)
	binary.BigEndian.PutUint32(body[12:16], valLen)
	body[16] = flag

	h := make([]byte, HeaderSize)
	binary.BigEndian.PutUint32(h[0:4], crc32.Checksum(body, castagnoli))
	copy(h[4:], body)
	return h
}

func TestHeaderSizeIs21Bytes(t *testing.T) {
	// 4 CRC + 8 timestamp + 4 key length + 4 value length + 1 flag, packed.
	if HeaderSize != 21 {
		t.Fatalf("HeaderSize = %d, want 21", HeaderSize)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		rec  Record
	}{
		{"simple", Record{Key: "alpha", Value: []byte("one"), Timestamp: 1}},
		{"empty value", Record{Key: "alpha", Value: []byte{}, Timestamp: 2}},
		{"nil value", Record{Key: "alpha", Timestamp: 3}},
		{"empty key", Record{Key: "", Value: []byte("v"), Timestamp: 4}},
		{"tombstone", Record{Key: "alpha", Timestamp: 5, Tombstone: true}},
		{"unicode", Record{Key: "ключ", Value: []byte("значение"), Timestamp: 6}},
		{"max timestamp", Record{Key: "k", Timestamp: math.MaxInt64}},
		{"negative timestamp", Record{Key: "k", Timestamp: -1}},
		{"one megabyte value", Record{Key: "big", Value: bytes.Repeat([]byte("x"), 1<<20), Timestamp: 7}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Decode(bytes.NewReader(Encode(tc.rec)))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got.Key != tc.rec.Key {
				t.Errorf("Key = %q, want %q", got.Key, tc.rec.Key)
			}
			if !bytes.Equal(got.Value, tc.rec.Value) {
				t.Errorf("Value length = %d, want %d", len(got.Value), len(tc.rec.Value))
			}
			if got.Timestamp != tc.rec.Timestamp {
				t.Errorf("Timestamp = %d, want %d", got.Timestamp, tc.rec.Timestamp)
			}
			if got.Tombstone != tc.rec.Tombstone {
				t.Errorf("Tombstone = %v, want %v", got.Tombstone, tc.rec.Tombstone)
			}
			if len(got.Value) != len(tc.rec.Value) {
				t.Errorf("len(Value) = %d, want %d", len(got.Value), len(tc.rec.Value))
			}
		})
	}
}

func TestEncodedLayout(t *testing.T) {
	const ts = int64(0x0102030405060708)
	rec := Record{Key: "k", Value: []byte("vv"), Timestamp: ts}
	buf := Encode(rec)

	if want := HeaderSize + len(rec.Key) + len(rec.Value); len(buf) != want {
		t.Fatalf("len(Encode) = %d, want %d", len(buf), want)
	}
	if got := int64(binary.BigEndian.Uint64(buf[4:12])); got != ts {
		t.Errorf("timestamp bytes = %#x, want %#x", got, ts)
	}
	if got := binary.BigEndian.Uint32(buf[12:16]); got != 1 {
		t.Errorf("key length = %d, want 1", got)
	}
	if got := binary.BigEndian.Uint32(buf[16:20]); got != 2 {
		t.Errorf("value length = %d, want 2", got)
	}
	if got := buf[20]; got != 0 {
		t.Errorf("flag = %d, want 0 for a live record", got)
	}
	if got := string(buf[21:22]); got != "k" {
		t.Errorf("key bytes = %q, want %q", got, "k")
	}
	if got := string(buf[22:24]); got != "vv" {
		t.Errorf("value bytes = %q, want %q", got, "vv")
	}
	if got, want := binary.BigEndian.Uint32(buf[0:4]), crc32.Checksum(buf[4:], castagnoli); got != want {
		t.Errorf("CRC = %#x, want %#x over everything after the CRC field", got, want)
	}
}

func TestEncodedTombstoneFlag(t *testing.T) {
	buf := Encode(Record{Key: "gone", Timestamp: 1, Tombstone: true})
	if buf[20] != 1 {
		t.Errorf("flag = %d, want 1 for a tombstone", buf[20])
	}
}

func TestDecodeRejectsCorruptValue(t *testing.T) {
	buf := Encode(Record{Key: "alpha", Value: []byte("one"), Timestamp: 1})
	buf[len(buf)-1] ^= 0xFF // flip a bit in the value

	_, err := Decode(bytes.NewReader(buf))
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("Decode err = %v, want ErrChecksum", err)
	}
}

func TestDecodeRejectsCorruptKey(t *testing.T) {
	buf := Encode(Record{Key: "alpha", Value: []byte("one"), Timestamp: 1})
	buf[HeaderSize] ^= 0xFF // flip a bit in the key, not the value

	_, err := Decode(bytes.NewReader(buf))
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("Decode err = %v, want ErrChecksum", err)
	}
}

func TestDecodeRejectsCorruptTimestamp(t *testing.T) {
	buf := Encode(Record{Key: "alpha", Value: []byte("one"), Timestamp: 1})
	buf[4] ^= 0x01 // timestamp is before the length fields

	_, err := Decode(bytes.NewReader(buf))
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("Decode err = %v, want ErrChecksum", err)
	}
}

func TestDecodeEmptyReaderIsEOF(t *testing.T) {
	// Recovery needs to tell "clean end of log" from "torn record".
	_, err := Decode(bytes.NewReader(nil))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Decode err = %v, want io.EOF", err)
	}
}

func TestDecodeTruncatedHeader(t *testing.T) {
	buf := Encode(Record{Key: "alpha", Value: []byte("one"), Timestamp: 1})

	_, err := Decode(bytes.NewReader(buf[:10]))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Decode err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestDecodeTruncatedPayload(t *testing.T) {
	buf := Encode(Record{Key: "alpha", Value: []byte("one"), Timestamp: 1})

	_, err := Decode(bytes.NewReader(buf[:len(buf)-1]))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Decode err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestDecodeRejectsOversizedLength(t *testing.T) {
	// Header is internally consistent (CRC matches), so only the size guard
	// can reject it. Without the guard this allocates 2 GiB.
	buf := makeHeader(t, 1<<31, 0, 0, 1)

	_, err := Decode(bytes.NewReader(buf))
	if !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("Decode err = %v, want ErrRecordTooLarge", err)
	}
}
