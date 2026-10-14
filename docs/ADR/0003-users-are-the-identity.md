# 0003. Users are the identity; access keys rotate under them

- Status: Accepted (recorded retroactively)
- Date: 2026-08-01
- Pull requests: [#13](https://github.com/fujiwara/s3rp/pull/13),
  [#14](https://github.com/fujiwara/s3rp/pull/14)

## Context

The first version tied front-side access keys to buckets. A multi-tenant
service needs an owner for buckets and credentials, and an identity that
policies can name. Access keys are a poor identity: they are rotated, and a
policy naming a key would have to be rewritten at every rotation.

## Decision

- A **tenant** owns buckets and users.
- A **user** (`[a-z0-9][a-z0-9_-]+`, unique within its tenant) is the
  rotation-stable identity. Access keys belong to a user and rotate under
  it.
- Policies name users (tenant-qualified, see
  [0015](0015-bucket-policy-model.md)), never access key ids.
- Bucket names and access key ids are globally unique across tenants: a
  path-style URL carries no tenant, and unauthenticated CORS preflights and
  cross-tenant access resolve a bucket by name alone.

## Alternatives considered

- **Access keys as the principal.** Rejected: every rotation would require
  editing every policy that names the key.
- **Bucket names unique per tenant.** Rejected: the bucket in a URL could
  not be resolved without knowing the tenant first.

## Consequences

- Rotation needs no policy change: add a key, move clients, remove the old
  key.
- Per-key accounting (last use) is still possible, because the verified
  access key id is reported alongside tenant and user.
