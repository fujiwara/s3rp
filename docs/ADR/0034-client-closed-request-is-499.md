# 0034. A request the client abandoned is recorded as 499

- Status: Accepted
- Date: 2026-11-18
- Pull request: [#224](https://github.com/fujiwara/s3rp/pull/224)

## Context

When a client disconnects before it is answered, net/http cancels the
request's context. The backend call then fails — with that cancellation, or
with the read error of a body that stopped arriving — and the error mapping
turned it into `504 InternalError "backend request timed out"` or
`502 InternalError`. Nobody receives that response, but the observer
records it: a client giving up on a large transfer read as a backend
failure in the log and counted as a 5xx in every metric derived from it.

## Decision

- Such a request is recorded with status 499 and code
  `ClientClosedRequest`, and its cause wraps `s3gw.ErrClientClosed`.
- It is recognized at the boundary, for every operation at once: the
  inbound context is canceled, and the failure stems from that — the
  cancellation is in the error's chain, or reading the request body failed.
- A panic is never relabeled.

## Alternatives considered

- **Leave the status and let the service look at the cause.** Every service
  would have to know which SDK error strings mean "the client left", and
  the metrics would still count a 5xx.
- **Relabel any failure of a request whose context is canceled.** A refusal
  the client was owed anyway (a 501 for an unknown header, a 403) would be
  recorded as 499 whenever the client left before reading it, hiding what
  the gateway decided.
- **Relabel only 5xx.** 501 and 503 are refusals the gateway or a hook
  chose; the status does not say whether the client caused the failure.
- **An S3 status such as `400 RequestTimeout`.** It means the server gave up
  on an idle socket, which is a different event, and it would hide these
  among real client errors.

## Consequences

- 499 is not an S3 or HTTP status. It appears only in the record, as on
  nginx; error-rate objectives should exclude it.
- A backend timeout (`context.DeadlineExceeded`) is still a 504.
- A client that leaves after the response has started is not covered: the
  status was already sent.
