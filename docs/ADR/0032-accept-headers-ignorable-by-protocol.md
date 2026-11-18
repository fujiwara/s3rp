# 0032. Accept and ignore a header only when its protocol makes ignoring safe

- Status: Accepted
- Date: 2026-11-18
- Pull request: [#221](https://github.com/fujiwara/s3rp/pull/221)

## Context

An `x-amz-*` request header no operation reads is refused with
`501 NotImplemented` ([0002](0002-refuse-what-cannot-be-honored.md)):
ignoring a signed header applies less than the client believes.

The AWS SDK for Java v2 adds `x-amz-te: append-md5` to every GetObject
unless the caller opts out. It asks the server to append an MD5 of the body,
and the SDK strips and validates that MD5 only when the response carries
`x-amz-transfer-encoding: append-md5`; without the acknowledgement it reads
the body as it is. A client built on the Java SDK v2 therefore could not
read a single object through the gateway unless it disabled the SDK's
checksum validation.

`x-amz-user-agent` was already accepted without being read, listed among
the known headers and exempted in the test that keeps that list in step
with the code.

## Decision

- Headers accepted and deliberately not acted on live in their own list,
  `ignoredAmzHeaders`, apart from `knownAmzHeaders`.
- A header qualifies only when the client cannot come to believe it
  applied: it carries no instruction (`x-amz-user-agent`), or the client
  acts on it only after the response acknowledges it (`x-amz-te`).
- `TestKnownAmzHeadersCoverSource` fails when an operation file reads a
  header from the ignored list, or when a header is in both lists.

## Alternatives considered

- **Implement append-md5.** It is a legacy, undocumented transfer encoding;
  the `x-amz-checksum-*` headers are the supported integrity mechanism and
  the same SDK uses them when asked.
- **Keep the 501 and have clients disable the validation.** The SDK option
  exists, but an application embedding the SDK does not necessarily expose
  it.
- **Add the header to `knownAmzHeaders` with a test exemption**, as
  `x-amz-user-agent` was. The exemption list would then mix "read
  elsewhere" with "read nowhere", and the criterion for the second kind
  would be written down nowhere.

## Consequences

- GetObject from the AWS SDK for Java v2 works with its defaults. The
  download is not MD5-validated by the SDK, as against any server that does
  not acknowledge the header.
- The gateway must never send `x-amz-transfer-encoding`; responses are
  reconstructed, so a backend's own does not pass through.
- The rule of 0002 stands. A header whose effect the client assumes without
  an acknowledgement — an encryption context, a redirect location — is
  still refused.
