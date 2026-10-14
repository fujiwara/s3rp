# 0011. Bucket configuration is written by the control plane, never proxied

- Status: Accepted (recorded retroactively)
- Date: 2026-08-02
- Pull requests: [#59](https://github.com/fujiwara/s3rp/pull/59) (lifecycle),
  [#79](https://github.com/fujiwara/s3rp/pull/79) (versioning, Object Lock)

## Context

Bucket policies and CORS rules were already read from the store, with their
writes returning `NotImplemented`. The question came up for the
configuration that lives on the backend bucket: lifecycle, versioning,
Object Lock, encryption.

- Lifecycle is enforced by the backend's own daemon, so its rules must be on
  the backend bucket. A bucket holds exactly one lifecycle configuration: a
  tenant's PUT would replace the operator's baseline rules (for example
  aborting stale multipart uploads).
- The authorization baseline gives every key of the owning tenant full
  access. Proxying PutBucketVersioning or PutObjectLockConfiguration let any
  data-plane key suspend versioning or rewrite the default retention,
  undoing what the bucket was provisioned with.

## Decision

Every bucket-configuration write is `NotImplemented`: bucket policy, CORS,
lifecycle, versioning, Object Lock configuration, encryption, ownership
controls and public access block, and their deletes. Configuration is
written where the bucket is created — the control plane, with backend
credentials. The reads stay available: proxied (GetBucketVersioning,
GetObjectLockConfiguration, GetBucketEncryption) or answered from the
store.

## Alternatives considered

- **Pass the writes through.** Rejected for the reasons above.
- **Merge tenant lifecycle rules with the operator's.** Possible later, but
  it is a merge layer, not a pass-through; a read-only GET from the store is
  the acceptable first step if tenants need visibility.

## Consequences

- A data-plane key cannot weaken retention or versioning.
- Tenants manage bucket configuration through the service's own API, not
  through S3 tools.
