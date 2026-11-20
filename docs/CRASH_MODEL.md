# What Cinder's crash tests prove — and what they do not

## The harness result

`go run ./cmd/crashtest -runs 200` kills a writer process at a random moment
between 5 and 500 ms, reopens the store, and verifies that every key the writer
acknowledged is readable with the right value. It passes **200/200** with both
`SyncAlways` and `SyncNever`. An acknowledgement is a complete line on stdout:
`cmd/writer` prints a key only after `Put` returned, and a partial final line is
discarded.

## What that establishes

- A process dying at an arbitrary instruction boundary cannot leave the index
  disagreeing with the log. Recovery replay rebuilds the index from records
  whose CRC matches, and truncates a torn tail at the last good offset.
- A damaged record with good records after it is not silently discarded:
  `Open` returns `ErrCorrupt` and leaves the file untouched.
- Tombstones survive replay, so a deleted key does not come back after a
  restart or after compaction.

## What it does not establish

**A process kill is not a power cut.** When the OS kills a process, its page
cache remains intact and is eventually flushed to disk. A successful `write`
followed by a kill therefore usually survives even with `SyncNever`. Only
removing power, or a filesystem that can drop unflushed pages, tests whether
`fsync` is doing its job. The 200/200 result with `SyncNever` is expected and
should not be read as evidence that `SyncNever` is durable.

## The fault-injection tests

`store/fault_test.go` probes the boundary the harness cannot reach:

1. **Injected fsync failure.** A test seam (`syncFile`) returns an error from
   the sync call. `Put` must return that error. The test also asserts that the
   *next* append still goes after the failed record, because the write did
   reach the file even though it was not acknowledged.
2. **Simulated cache loss.** With `SyncNever`, ten records are written, the
   log file is truncated to half its size (modelling pages that were never
   flushed), and the store is reopened. Fewer than ten keys survive. This
   demonstrates the difference the sync policy makes, but it is a simulation:
   real hardware failure modes are more varied.
3. **Torn tail and mid-file corruption** are covered in `recover_test.go`: a
   seven-byte garbage tail and a halved last record are cut; a corrupt record
   with 49 good records behind it refuses to open.

## What a real power-loss test would need

- A test machine with a controllable power supply, or
- A fault-injecting block device / filesystem (for example dm-flakey on Linux)
  that can drop specific writes, and
- A harness that writes a known sequence, cuts power, reboots, and compares the
  acknowledged set against what survived.

Neither is available in this environment. The report says so explicitly rather
than claiming a guarantee the test does not provide.
