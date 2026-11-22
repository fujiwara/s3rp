# 0042. CopyObject accepts `x-amz-object-annotation-directive: EXCLUDE`

- Status: Accepted
- Date: 2026-11-22
- Pull request: [#251](https://github.com/fujiwara/s3rp/pull/251)

## Context

An `x-amz-*` request header no operation reads is refused with
`501 NotImplemented` ([0002](0002-refuse-what-cannot-be-honored.md)).

Amazon S3 added object annotations, and with them a CopyObject header,
`x-amz-object-annotation-directive`, taking `COPY` (the default: the
source's annotations are copied) or `EXCLUDE`. Since 2.35.6, the release
that brought annotations, aws-cli v2 sends `EXCLUDE` on every
single-request copy of `aws s3 cp`, `mv` and `sync` between S3 locations
unless `--copy-props all` or `none` is given (measured by capturing the
requests of 2.40.3; `s3api copy-object` and aws-cli v1 send no such
header). The
gateway refused it, so the most used S3 client could not copy an object.
The client compatibility run found it when aws-cli was added
([0039](0039-run-real-clients-against-the-gateway.md)).

The gateway has no object annotations: none of their operations is in the
catalog, so no object can carry one.

## Decision

CopyObject reads the header. `EXCLUDE` is accepted and not forwarded to
the backend; any other value is `501 NotImplemented`.

`EXCLUDE` is honored exactly, not ignored: the copy carries no
annotations, which is what the client asked for. `COPY` asks for
something the gateway cannot do, and S3 answers it with 501 where
annotations do not exist (directory buckets, by its API reference).

## Alternatives considered

- **Keep the 501 for both values.** `aws s3 cp --copy-props none` avoids
  the header, but also stops the CLI carrying tags and metadata over a
  multipart copy, and a user has no reason to look for it.
- **List the header in `ignoredAmzHeaders`**
  ([0032](0032-accept-headers-ignorable-by-protocol.md)). That would
  accept `COPY` as well; nothing acknowledges it in the response, so a
  client would believe annotations had been copied.
- **Accept `COPY` too, since a source has no annotations to lose.** True
  today, but it would turn into a silent drop the day a backend stores
  annotations the gateway does not relay.
- **Forward the header.** A backend that does not know it may refuse or
  ignore it, and the answer would then depend on the backend.

## Consequences

- `aws s3 cp`, `mv` and `sync` between buckets work with the CLI's
  defaults; `--copy-props all` is refused by name.
- Supporting annotations later means their operations in the catalog and
  `COPY` honored here; this record does not stand in the way.
