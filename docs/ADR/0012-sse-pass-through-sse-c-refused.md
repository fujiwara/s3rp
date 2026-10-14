# 0012. SSE-S3 and SSE-KMS pass through; SSE-C is refused

- Status: Accepted (recorded retroactively)
- Date: 2026-08-02
- Pull request: [#58](https://github.com/fujiwara/s3rp/pull/58)

## Context

Clients request server-side encryption with headers. The backend does the
encrypting: Ceph RGW talks to a KMS, versitygw does not encrypt at all. A
backend without SSE may silently ignore the request instead of refusing it.
With SSE-C the client sends the key with every request.

## Decision

- SSE-S3 and SSE-KMS are passed to the backend, and the result headers are
  returned. The KMS key id is an opaque name the backend's KMS resolves.
- The requested mode and key id are exposed to the hooks (today on
  `Op.Request`, with what the backend reports applying on `Op.Response`), so a service can authorize key
  ownership and per-backend capability at the gateway — the backend's KMS
  request carries no tenant identity.
- **SSE-C is refused** with `NotImplemented` on every operation, checked
  once in dispatch. Silently dropping the key headers would store the object
  unprotected while the client believes otherwise.

## Alternatives considered

- **Drop the SSE-C headers and store the object normally.** Rejected: the
  client would believe the object is protected by its key when it is not.

## Consequences

- Encryption capability depends on the backend; a service must check its
  backends honor what it offers.
- Key ids are backend names; [0024](0024-map-backend-storage-class-and-kms-keys.md)
  adds a mapper so tenants do not see them.
