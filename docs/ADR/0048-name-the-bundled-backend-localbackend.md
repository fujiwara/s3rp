# 0048. The bundled backend is named localbackend

- Status: Accepted
- Date: 2026-11-22
- Pull request: [#270](https://github.com/fujiwara/s3rp/pull/270)

## Context

The in-process backend of [0047](0047-backend-factory-and-in-process-backend.md)
was added as `s3gw/membackend`, after the one thing it did then: keep
everything in memory. Keeping its data in a directory, so that a local
stand-in for a service survives a restart, is a natural next step, and a
package named for memory could not hold it without its name becoming
false. The package had not been in a release.

## Decision

The package is `s3gw/localbackend`. The name says what stays true whatever
it stores in: the backend is in the gateway's own process, not behind an
S3 endpoint. Nothing else changes — the type is still `Backend`, `New`
still returns one held in memory.

## Alternatives considered

- **Keep `membackend`** and add a second package for a directory-backed
  one. The two would share every operation and differ only in where bytes
  are put.
- **`emulator`, `mockbackend`.** Names of a use. The emulator is a whole
  service — gateway, hooks, store — of which this is one part.
- **`embedded`, `inprocess`.** Accurate, but `embedded.Backend` does not
  say a backend of what, and neither reads as well in an import path.

## Consequences

- [0047](0047-backend-factory-and-in-process-backend.md) names the package
  as it was called when that record was written.
- The s3-tests harness keeps `S3RP_TEST_BACKEND_ENDPOINT=memory`: what it
  starts is the in-memory `New`.
