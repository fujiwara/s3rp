# 0015. Bucket policies: tenant-qualified principals, owner baseline, no ARNs

- Status: Accepted (recorded retroactively)
- Date: 2026-08-06
- Pull requests: [#15](https://github.com/fujiwara/s3rp/pull/15),
  [#70](https://github.com/fujiwara/s3rp/pull/70),
  [#71](https://github.com/fujiwara/s3rp/pull/71),
  [#72](https://github.com/fujiwara/s3rp/pull/72)

## Context

Bucket policies were added early (#15) with plain user names as principals.
Cross-tenant access (#70) made plain names ambiguous: a name is unique only
within a tenant. Tenants author the policies, so the syntax is a user-facing
contract, and the policies are evaluated on every request.

## Decision

- **Syntax:** AWS-style policy JSON. Principals sit under the `"S3RP"` key
  and are always tenant-qualified (#71): `"tenant/user"`, `"tenant/*"`, or
  `"*"` for every authenticated user of any tenant. Anonymous access does
  not exist. Resources are plain `bucket/prefix*`, with no ARNs anywhere.
- **Baseline:** users of the bucket-owning tenant have full access, and
  explicit `Deny` restricts it. Cross-tenant requesters get the inverse
  baseline: denied unless an `Allow` matches, and `Deny` still wins.
- **Matching:** principal matching is uniform, so `Allow "*"` really opens
  the bucket to every tenant. `NotPrincipal` is accepted only with `Deny`.
- **Visibility:** a cross-tenant requester whom the policy grants nothing
  gets the same `AccessDenied` as for a missing bucket, so bucket names
  cannot be probed across tenants.
- **Conditions:** only `IpAddress`/`NotIpAddress` on `aws:SourceIp` (#72),
  both failing closed; unknown operators and keys are rejected at parse.
- **Dialects:** a `policy.Dialect` can map other surface syntaxes into this
  form. Evaluation itself is never configurable.
- **Bounds:** policies are untrusted input with structural caps (size,
  statements, actions, resources, principals, condition values), and
  patterns are compiled once.

## Alternatives considered

- **Plain user names, resolved in the bucket's tenant.** Rejected once
  cross-tenant grants existed: the same text would mean different users.
- **ARNs.** Rejected: an explicit preference of the project owner for plain
  `bucket/prefix` resources.

## Consequences

- Policy authors write `"mytenant/*"` to mean their own users; `"*"` is a
  deliberate, cross-tenant grant.
- Copying from another tenant's bucket stays impossible even with a read
  grant, since the copy source resolves within the requester's tenant.
