// Package store implements Cinder, a Bitcask-style crash-safe key-value store.
//
// Every write is appended to a log record with a checksum; an in-memory index
// maps each key to the offset of its newest record, and recovery replays the
// log on open.
package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// Record is one entry in the log.
type Record struct {
	Key       string
	Value     []byte
	Timestamp int64 // unix nanoseconds
	Tombstone bool
}

// HeaderSize is the packed on-disk header, all fields big-endian:
//
//	0..4    CRC-32C over bytes 4..end (header remainder + key + value)
//	4..12   timestamp
//	12..16  key length
//	16..20  value length
//	20      flag: 0 live, 1 tombstone
//
// Key bytes follow at 21, value bytes after the key.
const HeaderSize = 21

const (
	flagLive      = 0
	flagTombstone = 1
)

// MaxRecordSize caps key+value so that a corrupt length field cannot make the
// reader allocate gigabytes.
const MaxRecordSize = 64 << 20

var (
	// ErrChecksum means a record was read but its CRC did not match its bytes.
	ErrChecksum = errors.New("cinder: checksum mismatch")
	// ErrRecordTooLarge means the encoded key+value length exceeds MaxRecordSize.
	ErrRecordTooLarge = errors.New("cinder: record exceeds max size")
	// ErrBadFlag means the flag byte is neither live nor tombstone.
	ErrBadFlag = errors.New("cinder: unknown record flag")
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// Encode returns the on-disk bytes for r.
func Encode(r Record) []byte {
	buf := make([]byte, HeaderSize+len(r.Key)+len(r.Value))
	binary.BigEndian.PutUint64(buf[4:12], uint64(r.Timestamp))
	binary.BigEndian.PutUint32(buf[12:16], uint32(len(r.Key)))
	binary.BigEndian.PutUint32(buf[16:20], uint32(len(r.Value)))
	if r.Tombstone {
		buf[20] = flagTombstone
	}
	copy(buf[HeaderSize:], r.Key)
	copy(buf[HeaderSize+len(r.Key):], r.Value)
	binary.BigEndian.PutUint32(buf[0:4], crc32.Checksum(buf[4:], crcTable))
	return buf
}

// Decode reads one record from r.
//
// A clean end of stream returns io.EOF; a partially written record returns
// io.ErrUnexpectedEOF. Callers use that difference to tell a torn tail from a
// corrupt middle.
func Decode(r io.Reader) (Record, error) {
	rec, _, err := decode(r)
	return rec, err
}

// decode is Decode plus the number of bytes actually consumed, which recovery
// needs to know whether the failure was at the tail of the file or inside it.
func decode(r io.Reader) (Record, int64, error) {
	var hdr [HeaderSize]byte
	n, err := io.ReadFull(r, hdr[:])
	if err != nil {
		return Record{}, int64(n), err
	}

	keyLen := binary.BigEndian.Uint32(hdr[12:16])
	valLen := binary.BigEndian.Uint32(hdr[16:20])
	total := int64(keyLen) + int64(valLen)
	if total > MaxRecordSize {
		return Record{}, HeaderSize, fmt.Errorf("%w: %d bytes", ErrRecordTooLarge, total)
	}

	flag := hdr[20]
	if flag != flagLive && flag != flagTombstone {
		return Record{}, HeaderSize, fmt.Errorf("%w: %d", ErrBadFlag, flag)
	}

	var payload []byte
	if total > 0 {
		payload = make([]byte, total)
		m, err := io.ReadFull(r, payload)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return Record{}, int64(n + m), err
		}
	}

	crc := crc32.New(crcTable)
	crc.Write(hdr[4:])
	crc.Write(payload)
	if binary.BigEndian.Uint32(hdr[0:4]) != crc.Sum32() {
		return Record{}, HeaderSize + total, fmt.Errorf("%w: key %q", ErrChecksum, payload[:keyLen])
	}

	rec := Record{
		Key:       string(payload[:keyLen]),
		Timestamp: int64(binary.BigEndian.Uint64(hdr[4:12])),
		Tombstone: flag == flagTombstone,
	}
	if valLen > 0 {
		rec.Value = payload[keyLen:]
	}
	return rec, HeaderSize + total, nil
}
