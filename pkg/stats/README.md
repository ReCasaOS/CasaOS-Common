# pkg/stats

Per-item resource statistics: 15 minutes at 5 s in memory, 7 days of minutes on disk.

## Format and limits

One fixed-size ring file per item, `<hash of the item>.ring`. A query never waits for an item's writer: it reads the ring file without the writer's lock. It does take the store read lock for the lookup, so it can wait behind the creation of an item (two fsyncs under the store lock), `Forget` and `Sweep`, which hold that lock.

- Layout: an 8192-byte header area (two header copies, format version 1, item name, series keys), then columns of `Minutes` (10 080) cells. Column 0 holds the minute stamps (4 bytes each); every series adds three columns (avg, min, max; float32). A cell holds no value when its stamp does not match the minute.
- Size is a pure function of the series count: `8192 + 40320 x (1 + 3 x series)` bytes. It never changes while the ring wraps.
- Bounds: 48 series per item (a typical container with 24 series is 2 951 552 bytes, about 2.8 MiB; the maximum is 5 854 592 bytes, 5.6 MiB), series key at most 63 bytes, item name at most 256 bytes, 256 live items plus at most 256 forgotten items.
- Worst case for the directory: (256 live + 256 forgotten) x 5.6 MiB = 2.8 GiB. Realistic case: 41 items of 24 series, about 115 MiB.
- File handles: each ring keeps one open handle for the life of the store (up to 512 with the forgotten items); the service's `LimitNOFILE` (or ulimit) must allow it.
- Memory: 180 live slots per series, no growth with time.
- Modes: the directory is created 0750 and every file the package writes (`*.ring`, `alerts.json`, their `.tmp` and `.bad`) 0640, since they hold container, VM and interface names. `Open` narrows an existing directory and existing `*.ring` files to these modes (best effort, an error is ignored); `NewEvaluator` does the same for an existing `alerts.json`. Windows ignores modes.
- `*.ring.bad` (a quarantined file) is kept once per item; `alerts.json` only holds items that differ from defaults or are active, and is pruned with the store.

## Series keys and partial errors

A ring never frees a series slot. Once an item has seen 48 distinct keys, every new key is rejected for the life of the item, so key suffixes MUST be stable: a MAC address or an index, never a name that changes at each start (libvirt `vnetN`, host veth names).

`Record` returns an error wrapping `ErrSeriesCap` or `ErrBadKey` when some keys of a sample were rejected; the other keys were stored. This is PARTIAL, not a failure: do not retry and do not drop the sample. `stats.IsPartial(err)` is true exactly for such errors (and false if a real failure, such as a write error, is joined to them).

Other behaviour: negative values are stored as given (services clamp what must not be negative); item names must be valid UTF-8 (`ErrBadItem` otherwise, from `Record` and `SetRules`); `Sweep` also removes live items silent for 7 days, and `Prune` then drops their rules.

## How to wire

```go
st, _ := stats.Open(dir, stats.SystemClock{})
ev, _ := stats.NewEvaluator(st, dir, emit) // emit(Event) must not block
st.SetOnMinute(ev.OnMinute)                // the only wiring; services never call OnMinute
// at start, per item that has history:   ev.Rebuild(item)
// every 5 s, per item:                   st.Record(item, now, sample)
// item stops being measured:             ev.Resolve(item)
// item removed:                          ev.Resolve(item); st.Forget(item, now)
// hourly:                                st.Sweep(now); ev.Prune()
```

Notes:

