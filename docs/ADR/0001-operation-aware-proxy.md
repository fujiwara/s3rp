# 0001. Re-execute S3 operations instead of forwarding requests

- Status: Accepted (recorded retroactively)
- Date: 2026-07-31
- Pull request: initial implementation (506d1a2)

## Context

s3rp is a proof of concept toward a multi-tenant S3-compatible object
storage service in front of heterogeneous backends (Ceph RGW, versitygw,
Amazon S3, ...). A proxy in front of S3 backends can work at two levels:

- **Transparent:** verify the client's signature, re-sign the same HTTP
  request with backend credentials and forward it byte for byte.
- **Operation-aware:** parse each request into an S3 operation, decide
  about it, and execute it again through an SDK client against the backend.

## Decision

The proxy is operation-aware. Every request is recognized as an S3
operation and executed again through aws-sdk-go-v2 against the bucket's
backend; the response is built by the proxy from the SDK result.

## Alternatives considered

- **Transparent re-signing proxy.** Less code, and new S3 features pass
  through for free. Rejected: a multi-tenant service has to understand each
  operation anyway — for authorization, metering and quotas — and a
  pass-through forwards whatever it does not understand, including features
  the service never decided to offer.

## Consequences

- Every operation is implemented explicitly, and an operation that is not
  implemented is refused loudly ([0002](0002-refuse-what-cannot-be-honored.md)).
- The API surface is uniform whatever the backend, and the proxy controls
  every name in a response: backend bucket names, regions and owners never
  reach the client ([0014](0014-responses-expose-only-the-front-namespace.md)).
- Authorization, metering and admission hooks have a single, well-defined
  place to run, per operation.
- Backend quirks become the proxy's to handle (backend client construction
  in AGENTS.md lists the ones met so far).
