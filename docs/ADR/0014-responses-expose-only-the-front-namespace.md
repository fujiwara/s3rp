# 0014. Responses expose only the front namespace

- Status: Accepted (recorded retroactively)
- Date: 2026-08-02
- Pull request: [#68](https://github.com/fujiwara/s3rp/pull/68); the rule
  dates from the per-bucket backend mapping of the initial implementation

## Context

A front bucket maps to a backend bucket that may have another name, in
another region, owned by the operator's backend account. Backend responses
carry all of these. Each one leaks the service's physical layout to tenants,
and some break clients: a region-honoring client that is told the backend's
region re-signs for it, and a gateway pinned to its own region then refuses
the request.

## Decision

Everything a response shows is in the front namespace:

- **Bucket names:** the front name (ListObjectsV2 `Name`, multipart results,
  error `Resource`), never the backend's.
- **Region:** GetBucketLocation and HeadBucket's `x-amz-bucket-region`
  report the gateway's own region (`SetRegion`, `us-east-1` when unset).
- **Owners:** every `Owner`/`Initiator` (listings, the ACL stub,
  ListBuckets) is a tenant — the **bucket-owning** tenant, which differs from
  the requester on a cross-tenant request — never the backend account.
- **Backend sharing:** two tenants may not map to the same physical backend
  bucket (endpoint + bucket name); config validation rejects it, since
  sharing would break tenant isolation.

## Alternatives considered

- **Relay backend values.** Rejected: they describe the operator's
  infrastructure, not the tenant's view.

## Consequences

- New response fields must be checked against this rule.
- The same reasoning later covered storage classes and KMS key ids
  ([0024](0024-map-backend-storage-class-and-kms-keys.md)) and error
  messages ([0013](0013-backend-error-messages-not-relayed.md)).
