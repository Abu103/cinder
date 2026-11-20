# Cinder: a crash-safe log-structured key-value store

**BCS752 Mini Project — project report**

## Abstract

Cinder is a key-value store written in Go that is designed around one
guarantee: a write that has been acknowledged to the caller survives a crash.
The design follows Bitcask: an append-only log of checksummed records, an
in-memory index from key to the file position of its newest record, and a
replay of the log at startup that rebuilds the index and discards a partially
written tail. The store supports segment rotation, background-safe compaction,
500 concurrent goroutines and a command-line interface. Correctness is
demonstrated by a harness that kills a writer process 200 times at random
moments and verifies that every acknowledged key is readable afterwards; it
passes 200/200 under both synchronous and OS-buffered write policies.

## 1. Goals and non-goals

Goals:

- Never lose an acknowledged write across a process crash.
- Detect corruption rather than returning wrong data.
- Keep the on-disk format small enough to explain in one diagram.
- Be safe for concurrent use by many goroutines, verified by the race detector.
- Bound the damage a single corrupted byte can do.

Non-goals: multi-process access, transactions, range queries, and a B-tree
index. Those are listed in future work. Cinder is a single-node, single-process
store with put/get/delete and full-log compaction.

## 2. Design

### 2.1 Record format

Every record has a fixed 21-byte header followed by the key and the value:

```
 0        4        12       16       20  21      21+k
 +--------+--------+--------+--------+---+-------+---------+
 | CRC-32C|  ts    | keylen | vallen |flg|  key  |  value  |
 +--------+--------+--------+--------+---+-------+---------+
```

The checksum is CRC-32C (Castagnoli) over the timestamp, both lengths, the
flag byte, the key and the value — everything after the CRC field itself. This
matters: a checksum over the value alone would let a flipped key or length go
unnoticed, and a flipped length is exactly the corruption that produces a
gigabyte allocation on read. `Decode` therefore validates the claimed length
against a 64 MiB cap *before* allocating, and returns `ErrRecordTooLarge`.

The reader distinguishes a clean end of file (`io.EOF`) from a half-written
record (`io.ErrUnexpectedEOF`). Recovery depends on that distinction.

### 2.2 Append-only log and durability

`Log` holds an `os.File`, the tracked end offset and a `SyncPolicy`. `Append`
encodes a record, writes it at the tracked offset, advances the offset, and
under `SyncAlways` calls `fsync` before returning.

Two details are worth stating because they were found by testing, not by
reasoning:

1. The offset is advanced *before* the sync. If `fsync` fails, the bytes are
   already in the file, and the next append must not overwrite them.
2. The file is opened without `O_APPEND`. On Windows, a handle opened with
   `O_APPEND` receives only `FILE_APPEND_DATA` access, so `SetEndOfFile` fails
   with "Access is denied" and torn-tail recovery becomes impossible. Writing
   at a tracked offset with `WriteAt` costs nothing here because appends are
   serialised by the store's lock.

`SyncAlways` costs about a millisecond per write on the test machine, roughly
380 times the cost of `SyncNever`.

### 2.3 Index

The index is `map[string]entry`, where `entry` records the segment id, offset,
encoded size and timestamp of the newest record for that key. `Get` is one map
lookup and one `ReadAt`; the value is decoded from disk on every call, so a
caller that mutates its result cannot corrupt the store.

### 2.4 Segments, manifest and compaction

An append-only log grows without bound, and overwrites and deletes leave dead
bytes behind. Segments cap one file at a configurable size and give compaction
something to rewrite.

Compaction copies every live record into fresh segments, drops tombstones, and
then replaces the old files. The naive order — write the new files, delete the
old ones — has a correctness hole. If a crash leaves both sets on disk, replay
sees the compacted copy and the original `Put` of a key that was later deleted.
The compacted output has no tombstone, so the deleted key is resurrected. This
is a known failure mode of Bitcask-style stores, and there is no in-memory
state during replay with which to repair it.

Cinder therefore keeps a `MANIFEST` file listing the live segment ids, and
replay reads only what it names. Compaction writes `NNNNNN.log.tmp`, renames
each file into place, installs a new manifest by writing `MANIFEST.tmp` and
renaming it (an atomic replacement), and only then closes and deletes the old
segments. Every crash window resolves to either the old manifest or the new
one; files nothing names are deleted as orphans on the next open.

### 2.5 Recovery

`Open` reads the manifest, replays each segment in id order, and rebuilds the
index. For each segment it tracks the current offset and the number of bytes
each decoded record actually consumed. On a decode failure it computes how many
bytes follow the damaged position:

- nothing follows, and this is the newest segment → a torn write. Truncate to
  the last good offset and report the cut size.
- bytes follow, or the segment is sealed → real corruption. Return `ErrCorrupt`
  and truncate nothing.

Rotation happens only between records, so a sealed segment can never be
legitimately torn. This rule is what stops one flipped byte in record 50 of 100
from silently discarding records 51–99.

### 2.6 Concurrency

A single `sync.RWMutex` guards the index, segment map and rotation state.
Readers hold `RLock` for the whole disk read, so compaction cannot close a
segment underneath a reader. Writers, rotation, compaction and `Close` take the
write lock. Compaction therefore blocks writers but not other readers.

The alternative — copying live records without the write lock — requires
segment handles that survive being closed, i.e. reference counting or epochs.
At the measured compaction times (a 1 MB segment takes milliseconds) that
complexity buys nothing, and correctness came first.

## 3. Crash model and testing

