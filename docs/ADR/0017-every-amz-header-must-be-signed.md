# 0017. Every x-amz-* header must be signed; handlers read through signedHeader

- Status: Accepted (recorded retroactively)
- Date: 2026-08-20
- Pull requests: [#108](https://github.com/fujiwara/s3rp/pull/108),
  [#109](https://github.com/fujiwara/s3rp/pull/109)

## Context

A SigV4 signature only commits to the headers in `SignedHeaders`. The
handlers read `x-amz-*` headers straight off the request, so the holder of
a presigned URL could add headers the grantor never signed: storage class,
Object Lock retention, an SSE mode and key, a copy source, tagging, user
metadata. Ceph RGW had exactly this bug (CVE-2026-54330, published
2026-08-19), and s3rp had the same gap.

## Decision

- The verifier refuses any request carrying an `x-amz-*` header outside
  `SignedHeaders` with `AccessDenied`, on both the header and the presigned
  path, as Amazon S3 does (#108).
- Non-`x-amz` headers are deliberately not gated: S3 requires only `x-amz-*`
  to be signed, and presigners leave `Content-Type` unsigned.
- Handlers read headers only through `signedHeader` (#109): `Signed` for
  anything with meaning beyond the object's own attributes, `Attribute` for
  the object's attributes. The check helpers (SSE, ACL) choose between the
  two inside, next to the reason, never through a getter the caller passes.
  A test bans direct `r.Header` reads in the operation files.

## Alternatives considered

- **Only the gate, no accessor.** Rejected: the gate is the primary defense,
  but the accessor makes the rule visible in every handler and keeps it if
  the gate ever regresses.
- **Caller-supplied getter functions.** Rejected: the safety-critical choice
  would move to every call site, where the type system cannot check it.

## Consequences

- Authorization, metering and resource selection only ever use
  signature-covered inputs.
- A presigned URL grants exactly what was signed.
