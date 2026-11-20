package store

import (
	"fmt"
	"io"
	"os"
)

// SyncPolicy controls whether Append asks the OS to put the bytes on disk.
type SyncPolicy int

const (
	// SyncAlways fsyncs after every record, so an acknowledged Put survives a
	// power cut. Roughly 10-100x slower than SyncNever.
	SyncAlways SyncPolicy = iota
	// SyncNever leaves flushing to the OS. Survives a process kill, not a
	// power cut.
	SyncNever
)

// syncFile exists so tests can inject an fsync failure. Production code never
// reassigns it.
var syncFile = func(f *os.File) error { return f.Sync() }

// Log is one append-only file of records. Writes only ever happen at the end.
type Log struct {
	f      *os.File
	path   string
	policy SyncPolicy
	// end is the offset of the next byte to be written. Tracked here so
	// Append never has to Seek.
	end int64
}

// OpenLog opens (creating if needed) the log at path and positions the write
// cursor at the current end of the file.
//
// The file is opened O_RDWR without O_APPEND, and Append writes at a tracked
// offset with WriteAt. On Windows a handle opened with O_APPEND is granted
// FILE_APPEND_DATA only, and SetEndOfFile then fails with "Access is denied",
// which would make torn-tail recovery impossible. Tracked offsets cost nothing
// here because appended writes are serialized by the store's lock.
func OpenLog(path string, policy SyncPolicy) (*Log, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("cinder: open log %s: %w", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("cinder: stat log %s: %w", path, err)
	}
	return &Log{f: f, path: path, policy: policy, end: st.Size()}, nil
}

// Append writes r at the end of the log and returns the offset it landed at
// and its encoded size.
func (l *Log) Append(r Record) (offset int64, size int, err error) {
	buf := Encode(r)
	off := l.end

	n, err := l.f.WriteAt(buf, off)
	if err != nil {
		return 0, 0, fmt.Errorf("cinder: append to %s: %w", l.path, err)
	}
	if n != len(buf) {
		return 0, 0, fmt.Errorf("cinder: append to %s: %w", l.path, io.ErrShortWrite)
	}
	// Advance before syncing: even a failed fsync leaves the bytes in the file,
	// so the next append must not overwrite them.
	l.end += int64(n)

	if l.policy == SyncAlways {
		if err := syncFile(l.f); err != nil {
			return off, n, fmt.Errorf("cinder: sync %s: %w", l.path, err)
		}
	}
	return off, n, nil
}

// ReadAt decodes the record occupying [offset, offset+size).
func (l *Log) ReadAt(offset, size int64) (Record, error) {
	if offset < 0 || size < 0 || size > HeaderSize+MaxRecordSize {
		return Record{}, fmt.Errorf("cinder: bad read range offset=%d size=%d in %s", offset, size, l.path)
	}
	rec, err := Decode(io.NewSectionReader(l.f, offset, size))
	if err != nil {
		return Record{}, fmt.Errorf("cinder: read at %d in %s: %w", offset, l.path, err)
	}
	return rec, nil
}

// Size is the offset one past the last written byte.
func (l *Log) Size() int64 { return l.end }

// Truncate cuts the log to size bytes and moves the write cursor there.
// Recovery uses it to drop a half-written record from the tail.
func (l *Log) Truncate(size int64) error {
	if err := l.f.Truncate(size); err != nil {
		return fmt.Errorf("cinder: truncate %s to %d: %w", l.path, size, err)
	}
	l.end = size
	return nil
}

// Path is the file this log writes to.
func (l *Log) Path() string { return l.path }

// Close releases the file handle.
func (l *Log) Close() error {
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("cinder: close %s: %w", l.path, err)
	}
	return nil
}
