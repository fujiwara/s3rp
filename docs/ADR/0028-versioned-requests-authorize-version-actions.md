# 0028. Versioned requests authorize the version action, as on AWS

- Status: Accepted (recorded retroactively)
- Date: 2026-09-30
- Pull request: [#166](https://github.com/fujiwara/s3rp/pull/166)

## Context

A request naming an object version was authorized with the plain action
(`s3:GetObject`, `s3:DeleteObject`). AWS policies written with
`s3:GetObjectVersion` or `s3:DeleteObjectVersion` therefore had no effect.
There were two ways to fix it: require both actions, or follow AWS and
require only the version action.

## Decision

- A request that names a version — the `versionId` query, a copy source's
  `versionId`, or a DeleteObjects entry's `VersionId` — is authorized with
  the version action **instead of** the plain one, as on AWS. The `s3op`
  catalog carries the pairing as `VersionAction`.
- ListObjectVersions authorizes `s3:ListBucketVersions`, and ListBuckets
  authorizes `s3:ListAllMyBuckets` against the user policy alone.

## Alternatives considered

- **Require both actions.** Rejected: it would differ from AWS, so policies
  would not port.

## Consequences

- A `Deny` on `s3:DeleteObject` alone does not cover versioned deletes, as
  on AWS. The docs tell authors to name both.
- Breaking for existing policies that denied only the plain actions, or
  allow-listed them.
