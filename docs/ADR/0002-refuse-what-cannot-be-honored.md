# 0002. Refuse what cannot be honored, never ignore it

- Status: Accepted (recorded retroactively)
- Date: 2026-07-31
- Pull requests: initial implementation (506d1a2); extended in
  [#58](https://github.com/fujiwara/s3rp/pull/58),
  [#138](https://github.com/fujiwara/s3rp/pull/138),
  [#159](https://github.com/fujiwara/s3rp/pull/159)

## Context

An operation-aware proxy ([0001](0001-operation-aware-proxy.md)) only
implements what it understands. A request can always carry more than that:
an unknown query subresource, an `x-amz-*` header no operation reads, a
checksum trailer the gateway cannot verify, a customer-provided encryption
key. Ignoring such input makes the request succeed while doing less than
the client believes — an encryption context dropped, a grant not applied, an
object stored unencrypted, an integrity check that never ran.

## Decision

Anything the gateway cannot honor is refused with an error, never silently
dropped:

- An unknown query subresource is `501 NotImplemented`. The SDK's `x-id`
  hint and `x-amz-*` query keys (presigned auth, hoisted headers) are exempt.
- An `x-amz-*` request header outside `knownAmzHeaders` is `501
  NotImplemented` on every authenticated entry path (#138). The list is
  global, and a test keeps it in step with the headers the code reads.
- SSE-C is `NotImplemented` for every operation (#58): dropping the key
  headers would store the object unprotected.
- A declared `x-amz-trailer` checksum the gateway cannot verify is `501`
  (#159); it used to be skipped, so an unverified upload passed as verified.
- Unknown POST form fields are `501`, mirroring unknown subresources.

## Alternatives considered

- **Ignore what is not understood, as many HTTP servers do.** Rejected: for
  storage, "succeeded with less than asked" is worse than an error, since
  the client cannot detect it.

## Consequences

- SDK options the gateway does not support fail visibly, so an operator
  learns what clients need, and no client is quietly given weaker
  guarantees.
- Every newly honored header or parameter has to be added to the known set
  (tests fail otherwise), and docs/request-headers.md lists the refusals.
