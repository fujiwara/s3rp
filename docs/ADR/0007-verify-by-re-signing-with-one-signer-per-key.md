# 0007. Verify by re-signing with the SDK signer, one signer per access key

- Status: Superseded by [0029](0029-build-sigv4-canonical-request-in-verifier.md)
  (recorded retroactively)
- Date: 2026-08-01
- Pull requests: initial implementation (506d1a2),
  [#37](https://github.com/fujiwara/s3rp/pull/37)

## Context

The gateway must verify the SigV4 signature of every request. Writing a
canonicalizer means owning many subtle rules: escaping, query encoding,
header normalization. aws-sdk-go-v2 already contains a signer.

That signer caches the derived signing key in a single entry keyed by
(service, region) and checks it against the access key id. An SDK client
has one credential, so the entry always hits. A proxy verifies with a
different key on almost every request, so the entry missed every time:
each request re-derived the key and took the cache's write lock, and
verification serialized across the whole process. With 16 keys the
parallel benchmark was slower than the serial one (16.4µs vs 13.8µs).

## Decision

- Verify a request by re-signing a clone of it with aws-sdk-go-v2's
  `v4.Signer` and comparing the signatures in constant time.
- Keep one signer per access key (#37) in a fixed-size, direct-mapped table:
  a collision replaces the occupant, so memory stays bounded however many
  access keys exist. Default 512 slots, resizable with
  `SetSignerCacheSize`.
- Do not use an LRU for it: golang-lru takes an exclusive lock even on
  `Get`, which would bring the serialization back.

## Alternatives considered

- **Write our own canonicalizer.** Rejected at the time: the SDK signer was
  thought to be the definitive implementation, with no code of our own to
  get wrong. The 2026-08-28 review looked at it again and kept the SDK
  signer for the same reason.
- **One shared signer.** Rejected: measured to serialize verification.

## Consequences

- Many keys cost the same as one (parallel 16.4µs → 8.7µs, 117 → 89
  allocs). `BenchmarkVerifyKeyDiversity` guards this.
- The verifier can only accept what the SDK's signer would sign. That
  turned out to exclude valid requests — a signed `content-length:0` from
  aws-cli's CRT — and led to [0029](0029-build-sigv4-canonical-request-in-verifier.md),
  which builds the canonical request itself but keeps the direct-mapped
  per-key cache, now holding derived signing keys.
