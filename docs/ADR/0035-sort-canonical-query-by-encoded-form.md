# 0035. The canonical query is sorted by its encoded form

- Status: Accepted
- Date: 2026-11-19
- Pull request: [#228](https://github.com/fujiwara/s3rp/pull/228)

## Context

The verifier builds the canonical query the way aws-sdk-go-v2 does
([ADR 0029](0029-build-sigv4-canonical-request-in-verifier.md)): decode,
re-encode, sort. Both sorted by the decoded key and value, which is what
`url.Values.Encode` does. aws-sdk-go-v2 v1.47.6 changed its signer to sort
by the encoded key and value
([aws/aws-sdk-go-v2#3617](https://github.com/aws/aws-sdk-go-v2/pull/3617)),
as the SigV4 specification describes and as AWS's SigV4 test suite asserts
(`get-vanilla-query-order-encoded`, a vector the verifier's own suite test
had to leave out until now). The
orders differ where an escaped byte decides a comparison: `%CD` sorts
before `X-Amz-Algorithm` encoded and after it decoded.

The dependency bump to v1.48.0 therefore left the verifier refusing
requests the SDK signs. `FuzzVerifyPresignRoundtrip` found one the next
night (a presigned URL with the query key `%CD`).

## Decision

Sort the canonical query by encoded key, then encoded value, matching the
SDK's current signer.

## Alternatives considered

- **Verify against both orders.** Keeps requests signed by an older SDK
  verifying, as AWS does for duplicate keys. Rejected like multi-candidate
  verification before it
  ([SigV4 query canonicalization](../sigv4-canonicalization.md)): the
  affected requests are ones no S3 operation produces.
- **Keep the decoded order and pin the SDK.** The fuzz target would stay
  green while the clients in the field moved on.

## Consequences

- Requests signed by aws-sdk-go-v2 v1.47.6 and later verify whatever their
  query holds; botocore's sign-as-sent agrees with the same order for
  canonically encoded wire.
- A request signed by an older aws-sdk-go-v2 no longer verifies when its
  query has a key, or a duplicated key's value, whose order depends on an
  escaped byte. S3's own parameter names are not such keys.
- In the measured duplicate-key matrix s3rp now accepts the encoded-sorted
  canonical string (dup A) and refuses the decoded-sorted one (dup D).
