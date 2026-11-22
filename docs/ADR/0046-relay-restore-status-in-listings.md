# 0046. Listings relay the restore status the backend reports

- Status: Accepted
- Date: 2026-11-22
- Pull request: [#267](https://github.com/fujiwara/s3rp/pull/267)

## Context

`x-amz-optional-object-attributes: RestoreStatus` asks ListObjects,
ListObjectsV2 and ListObjectVersions to add a `RestoreStatus` element to
each entry of an archived object that is being restored or has been:
whether the restore is in progress, and when the restored copy expires.
Without it a client learns the same from `x-amz-restore` on a HeadObject
per object.

The header was refused with `501 NotImplemented`, as a header no operation
reads ([0002](0002-refuse-what-cannot-be-honored.md)), on the ground that
RestoreObject is not implemented.

Mountpoint for Amazon S3 sends it on every ListObjectsV2 and has no option
that drops it: it shows an archived object as unreadable unless the
listing says it is restored. Its first request is such a listing, so the
mount itself failed. The client compatibility run found it
([0039](0039-run-real-clients-against-the-gateway.md)).

## Decision

The three listings forward `RestoreStatus` to the backend as
`OptionalObjectAttributes` and put the `RestoreStatus` the backend reports
on each entry. A listing that did not ask sends nothing and shows nothing.

`RestoreStatus` is the only value S3 defines. Another value is `501`: it
names an element the gateway would not relay.

RestoreObject stays unsupported, and `x-amz-restore` on GetObject and
HeadObject stays unrelayed.

## Alternatives considered

- **Keep the 501.** Mountpoint could not be used at all, for a header
  whose honest answer on a backend without an archive tier is to add
  nothing.
- **Accept and ignore it.** The criterion of
  [0032](0032-accept-headers-ignorable-by-protocol.md) is that the client
  cannot come to believe the header applied. Here it can: an entry without
  `RestoreStatus` is S3's way of saying the object is not restored, so a
  restored object would be reported as archived.
- **Relay `x-amz-restore` too.** It is the same fact on another operation
  and a client that looks an object up by HEAD reads it there. It is left
  for when a service runs an archive tier behind the gateway; nothing
  refuses a request for lack of it.

## Consequences

- Mountpoint for Amazon S3 mounts a bucket and is part of the client
  compatibility run.
- What a listing shows is the backend's: a backend that does not know the
  attribute adds nothing, which reads as "not restored".
- A restore is started outside the gateway, since RestoreObject is not
  served. The status is relayed whoever started it.
- A second optional attribute, when S3 defines one, is a decision of its
  own: it is refused until the listings relay it.
