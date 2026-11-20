# Cinder — working notes

## Task 1: record format

One record, packed, big-endian, no padding. Header is 21 bytes; key bytes
follow immediately, value bytes after the key.

| Offset | Size | Field | Notes |
|---|---|---|---|
| 0 | 4 | CRC-32C | Castagnoli, over **bytes 4 to end** (header tail + key + value) |
| 4 | 8 | timestamp | signed unix nanoseconds |
| 12 | 4 | key length | uint32 |
| 16 | 4 | value length | uint32 |
| 20 | 1 | flag | 0 = live, 1 = tombstone |
| 21 | n | key | raw bytes, not NUL-terminated |
| 21+n | m | value | raw bytes |

### Hex dump of a real encoded record

`Record{Key: "alpha", Value: []byte("one"), Timestamp: 1700000000000000000}` → 29 bytes,
produced by `store.Encode`:

```
offset  bytes                            field
00      bc 3d 5f 59                      CRC-32C = 0xbc3d5f59
04      17 97 9c fe 36 2a 00 00          timestamp = 1700000000000000000
12      00 00 00 05                      key length = 5
16      00 00 00 03                      value length = 3
20      00                               flag = 0 (live)
21      61 6c 70 68 61                   "alpha"
26      6f 6e 65                         "one"
```

### Decisions taken

- **CRC covers the header tail too**, so a flipped key, length or timestamp is
  caught, not just a flipped value.
- **Length guard before allocation.** `Decode` rejects `keyLen+valLen >
  64 MiB` with `ErrRecordTooLarge` *before* allocating. A corrupt length field
  on disk is otherwise a 2 GiB allocation, i.e. a denial of service on your own
  database.
- **`io.EOF` vs `io.ErrUnexpectedEOF`.** A clean end of file returns `io.EOF`;
  a half-written record returns `io.ErrUnexpectedEOF`. Recovery (Task 4) uses
  exactly this distinction to decide "torn tail, truncate" vs "corruption,
  refuse".
- **`nil` and `[]byte{}` are the same on disk.** Zero value length, nothing
  stored, and `Decode` returns a nil `Value`. There is no spare bit to
  distinguish them, and nothing needs to.
- **Empty key is legal.** `""` round-trips; `Get("")` just hits the index like
  any other key.

### Test coverage (store/record_test.go)

Round trip (9 cases incl. 1 MB value, UTF-8 key, max/negative timestamp),
byte-exact layout, tombstone flag, corruption in value/key/timestamp, clean
EOF, truncated header, truncated payload, oversized length with a *valid* CRC.

## Task 2: append-only log

`Log` holds an `*os.File` opened `O_CREATE|O_RDWR|O_APPEND` plus `end`, the
offset of the next byte. `Append` encodes, writes, advances `end`, then syncs
if the policy is `SyncAlways`. It never calls `Seek`, and it advances `end`
*before* the sync so a failed fsync cannot cause the next append to overwrite
the bytes already in the file.

`ReadAt(offset, size)` decodes through an `io.SectionReader`, so a short range
fails cleanly instead of silently reading the neighbouring record's bytes.

### Benchmark: append rate by sync policy

`go test ./store -run '^$' -bench BenchmarkAppend -benchtime=2000x -count=5`,
64-byte value (84-byte record), i5-1135G7 / Windows, median of 5 runs:

| Policy | Median ns/op | Writes/sec | Relative |
|---|---|---|---|
| `SyncAlways` | 956,118 | ~1,046 | 1x |
| `SyncNever` | 2,513 | ~397,900 | ~380x faster |

The roadmap guessed 10-100x; on Windows `FlushFileBuffers` costs ~1 ms and the
real gap is **~380x**. That is the honest number, and it makes the point
better: a durable write is three orders of magnitude more expensive than one
that only reached the OS cache.

## Task 3: index, Put, Get, Delete

`entry{offset, size, ts}` plus `index map[string]entry`. `Put` appends, then
points the index at the new offset; `Get` does one `ReadAt`. `Delete` appends a
tombstone and drops the key from the index — the write matters more than the
map edit, because that is what survives a restart.

Decisions:

