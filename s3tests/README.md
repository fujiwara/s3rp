# ceph/s3-tests compatibility harness

Runs the [ceph/s3-tests](https://github.com/ceph/s3-tests) S3 compatibility
suite against s3rp, proxying to the Ceph RGW backend from `compose.yml`.
The curated results live in [docs/s3-tests.md](../docs/s3-tests.md).

## Why a harness

Nearly every s3-tests test creates its own bucket, but s3rp deliberately
does not proxy CreateBucket/DeleteBucket — bucket provisioning belongs to
the control plane of a service built on the gateway. The harness
(`cmd/s3tests-harness`, `s3tests/harness/`) plays that control plane over
the S3 API: a small interceptor in front of the unmodified gateway handles
exactly those two operations — verifying SigV4 with the same `sigv4`
package, creating the real bucket on the backend, and registering it in a
mutable in-memory store the gateway reads. Everything else passes through
the gateway untouched, so the suite exercises the real proxy.

Two deliberate harness behaviors to keep in mind when reading results:

- Its own two operations answer AWS-compatibly (404 `NoSuchBucket` for a
  missing bucket on DeleteBucket). The gateway's operations keep their
  anti-probing 403 `AccessDenied`, which is a known, deliberate difference
  from AWS that shows up in the results.
- The signed `CreateBucketConfiguration` body is discarded without
  re-verifying its payload hash; the gateway's own operations keep their
  full payload verification.

The suite's `[s3 main]` / `[s3 alt]` / `[s3 tenant]` credential sections
represent separate AWS accounts and map to three s3rp tenants (`main`,
`alt`, `tenanted`), so cross-account expectations line up with s3rp's
cross-tenant semantics. Credentials are fixed in `harness/harness.go` and
mirrored in `s3tests.conf.in`.

## Running

```sh
./s3tests/run.sh
```

This starts the `ceph` compose service (heavyweight; first pull is slow),
builds and starts the harness on `127.0.0.1:7481`, checks out s3-tests at
the pinned SHA into `s3tests/work/` (gitignored), sets up a venv, runs the
boto3 functional suite (`test_s3.py`, `test_headers.py`) with the marker
filter documented in `run.sh`, and writes:

- `s3tests/work/results/pytest.out` — raw pytest output
- `s3tests/work/results/results.xml` — junit XML
- `s3tests/work/results/harness.log` — one JSON line per request
  (gateway observer records plus the harness's create/delete operations)
- `s3tests/work/results/report.md` — `triage.py`'s classification

Environment overrides: `PORT`, `S3RP_TEST_BACKEND_ENDPOINT` (default
`http://127.0.0.1:7480` — use 127.0.0.1, never localhost, for RGW),
`S3RP_TEST_BACKEND_ACCESS_KEY_ID`/`..._SECRET_ACCESS_KEY`, `S3TESTS_REF`,
`MARKERS`, and `PYTEST_ARGS` for extra pytest arguments, e.g. rerunning a
single test while triaging:

```sh
PYTEST_ARGS='-k test_bucket_list_empty' ./s3tests/run.sh
```

## Triage

`triage.py` buckets failures into: deliberately unimplemented operations
(501), the ACL-disabled-bucket model, the anti-probing 403, the stricter
bucket-name charset, backend checksum limitations — and the remainder,
which is the list worth reading: candidate compatibility bugs. Categories
based on name heuristics are flagged for manual confirmation.
