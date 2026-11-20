# 0040. ListBuckets accepts and ignores `max-keys`

- Status: Accepted
- Date: 2026-11-20
- Pull request: [#240](https://github.com/fujiwara/s3rp/pull/240)

## Context

A query parameter no operation reads is refused with `501 NotImplemented`
([0002](0002-refuse-what-cannot-be-honored.md)). ListBuckets takes
`max-buckets`, `continuation-token` and `prefix`.

WinSCP opens every S3 session with `GET /?max-keys=1`: a ListBuckets
carrying the page-size parameter of an object listing. Amazon S3 does not
define `max-keys` there; the AWS Emulator (1.0.533), asked the same, answers
with the full bucket list and no continuation token, as it does for any
parameter it does not know (the fidelity suite has no test of it). The
gateway refused the request, so WinSCP could not connect at all, whatever
the session went on to do. The client compatibility run found it
([0039](0039-run-real- clients-against-the-gateway.md)).

## Decision

ListBuckets accepts `max-keys` and does not read it. The response is what
it would be without the parameter.

This follows the criterion [0032](0032-accept-headers-ignorable-by-protocol.md)
set for headers: a value may be ignored only when the client cannot come
to believe it applied. A client that asks for one key and receives the
whole list sees that in the response, and receives the same from S3.

## Alternatives considered

- **Keep the 501.** WinSCP has no setting that stops the request, so its
  users would have no way to connect.
- **Honor it as `max-buckets`.** A client that sends it to S3 gets an
  untruncated list and has no reason to follow a continuation token; a
  truncated answer would hide buckets from it.
- **Ignore every unknown parameter on ListBuckets.** The rule of 0002
  stands; this is one named parameter with a known sender.

## Consequences

- WinSCP connects.
- `max-keys` on ListBuckets limits nothing. docs/s3-api.md says so.