The harness has three parts. `cmd/writer` opens a store and writes `k0`,
`k1`, … in a loop, printing each key to stdout **only after `Put` returned**.
`cmd/crashtest` builds that binary, starts it, reads its stdout, kills it after
a random 5–500 ms, reopens the store, and verifies every key it read against a
value derived from the key. A complete line is an acknowledgement; a partial
final line is discarded because the harness cannot know whether that `Put`
returned.

Two implementation traps were avoided. `go run` cannot be used to start the
writer, because killing the `go run` parent leaves the compiled child alive and
the test would not be a crash. And all reads must finish before `cmd.Wait()`,
because `Wait` closes the stdout pipe and can drop the last acknowledgements,
turning correct behaviour into reported data loss.

Results:

```
$ go run ./cmd/crashtest -runs 200
200/200 passed in 56.27s

$ go run ./cmd/crashtest -runs 200 -sync never
200/200 passed in 1m35.186s
```

What this proves: a process killed at an arbitrary point never loses a write
that had returned. What it does not prove: that a *power cut* cannot lose a
write. Killing a process still leaves the operating system's page cache intact,
and the kernel flushes it afterwards. Only removing power, or a fault-injecting
filesystem, tests `fsync`. That is why `SyncAlways` exists and why this report
does not claim more than the harness demonstrates.

To probe the boundary the harness cannot reach, `store/fault_test.go` injects a
failed `fsync` and asserts that `Put` reports it (and that the next append does
not overwrite the record). A second test truncates a `SyncNever` log to
simulate the OS dropping pages that were never flushed; fewer records survive.
These are simulations, not a power cut: a real test needs controllable power or
a fault-injecting block device. The full argument is in `docs/CRASH_MODEL.md`.

## 4. Evaluation

Environment: Windows, Go 1.27, Intel i5-1135G7, 8 logical cores. Values 32
bytes, keys 8 digits, `SyncNever`, medians of three runs. The full suite was
also run on Debian WSL with Go 1.24.4 and passed, including `go test -race`;
the only change needed was lowering `go.mod` from `1.27.0` to `1.22`.

| Keys | Load (total) | Write per key | Random read | Reopen |
|---|---|---|---|---|
| 10,000 | 36.5 ms | 3.65 µs (274k/s) | 7.57 µs | 11.4 ms |
| 100,000 | 412 ms | 4.12 µs (243k/s) | 8.03 µs | 39.6 ms |
| 1,000,000 | 4.36 s | 4.36 µs (229k/s) | 8.94 µs | 499 ms |

Reopen is linear in the number of records (about 0.5 µs per record), which is
what a single-pass replay should cost. Writes slow slightly with size, from
3.65 to 4.36 µs, consistent with a growing file and more page-cache pressure.
Reads are dominated by the `ReadAt` syscall and CRC verification, not by index
lookup: 8 µs is high for an in-memory index, and a page cache is the obvious
next optimisation.

Durability cost:

| Policy | ns/op | Writes/sec |
|---|---|---|
| `SyncAlways` | 956,118 | 1,046 |
| `SyncNever` | 2,513 | 397,900 |

A factor of about 380. The commonly quoted 10–100x understates the cost of
`FlushFileBuffers` on this platform; the measured number is the one that goes in
the report.

Compaction: 100 keys overwritten 100 times each occupied 900,000 bytes before
compaction and 9,000 bytes after, a 99.0% reduction, with all 100 values
correct after a restart. The 90% target is met with room to spare because
each key retained only its newest record.

## 5. Limits

- **Single process.** There is no advisory file lock. Two processes on one data
  directory would allocate the same segment ids and corrupt each other.
- **Compaction blocks writers.** Readers are unaffected. For large stores this
  is a latency spike worth removing.
- **No encryption or access control.** Cinder assumes a trusted local disk.
- **Recovery reads every segment.** Startup is O(total records), not O(live
  keys), because compaction is what bounds the log, not the index.
- **`ErrCorrupt` is terminal.** There is no repair tool; an operator must
  inspect the segment by hand. Refusing to open is deliberate — silently
  discarding durable records is worse.
- **Windows is the measured platform.** The offset-based write path exists to
  make truncation work there; Linux uses the same code and is expected to be
  faster, but was not benchmarked in this environment (the WSL image had no Go
  toolchain installed at the time of writing).

## 6. Future work

1. A page cache or mmap for values, to remove the per-read syscall.
2. Compaction into a new segment without holding the write lock, using segment
   reference counts so open handles cannot be closed under a reader.
3. Hint files, so recovery reads a compact index summary instead of every
   record.
4. A page-based B-tree index for range queries (the roadmap's stretch goal).
5. Fault injection at the filesystem level to test `fsync` beyond process
   kills.

## 7. Conclusion

Cinder meets its stated guarantee under the crash model it can actually test: a
killed process never loses an acknowledged write, corrupted bytes are detected
rather than served, and a deleted key cannot be resurrected by compaction. The
parts that took real thought were not the append path but the boundaries —
what counts as an acknowledgement, what distinguishes a torn tail from durable
corruption, and what a manifest has to guarantee so that compaction is atomic
from the reader's point of view.

## References

1. J. Sheehy and D. Anderson, *Bitcask: A Log-Structured Hash Table for Fast
   Key/Value Data*, Basho Technologies, 2010.
2. D. Luu, *Files are hard*, 2015.
3. The Go Authors, *Package os*, *hash/crc32*, *encoding/binary*, and *Data Race
   Detector* documentation.
4. M. Kleppmann, *Designing Data-Intensive Applications*, O'Reilly, 2017 —
   chapters on storage engines and log-structured systems.
