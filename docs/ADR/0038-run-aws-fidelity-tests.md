# 0038. AWS's fidelity tests are run against the gateway

- Status: Accepted
- Date: 2026-11-20
- Pull request: [#235](https://github.com/fujiwara/s3rp/pull/235)

## Context

Outside SigV4, nothing checked the gateway against Amazon S3 itself.
ceph/s3-tests compares it with what RGW's own suite expects, the
integration suite with backends, and the unit tests with expectations
written here — the canonical error messages among them, which turned out
to follow S3's documentation rather than its responses
([ADR 0036](0036-error-messages-follow-s3-wire-wording.md)).

AWS published an emulator of its APIs together with
[aws-emulator-evaluation-suite](https://github.com/aws/aws-emulator-evaluation-suite),
the tests it scores the emulator with: boto3 programs asserting S3's
behavior, code and message included, each verified against real AWS
accounts, under CC0.

## Decision

Run the suite's S3 tests against the gateway (`s3fidelity/`), through the
s3-tests harness, selecting the tests whose operations the operation
catalog supports. The suite is pinned to a commit, and every expected
failure is listed with a category in `s3fidelity/expected.tsv`: a run
fails on any failure outside the list and on any listed test that passes.

The emulator itself is not used.

## Alternatives considered

- **Use the emulator as a backend or as a reference to compare against.**
  It documents that it does not evaluate bucket policies and that its
  data-plane fidelity varies, so it is neither a backend anyone deploys
  nor an authority; the tests carry the verified expectations.
- **Classify failures by name and text patterns, as `s3tests/triage.py`
  does.** That fits a large suite on a moving backend. Here the suite is
  pinned and the selection small, so an exact list is cheaper to keep and
  turns the run into a regression check.
- **Vendor the tests.** The suite changes daily and is large; a pinned
  checkout at run time keeps the repository free of it.

## Consequences

- Wording and validation decisions have an S3-verified reference; the
  first runs produced ADR 0036 and
  [ADR 0037](0037-validate-request-values-in-the-gateway.md).
- `expected.tsv` is exact for one suite commit and one Ceph image: a bump
  of either needs a re-triage in the same change.
- Tests needing bucket-configuration writes or IAM are not selected, so
  the suite says nothing about authorization.
