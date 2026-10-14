# 0020. A circuit breaker per backend

- Status: Accepted (recorded retroactively)
- Date: 2026-08-28
- Pull request: [#127](https://github.com/fujiwara/s3rp/pull/127)

## Context

One gateway fronts many backends. When one backend fails, requests to it
tie up connections and goroutines, and SDK retries multiply the load on the
failing backend. Buckets on healthy backends should not suffer for it.

## Decision

- `SetBreaker` gives each backend client (one per backend identity) a
  `Breaker`. It runs as SDK middleware **per attempt**, retries included.
- An open breaker refuses before the network with a `503 ServiceUnavailable`
  whose cause is not retryable, so the SDK does not retry the refusal.
- Attempt outcomes are classified: a status below 500 is the backend
  answering (a 404 proves it is up); 5xx, a transport failure and a
  deadline are failures; cancellation by the client is not reported. A
  transport failure arrives as an empty response with status 0, which must
  count as a failure.
- The bundled `ConsecutiveFailures` breaker advances only on `Allow` (no
  timers) and admits one probe per cooldown.
- Backend identity is not put on `Op`. The breaker's cause names the backend
  by scheme and host only.

## Alternatives considered

None were recorded. It was adopted from the framework comparison in
[0021](0021-framework-features-not-adopted.md).

## Consequences

- A dead backend fails its buckets fast instead of slowly.
- Stubs installed with `SetBackend` bypass the client builder and have no
  breaker; tests use a real client against a test server.
