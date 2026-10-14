# 0009. The gateway observes; it does not log

- Status: Accepted (recorded retroactively)
- Date: 2026-08-01
- Pull requests: [#44](https://github.com/fujiwara/s3rp/pull/44),
  [#48](https://github.com/fujiwara/s3rp/pull/48); panics in
  [#111](https://github.com/fujiwara/s3rp/pull/111)

## Context

Failures were mapped to S3 errors and their cause dropped: a backend
connection refused reached the client as a generic 502 and left no trace
for the operator. Logging at each failure site would scatter output, and a
library that logs imposes its logger and format on the service that embeds
it ([0008](0008-reusable-gateway-packages.md)).

## Decision

- `s3err.Error` carries an unexported cause. It never appears in the XML,
  since it may name backend endpoints and buckets.
- The gateway reports each request exactly once, after the response is
  written, to an observer (`SetObserver`) with a `RequestInfo`: identity
  (filled as soon as the signature verifies), the operation (once routing
  and policies pass), status, error code, the cause, byte counts and
  duration. The report is keyed by the same `x-amz-request-id` the client
  receives.
- Failure sites do not log. Extracted packages do not import `log/slog`;
  they return causes.
- Panics are reported the same way (#111): recovered, answered with
  `InternalError` or an aborted connection, and handed to the observer.
- Without an observer the gateway is silent. The binary installs one that
  writes the access and failure log.

## Alternatives considered

- **Log at the failure site.** Rejected: duplicate lines, no single record
  per request, and a library deciding the service's logging.

## Consequences

- A request id reported by a user leads to the reason.
- A service decides format, level and destination, and can derive metrics
  from the same records ([0025](0025-metrics-as-a-convention.md)).
- Backend-side facts (backend request ids) are carried in the request
  context by the service's own client middleware, not added to
  `RequestInfo`.
