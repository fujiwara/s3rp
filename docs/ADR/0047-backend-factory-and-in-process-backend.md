# 0047. Backend clients can come from a factory; an in-memory backend is bundled

- Status: Accepted
- Date: 2026-11-22
- Pull request: [#268](https://github.com/fujiwara/s3rp/pull/268)

## Context

The gateway reaches a backend through `BackendClient`, the methods of the
aws-sdk's S3 client it calls, and builds that client itself from the
backend definition. The interface existed for test stubs: `SetBackend`
puts one into the client cache for a bucket that is already defined.

A service built on the gateway wants to hand its users a local stand-in
for itself, as cloud providers ship emulators of theirs: one process, no
backend to install, answering as the service does. Most of what such a
stand-in must get right is the gateway's and the service's own code —
signature verification, policies, hooks, error wording, refusals — and
that part comes for free if the stand-in is the service assembled around
something other than an S3 backend. Two things were missing.

- A way to supply the backend. `SetBackend` works per bucket, on buckets
  defined when it is called, and its entry can be evicted from the cache,
  after which the gateway silently builds an SDK client for an endpoint
  that does not exist.
- The object store itself. A backend decides what an S3 API mostly is:
  ETags, versions, listings and their paging, multipart assembly,
  conditional requests, Object Lock.

## Decision

- **`Gateway.SetBackendFactory`** supplies the client for a backend
  definition. A nil client leaves the backend to the SDK client, so both
  kinds can be served by one gateway. The result is cached by backend
  identity like the SDK client, and `SetClientOptions` and `SetBreaker`
  do not apply to it: they shape an SDK client.
- **`BackendClient` is a contract now**, and says what an implementation
  that is not the SDK client must keep: read a write's body to its end
  and return the read's error, report failures in the SDK's error shape,
  compute the checksum named in `ChecksumAlgorithm`. Whether a client can
  do the last over a stream was inferred from the endpoint's scheme (the
  SDK client can over https only); a client now answers for itself
  through the optional `StreamChecksummer`.
- **`s3gw/membackend`** is an in-memory implementation, in the gateway's
  module because the contract it implements is the gateway's and every
  service would write the same one.
- It also has the bucket operations a control plane performs on a backend
  (`CreateBucket`, `DeleteBucket`, `PutBucketVersioning`,
  `PutObjectLockConfiguration`, `PutBucketEncryption`), with the SDK
  client's signatures. The gateway still proxies none of them
  ([0011](0011-bucket-configuration-not-proxied.md)); they are for the
  service's control plane, which then runs unchanged against either.
- **Where S3 and a backend disagree, it follows S3.** The fidelity tests
  ([0038](0038-run-aws-fidelity-tests.md)) and s3-tests run against it
  through the harness (`S3RP_TEST_BACKEND_ENDPOINT=memory`), and the root
  integration suite runs against it in every `go test`.

## Alternatives considered

- **Bundle an existing S3 server (versitygw) with the service.** No code,
  but a second process, and its behavior is another backend's again: a
  stand-in for a service on Ceph RGW would differ from it in the ways
  versitygw is known to (storage classes, governance bypass, keys a
  filesystem cannot hold apart).
- **A narrower storage interface under the operations** (put bytes, get
  bytes, list keys), with the S3 semantics kept in the gateway. The
  gateway deliberately leaves those semantics to the backend; moving them
  in would make the gateway an object store.
- **A mock mode of the `s3rp` binary with CreateBucket.** The stand-in is
  of a service, with that service's control plane and hooks, not of the
  PoC binary; and it would put bucket writes on the data plane.
- **Mimic the production backend instead of S3.** Which backend, and at
  which release; its deviations from S3 are what the gateway works to
  hide, not a specification.
- **Leave the implementation to each service.** The factory alone would
  do, but the store is some two thousand lines every service would repeat
  against a contract this repository owns and can test.

## Consequences

- An emulator of a service is that service's packages, a local store and
  `membackend`. The service still writes the emulated control plane and
  decides how credentials are issued.
- `membackend` must implement every method added to `BackendClient`; the
  compiler says so. Behavior the gateway comes to rely on from a backend
  has to be given to it as well, or the in-process integration run fails.
- It is a stand-in, not storage: everything is in memory, nothing
  persists, lifecycle and replication never run, nothing is encrypted and
  a storage class is a label.
- The differences from a given backend are not zero and are not listed
  here: they are the difference between two runs of the same suites.
- `SetBackend` stays, for stubs in tests.
