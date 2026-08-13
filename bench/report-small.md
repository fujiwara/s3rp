# s3rp benchmark report (small objects)

- Date: 2026-08-13
- Host: AMD Ryzen 7 255 w/ Radeon 780M Graphics (16 cores), kernel 6.8.0-134-generic
- s3rp: commit 590980c, go version go1.26.4 linux/amd64
- Backend: microceph RGW at 127.0.0.1:7490 (same host, loopback-file OSDs)
- warp: obj.size=16KiB, duration=20s, concurrent=8, get objects=2000
- s3rp log level: warn (access log suppressed)

## Results

| Scenario | MiB/s | obj/s | avg (ms) | p50 | p90 | p99 | s3rp CPU% avg/max | radosgw CPU% avg/max | warp CPU% avg |
|---|---|---|---|---|---|---|---|---|---|
| PUT (via s3rp) | 18.0 | 1151.8 | 6.9 | 6.7 | 8.1 | 11.1 | 77 / 118 | 194 / 232 | 41 |
| PUT (RGW direct) | 19.9 | 1275.3 | 6.5 | 6.2 | 8.6 | 10.9 | - | 216 / 279 | 49 |
| GET (via s3rp) | 56.1 | 3592.3 | 2.3 | 2.2 | 2.8 | 4.1 | 249 / 296 | 344 / 397 | 100 |
| GET (RGW direct) | 94.0 | 6016.2 | 1.3 | 1.3 | 1.6 | 2.1 | - | 506 / 593 | 147 |

## Proxy overhead (via s3rp vs RGW direct)

| Workload | direct MiB/s | via s3rp MiB/s | throughput ratio | direct p50 (ms) | via s3rp p50 (ms) |
|---|---|---|---|---|---|
| put | 19.9 | 18.0 | 0.90 | 6.2 | 6.7 |
| get | 94.0 | 56.1 | 0.60 | 1.3 | 2.2 |

## Notes

- warp, s3rp and radosgw all run on the same host and compete for CPU; absolute numbers are indicative only.
- Proxy scenarios run before the direct baselines, so RGW background work (gc of the earlier scenarios' deleted data) can depress the later direct runs — a write ratio above 1.0 means that, not that the proxy adds throughput.
- Disk IO and network are not representative on this machine and are deliberately not measured.
- CPU% is per process (all threads summed), sampled by pidstat at 1s intervals; 100% = one core.
- Raw data: `bench/out/` (warp benchdata, per-scenario warp output, pidstat logs).