- **`Get` returns a fresh slice every call.** The value is decoded out of the
  file, so a caller mutating its result cannot corrupt the store. Tested.
- **Empty value is not a missing key.** `Put("k", []byte{})` then `Get("k")`
  returns zero bytes and a nil error; `ErrNotFound` is a separate sentinel.
- **`Put` enforces `MaxRecordSize`** before writing, so a 100 MiB value is
  refused with `ErrRecordTooLarge` rather than written and then unreadable.
- **`Delete` of an unknown key is a no-op, not an error.** It still writes a
  tombstone, so a delete that races ahead of the data (or arrives after a
  partial restore) still wins on replay.
- **`Open` creates `Dir` and `000001.log`.** Task 4 adds the replay scan.

Load test: 100,000 `Put` + 100,000 `Get` in **0.84 s** (`SyncNever`, 2-core
laptop), no errors.

## Task 4: crash recovery

`Open` replays the log from byte 0 with a buffered reader over an
`io.SectionReader`. Each decoded record updates the index (tombstone → delete),
`cur` advances by the record's *actual* consumed length, and `Stats()` reports
`Records` and `BytesCut`.

The tail rule, which is the whole point of the step:

| What happened | Evidence | Action |
|---|---|---|
| Clean end of file | `io.EOF` at `cur == size` | nothing |
| Torn write | short read, and nothing follows the damaged record | `Truncate(cur)`, count `BytesCut` |
| Real corruption | decode/CRC failure **with bytes still after it** | `ErrCorrupt`, do not truncate |

"Nothing follows it" is computed as `size - (cur + consumed) == 0`, where
`consumed` is bytes actually read. That single comparison is what stops a
flipped bit in record 50 from silently deleting records 51–99, and it is the
mistake the roadmap's tip warns about. Tested both ways: a 7-byte garbage tail
is cut, a corrupt record with 49 good records behind it refuses to open and
leaves the file size untouched.

### A Windows trap worth recording

The roadmap says open the log with `O_APPEND`. On Windows a handle opened with
`O_APPEND` is granted `FILE_APPEND_DATA` only, and `SetEndOfFile` then fails
with **"Access is denied"** — so torn-tail recovery is impossible. Reproduced in
isolation (`O_RDWR|O_APPEND` fails, `O_RDWR` succeeds). Cinder therefore opens
`O_CREATE|O_RDWR` and writes with `WriteAt(trackedOffset)`. Nothing is lost: the
store serializes appends under its own mutex, so append-mode atomicity was
never doing any work for us.

### Reopen time at 1,000,000 records

`go test ./store -run '^$' -bench BenchmarkOpen -benchtime=3x`, 8-char keys,
5-byte values, 35-byte records:

| Records | Reopen (median) | Rate |
|---|---|---|
| 1,000,000 | **0.96 s** | ~1.04 M records/s |

Fast enough that a cold start on a million keys is a sub-second operation, and
slow enough to be worth reporting.

## Task 5: segments and compaction

Segments are `000001.log`, `000002.log`, … Active file is capped at
`Options.SegmentSize` (default 64 MiB, 256/512 bytes in tests). Rotation only
ever happens *between* records, so a sealed segment is never half-written —
which is what makes the recovery rule in Task 4 hold per segment.

Index entries now carry `fileID`, so `Get` follows a key into whichever segment
holds its newest value.

### The manifest, and the bug it exists to prevent

Compaction writes every live record into new segments and drops tombstones.
The obvious order — write new files, delete old ones — has a hole: if you crash
after the new files exist but before the old ones are gone, replay sees *both*.
The compacted copy has no tombstone, but the old segment still has the original
`Put` for the deleted key, and depending on id order the deleted key comes back
from the dead. That is the classic Bitcask resurrection bug, and there is no
in-memory state during replay to fix it with.

So Cinder keeps a `MANIFEST`: a tiny file listing the live segment ids.

```
cinder-manifest-v1
000004
000005
```

Recovery replays **only** what the manifest names. Compaction writes its output
to `NNNNNN.log.tmp`, renames each into place, then atomically installs a new
manifest (write `MANIFEST.tmp`, `os.Rename`), and only then closes and deletes
the old segments. Crash windows:

