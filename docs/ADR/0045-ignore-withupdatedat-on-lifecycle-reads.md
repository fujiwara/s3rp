# 0045. GetBucketLifecycleConfiguration accepts and ignores `withUpdatedAt`

- Status: Accepted
- Date: 2026-11-22
- Pull request: [#261](https://github.com/fujiwara/s3rp/pull/261)

## Context

A query parameter no operation reads is refused with `501 NotImplemented`
([0002](0002-refuse-what-cannot-be-honored.md)).

minio-go reads a bucket's lifecycle configuration with
`GET /bucket?lifecycle&withUpdatedAt=true`, always (v7.4.1,
`getBucketLifecycle`). The parameter is MinIO's own: it asks for the time
the configuration last changed, which MinIO returns in an
`X-Minio-LifecycleConfig-UpdatedAt` response header. minio-go parses that
header when it is present and reports a zero time when it is not; the
configuration itself is read from the body either way.

So once GetBucketLifecycleConfiguration was served
([0044](0044-serve-bucket-configuration-reads-from-the-store.md)), no
program built on minio-go could use it: `mc ilm rule ls` failed with the
501. The client compatibility run found it
([0039](0039-run-real-clients-against-the-gateway.md)).

## Decision

GetBucketLifecycleConfiguration accepts `withUpdatedAt` and does not read
it. The response is what it would be without the parameter, and carries no
`X-Minio-*` header.

This follows the criterion of
[0032](0032-accept-headers-ignorable-by-protocol.md), applied to a query
parameter as [0040](0040-ignore-max-keys-on-listbuckets.md) did: a value
may be ignored only when the client cannot come to believe it applied. The
client acts on the parameter only after a response header acknowledges it,
which the gateway never sends.

## Alternatives considered

- **Keep the 501.** minio-go has no option that drops the parameter, so
  the read would be closed to every program built on it.
- **Honor it.** The store would have to carry a modification time for the
  document, for a header only MinIO's clients read.
- **Ignore every unknown parameter on bucket-configuration reads.** The
  rule of 0002 stands; this is one named parameter with a known sender.

## Consequences

- `mc ilm rule ls` and minio-go's `GetBucketLifecycle` work. The update
  time minio-go reports is the zero time, as against any server that is
  not MinIO.
- The gateway must not send `X-Minio-LifecycleConfig-UpdatedAt`; responses
  are built by the gateway, so there is nothing to pass through.
