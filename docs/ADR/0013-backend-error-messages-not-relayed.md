# 0013. Backend error messages are not relayed to clients

- Status: Accepted (recorded retroactively)
- Date: 2026-08-02
- Pull request: [#63](https://github.com/fujiwara/s3rp/pull/63)

## Context

`s3err.FromSDKError` forwarded the backend's error message verbatim. A
security review flagged it as a disclosure risk. Probing showed the risk is
real but narrower than claimed:

- versitygw, Ceph RGW and Amazon S3 word their messages generically. Where
  Amazon S3 names a resource, it does so in separate XML elements
  (`BucketName`, `Endpoint`) that the SDK's deserializer drops, so no bundled
  backend leaked anything.
- Amazon S3 does document detailed authorization messages that name the
  principal and the resource. The principal there is the operator's backend
  identity, not the tenant's. Any backend may put anything into a message.
- Backends word the same error differently ("The specified key does not
  exist." vs "NoSuchKey").

## Decision

The client gets the backend's error **code** and HTTP status, with S3's
canonical message for that code (`canonicalMessage`); an unknown code's
message is the code itself. The backend's wording is kept as the error's
cause, for the observer only ([0009](0009-observe-not-log.md)). Errors the
gateway raises itself keep their own message.

## Alternatives considered

- **Forward the backend's message.** Rejected: a message written by another
  system is not the proxy's to relay.

## Consequences

- This is defense in depth and uniformity across backends, not the fix for a
  reproduced leak; the record says so to avoid overstating it.
- Operators still see the backend's wording, in the log.
