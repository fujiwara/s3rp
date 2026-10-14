# 0004. ACLs behave as on ACL-disabled buckets

- Status: Accepted (recorded retroactively)
- Date: 2026-08-01
- Pull request: [#14](https://github.com/fujiwara/s3rp/pull/14) (236db5b)

## Context

S3 ACLs are a legacy access-control model; Amazon S3 disables them on new
buckets by default since 2023 (Object Ownership `BucketOwnerEnforced`).
Clients and SDKs still call the ACL operations and send canned-ACL headers.
Proxying ACLs to the backend would expose the backend's account model and
add a second access-control system next to bucket policies.

## Decision

The gateway answers as an ACL-disabled bucket does, without a backend call:

- GetBucketAcl / GetObjectAcl return a fixed stub granting the
  **bucket-owning tenant** FULL_CONTROL.
- ACL writes, canned ACL headers and `x-amz-grant-*` headers are refused
  with `AccessControlListNotSupported`.
- The related bucket-inspection reads are answered from the same model
  (added in [#120](https://github.com/fujiwara/s3rp/pull/120)):
  GetBucketPolicyStatus (`IsPublic=false`), GetBucketOwnershipControls
  (`BucketOwnerEnforced`), GetPublicAccessBlock (all `true`).

## Alternatives considered

- **Proxy ACLs to the backend.** Rejected: it duplicates access control,
  exposes backend accounts, and grants that bypass bucket policies would be
  possible.

## Consequences

- Access control is bucket policies and user policies only.
- Tools that read ACLs keep working; tools that write them get the same
  error Amazon S3 gives for an ACL-disabled bucket.
