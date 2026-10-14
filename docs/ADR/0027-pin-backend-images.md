# 0027. Pin the integration backend images; dependabot bumps them

- Status: Accepted (recorded retroactively)
- Date: 2026-09-28
- Pull request: [#163](https://github.com/fujiwara/s3rp/pull/163)

## Context

versitygw ran as `latest`, so CI and a local image cache could test
different versions without anyone noticing. RustFS and Ceph were never
bumped at all.

## Decision

- The backend images in compose.yml are pinned by tag, never `latest`.
- dependabot's `docker-compose` ecosystem bumps them, so every backend
  update arrives as a pull request that the integration matrix tests. All
  dependabot updates run weekly.
- Ceph major versions are bumped by hand: x.1 release candidates cannot be
  told apart by tag, and a major can need changes to `docker/ceph-demo.sh`.
- The s3-tests workflow uses the compose image instead of its own tag.

## Alternatives considered

- **Pin by digest.** Not chosen: tags were the owner's choice.

## Consequences

- A backend behavior change shows up in a dependabot pull request, not in an
  unrelated one.
