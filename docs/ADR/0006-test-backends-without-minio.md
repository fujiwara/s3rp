# 0006. Test against versitygw and Ceph RGW, never MinIO

- Status: Accepted (recorded retroactively)
- Date: 2026-08-01
- Pull requests: [#17](https://github.com/fujiwara/s3rp/pull/17),
  [#18](https://github.com/fujiwara/s3rp/pull/18); RustFS added in
  [#142](https://github.com/fujiwara/s3rp/pull/142)

## Context

The integration suite needs real S3-compatible backends. MinIO was the
usual choice for such tests, but its open-source edition is effectively
unmaintained.

## Decision

- **versitygw** (posix backend) is the fast default backend for local runs
  and CI.
- **Ceph RGW** runs as a second CI matrix backend, since it is the expected
  production backend. It is reached through `127.0.0.1`, never `localhost`,
  because RGW treats a non-matching Host as a virtual-hosted bucket name.
- **RustFS** joined the matrix later (#142).
- **MinIO is never used.**

## Alternatives considered

- **MinIO.** Rejected: its open-source edition is no longer properly
  maintained.

## Consequences

- Backend differences are tested rather than assumed. Subtests that depend
  on a feature a backend lacks skip on it, and CI's integration summary
  shows each skip per backend.
- The Ceph service runs a trimmed bootstrap of the plain Ceph release image
  (`docker/ceph-demo.sh`), since the old demo image stopped being published.
