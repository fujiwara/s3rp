# 0018. Op.Request is the client's claim, Op.Response the backend's fact

- Status: Accepted (recorded retroactively)
- Date: 2026-08-20
- Pull request: [#110](https://github.com/fujiwara/s3rp/pull/110)

## Context

Hooks could not see the object's storage class or user metadata, and
`Op.SSE` mixed what the client asked for with nothing to compare it
against. Metering wants a per-object billing tier. Measuring backends
showed that a requested attribute is not a fact: Ceph RGW refuses an
undefined storage class with `InvalidArgument`, versitygw silently stores
it as STANDARD, and no S3 write returns the resulting class.

## Decision

- `Op.Request` holds the client's claim (SSE, storage class, `x-amz-meta-*`),
  settled before the Authorizer. It is used to authorize.
- `Op.Response` holds what the backend reported (SSE, stored class and
  metadata on reads, ETag, version id), filled once the operation returns.
  It is used to bill and audit.
- Both are pointers; `nil` is the only way to say "nothing to report".
- A per-object billing tier rides on the storage class, not on user
  metadata, because listings return the storage class but not metadata or
  tags. Capacity per class comes from a backend listing sweep; the hooks
  cover requests and egress.

## Alternatives considered

- **Bill from the requested class.** Rejected: the request is not what the
  backend stored.
- **A tier in user metadata.** Rejected: a capacity sweep would need a HEAD
  per object.

## Consequences

- `OpRequest` is bounded: attributes S3 models as permissions go through
  authorization instead ([0023](0023-s3-permissions-are-authorized-as-actions.md)).
