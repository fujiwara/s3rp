# 0024. Storage classes and KMS key ids are mapped, both ways, by one interface

- Status: Accepted (recorded retroactively)
- Date: 2026-09-11
- Pull requests: [#135](https://github.com/fujiwara/s3rp/pull/135),
  [#139](https://github.com/fujiwara/s3rp/pull/139)

## Context

Storage class names are the backend's layout: physical pools or tiers such
as hdd and ssd. Letting a client choose `x-amz-storage-class` hands it
control of placement, and echoing the backend's class in responses leaks
the layout. KMS key ids are names in the backend's KMS, and have the same
problem. This is the namespace rule of
[0014](0014-responses-expose-only-the-front-namespace.md) applied to two more
values.

## Decision

- `StorageClassMapper` (`SetStorageClassMapper`): `ToBackend(op, size)`
  decides what every write tells the backend (size-based placement is
  possible; `SizeUnknown` where the size is not known yet), replacing the
  client's header. `ToClient(op, backendClass)` decides what every response
  shows. `Op.Response.StorageClass` keeps the backend's own value for
  billing.
- `KMSKeyMapper` (`SetKMSKeyMapper`) does the same for `aws:kms` key ids, on
  writes and at every place a key id is reported. A mapper keys its
  namespace on `Op.BucketOwner`, never the requester, and fails closed on an
  id it cannot map.
- Each is **one interface with both directions**, not two setters. A
  write-only mapping would show clients backend names they never asked for,
  so passing one direction through has to be written out in code.
- All sending and reporting sites go through the helpers.

## Alternatives considered

- **Two setters, one per direction.** Rejected for the reason above.
- **Refuse client classes in an Authorizer only.** Not enough: reads would
  still show backend names.

## Consequences

- A `nil` mapper passes values through, as before.
- Keying on the requester instead of the bucket owner was caught in review
  as a cross-tenant leak; the bucket owner is now on `Op` for this reason.