| Crash point | On next open |
|---|---|
| mid-write of compacted output | old manifest wins; `.tmp` files and unlisted segments deleted as orphans |
| after manifest installed, before old files deleted | new manifest wins; old segments deleted as orphans |
| during manifest rename | rename is atomic: either old or new manifest, never half |

Ordering guarantees the manifest never names an incomplete file, and
tombstones never need to survive compaction because the records they killed are
not copied.

`TestCompactionDropsDeletedKeysForGood` is the regression test: put `dead`,
delete it, compact, reopen, and `Get("dead")` must still be `ErrNotFound`.

### Results

Compaction, 100 keys × 100 overwrites of a 64-byte value:

```
disk usage before=900000 bytes after=9000 bytes reduction=99.0%
```

That is a 100x shrink, well past the 90% the roadmap asks for, and a restart
after compaction returns all 100 keys with the compacted values.

Files opened for reading are never closed behind the caller's back: old
segments are closed *before* `os.Remove`, which Windows requires anyway.

## Task 6: concurrency

One `sync.RWMutex` guards the index, the segment map, `activeID` and `nextID`.

| Operation | Lock | Why |
|---|---|---|
| `Get` | `RLock`, held through the `ReadAt` | compaction must not close the segment mid-read |
| `Put`, `Delete`, rotation | `Lock` | index + append + manifest must move together |
| `Compact` | `Lock` | swaps the whole index and closes old handles |
| `Len`, `Stats` | `RLock` | cheap snapshots |
| `Close` | `Lock` | no read may outlive its file |

The roadmap suggests copying during compaction without holding the write lock.
That needs segment handles that survive being closed, i.e. a refcount or a
generation scheme, and it buys nothing at this size: compaction of a 1 MB
segment is milliseconds. Correct first; `NOTES.md` records this as the known
tradeoff (writes are blocked while compacting, reads are not blocked by each
other).

Verification:

```
go test ./... -race -count=1     # 34.4 s, full 10 s concurrency windows
go test ./... -race -short -count=1   # 5.0 s
```

Both clean. The first run before the mutex existed reported a genuine race
between `Log.Append` and `Log.Size` from two `Put` goroutines — that report is
why the test suite runs under `-race` from here on.

Tests: 50 goroutines mixing Put/Get/Delete for 10 s; 40 readers against 10
writers where a `Get` of a never-deleted key must never return `ErrNotFound`;
8 writers plus a goroutine compacting in a loop, then a reopen that must still
see every stable key.

## Task 7: crash harness

Two programs and one rule.

- `cmd/writer` loops `Put("k< i >", "value-k< i >")` and prints the key to
  stdout **only after `Put` returned**. `os.Stdout` is unbuffered, so the line
  leaves the process in one write.
- `cmd/crashtest` starts the writer, reads its stdout, kills it after a random
  5–500 ms, reopens the store, and checks that every key it read is present
  with the right value.

The acknowledgement rule is the whole test. A complete line (`\n` seen) is an
acknowledged write; a partial last line is discarded, because the reader cannot
know whether that `Put` returned. Counting a written-but-unacknowledged key
produces false failures — the roadmap's stated common mistake, avoided here.

Two implementation details that matter:

- **The harness builds `cmd/writer` and execs the binary.** `go run` would leave
  the compiled child alive after its parent is killed, so the "crash" would not
  be a crash.
- **All reads finish before `cmd.Wait()`.** `Wait` closes the stdout pipe, so
  calling it first can drop the last acknowledged lines and report lost data
  that was never lost.

### Results

```
$ go run ./cmd/crashtest -runs 200
cinder crashtest: 200 runs, kills after 5-500 ms, sync=always
200/200 passed in 56.27s

$ go run ./cmd/crashtest -runs 200 -sync never
cinder crashtest: 200 runs, kills after 5-500 ms, sync=never
200/200 passed in 1m35.186s
```

The `sync=never` run takes longer because the writer acknowledges far more keys
in the same window, so there is more to verify.

Honest limit, stated in the harness output itself: `Process.Kill` tests
process crashes, and the OS still flushes its page cache afterwards. Only a
power cut tests `fsync`. That is exactly why `SyncAlways` exists, and why the
report must not claim more than this test proves.






