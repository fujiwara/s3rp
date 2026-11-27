# 0058. The tags a request writes are reported to the hooks

- Status: Accepted
- Date: 2026-11-27
- Pull request: TBD

## Context

A service built on the gateway has to decide on the tags themselves: refuse
a key under a prefix it keeps for its own use, or a set its bucket's backend
would not store. [0023](0023-s3-permissions-are-authorized-as-actions.md)
made tagging a permission — `s3:PutObjectTagging`, authorized and listed on
`Op.Actions` — and kept the values off `OpRequest`. An action says that a
request writes tags. It cannot say which, so an Authorizer could refuse all
tags or none.

The tags of a PutObjectTagging are in its body, which the handler read after
the hooks had run.

## Decision

- `OpRequest.Tags` is the tag set a request writes, as a map: the
  `x-amz-tagging` header of PutObject, CopyObject and
  CreateMultipartUpload, the tagging field of a POST upload, and the body of
  a PutObjectTagging. It is set before the Authorizer and has passed the
  gateway's validation ([0037](0037-validate-request-values-in-the-gateway.md)),
  which refuses a repeated key, so the map loses nothing.
- The body of a PutObjectTagging is read in `dispatch`, after the policies
  allowed the request and before the hooks. A request the policies refuse
  costs no body read; a malformed body is refused before the Authorizer is
  asked.
- Nil when the request writes no tags, as for `Metadata`, and excluded from
  JSON like it. A PutObjectTagging with an empty set reports none.
- A copy reports the tags it sends whatever its `x-amz-tagging-directive`.
- 0023 stands. Its boundary is that `OpRequest` carries what S3 has no
  action for, and which tags are written is that. Whether a request may tag
  at all is still decided by the action.

## Alternatives considered

- **A hook of its own for tags**, in the shape of `StorageClassMapper`.
  Rejected: a mapper exists to rewrite a value in both directions, and tags
  are only admitted or refused, which the Authorizer already does.
- **A `SetClientOptions` middleware inspecting the SDK input.** Possible
  today, but it runs per backend without the `Op`, so the tenant and the
  bucket's metadata have to be carried to it through the context.
- **Reporting the tags after the handler read them**, as `Op.Deleted` is.
  Rejected: that is after the Authorizer, too late to refuse.
- **Reporting a copy's tags only with `REPLACE`.** Rejected: the hook would
  have to know the directive's default to read an absent value, and
  refusing a tag set the backend would have ignored costs a client nothing
  it was owed.

## Consequences

- An Authorizer refuses a write on its tags, with the code and message of
  its choosing, through every way S3 has to send them.
- The body of a PutObjectTagging is read before `SetBandwidthLimit`'s
  limiters are armed, so it is not paced. It is at most the size of an XML
  request body and is counted in `BytesIn` as before.
- Tags a copy takes from its source do not pass through the gateway and are
  not reported. A reserved prefix holds all the same, since no client could
  write such a tag onto the source.
- Tags the service writes on the backend by its own means are returned by
  GetObjectTagging as stored: this reports writes and filters no read.
