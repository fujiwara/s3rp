# 0016. SigV4 query canonicalization stays as aws-sdk-go-v2 does it

- Status: Accepted (recorded retroactively)
- Date: 2026-08-15
- Pull requests: [#103](https://github.com/fujiwara/s3rp/pull/103),
  [#104](https://github.com/fujiwara/s3rp/pull/104),
  [#107](https://github.com/fujiwara/s3rp/pull/107)

## Context

The botocore cross-check (#103) found requests that botocore signs and the
verifier refuses. There are two ways to canonicalize a query: botocore signs
it as sent; aws-sdk-go-v2, which the verifier used, decodes and re-encodes
it. They disagree only on wire forms no mainstream serializer emits: raw
reserved characters in values, non-canonical escapes, and the order of
values for a duplicated key. Measurements against Amazon S3, versitygw and
Ceph RGW are in [SigV4 query canonicalization](../sigv4-canonicalization.md).

## Decision

Keep aws-sdk-go-v2's canonicalization:

- Raw reserved characters signed as sent are refused. AWS, versitygw and RGW
  all refuse them too.
- Exactly one ordering of a duplicated key's values is accepted, the SDK's
  decoded sort. The ecosystem disagrees here: AWS accepts every ordering,
  versitygw none, RGW some.

## Alternatives considered

- **Multi-candidate verification** (try other orderings when the first
  fails). Rejected: a second canonicalization and signature for wire forms
  with no traffic, while s3rp already sits inside ecosystem norms.

## Consequences

- The failure mode is fail-closed and observable (`SignatureDoesNotMatch`
  with a cause in the log).
- The cross-check generators emit only canonical encodings, which is what
  real clients send. If a real client ever lands on the wrong side, the
  revisit path is a second pass only when the first fails.
- When the verifier later built its own canonical request
  ([0029](0029-build-sigv4-canonical-request-in-verifier.md)), it kept this
  query behavior on purpose.
