# 0044. Bucket-configuration reads are served from the store

- Status: Accepted
- Date: 2026-11-22
- Pull request: [#261](https://github.com/fujiwara/s3rp/pull/261)

## Context

Bucket configuration is written by the control plane, never through the
S3 API ([0011](0011-bucket-configuration-not-proxied.md)): a data-plane
key must not overwrite what a bucket was provisioned with. For lifecycle
there is a second reason. A backend bucket holds one lifecycle
configuration, which also carries the operator's baseline rules, so a
tenant's PutBucketLifecycleConfiguration would replace them.

That left `?lifecycle` answered `501 NotImplemented` in both directions,
and a tenant with no way to read the rules that apply to its bucket. The
same holds for the other bucket configuration a service may offer through
its control plane: replication, bucket tags, notifications, logging
([#255](https://github.com/fujiwara/s3rp/issues/255) to
[#260](https://github.com/fujiwara/s3rp/issues/260)).

Bucket policies and CORS already have the answer: the definition store
carries them, and GetBucketPolicy returns `PolicyText` without asking a
backend.

## Decision

A bucket-configuration read is answered from the store's definition of the
bucket.

- `store.Bucket` carries the document a tenant is shown. For lifecycle it
  is `LifecycleText`, the XML GetBucketLifecycleConfiguration returns
  verbatim; without one the answer is `NoSuchLifecycleConfiguration`.
- The gateway does not parse the document and applies nothing. A
  write-time check in the `store` package (`ValidateLifecycleText`) covers
  only what serving needs: well-formed XML with the right root element.
- The document is the tenant's own configuration, which is not
  necessarily what the backend bucket holds. Keeping the two in step is
  the control plane's.
- The read is `supported` in the catalog with the action S3 uses
  (`s3:GetLifecycleConfiguration`); the `Put` and `Delete` are `refused`,
  so an attempt is recorded under its name.

## Alternatives considered

- **Proxy the read to the backend.** The backend's configuration includes
  the operator's rules and names things by the backend's names; showing it
  would leak both.
- **A typed rule set on `store.Bucket`, rendered by the gateway.** The
  lifecycle schema is large and S3 keeps adding to it; the gateway would
  have to follow a schema it never acts on. CORS is typed because the
  gateway evaluates it.
- **One generic map of documents for every configuration.** What an unset
  configuration answers differs (a 404 for lifecycle, an empty document
  for notifications), and bucket tags are better typed, so the fields stay
  named and only the writing of a stored document is shared.
- **Accept the write and merge it with the operator's rules.** That is the
  control plane's work, with backend credentials; the gateway holds
  neither the baseline nor the right to write bucket configuration.

## Consequences

- A tenant can read its lifecycle rules with any S3 client. Tools that
  write them (`aws s3api put-bucket-lifecycle-configuration`, Terraform)
  still meet a 501 and have to go through the service's own interface.
- What is shown is what the control plane recorded. A store that leaves
  the field empty answers as a bucket without rules does.
- `x-amz-expiration` on object responses stays unrelayed: the backend
  computes it from its own rules, the operator's included.
- The bundled YAML config has a `lifecycle` key. Nothing applies it to the
  backend, so there it is a declaration the operator keeps true by hand.
