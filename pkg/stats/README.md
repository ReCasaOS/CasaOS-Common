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
