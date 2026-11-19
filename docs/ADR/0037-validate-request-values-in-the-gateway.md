# 0037. Request values S3 refuses are refused by the gateway

- Status: Accepted
- Date: 2026-11-19
- Pull request: [#234](https://github.com/fujiwara/s3rp/pull/234)

## Context

The gateway forwarded object tags, copy directives, the attribute list of
GetObjectAttributes and the part list of CompleteMultipartUpload to the
backend unexamined, and let the backend judge them. Two things follow from
that.

Backends judge differently. Running tests that assert Amazon S3's own
responses against the gateway on Ceph RGW showed RGW accepting what S3
refuses: a duplicate tag key, a tag value with an emoji, an unknown
tagging directive, a part list in descending order, an attribute name S3
does not define. The API a client sees then depends on the backend, which
the gateway exists to prevent.

And where a backend does refuse, the client learns nothing. The backend's
message is never relayed ([ADR 0036](0036-error-messages-follow-s3-wire-wording.md)),
so the answer is `InvalidArgument` "Invalid Argument" — for the commonest
client mistakes, the ones a message would let the client fix alone.

## Decision

Values whose validity S3 defines independently of the bucket's state are
checked in the gateway, before the hooks and the backend, and refused with
S3's code and message: object tags (count, key and value length, character
set, duplicate keys) on every path that carries them, the copy directives,
GetObjectAttributes' attribute names, and the order of
CompleteMultipartUpload's parts.

Storage class names are not checked. Which classes exist is the backend's
and the service's to define — a backend has classes of its own, and a
service may offer any name through its `StorageClassMapper` — so the
gateway refuses none, including the names S3 reserves for other bucket
types.

## Alternatives considered

- **Relay the backend's message for these errors.** Restores the detail
  but not the uniformity, and reopens the question ADR 0036 and the
  message rule closed: a message written by another system is not the
  gateway's to pass on.
- **Map known backend messages to S3's wording.** Needs a table per
  backend and still cannot make a backend refuse what it accepts.
- **Refuse `EXPRESS_ONEZONE` and `OUTPOSTS` as S3 does.** Implemented and
  removed: a service that wants to offer a class under one of those names
  could not.

## Consequences

- The gateway is stricter than RGW: a tag set, directive or part list RGW
  accepted and S3 refuses now gets a 400. A client relying on the laxer
  backend behavior sees the difference.
- These refusals are the same on every backend and say what is wrong.
- Checks that need the bucket's or object's state (Object Lock not
  enabled, a copy onto itself with nothing changed) stay with the backend
  and keep their general message.
- Three of the messages are not asserted by the tests the others were
  taken from: the tag-key `InvalidTag`, the metadata-directive
  `InvalidArgument`, and the malformed-`x-amz-tagging` `InvalidArgument`.
