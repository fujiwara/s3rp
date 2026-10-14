# 0023. What S3 models as a permission is authorized as an action

- Status: Accepted (recorded retroactively)
- Date: 2026-09-02
- Pull request: [#134](https://github.com/fujiwara/s3rp/pull/134)

## Context

A service built on the gateway needed to refuse tags and Object Lock
settings on plain uploads. The obvious change was to add a field to
`OpRequest` for each such header, so an Authorizer could inspect it. That
would grow `OpRequest` by a field per header. Amazon S3 already models
these as permissions: a `PutObject` with `x-amz-tagging` also requires
`s3:PutObjectTagging`.

## Decision

- An operation authorizes every action S3 requires for it, including those
  a header adds: `x-amz-tagging` → `s3:PutObjectTagging`, Object Lock
  retention headers → `s3:PutObjectRetention`, legal hold →
  `s3:PutObjectLegalHold`, `x-amz-bypass-governance-retention` →
  `s3:BypassGovernanceRetention`.
- `Op.Actions` lists every action authorized, the operation's own first.
  The single `Op.Action` field was removed, since it invited matching
  against the wrong one.
- The boundary: what S3 models as a permission is authorized and refused by
  name, through a policy `Deny` or an Authorizer over `Op.Actions`. Only
  attributes S3 has no action for (SSE, storage class, user metadata) are
  values on `OpRequest`.

## Alternatives considered

- **A field per header on `OpRequest`.** Rejected: unbounded growth, and a
  second way to express something policies already express.

## Consequences

- A tenant's policy and a service's Authorizer can both refuse the same
  thing, in S3's own vocabulary.
- Breaking change for hooks that read `Op.Action` (`slices.Contains(op.Actions, …)`
  instead).
- The action table later became the `s3op` catalog
  ([#164](https://github.com/fujiwara/s3rp/pull/164)), the single place an
  operation's actions are written.
