package store

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// replaySegment rebuilds index entries for one segment file.
//
// only the newest segment may end inside a record: rotation always happens
// between records, so damage in a sealed segment is corruption, not a torn
// write, and must not cost the records stored after it.
func (s *Store) replaySegment(id uint32, l *Log, newest bool) error {
	size := l.Size()
	if size == 0 {
		return nil
	}
	r := bufio.NewReaderSize(io.NewSectionReader(l.f, 0, size), 64<<10)

	cur := int64(0)
	for cur < size {
		rec, consumed, err := decode(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			remaining := size - (cur + consumed)
			if remaining > 0 || !newest {
				return fmt.Errorf("%w: segment %s at offset %d (%d bytes follow it): %w",
					ErrCorrupt, segName(id), cur, remaining, err)
			}

			cut := size - cur
			if err := l.Truncate(cur); err != nil {
				return err
			}
			s.stats.BytesCut += cut
			break
		}

		if rec.Tombstone {
			delete(s.index, rec.Key)
		} else {
			s.index[rec.Key] = entry{fileID: id, offset: cur, size: int(consumed), ts: rec.Timestamp}
		}
		cur += consumed
		s.stats.Records++
	}
	return nil
}
