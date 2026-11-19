# 0036. Canonical error messages follow S3's wire wording

- Status: Accepted
- Date: 2026-11-19
- Pull request: [#231](https://github.com/fujiwara/s3rp/pull/231)

## Context

The gateway never relays a backend's error message; `s3err` gives each
error code one canonical message instead. That table was worded after the
error list in Amazon S3's documentation, on the assumption that the
documentation quotes the service.

It does not. Running the S3 fidelity tests of
[aws-emulator-evaluation-suite](https://github.com/aws/aws-emulator-evaluation-suite)
— tests that assert real S3's responses, messages included, each verified
against AWS accounts — against the gateway showed the wire wording differs
for sixteen codes: `PreconditionFailed` says "pre-conditions you specified",
`NoSuchBucket` and several others end without a period, `NoSuchUpload`,
`NoSuchVersion`, `InvalidPart` and `InvalidPartOrder` are different
sentences. Clients and test suites compare these strings literally.

## Decision

The canonical message of a code is what Amazon S3 sends on the wire. The
entries verified that way are pinned by `TestCanonicalMessagesVerified`;
the documentation's table is not a source for correcting them.

## Alternatives considered

- **Keep the documentation's wording.** It is the published reference, but
  nothing a client observes: a client written against S3 sees the wire.
- **Choose the message per operation or cause**, as S3 does for
  `BadDigest` (names the algorithm), `InvalidRequest`, `InvalidArgument`
  and an Object Lock `AccessDenied`. Not done here: the cause is known only
  from the backend's message, which the gateway deliberately does not
  relay, so it needs a design of its own.

## Consequences

- The `Message` of sixteen error codes changes; a client matching the old
  text literally sees the difference.
- Codes with several messages on S3 keep one general message and still
  differ from S3 in the specific cases.
- Codes the suite does not assert (`EntityTooLarge`, `InternalError`, ...)
  remain as they were, unverified.
