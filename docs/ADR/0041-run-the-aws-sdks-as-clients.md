# 0041. The AWS SDKs are run as clients, one per language

- Status: Accepted
- Date: 2026-11-20
- Pull request: [#243](https://github.com/fujiwara/s3rp/pull/243)

## Context

The client compatibility run ([0039](0039-run-real-clients-against-the-gateway.md))
left the AWS SDKs out: the gateway's own tests drive it with
aws-sdk-go-v2, and s3-tests and the fidelity suite with boto3, so "the
SDK" looked covered.

It is not one implementation. Each language's SDK has its own signer,
HTTP stack and transfer manager, generated from a shared model but
written apart. `x-amz-te: append-md5` is sent by the SDK for Java alone
([0032](0032-accept-headers-ignorable-by-protocol.md)), and was found by
a user, not by a test.

## Decision

The run includes the AWS SDKs for Python, JavaScript, Go, Java, .NET,
Ruby, PHP and Rust as clients `sdk-*`, each a small script under
`clients/lib/sdk/` implementing the same verbs as every other client.

- **Default configuration.** A script sets the endpoint, path style and
  credentials and nothing else, and uploads through the SDK's transfer
  manager where it has one: the point is what a user following the
  SDK's documentation sends, not what the SDK can be made to send.
- **Go and Python are included** although other suites use them: those
  suites call single operations, not the transfer managers, and over
  http only.
- **Versions are pinned** in the form each language uses — a lock file,
  or the dependency line of a single-file script — and the runtimes in
  `clients/mise.toml`. PHP runs in its official container image, having
  no prebuilt runtime in mise.
- **The run is https on both sides**, client to gateway and gateway to
  backend, and no longer repeats every client over http. An SDK sends a
  checksum as a trailer by default, and the gateway forwards a trailing
  checksum to an https backend only (docs/s3-api.md, Checksums): on the
  http backend the run used, the multipart uploads of the SDKs for
  Python, Go and Java were refused by the backend. That is a limitation
  of http backends, which are for development; the run is to show what a
  deployment serves. A client that cannot be given the run's CA is run
  over http.
- **The backend is Ceph RGW**, no longer versitygw
  ([0039](0039-run-real-clients-against-the-gateway.md)). What a client
  ends up with depends on the backend wherever the backend departs from
  S3, and versitygw's posix store does where RGW does not: `dir//x` and
  `dir/x` are one object there, so four clients that drop the empty
  segment passed a scenario they fail on S3. A failure that is RGW's own
  is listed as `backend:`.
- **The certificates are issued by a CA made for the run**, not
  self-signed: rustls refuses a certificate that is both the trust
  anchor and the server's.

## Alternatives considered

- **Keep the http backend and fix or list the multipart failures.**
  Forwarding a trailer to an http backend means the gateway framing and
  signing `aws-chunked` itself; dropping the algorithm loses the
  checksums of clients that send them in headers. Either is work for a
  configuration no deployment should use.
- **Run on both backends.** Twice the run again, and a result that
  differs between them is the backend's, which the integration matrix
  is for.
- **Keep both client schemes.** Twice the run time, for the framing a
  client uses only without TLS, which the unit tests and the signature
  cross-check cover.

- **Only the SDKs with a known difference (Java).** The differences are
  known only once the SDK has been run.
- **Capture each SDK's requests once and replay them** as the signature
  cross-check does. It checks signatures; a request sequence, a response
  the SDK must parse and a retry are not replayable.
- **Follow the latest version of every SDK.** A run must be
  reproducible; an SDK released daily would change the result between
  two runs of the same commit.

## Consequences

- Found on the first run: the SDK for .NET escapes the slash between
  bucket and key in `x-amz-copy-source` and sends `x-amz-api-version`,
  both refused until then; boto3 presigns with SigV2 against a custom
  endpoint unless told otherwise.
- The run needs eight language toolchains; dropping the http pass keeps
  its duration about where it was.
- Multipart uploads with trailing checksums through an http backend stay
  broken, and documented as such.
- Found by the change of backend: the SDK for Java's parallel download
  needs `Content-Range` on a `partNumber` read, which RGW does not send.
- The run starts a Ceph cluster, which takes longer to come up than
  versitygw did.
- The lock files are read by Dependabot's security alerts.
