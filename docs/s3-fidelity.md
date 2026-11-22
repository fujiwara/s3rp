# AWS fidelity tests

How the S3 tests of
[aws/aws-emulator-evaluation-suite](https://github.com/aws/aws-emulator-evaluation-suite)
are run against s3rp, and how to read a run.
[s3fidelity/README.md](../s3fidelity/README.md) describes the harness and
the local run (`./s3fidelity/run.sh`); CI runs the same script weekly, and
on demand, via the
[`s3-fidelity` workflow](../.github/workflows/s3fidelity.yml). A failed
weekly run means the gateway or the backend image moved a result since the
last one: triage it as below.

## What it adds

The suite's tests assert Amazon S3's own behavior, error code and message
included, and each was verified against real AWS accounts. A failure
therefore reads "differs from S3", where a failure of
[ceph/s3-tests](s3-tests.md) reads "differs from what RGW's suite
expects". It is the reference for what the gateway words and decides
itself: canonical error messages, request validation, the detail elements
of a refusal.

It does not test authorization: the tests that set bucket policies or use
IAM need operations the gateway does not have and are not selected.

## Setup

- Suite pinned at `abd08117ad242c3e244125df9d525c41eb6f7d91` (2026-11).
  The repository is maintained by AI agents and changes daily; bump the
  pin deliberately and re-triage.
- Backend: the compose `ceph` service at the image pinned in
  `compose.yml`. `expected.tsv` is exact for that image, and releases do
  differ: tentacle fails three tests umbrella passes (the tagging
  directive on copy, a conditional-write race).
- A second run serves the same selection from the in-process backend
  ([`s3gw/localbackend`](building-a-service.md#running-without-a-backend),
  kept in a directory) instead of Ceph: the workflow's `localbackend` job,
  or `S3RP_TEST_BACKEND_ENDPOINT=directory:PATH ./s3fidelity/run.sh`
  (`memory` for the form held in memory). Its expected failures are
  `expected-localbackend.tsv`, which `run.sh` picks by the endpoint. It is
  what holds that backend to S3: a result that moves there is a change in
  `localbackend` or in the gateway, never in an image.
- Region `us-east-1`, the gateway's own: the runner's default
  (`us-west-2`) would fail the tests that read the bucket region.
- The emulator the suite was written for is not part of the setup.

## Asking the emulator

When a question is what S3 does with a request — a parameter it does not
define, a combination the documentation leaves open — and the suite has no
test of it, ask the [AWS Emulator](https://github.com/aws/aws-emulator)
the suite was written for: send it the request and read the answer.

- Look in the suite first (`s3fidelity/work/suite`, `manifest.jsonl` lists
  each test's operations). A test there was verified against AWS; the
  emulator's answer is the emulator's.
- Use the latest release where possible. It is released several times a
  day and its fidelity moves with it, so an answer from an old build may
  no longer be its answer. The Debian package runs unpacked, without
  installing:

  ```console
  $ curl -fsSLo aws-emulator.deb https://downloads.aws.com/aws-emulator/packages/aws-emulator/LTS/latest/aws-emulator_amd64.deb
  $ dpkg-deb -x aws-emulator.deb emulator
  $ home=$(mktemp -d); HOME=$home emulator/opt/aws-emulator/bin/aws-emulator -p 4566 --telemetry off &
  ```

  It serves https on loopback and writes its access keys and CA
  certificate under `$home/.aws/emulator/4566/` (`emulator_credentials`,
  `ca.pem`); `curl --aws-sigv4 "aws:amz:us-east-1:s3" --cacert ...` is
  enough to send a request.
- Write down what was asked and of which version (`aws-emulator -v`) where
  the answer is used — the ADR or the pull request — and say that it was
  the emulator, not S3.
- It answers for S3, not for the gateway's rules: it ignores every query
  parameter it does not know, and the gateway refuses them
  ([ADR 0002](ADR/0002-refuse-what-cannot-be-honored.md)).

## Reading a run

`expected.tsv` lists every test expected to fail, with a category, so a
run needs a person only for two lists in the report:

- **UNMATCHED** — a failure the file does not list: a regression in the
  gateway, a backend change, or a new deliberate difference. Fix it, or
  add it to `expected.tsv` under the category that explains it.
- **Expected failures that passed** — remove them from `expected.tsv`.
  An entry marked `flaky` in the third column is exempt: its failure
  depends on timing (RGW answers `InternalError` to some concurrent
  conditional writes, and a slower machine does not hit it), so either
  result is accepted.

To tell the gateway from the backend, run the failing test against RGW
directly (`fidelity-tests-runner run <test> --endpoint
http://127.0.0.1:7480 --region us-east-1 --credentials-file <file>` with
the backend key) and compare the two failures: the same failure on both is
the backend's. `harness.log` holds the backend's own error for every
refusal, by request id.

## Why each category is expected

- **backend** — Ceph RGW differs from S3 and the gateway passes the
  difference on: storage classes RGW does not define (`GLACIER`,
  `DEEP_ARCHIVE`, the infrequent-access classes), multipart checksum types
  it does not echo or verify, `NoSuchKey` where S3 says `NoSuchUpload` or
  `NoSuchVersion`, `x-amz-mp-parts-count` on a whole-object read, its own
  URL-encoding of listed keys, Range and copy-precondition edge cases, a
  retryable error under concurrent conditional writes. The whole
  selection was run against RGW directly when the list was made: none of
  these passes there.
- **message** — the code is S3's, the message is not. S3 words some codes
  by cause ("Access Denied because object protected by object lock.", the
  reason of an `InvalidRequest`, the algorithm of a `BadDigest`); the
  gateway gives each code one message and does not relay the backend's
  ([ADR 0036](ADR/0036-error-messages-follow-s3-wire-wording.md)). The
  backend's wording is in the observer's record for a service to use.
- **deliberate** — the gateway answers differently on purpose: `403
  AccessDenied` for a bucket that does not exist (no name probing across
  tenants), no SigV2 (boto3 presigns with it against a custom endpoint),
  no check of storage class names
  ([ADR 0037](ADR/0037-validate-request-values-in-the-gateway.md)), and a
  NUL byte in a key refused by the HTTP server before the gateway.
- **out-of-scope** — S3 Express One Zone (directory buckets, session
  auth).

`expected-localbackend.tsv` uses the same categories, and most of its
entries are the gateway's (**message**, **deliberate**, **out-of-scope**),
shared with the Ceph list. What differs:

- **backend** there is what `localbackend` leaves out, which is one thing:
  a storage class is a label, so an object stored as `GLACIER` or
  `DEEP_ARCHIVE` reads like any other. Where RGW differs from S3 in the
  ways listed above, `localbackend` follows S3, so those tests pass.
- Some failures the Ceph list files under **backend** are **message**
  there: with the backend answering S3's code, what is left is the
  gateway's one message per code.

## Maintaining

- A classification that changes (a deliberate refusal lifted, a backend
  behavior that moved with a release) changes `expected.tsv` in the same
  commit as the code or the image bump. A change to `s3gw/localbackend`
  that moves a result changes `expected-localbackend.tsv` in its commit;
  the workflow runs on pull requests that touch the package.
- After bumping the suite pin, expect new tests in the selection: triage
  each new failure the way above before listing it.
- Run counts are not recorded here; the latest workflow run's report has
  them.
