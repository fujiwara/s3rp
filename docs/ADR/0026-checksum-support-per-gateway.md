# 0026. Checksum support is one policy for the whole gateway

- Status: Accepted (recorded retroactively)
- Date: 2026-09-28
- Pull requests: [#160](https://github.com/fujiwara/s3rp/pull/160),
  [#161](https://github.com/fujiwara/s3rp/pull/161),
  [#162](https://github.com/fujiwara/s3rp/pull/162)

## Context

S3 defines ten checksum algorithms. Backends support different subsets:
versitygw and RustFS handle all of them, while Ceph RGW (tentacle) refuses
MD5 and the XXHASH family and reports SHA512 under a nonstandard header.
The gateway can verify only what the standard library computes. Clients
cannot tell which backend serves a bucket.

## Decision

- Capability and policy are separate. `checksum.Algorithms` lists every
  algorithm and whether the gateway can compute it (`Computable`). What a
  service offers is `Gateway.SetChecksumSupport`: for each algorithm,
  verified, forwarded or refused.
- The policy is for the **whole gateway, never per backend**: a service on
  mixed backends offers the common subset, so every bucket behaves the same.
- `SetChecksumSupport` replaces the policy; an algorithm it does not list is
  refused, so a later default cannot widen a service.
  `DefaultChecksumSupport()` verifies CRC32, CRC32C, CRC64NVME, SHA1 and
  SHA256.
- Responses show only offered algorithms.
- XXHASH is not supported for now: it needs a non-standard-library
  implementation.
- On a plain-http backend, trailer checksums are verified by the gateway but
  not stored by the backend. This is documented as a limitation (#162), and
  https backends are recommended, rather than reporting the gateway's check
  as a stored checksum.

## Alternatives considered

- **Per-backend support.** Rejected: behavior would depend on something the
  client cannot see.
- **Report the gateway-verified value as the stored checksum on http
  backends.** Rejected: it would present a check of what the gateway
  received as a checksum of what the backend stored.

## Consequences

- Offering an algorithm is a promise for every bucket; a service must check
  each backend verifies, stores and reports it.
- The per-algorithm conversions are generated, and tests keep the table in
  step with the SDK and the docs.
