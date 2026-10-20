# pkg/stats

Per-item resource statistics: 15 minutes at 5 s in memory, 7 days of minutes on disk.

## Format and limits

One fixed-size ring file per item, `<hash of the item>.ring`, read without a lock.

- Layout: an 8192-byte header area (two header copies, format version 1, item name, series keys), then columns of `Minutes` (10 080) cells. Column 0 holds the minute stamps (4 bytes each); every series adds three columns (avg, min, max; float32). A cell holds no value when its stamp does not match the minute.
- Size is a pure function of the series count: `8192 + 40320 x (1 + 3 x series)` bytes. It never changes while the ring wraps.
- Bounds: 48 series per item (a typical container with 24 series is 2 951 552 bytes, about 2.8 MiB; the maximum is 5 854 592 bytes, 5.6 MiB), series key at most 63 bytes, item name at most 256 bytes, 256 live items plus at most 256 forgotten items.
- Worst case for the directory: (256 live + 256 forgotten) x 5.6 MiB = 2.8 GiB. Realistic case: 41 items of 24 series, about 115 MiB.
- Memory: 180 live slots per series, no growth with time.
- `*.ring.bad` (a quarantined file) is kept once per item; `alerts.json` only holds items that differ from defaults or are active, and is pruned with the store.

## Measured cost

50 items, 24 series each, one `Record` per item every 5 s (one tick in twelve commits a minute for all 50: the heavy tick). Host: AMD Ryzen 9 5900X, 24 threads, NVMe SSD (Windows) and the ext4 virtual disk of a WSL2 distribution (Linux), measured 2026-10-20 with `go test ./pkg/stats/ -run xxx -bench 'RecordTick|MinuteValue' -benchmem -count 3`. `cpu-percent-of-core` is the mean tick time over 5 s (wall time, so it includes the wait for fsync).

| Case | Mean tick | Worst tick | B/op | allocs/op | % of a core |
|---|---|---|---|---|---|
| Windows, no fsync | 2.4 ms | 133-147 ms | 394 KB | 462 | 0.048 |
| Windows, fsync | 21-35 ms | 0.95-1.2 s | 1.6 MB | 516 | 0.42-0.70 |
| Linux ext4, no fsync | 0.65-0.69 ms | 77-83 ms | 170-186 KB | 453 | 0.013 |
| Linux ext4 (WSL2 disk), fsync | 38-42 ms | 2.5-3.2 s | 1.6 MB | 515 | 0.75-0.84 |

With the Evaluator wired through `SetOnMinute` (20 of the 50 items above their alert thresholds, so their state changes at every minute): Windows no fsync 7.6-8.0 ms mean (0.15-0.16 %), 2 400 allocs, `OnMinute` 1.2 ms mean; Windows fsync 26-30 ms mean, worst tick about 1.0-1.3 s; Linux no fsync 3.4-5.5 ms mean, fsync 51-59 ms mean, worst tick 3.7-4.5 s. `Evaluator.minuteValue` reads the minute of every series of the item: 0.41 ms and 175 allocs on Windows, 0.05 ms on Linux, twice per item per minute. The Evaluator holds its lock through marshal and the write of `alerts.json` (about 1 ms on average, 3-9 ms worst on both hosts, up to 58 ms once).

`TestRecordTickBudget` asserts, over one simulated hour: no fsync, mean tick < 5 ms and worst tick < 250 ms (< 25 ms mean with the Evaluator); fsync, worst tick < 2 s. The fsync worst tick is dominated by the heavy tick (50 commits, several fsyncs each) and scales with the disk's fsync latency: the WSL2 virtual disk, where one fsync takes tens of milliseconds, exceeds 2 s. Committing minutes from a per-store goroutine is the upgrade if a real disk does too; it is not done.
