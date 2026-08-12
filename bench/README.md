# bench

Simple performance benchmark for s3rp fronting a local RGW (microceph).
Measures PUT / GET / multipart throughput and latency with
[warp](https://github.com/minio/warp), both **through the proxy** and
**directly against RGW** (baseline), and samples CPU usage of `s3rp`,
`radosgw` and `warp` with `pidstat`.

## Prerequisites

- warp: `go install github.com/minio/warp@latest`
- sysstat (`pidstat`), aws cli, python3
- A local microceph RGW, set up by `sudo ./s3tests/setup-microceph.sh`
  (idempotent; binds everything to 127.0.0.1 and creates the `backend`
  S3 user whose credentials this benchmark defaults to). To use different
  credentials, export `RGW_ACCESS_KEY` / `RGW_SECRET_KEY`.

## Run

```console
$ bash bench/run.sh
```

This will:

1. create the backend bucket `s3rp-bench` on RGW if missing
2. build s3rp and start it on `:8090` with `bench/config.yml`
   (front bucket `warp-benchmark-bucket` → backend bucket `s3rp-bench`)
3. run warp scenarios (each sampled by `pidstat 1`):
   - `put` 1MiB objects, `get` 1MiB objects, `multipart-put` 5MiB parts
     (MPU write path) — first via s3rp, then directly against RGW as the
     baseline
4. write the aggregated report to `bench/report.md` (raw data in `bench/out/`)

Tunables (env vars): `DURATION` (20s), `CONCURRENT` (8), `OBJ_SIZE` (1MiB),
`GET_OBJECTS` (500), `PART_SIZE` (5MiB), `PARTS` (50, per client),
`S3RP_LOG_LEVEL` (warn — set `info` to include access-log formatting cost),
`RGW_ENDPOINT` (127.0.0.1:7490), `WARP` (path to warp binary).

## Caveats

- Everything runs on one host: warp, s3rp and radosgw compete for CPU, so
  absolute numbers are indicative. The proxy-vs-direct ratio is the useful signal.
- Disk IO / network are intentionally not measured (not representative here).
- `bench/config.yml` contains no secrets; RGW credentials are expanded from
  the environment at config load.
