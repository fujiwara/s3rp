# Architecture Decision Records

Each record captures one decision: the context that forced it, what was
decided, the alternatives rejected and the consequences accepted. Records
are numbered in the order the decisions were made, and a record is never
rewritten after it is accepted: a later decision that changes it gets a new
record, and the old record's status points to it ("Superseded by NNNN").

Records 0001–0028 were written after the fact, from the pull requests and
design notes of the time, and say so in their status ("recorded
retroactively").

| # | Decision | Date | Status |
|---|---|---|---|
| [0001](0001-operation-aware-proxy.md) | Re-execute S3 operations instead of forwarding requests | 2026-07-31 | Accepted |
| [0002](0002-refuse-what-cannot-be-honored.md) | Refuse what cannot be honored, never ignore it | 2026-07-31 | Accepted |
| [0003](0003-users-are-the-identity.md) | Users are the identity; access keys rotate under them | 2026-08-01 | Accepted |
| [0004](0004-acl-disabled-buckets.md) | ACLs behave as on ACL-disabled buckets | 2026-08-01 | Accepted |
| [0005](0005-cors-in-the-gateway.md) | CORS is handled by the gateway | 2026-08-01 | Accepted |
| [0006](0006-test-backends-without-minio.md) | Test against versitygw and Ceph RGW, never MinIO | 2026-08-01 | Accepted |
| [0007](0007-verify-by-re-signing-with-one-signer-per-key.md) | Verify by re-signing with the SDK signer, one signer per access key | 2026-08-01 | Superseded |
| [0008](0008-reusable-gateway-packages.md) | Split the gateway into reusable packages, in one repository for now | 2026-08-01 | Accepted |
| [0009](0009-observe-not-log.md) | The gateway observes; it does not log | 2026-08-01 | Accepted |
| [0010](0010-read-only-definition-store.md) | The definition store is read-only; writes belong to the control plane | 2026-08-02 | Accepted |
| [0011](0011-bucket-configuration-not-proxied.md) | Bucket configuration is written by the control plane, never proxied | 2026-08-02 | Accepted |
| [0012](0012-sse-pass-through-sse-c-refused.md) | SSE-S3 and SSE-KMS pass through; SSE-C is refused | 2026-08-02 | Accepted |
| [0013](0013-backend-error-messages-not-relayed.md) | Backend error messages are not relayed to clients | 2026-08-02 | Accepted |
| [0014](0014-responses-expose-only-the-front-namespace.md) | Responses expose only the front namespace | 2026-08-02 | Accepted |
| [0015](0015-bucket-policy-model.md) | Bucket policies: tenant-qualified principals, owner baseline, no ARNs | 2026-08-06 | Accepted |
| [0016](0016-sigv4-query-canonicalization-settled.md) | SigV4 query canonicalization stays as aws-sdk-go-v2 does it | 2026-08-15 | Accepted |
| [0017](0017-every-amz-header-must-be-signed.md) | Every x-amz-* header must be signed; handlers read through signedHeader | 2026-08-20 | Accepted |
| [0018](0018-op-request-claim-op-response-fact.md) | Op.Request is the client's claim, Op.Response the backend's fact | 2026-08-20 | Accepted |
| [0019](0019-denial-reasons-for-the-operator-only.md) | Denial reasons go to the operator, never to the client | 2026-08-28 | Accepted |
| [0020](0020-per-backend-circuit-breaker.md) | A circuit breaker per backend | 2026-08-28 | Accepted |
| [0021](0021-framework-features-not-adopted.md) | Proxy-framework features deliberately not adopted | 2026-08-28 | Accepted |
| [0022](0022-declined-lock-contention-work.md) | Keep the backend client LRU lock and the random request id | 2026-08-31 | Accepted |
| [0023](0023-s3-permissions-are-authorized-as-actions.md) | What S3 models as a permission is authorized as an action | 2026-09-02 | Accepted |
| [0024](0024-map-backend-storage-class-and-kms-keys.md) | Storage classes and KMS key ids are mapped, both ways, by one interface | 2026-09-11 | Accepted |
| [0025](0025-metrics-as-a-convention.md) | Metrics are a convention defined in a Weaver registry | 2026-09-28 | Accepted |
| [0026](0026-checksum-support-per-gateway.md) | Checksum support is one policy for the whole gateway | 2026-09-28 | Accepted |
| [0027](0027-pin-backend-images.md) | Pin the integration backend images; dependabot bumps them | 2026-09-28 | Accepted |
| [0028](0028-versioned-requests-authorize-version-actions.md) | Versioned requests authorize the version action, as on AWS | 2026-09-30 | Accepted |
| [0029](0029-build-sigv4-canonical-request-in-verifier.md) | Build the SigV4 canonical request in the verifier | 2026-10-14 | Accepted |
| [0030](0030-verify-with-derived-signing-key.md) | Verify SigV4 with a derived signing key | 2026-10-19 | Accepted |

## Writing a record

Name the file `NNNN-short-title.md`: a four-digit, zero-padded number one
past the highest existing record, and a kebab-case title. Add a row to the
table above. Use this layout:

```markdown
# NNNN. Title

- Status: Proposed | Accepted | Superseded by NNNN
- Date: YYYY-MM-DD
- Pull request: #N

## Context
## Decision
## Alternatives considered
## Consequences
```
