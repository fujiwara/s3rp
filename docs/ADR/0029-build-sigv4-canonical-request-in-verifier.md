# 0029. Build the SigV4 canonical request in the verifier

- Status: Accepted (supersedes [0007](0007-verify-by-re-signing-with-one-signer-per-key.md))
- Date: 2026-10-14
- Pull request: [#186](https://github.com/fujiwara/s3rp/pull/186)

## Context

The verifier (`sigv4/`) checked a signature by re-signing a clone of the
request with aws-sdk-go-v2's `v4.Signer` and comparing the results
([0007](0007-verify-by-re-signing-with-one-signer-per-key.md)). Reusing the
SDK's signer meant no canonicalization code of our own; the 2026-08-28
review had looked at replacing it and kept it on purpose, for definitional
SDK compatibility.

A service built on the gateway reported that aws-cli 2.38.0 failed
`CreateMultipartUpload` with `SignatureDoesNotMatch`, while the classic
(Python) transfer client worked. aws-cli 2.38 made the AWS Common Runtime
(CRT) its default S3 transfer client. Reproducing it showed that every
request without a body failed, not only `CreateMultipartUpload`:
`HeadObject` on download and `DeleteObject` failed too.

- The CRT signs `content-length:0` on every request that has no body. This
  is valid SigV4, and Amazon S3 accepts it.
- aws-sdk-go-v2's signer adds `content-length` to the canonical headers only
  when `Request.ContentLength > 0`, and drops a `Content-Length` header it is
  handed. The verifier therefore had no way to reproduce a signature over
  `content-length:0`. This is unchanged in the latest release (v1.47.3) and
  on `main`; the signer code was last changed in 2024-09.
- The SDK signer is a client: it decides what to sign. Verifying a request
  signed by any other client is a different job, and it was never designed
  for that.
- versitygw, Ceph RGW and RustFS accept these requests, both directly and
  with versitygw run as an S3 proxy. versitygw keeps its own fork of the
  canonicalization in `internal/sigv4auth`, which signs `content-length`
  whenever the client listed it.

## Decision

The verifier builds the canonical request itself (`sigv4/canonical.go`),
from the request as it arrived:

- **Headers:** exactly the client's `SignedHeaders`, each taken from its own
  value. Multiple values are joined with commas, and each value is trimmed
  with runs of spaces collapsed. `content-length` is signed whatever its
  value, 0 included.
- **Whitespace in header values:** each run of spaces *and tabs* becomes one
  space. This is where aws-sdk-go-v2 (which keeps tabs) and Amazon S3
  disagree, and the verifier follows S3. Measured with
  `probe_header_whitespace.py`: S3 accepts only the collapsed form, which is
  what botocore and the CRT sign, and refuses the SDK's; versitygw and Ceph
  RGW behave like the SDK. aws-cli uploads with a tab in `--metadata` failed
  until this was changed (review of the same pull request).
- **Signed-header list:** must be lower-case, strictly sorted, include
  `host` and exclude `authorization`; anything else is `SignatureDoesNotMatch`.
  There is no ignored-header list of the kind the SDK signer keeps, so a
  client may sign `user-agent`, as AWS allows.
- **URI path and query:** follow aws-sdk-go-v2 exactly. The path is signed
  as sent; the query is decoded and re-encoded. This keeps the query
  behavior settled in [SigV4 query canonicalization](../sigv4-canonicalization.md)
  and makes every SDK-signed request canonicalize to the same bytes as
  before, except header values with tabs (above).
- **Presigned URLs:** the canonical query is the request's own query without
  `X-Amz-Signature`. An auth parameter that appears more than once is
  refused, because the checks read the first value while the signature
  covers all of them.
- **Signing-key cache:** the per-access-key cache now holds derived signing
  keys instead of SDK signers. Its key is (access key, secret, date,
  region), so a secret rotated under the same access key id stops verifying
  at once. The public names (`SetSignerCacheSize`, `SignerCacheStats`, the
  `signer` cache metric) are kept for compatibility.

## Alternatives considered

- **Keep tabs as aws-sdk-go-v2 does.** Rejected after measuring Amazon S3,
  which refuses that form; the mainstream client (aws-cli) signs the other
  one.
- **Wait for a fix in aws-sdk-go-v2.** Rejected. The SDK signer behaves
  correctly as a client, so there is nothing for upstream to fix and nothing
  to wait for.
- **Keep the SDK signer and add a second, own implementation only for the
  `content-length:0` case.** Rejected. It would mean two canonicalizations
  that can drift apart, with the less-tested one running only on the path
  that is rarely exercised.
- **Ask users to set `preferred_transfer_client = classic`.** Rejected. It
  is aws-cli's default, and other CRT-based clients would hit the same
  failure.

## Consequences

- aws-cli ≥ 2.38 with the CRT works through the gateway: uploads (multipart,
  single, empty), downloads, `sync` and deletes. This was verified directly
  and behind the documented nginx and HAProxy configurations.
- The gateway now owns its canonicalization, so its correctness rests on
  tests rather than on sharing the SDK's code:
  - **Known answers:** vectors from AWS's SigV4 test suite.
  - **Differential fuzzing:** the fuzz targets sign with aws-sdk-go-v2, so
    every SDK-signed request must still verify.
  - **Cross-implementation vectors:** requests signed by four independent
    signers must verify: botocore, curl, aws4 and the AWS CRT
    (`generate_crt.py`).
  - **Nightly regeneration:** a nightly job regenerates CRT vectors with the
    latest aws-cli, so a change in how the CRT signs shows up there first.
- Verification is faster: `BenchmarkVerifyKeyDiversity` dropped about 25% in
  ns/op and from 111 to 91 allocs/op.
- More requests are accepted. Clients may sign headers the SDK signer
  ignores, and presigned URLs may carry `x-amz-*` headers that were signed
  rather than moved into the query. In both cases the signature covers what
  it claims to, so this is spec behavior, not a loosening.
- Proxies in front of the gateway must pass `Content-Length: 0` through on
  requests without a body, since it is now part of what CRT clients sign.
  [Building a service](../building-a-service.md#behind-a-tls-terminator)
  says so.
