# Cinder

A crash-safe key-value store in Go. Bitcask-style: every write is appended to a
log record with a checksum, an in-memory index points at the newest record for
each key, and opening the store replays the log to rebuild that index.

**200/200 crash runs passed.** A writer process was killed at a random moment
200 times in a row; every key it acknowledged before dying was still readable
after recovery, under both durability policies.

## Build and use

```sh
cd cinder
go build ./...

go run ./cmd/cinder -dir ./data set a 1
go run ./cmd/cinder -dir ./data get a      # prints 1
go run ./cmd/cinder -dir ./data del a
go run ./cmd/cinder -dir ./data compact
```

`-sync always` (the default) fsyncs every write. `-sync never` leaves flushing
to the OS: much faster, still survives a process crash, not a power cut.

Library use:

```go
s, err := store.Open(store.Options{Dir: "./data", Sync: store.SyncAlways})
defer s.Close()

s.Put("user:1", []byte(`{"name":"ada"}`))
v, err := s.Get("user:1")     // errors.Is(err, store.ErrNotFound)
s.Delete("user:1")
```

## Record format

Packed, big-endian, 21-byte header, then key bytes, then value bytes.

```
 0        4        12       16       20  21      21+k
 +--------+--------+--------+--------+---+-------+---------+
 | CRC-32C|  ts    | keylen | vallen |flg|  key  |  value  |
 +--------+--------+--------+--------+---+-------+---------+
  4 bytes  8 bytes  4 bytes  4 bytes  1B   k bytes  v bytes
```

The CRC covers **everything after the CRC field**: timestamp, both lengths,
flag, key and value. A flipped bit in a key or a length is caught, not just a
flipped value.

## Recovery in five sentences

1. `Open` reads `MANIFEST`, which lists the live segment files.
2. It replays each listed segment in id order, decoding records and updating
   the index; a tombstone removes its key.
3. If the newest segment ends inside a record, that is a torn write from a
   crash: the file is truncated back to the last good offset and the cut is
   counted in `Stats().BytesCut`.
4. If a record fails to decode while bytes still follow it, or fails in a
   sealed segment, `Open` returns `ErrCorrupt` and truncates nothing — that is
   damage to data that was already durable, not a half-finished write.
5. Writes resume at the end of the newest segment, rotating to a new file when
   the segment size cap is reached.

## Segments and compaction

Segments are `000001.log`, `000002.log`, … capped at `Options.SegmentSize`
(64 MiB by default). Compaction rewrites live records into fresh segments,
drops tombstones and dead records, installs a new `MANIFEST` atomically, and
only then deletes the old files.

The manifest exists for one specific bug: if a crash leaves both the old and the
new segments on disk, replay sees the compacted copy *and* the original `Put`
of a key that was later deleted — and the deleted key comes back. Replay reads
only what the manifest names, so a delete cannot resurrect.

Measured: 100 keys × 100 overwrites took **900,000 bytes** before compaction and
**9,000 bytes** after — a **99.0%** reduction, with every value intact after a
restart.

## Benchmarks

`go test ./store -run '^$' -bench . -count 3`, medians of 3 runs. 32-byte values,
8-digit keys, `SyncNever`, Windows on an i5-1135G7 (8 logical cores). `benchstat`
not used; medians are hand-picked.

| Keys | Load (total) | Write per key | Random read | Reopen |
|---|---|---|---|---|
| 10k | 36.5 ms | 3.65 µs (~274k/s) | 7.57 µs (~132k/s) | 11.4 ms |
| 100k | 412 ms | 4.12 µs (~243k/s) | 8.03 µs (~124k/s) | 39.6 ms |
| 1M | 4.36 s | 4.36 µs (~229k/s) | 8.94 µs (~112k/s) | 499 ms |

Durability cost, same 84-byte record:

| Sync policy | ns/op | Writes/sec |
|---|---|---|
| `SyncAlways` | 956,118 | ~1,046 |
| `SyncNever` | 2,513 | ~397,900 |

That is a **~380x** durability tax on Windows: `FlushFileBuffers` costs about a
millisecond.

## Crash test

```sh
go run ./cmd/crashtest -runs 200              # sync=always: 200/200, 56 s
go run ./cmd/crashtest -runs 200 -sync never  # sync=never:  200/200, 95 s
```

`cmd/writer` prints a key only after `Put` returns; `cmd/crashtest` kills it
after 5–500 ms, reopens the store and verifies every acknowledged key. A
half-written last line is discarded, because the harness cannot know whether
that `Put` returned.

Honest limit: killing a process tests process crashes. The OS still flushes its
page cache afterwards. Only a power cut tests whether `fsync` works, which is
what `SyncAlways` is for.

See `docs/CRASH_MODEL.md` for what this proves, the injected-fsync and
simulated-cache-loss tests, and what a real power-cut test would require.

## Correctness checks

```sh
go test ./...            # unit, recovery, segments, compaction, CLI, crash logic
go test ./... -race      # full 10 s concurrency windows, ~35 s
```

- Record round trip incl. 1 MB values, UTF-8 and empty keys, corruption in
  value, key and timestamp, oversized length with a valid CRC.
- Recovery: 100 records + garbage tail → exactly 100 keys; half a record →
  earlier records intact; corrupt record 50 of 100 → refuses to open.
- Compaction: 99% shrink, delete stays deleted across compaction + restart,
  orphan segments from a crashed compaction are cleaned up.
- Concurrency: 50 goroutines mixing Put/Get/Delete; 40 readers against 10
  writers with no false `ErrNotFound`; background compaction under load.

## Limits and future work

- One process per data directory. There is no file lock; two processes would
  fight over segment ids.
- Compaction holds the write lock. Reads keep flowing (they share `RLock`).
- Every read is a `ReadAt` plus CRC verification. A page cache or mmap index
  would cut the ~8 µs read latency.
- Stretch not attempted: a page-based B-tree index.
- Values are capped at 64 MiB per record to bound allocation from a corrupt
  length field.
