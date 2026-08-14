# ceph/s3-tests compatibility testing

How the [ceph/s3-tests](https://github.com/ceph/s3-tests) S3
compatibility suite is run continuously against s3rp, and how to read a
run. The suite needs a harness because s3rp deliberately has no
CreateBucket/DeleteBucket — [s3tests/README.md](../s3tests/README.md)
describes it and the local run (`./s3tests/run.sh`); CI runs the same
script via the manually-triggered
[`s3-tests` workflow](../.github/workflows/s3tests.yml).

## Setup

- s3-tests pinned at `5522d1c351f75bc00ae0f64f742f3f095f5939d9`
  (2026-08; bump deliberately — runs must stay comparable), boto3
  functional suite: `test_s3.py` + `test_headers.py`. Excluded files:
  `test_iam.py`, `test_sts.py`, `test_s3select.py` (features s3rp does
  not have).
- Marker filter (rationale per marker in `run.sh`):
  `not lifecycle_expiration/transition, cloud_*, s3website, sns,
  storage_class, fails_on_rgw, auth_aws2`.
- Backends: locally the compose `ceph` service (frozen at Ceph 19.2.0);
  in CI a MicroCeph RGW (`s3tests/setup-microceph.sh`; the snap channel
  is a workflow input, default `tentacle/stable`, which tracks point
  releases — the job summary states the exact Ceph version). On the
  compose backend `run.sh` deselects
  `test_upload_part_copy_percent_encoded_key`: it crashes that RGW
  itself (abort in `RGWObjectCtx::set_atomic`; fixed in later Ceph),
  killing the daemon for the rest of the run.

## Reading a run

`triage.py` encodes the hand-verified failure classification, so its
report — the CI job summary, with full results as artifacts — is the
per-test verdict: on a healthy run every failure lands in a named
expected category and only the `UNMATCHED` bucket needs hand triage.
The rules are partly name-based heuristics; how to verify what lands
where, split the 501 bucket by message text, and judge
implementable-vs-deliberate is in
[s3tests/CLAUDE.md](../s3tests/CLAUDE.md). Run numbers move without any
repo change (the backend tracks Ceph point releases), so this document
records none — compare runs by diffing their junit XMLs, not by counts
remembered from a document.

The expected-failure categories, and why each failure is expected:

- **Deliberate design surface** — the bulk of the failures.
  Unimplemented bucket-configuration writes (501, including tests that
  probe invalid inputs of those operations and expect 400/404/409 — the
  refusal comes before input validation), the ACL stub, SSE-C refusal,
  SigV4-only (no anonymous access, no SigV2 — the suite's
  `test_post_object_*` tests sign with SigV2), the anti-probing 403 on
  nonexistent buckets, the stricter bucket-name charset. Rationale in
  the root CLAUDE.md; these are do-not-regress decisions, so a test
  that starts *passing* here means a deliberate refusal disappeared —
  investigate, don't celebrate.
- **Platform edge cases** — Go's `net/http` answers a malformed
  `Expect` header with 417 before the proxy runs; plain
  `Transfer-Encoding: chunked` without Content-Length is not accepted;
  the suite's "expired presign" sends a *negative* `X-Amz-Expires`,
  which s3rp refuses with 400 like AWS while RGW answers 403.
- **Backend limitations** — behavior confirmed by probing the backend
  directly (bypassing the proxy), never assumed; the mix shifts with
  the backend version. The frozen demo RGW (19.2.0) lacks SSE-S3
  configuration, does not store checksums, and enforces write
  preconditions only partially; current Ceph keeps its own semantics
  for part ETags on `partNumber` reads and COMPOSITE-only multipart
  checksum types, and refuses some SSE copy combinations.
- **RGW extension APIs** — usage/account APIs, `x-rgw-*` headers,
  `allow-unordered` listing, `x-amz-tagging-count` on HEAD: not in the
  S3 API model; matching RGW here is a non-goal.
- **Harness/conf artifacts** — CreateBucket-conflict semantics are
  answered by the harness (409 `BucketAlreadyOwnedByYou`, RGW-style);
  `GetBucketLocation` returns the gateway region while the conf's
  `api_name` expects RGW's zonegroup name; leftover Object Lock buckets
  (undeletable for the suite's cleaner) trip bucket-count assertions.
- **Upstream s3-tests bug** — `test_bucket_create_exists` reads
  `e.status` on a `ClientError`, an attribute that does not exist; it
  fails against any backend answering 409 `BucketAlreadyOwnedByYou`,
  including RGW directly.

## Maintaining this document

Update it only when a classification *decision* changes — a new
deliberate refusal, a backend limitation verified (or fixed) upstream —
in the same commit that adjusts `triage.py`'s rules. A re-run that
merely moves counts never requires an edit here.

## Artifacts

A run leaves everything under `s3tests/work/results/` (gitignored):
`pytest.out`, junit `results.xml`, `harness.log` (one JSON line per
request, correlate by bucket name / `x-amz-request-id`), and `report.md`
(`triage.py`'s classification). A CI run uploads the same directory as
the `s3tests-results` artifact
(`gh run download <run-id> -n s3tests-results`) and puts `report.md` in
the job summary, alongside the backend Ceph version.
