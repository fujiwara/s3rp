# s3rp benchmark report

- Date: 2026-08-12
- Host: AMD Ryzen 7 255 w/ Radeon 780M Graphics (16 cores), kernel 6.8.0-134-generic
- s3rp: commit a5ebd7e, go version go1.26.4 linux/amd64
- Backend: microceph RGW at 127.0.0.1:7490 (same host, loopback-file OSDs)
- warp: obj.size=1MiB, duration=20s, concurrent=8, multipart part.size=5MiB x 50 parts/client, get objects=500
- s3rp log level: warn (access log suppressed)

## Results

| Scenario | MiB/s | obj/s | avg (ms) | p50 | p90 | p99 | s3rp CPU% avg/max | radosgw CPU% avg/max | warp CPU% avg |
|---|---|---|---|---|---|---|---|---|---|
| PUT (via s3rp) | 472.9 | 472.9 | 18.3 | 17.7 | 22.9 | 27.2 | 123 / 186 | 275 / 375 | 88 |
| PUT (RGW direct) | 397.6 | 397.6 | 20.7 | 20.6 | 25.9 | 29.2 | - | 339 / 434 | 77 |
| GET (via s3rp) | 1297.1 | 1297.1 | 6.3 | 6.1 | 8.1 | 10.9 | 258 / 301 | 333 / 382 | 137 |
| GET (RGW direct) | 1791.5 | 1791.5 | 4.5 | 4.4 | 5.9 | 8.2 | - | 438 / 516 | 175 |
| Multipart (via s3rp) | 651.4 | 130.3 | 61.7 | 61.8 | 69.5 | 81.8 | 146 / 175 | 313 / 373 | 110 |
| Multipart (RGW direct) | 578.2 | 115.7 | 69.5 | 69.0 | 77.6 | 84.7 | - | 451 / 594 | 90 |

## Proxy overhead (via s3rp vs RGW direct)

| Workload | direct MiB/s | via s3rp MiB/s | throughput ratio | direct p50 (ms) | via s3rp p50 (ms) |
|---|---|---|---|---|---|
| put | 397.6 | 472.9 | 1.19 | 20.6 | 17.7 |
| get | 1791.5 | 1297.1 | 0.72 | 4.4 | 6.1 |
| multipart | 578.2 | 651.4 | 1.13 | 69.0 | 61.8 |

## Notes

- warp, s3rp and radosgw all run on the same host and compete for CPU; absolute numbers are indicative only.
- Proxy scenarios run before the direct baselines, so RGW background work (gc of the earlier scenarios' deleted data) can depress the later direct runs — a write ratio above 1.0 means that, not that the proxy adds throughput.
- Disk IO and network are not representative on this machine and are deliberately not measured.
- CPU% is per process (all threads summed), sampled by pidstat at 1s intervals; 100% = one core.
- Raw data: `bench/out/` (warp benchdata, per-scenario warp output, pidstat logs).
