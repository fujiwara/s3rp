# Documentation

The [top-level README](../README.md) covers the bundled `s3rp` binary: what the PoC validates, install, the YAML configuration, client usage. Everything else is here, split by who is reading.

## For clients of the gateway

- **[The S3 API](s3-api.md)** — the tenant-facing contract: supported operations, bucket and user policies (principals, resources, IP conditions, dialects), CORS, ACLs, checksums, server-side encryption, storage class, Object Lock, addressing, presigned URLs, browser-based POST uploads, and the API-level limitations. Shared by the bundled binary and any service built on the gateway.
- **[Request headers](request-headers.md)** — the header-level view of the same surface: every `x-amz-*` header the gateway honors and on which operations, the ones refused by name (SSE-C, ACLs, grants), the ones not supported and the SDK option each comes from, standard headers, and the backend response headers that are not relayed.

## For building a service on the gateway

- **[Building a service on the gateway](building-a-service.md)** — embedding `s3gw` in a production service: the minimal code, implementing the store (caching, policy parsing and dialects, write-time validation, temporary credentials), observation, the hooks (Authorizer, interceptors, concurrency and bandwidth limits, the storage class mapper), retries and write side effects, what to run in front of the gateway and behind a TLS terminator, backend client options and cache sizing, and the reusable leaf packages.

## For working on this repository

- **[ceph/s3-tests compatibility testing](s3-tests.md)** — how the upstream S3 compatibility suite runs against s3rp in CI and locally, and why each expected-failure category is expected.
- **[SigV4 query canonicalization across implementations](sigv4-canonicalization.md)** — measured behavior of AWS, Ceph RGW, versitygw and s3rp on non-canonical query strings, and the divergences between client signers; the basis for the verifier's settled behavior.
- **[AGENTS.md](../AGENTS.md)** — architecture notes and the design decisions not to regress, written for contributors (human or otherwise).
