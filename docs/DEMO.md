# Cinder demo script (3 minutes)

Have two terminals open in `cinder/`, and a clean data directory:
`rm -rf demo-data` (PowerShell: `Remove-Item -Recurse -Force demo-data -ErrorAction SilentlyContinue`).

---

## 0:00 — What it is (20 s)

> "Cinder is a crash-safe key-value store. Every write is an append to a log
> record with a checksum. The index in memory maps each key to the offset of
> its newest record. On startup it replays the log, rebuilds the index, and
> throws away a half-written last record."

Show the record diagram from `README.md`, and the real hex dump from
`NOTES.md`:

```
00  bc 3d 5f 59                  CRC-32C
04  17 97 9c fe 36 2a 00 00      timestamp
12  00 00 00 05                  key length
16  00 00 00 03                  value length
20  00                           flag
21  61 6c 70 68 61               "alpha"
26  6f 6e 65                     "one"
```

Point at the CRC line: *"it covers the header tail, the key and the value — a
flipped bit in the key is caught too."*

## 0:30 — CLI (30 s)

```sh
go build -o cinder ./cmd/cinder
./cinder -dir demo-data set user:1 ada
./cinder -dir demo-data get user:1        # ada
./cinder -dir demo-data set user:1 grace  # overwrite
./cinder -dir demo-data get user:1        # grace
./cinder -dir demo-data del user:1
./cinder -dir demo-data get user:1        # key not found, exit 1
```

> "Every command opens the store, which means every command you see here has
> already replayed the log and rebuilt the index."

## 1:00 — Restart persistence (20 s)

```sh
./cinder -dir demo-data set survived restart
# kill nothing, just run another process:
./cinder -dir demo-data get survived
```

> "Two separate processes, no shared memory: the second one knows the key
> because it replayed the log."

## 1:20 — Compaction (30 s)

```sh
for i in $(seq 1 2000); do ./cinder -dir demo-data set hot value-$i; done
du -sh demo-data                     # ~ a few hundred KB
./cinder -dir demo-data compact
du -sh demo-data                     # much smaller
./cinder -dir demo-data get hot      # value-2000
```

On PowerShell, `du` is not available: use
`(Get-ChildItem demo-data | Measure-Object Length -Sum).Sum` before and after,
and `1..2000 | ForEach-Object { ./cinder -dir demo-data set hot value-$_ }`
for the loop.

> "2000 writes to one key left 2000 dead records. Compaction copies only live
> records into new segments and installs a new manifest atomically — and the
> deleted `user:1` stays deleted, because tombstones are never copied."

## 1:50 — The numbers (30 s)

Show the two benchmark tables from `README.md`.

> "Durability costs about 380x on Windows: 1,046 durable writes per second
> versus 397,900 in the OS cache. Reopening a million records takes half a
> second. Compaction shrinks 900 KB of churn to 9 KB, 99%."

## 2:20 — Crash test (40 s, the ending)

```sh
go run ./cmd/crashtest -runs 20 -v
```

> "A writer writes keys and prints each one only after the write returned. The
> harness kills it at a random moment between 5 and 500 milliseconds, reopens
> the store, and checks every key it acknowledged. Twenty runs here; the
> recorded run is 200 out of 200, under both sync policies."

Let it finish, read the summary line aloud:

```
20/20 passed in ...
```

> "The honest limit: killing a process still leaves the OS page cache intact.
> Only a power cut tests `fsync`, which is what `SyncAlways` is for."

---

## If asked: why a manifest?

> "Compaction writes new segments and drops tombstones. If a crash left both
> old and new files on disk, replay would see the compacted copy and the
> original put of a deleted key — and the delete would be undone. The manifest
> names the live segments, so replay can't see the old ones. It is installed by
> an atomic rename; every crash window resolves to either the old manifest or
> the new one."
