# 0033. Headers an intermediary adds pass the signed-header gate unsigned

- Status: Accepted
- Date: 2026-11-18
- Pull request: [#223](https://github.com/fujiwara/s3rp/pull/223)

## Context

Every `x-amz-*` request header must be covered by the signature
([0017](0017-every-amz-header-must-be-signed.md)); one that is not is
refused with `AccessDenied`.

CloudFront adds `X-Amz-Cf-Id`, its request id, to every request it forwards
to an origin. Its documentation lists the header among those "automatically
include[d] in every origin request" and describes no setting that removes
it. The client signed before CloudFront added it, so the header is never in
`SignedHeaders` and the gate refused every request served through
CloudFront.

## Decision

- The verifier exempts a fixed list, `intermediaryAmzHeaders`, from the
  gate. It holds `x-amz-cf-id`.
- A header may join the list only when a hop in front adds it and it
  carries no instruction. An exempt header is attacker-controlled, so
  nothing may read it.
- The gateway lists every exempt header in `ignoredAmzHeaders`
  ([0032](0032-accept-headers-ignorable-by-protocol.md)), whose test bans
  reading a listed header; `TestIntermediaryAmzHeadersAreIgnored` keeps the
  two lists in step through `sigv4.IntermediaryAmzHeaders`.
- The list is not configurable.

## Alternatives considered

- **Have the service delete the header in a handler in front of the
  gateway.** It works and needs no change here, but every service behind
  CloudFront would have to discover the header from a 403, and the bundled
  binary has no such handler.
- **A setter for extra unsigned headers.** A service could then exempt a
  header that does carry meaning, which is the hole 0017 closed. The
  exemption is safe only for headers known to be read nowhere, and that is
  knowledge the gateway holds, not the service.
- **Gate only the headers the gateway reads.** Inverts 0017 from
  refuse-by-default to accept-by-default; a newly handled header would be
  unguarded until someone remembered to list it.

## Consequences

- A request through CloudFront is no longer refused for `X-Amz-Cf-Id`.
  CloudFront must still forward `Host`, `Authorization` and the client's
  `x-amz-*` headers unchanged.
- Anyone can attach an unsigned `x-amz-cf-id` to a request, with or without
  CloudFront in front. It changes nothing, because nothing reads it.
- Whether Amazon S3 itself exempts the header was not measured.
- A service that wants the CloudFront id in its log reads it in its own
  handler, knowing the value is unauthenticated.
