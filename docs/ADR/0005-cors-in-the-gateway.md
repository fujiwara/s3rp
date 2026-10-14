# 0005. CORS is handled by the gateway

- Status: Accepted (recorded retroactively)
- Date: 2026-08-01
- Pull request: [#16](https://github.com/fujiwara/s3rp/pull/16)

## Context

Browser-direct uploads with presigned URLs or POST policies need CORS: the
browser sends an unauthenticated `OPTIONS` preflight before the real
request. Delegating CORS to a CDN or to the backend was considered, but
the preflight must answer per bucket, and the backend bucket is not what
the client addresses.

## Decision

- The gateway evaluates CORS rules itself. Rules are part of the bucket
  definition in the store (`store.Bucket.CORS`).
- Preflight `OPTIONS` is handled before signature verification, resolving
  the bucket by name; a refusal is `AccessForbidden`.
- GetBucketCors reads the stored rules. PutBucketCors / DeleteBucketCors
  are not proxied ([0011](0011-bucket-configuration-not-proxied.md)).
- Rule matching lives in the reusable `cors/` package; `Rule.Validate` is
  the write-time check a store must run.

## Alternatives considered

- **Pass CORS through to the backend.** Rejected: the backend sees backend
  bucket names and would need its rules written there anyway.
- **Leave CORS to a CDN in front.** Rejected: rules differ per bucket and
  belong to the tenant's bucket definition.

## Consequences

- An unauthenticated code path exists before verification; it resolves the
  bucket but touches no backend and reveals nothing beyond the CORS answer.
- Bucket names must be globally unique for the preflight to resolve them
  ([0003](0003-users-are-the-identity.md)).
