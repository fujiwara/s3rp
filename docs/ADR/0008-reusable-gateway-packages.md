# 0008. Split the gateway into reusable packages, in one repository for now

- Status: Accepted (recorded retroactively)
- Date: 2026-08-01
- Pull requests: [#39](https://github.com/fujiwara/s3rp/pull/39) through
  [#46](https://github.com/fujiwara/s3rp/pull/46)

## Context

A production service is expected to be built on this PoC by reusing its
code, not by forking the proxy. The proxy had grown in the root package,
mixed with the YAML config, the CLI and the server.

## Decision

- The S3 API — verification, authorization, operations, routing — moves
  into `s3gw/`, which depends only on leaf packages and a `store.Store`. The
  root package keeps only the assembly (config, YAML store, server, CLI),
  about 500 lines.
- Protocol knowledge moves into leaf packages that carry no proxy types:
  `sigv4/`, `policy/`, `checksum/`, `s3err/`, `s3xml/`, `cors/`, `store/`
  (later `s3op/`).
- Extracted packages are referenced qualified; aliases in the root package
  were dropped so the dependency is visible (#43).
- The gateway is to become its own **repository** once its API stops
  moving, not a second module in this repository. A multi-module monorepo
  was rejected even as an intermediate step: it would mean two tag lines and
  two-step releases without being the final shape.

## Alternatives considered

- **Split into a separate repository now.** Postponed: while the hooks and
  `Op` were still changing, one pull request changing both sides was worth
  more than the cleaner boundary. Module graph pruning already keeps the
  binary's dependencies (YAML, CLI) out of a gateway consumer's build.
- **Multi-module repository.** Rejected, see above.

## Consequences

- A service embeds `s3gw.Gateway` with its own store; the docs are split
  so that what moves with the gateway (docs/s3-api.md,
  docs/building-a-service.md) is separate from the binary's README.
- Gateway tests live in `s3gw/`, not in the root package, so the package
  stays tested on its own when it moves.