- Use one sampling goroutine per item. `Record` calls for one item from two goroutines are safe for the data, but the `OnMinute` hooks they trigger can run out of order.
- A `Record` that crosses a minute boundary commits the previous minute at the cost of two fsyncs per item; see [Cost](#cost) before sampling many items from one goroutine.
- `Prune` drops the rules and state of every item no longer in the store, including an item that has rules but never recorded.
- `Evaluator.LastError()` returns the error of the last write of `alerts.json` (nil once a later write succeeds); the service can log or expose it.

## Extensions

Beyond the base contract: `SetOnMinute` (the single hook), `Evaluator.Resolve`, `Evaluator.Prune`, `Evaluator.LastError`, `SystemClock` and `FakeClock`, `Store.Quarantined` and `Store.Skipped`, `ErrRingSkipped`, the exported errors (`ErrUnknownItem`, `ErrBadRange`, `ErrBadItem`, `ErrBadKey`, `ErrSeriesCap`, `ErrTooManyItems`, `ErrOutOfOrder`, `ErrClosed`, `*RuleError`), the exported constants (`Minutes`, `LiveSlots`, `MaxSeries`, `MaxItems`, `MaxForgotten`, `MaxKeyLen`, `MaxItemLen`, `FormatVersion`, `MaxFileSize`) and `SplitKey`, `IsPartial`. At `Open`, a ring file that cannot be read for a reason other than corruption (read-only, dangling symlink, symlink to a directory, I/O error) is left in place and listed by `Skipped()`; `Open` carries on with the other items, and a `Record` of a skipped item returns `ErrRingSkipped` (new exported error, a full failure for that item whose text names the file; never partial) instead of replacing the file, so a temporary EACCES/EIO cannot destroy its history; the next `Open` that can read the file picks it up. A corrupt file that could not be moved to `*.bad` is also listed by `Skipped()` but is replaced by the next `Record`. A quarantined `*.ring.bad` gets the time of the quarantine as its mtime, so it is kept 7 days from then. Query results: series of chart `alert` are never returned, a series with no data in the window is omitted, `live` points have Min = Max = Avg, floats are stored as float32.

Added in v0.4.29:

- `Store.Unreadable(item)` is true exactly when a `Record` of item returns `ErrRingSkipped` (its ring file was left in place at `Open` as unreadable). It is false for a healthy or unknown item and for a corrupt file `Open` could not move aside, which `Record` replaces. A service needs no copy of the ring file naming rule.

- File modes 0750 (directory) and 0640 (files), existing ones narrowed at `Open`: see [Format and limits](#format-and-limits).

## Measured cost

50 items, 24 series each, one `Record` per item every 5 s (one tick in twelve commits a minute for all 50: the heavy tick). Host: AMD Ryzen 9 5900X, 24 threads, NVMe SSD (Windows) and the ext4 virtual disk of a WSL2 distribution (Linux), measured 2026-10-20 with `go test ./pkg/stats/ -run xxx -bench 'RecordTick|MinuteValue' -benchmem -count 3`. `cpu-percent-of-core` is the mean tick time over 5 s (wall time, so it includes the wait for fsync).

| Case | Mean tick | Worst tick | B/op | allocs/op | % of a core |
|---|---|---|---|---|---|
| Windows, no fsync | 2.4 ms | 133-147 ms | 394 KB | 462 | 0.048 |
| Windows, fsync | 21-35 ms | 0.95-1.2 s | 1.6 MB | 516 | 0.42-0.70 |
| Linux ext4, no fsync | 0.65-0.69 ms | 77-83 ms | 170-186 KB | 453 | 0.013 |
| Linux ext4 (WSL2 disk), fsync | 38-42 ms | 2.5-3.2 s | 1.6 MB | 515 | 0.75-0.84 |

With the Evaluator wired through `SetOnMinute` (20 of the 50 items above their alert thresholds, so their state changes at every minute), measured after `minuteValue` was made to read only the series it needs: Windows no fsync 4.1 ms mean tick, about 1 100 allocs, `OnMinute` 0.4 ms mean; Linux no fsync 3.0 ms; with fsync 18-40 ms mean and the same worst ticks as without the Evaluator (up to about 2.7 s). `Evaluator.minuteValue` reads only the requested series: 0.027 ms and 14 allocs on Windows (0.004 ms on Linux), twice per item per minute. The Evaluator holds its lock through marshal and the write of `alerts.json` (0.4-0.9 ms on average, a few ms worst).

`TestRecordTickBudget` runs one simulated hour. Without fsync it asserts the mean tick (< 5 ms, < 25 ms with the Evaluator) and fewer than 5 000 allocations per tick; every variant has a 15 s worst-tick hang guard, and the worst tick and the fsync numbers are logged, not asserted. The fsync worst tick is dominated by the heavy tick (50 commits, several fsyncs each) and scales with the disk's fsync latency; committing minutes from a per-store goroutine is the upgrade if a real disk needs it; it is not done.

### Cost

Headline numbers (50 items, 24 series, 5 s ticks): without fsync a tick costs 2.4 ms on Windows and 0.7 ms on Linux (0.05 % and 0.013 % of a core), about 460 allocations; with the Evaluator wired, 4 ms on Windows and 3 ms on Linux, about 1 100 allocations. With fsync the mean is 21-35 ms (Windows NVMe) and 38-42 ms (WSL2 virtual disk); the worst tick depends on the host, from about 1 s up to about 3 s.

A `Record` that crosses a minute boundary commits the previous minute, which costs two fsyncs per item. A service that samples its 50 items in one goroutine can therefore see one tick delayed by seconds, at the minute boundary, on a slow disk. Samples keep their own timestamps, so nothing is lost; the service should either sample each item from its own goroutine or accept the jitter. `TestRecordTickBudget` is strict on the mean and the allocation count without fsync, and only guards against a hang (15 s) on the worst tick; with fsync it logs the numbers.
