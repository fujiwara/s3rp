# 0022. Keep the backend client LRU lock and the random request id

- Status: Accepted (recorded retroactively)
- Date: 2026-08-31
- Pull requests: none (profiling)

## Context

Profiling `BenchmarkVerifyKeyDiversity` found two contention points:

- The backend client cache (golang-lru) takes an exclusive lock on every
  `Get`. It was 59% of the parallel mutex delay.
- The fallback request id is drawn from `crypto/rand` on every request.

## Decision

Neither is changed.

- **Client cache lock:** measured at about +0.7µs per request at 209k
  requests/s, with the lock held about 29ns, for a hard ceiling far beyond
  any reachable rate. At such request rates, any normal mix of object sizes
  saturates the network and the data-path CPU long before the lock (about
  0.25 cores per 100 MiB/s). A read-through front cache was designed and
  rejected as not worth its complexity.
- **Random request id:** a production service replaces it with its trace id
  through `SetRequestID`, so the fallback's cost does not matter.

## Alternatives considered

- **A lock-free read-through cache in front of the LRU.** Rejected, see
  above.

## Consequences

- Revisit only with a workload argument: a metadata-only or tiny-object
  profile that actually reaches those rates.
- This differs from the signer cache, which avoids a shared exclusive lock
  because there the contention was measured to serialize verification
  ([0007](0007-verify-by-re-signing-with-one-signer-per-key.md)).
