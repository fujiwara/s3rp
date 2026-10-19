# 0030. Verify SigV4 with a derived signing key

- Status: Accepted
- Date: 2026-10-19
- Pull request: [#195](https://github.com/fujiwara/s3rp/pull/195)

## Context

The verifier got each access key's secret from the store through
`SecretLookup` and derived the signing key itself. So the raw secret was in
the gateway's memory: in the store's result, in `sigv4.Credential`, in the
signing-key cache's key, and on `sigv4.Verified` for the whole request
(the aws-chunked reader re-derived the key from it). A memory disclosure —
a core file, a heap dump, a debugger — exposed long-lived secrets.

That was required while verification re-signed requests with aws-sdk-go-v2's
`v4.Signer`, which takes credentials and derives the key internally
([0007](0007-verify-by-re-signing-with-one-signer-per-key.md)). Since
[0029](0029-build-sigv4-canonical-request-in-verifier.md) the verifier builds
the canonical request and derives the key itself, and every mechanism —
header, presigned, aws-chunked chunk signatures, POST policy — uses only the
derived key `kSigning`. Amazon distributes such scoped keys to its regional
services for the same reason.

## Decision

A credential carries either the secret or a way to get the derived key:

- `sigv4.Credential` and `store.Key` gain `SigningKey` (a func of date and
  region returning the 32-byte `kSigning`) and `SecretVersion`. Exactly one
  of `SecretAccessKey` and `SigningKey` is set, and `SecretVersion` is
  required with `SigningKey`; anything else is an internal error.
- The func runs only when the signing-key cache misses, after the request's
  time checks and canonical request, so a remote key service is called about
  once per key, date and region. A failure is not cached. A not-found from it
  is `InvalidAccessKeyId`, anything else `InternalError`, and a key of the
  wrong length is an internal error rather than a signature mismatch.
- The cache is keyed by (access key, version, date, region). The version is
  the secret itself on the secret path, as before, and `SecretVersion` on the
  derived path; it replaces the secret as what makes a rotation take effect
  at once.
- `Verified` carries `SigningKey` instead of `SecretAccessKey`, so the chunked
  reader and POST policy use the verified key, and even the secret path no
  longer keeps the secret for the request's lifetime.
- `sigv4.DeriveSigningKey` (secret as `[]byte`) and
  `sigv4.SigningKeyFromDateKey` (for key services that compute only the
  first, secret-keyed HMAC, such as a KMS HMAC key) are exported for store
  implementations.
- `store` defines its own `SigningKeyFunc` and `s3gw` converts, rather than
  `store` importing `sigv4`, which would pull `s3err` and the AWS SDK
  packages into the contract every store implementer imports.
- The secret path stays: the bundled config store and existing services use
  it.

## Alternatives considered

- **Delegate verification itself to a remote API** (send string-to-sign and
  signature, get yes or no). Rejected: aws-chunked bodies need one
  verification per chunk, chained on the previous signature, which would be a
  round trip per 64 KB; handing out the key for that case is this decision
  anyway.
- **Have the lookup take date and region and return the key.** Rejected: the
  lookup runs on every request, so a remote derivation would too; a func the
  cache calls on a miss keeps it to once per scope.
- **Erase the secret with `runtime/secret` inside the gateway.** Rejected for
  `sigv4`/`s3gw`: the secret path's string is allocated by the store, outside
  any `secret.Do`, and Go strings cannot be erased; the package is an
  experiment behind `GOEXPERIMENT`, so a library would need build-tagged
  variants. It is documented for store implementations instead, where the
  plaintext can be produced and consumed within `secret.Do`.
- **Make `SecretVersion` optional.** Rejected: without it the cache would key
  on the access key id alone, and a secret rotated in place would keep
  verifying with the old key until the date changed or the slot was
  displaced.

## Consequences

- A service can keep secrets out of the gateway process entirely; a memory
  disclosure then exposes derived keys, each valid for one date and region.
  A presigned URL can still be forged for up to seven days past a leaked
  key's date, since `X-Amz-Expires` allows that.
- POST policies are bounded the same way: a form whose `x-amz-date` is more
  than seven days old is refused, checked before the key lookup. Their
  `expiration` is chosen by the signer, so without this a leaked day's key
  could sign policies that stay valid forever (review finding). AWS
  documents no such bound, so a form kept in use for more than seven days
  after signing now fails; S3-compatible services such as OVHcloud apply the
  same seven-day rule.
- Code execution in the gateway can still ask the derivation for any scope;
  that is for the key service to rate-limit and audit.
- The scope is client-chosen before the signature is checked, so anyone with
  an access key id can trigger derivations for other regions and dates of
  the last seven days; `SetRegion` closes the region half. Cache misses cost a remote call,
  with a burst at UTC midnight; cache sizing matters more. Documented in
  [Building a service](../building-a-service.md#your-store).
- `sigv4.Verified.SecretAccessKey` is removed (API change for anyone building
  a `Verified` by hand, as tests do).
- `BenchmarkVerifyKeyDiversity` is unchanged in ns/op and allocs/op.
