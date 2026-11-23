# 0056. An object key is UTF-8 of at most 1,024 bytes

- Status: Accepted
- Date: 2026-11-23
- Pull request: [#283](https://github.com/fujiwara/s3rp/pull/283)

## Context

S3 defines an object key as a sequence of Unicode characters whose UTF-8
encoding is at most 1,024 bytes. A request path can still carry any bytes
(`/bucket/k%FF`), and the gateway unescaped it and passed the result on.
What happened next was the backend's: `localbackend` stored it, could not
write it to `meta.json` as it was, and nothing that lists keys — an XML
response — can show such a key.

What S3 does was asked of Amazon S3 itself, with unauthenticated requests
to a public bucket: `/bucket/k%FF`, a key cut inside a character (`k%C3`)
and an encoded surrogate (`k%ED%A0%80`) are each
`400 InvalidURI` "Couldn't parse the specified URI.", before authorization;
`k%C3%A9` and `k%25FF` go on to `AccessDenied`. Asked the same way about
length: a key of 1,024 bytes goes on to `AccessDenied` and one of 1,025 is
`400 KeyTooLongError` "Your key is too long" — bytes of UTF-8, not
characters, 512 two-byte characters passing and 513 not.

The AWS Emulator (1.0.580) answers otherwise — it keeps such a path
unescaped, so `k%FF` and `k%25FF` are one key there — and was not followed.
It does refuse the same bytes in `x-amz-copy-source` with `InvalidArgument`.

## Decision

A key that is not UTF-8, or longer than 1,024 bytes, is refused wherever a
request names one, under
[0037](0037-validate-request-values-in-the-gateway.md):

- the request path, bucket included: `400 InvalidURI` with S3's message, the
  answer a path that cannot be unescaped already had;
- `x-amz-copy-source`: `400 InvalidArgument`, as for any malformed source;
- the `key` field of a POST upload, after `${filename}` is substituted:
  `400 InvalidArgument`. What S3 answers here was not measured;
- a key over the limit: `400 KeyTooLongError` with S3's message in the path,
  and the same in the copy source and the POST `key` field, where S3's
  answer was not measured. S3's `Size` and `MaxSizeAllowed` elements are
  not sent, the error body having no place for them;
- a DeleteObjects body was already `400 MalformedXML`, the XML decoder
  refusing it; a test now holds that.

## Alternatives considered

- **Leave it to the backend.** Backends differ, and the bundled one accepted
  what it could not keep.
- **Check the length of DeleteObjects keys.** A key that long names no
  object, and deleting what does not exist succeeds; the backend is left to
  answer.
- **Check listing parameters too** (`prefix`, `start-after`, a marker). They
  name no object, and what S3 does with them could not be asked without
  credentials. Left as they are.

## Consequences

- The path is checked after the signature, where S3 checks it before; a
  request with such a path and a bad signature is told about the signature.
- A client that stored such keys through the gateway on a backend that
  takes them can no longer reach them through it.
