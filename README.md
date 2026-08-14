# s3rp

s3rp is a multi-tenant S3 API gateway: an S3-compatible endpoint that authenticates tenants with their own access keys and forwards each operation to a per-bucket backend under the backend's own credentials.

> [!WARNING]
> This is a **proof of concept**. Its goal is to validate an architecture, not to be a product. **Do not use it for any other purpose.** It is not production-ready: definitions live in a static YAML file, the config may change without notice, and no security review has been done.

```
S3 client --(SigV4, tenant keys)--> s3rp --(SigV4, backend keys)--> S3-compatible backend
                                      │
                            reads definitions from
                                      ▼
                     store (YAML, or your own), read-only
```

## What this PoC validates

s3rp is not an object storage implementation — it stores no data itself. It explores the **data plane of a managed, multi-tenant S3 service** that sits in front of existing S3-compatible storage (Ceph RGW, versitygw, Amazon S3, ...). The questions it answers, and the design decisions behind them:

**Why a reverse proxy?** A managed service needs one identity/authorization plane over heterogeneous backends. Tenants get their own keys and never see the backend's credentials, endpoints, or even the real bucket names; the operator can place a tenant's bucket on any backend and move it without the tenant noticing. The proxy is where per-tenant authentication, authorization (bucket and user policies), metering, and a uniform API surface naturally live — the last two through [hooks a service installs](#building-a-service-on-the-gateway).

**Why reconstruct operations with aws-sdk-go-v2 instead of forwarding the HTTP request?** A transparent SigV4-resigning proxy would be less code, but a multi-tenant service must *understand* each request, not just relay bytes:

- **Authorization by operation** — every request maps to an `s3:*` action evaluated against the bucket policy; an allow-list of implemented operations means unsupported/dangerous ones fail closed rather than leaking through.
- **Namespace virtualization** — the front bucket name is rewritten to the backend bucket, and (crucially) rewritten *back* in responses (`ListBucketResult`, multipart results, error `Resource`), which a byte-forwarding proxy cannot do without parsing and rebuilding responses anyway.
- **Uniform behavior** — the SDK absorbs backend quirks (endpoint resolution, retries, checksum negotiation), so the tenant-facing contract is owned by s3rp, not by whichever backend happens to serve a bucket.

The cost is that each operation is implemented explicitly; that trade-off, and its edge cases (SigV4 verification, aws-chunked decoding, checksum pass-through), are much of what this PoC exercises.

**What is data plane vs. control plane?** This repo is the **data plane**: it only ever *reads* definitions (tenants, users, keys, buckets, policies), through a small read-only [`store.Store`](store/store.go) interface. All *writes* are deliberately out of scope — in a managed offering they belong to a separate control plane API (create tenants, issue/rotate keys, place buckets) with its own credentials and audit trail. The interface having no write methods is what keeps that boundary real: a proxy deployment can be given credentials that cannot write, and building the control plane itself (authn, quotas, billing, self-service) is conventional CRUD work with no architectural uncertainty, so it is intentionally left unimplemented. The bundled YAML store is the PoC's stand-in; a real service brings [its own `store.Store`](#building-a-service-on-the-gateway) over its own definition storage.

## Install

### Binary

Download the `s3rp` binary from [Releases](https://github.com/fujiwara/s3rp/releases).

### go install

```console
$ go install github.com/fujiwara/s3rp/cmd/s3rp@latest
```

## Usage

```
Usage: s3rp [flags]

S3 API reverse proxy with SigV4 re-signing

Flags:
  -h, --help                  Show context-sensitive help.
      --config="s3rp.yaml"    config file path ($S3RP_CONFIG)
      --listen=STRING         listen address (overrides config) ($S3RP_LISTEN)
      --log-level="info"      log level ($S3RP_LOG_LEVEL)
      --version               show version
```

## Configuration

The config file is YAML. Environment variables in the file are expanded (`${VAR}` or `$VAR`).

A tenant owns one or more buckets and users. Bucket names (`[a-z0-9-]`, 3–63 chars, globally unique) deliberately exclude `.`: a dotted name is not a single DNS label, so it would break virtual-hosted-style addressing under a wildcard TLS certificate. A user is the stable identity within a tenant (name: `[a-z0-9][a-z0-9_-]+`, shared with tenant names, so an AWS-account-ID-style all-numeric tenant name is valid — quote it in YAML, or it parses as a number); access keys are issued per user and rotate under it — add a new key, switch clients, then remove the old one. Every key of a tenant can access all of the tenant's buckets, unless restricted by a [bucket policy](docs/s3-api.md#bucket-policies) or a [user policy](docs/s3-api.md#user-policies); a bucket policy can also grant selected operations to another tenant's user ([cross-tenant access](docs/s3-api.md#cross-tenant-access)). Two tenants may not map their buckets to the same physical backend bucket (endpoint + backend bucket name); this is rejected at startup to preserve tenant isolation.

```yaml
listen: ":8080"
tenants:
  - name: acme                       # tenant identifier
    users:
      - name: app1                   # stable user identity
        keys:                        # access keys of the user (multiple for rotation)
          - access_key_id: S3RPKEY001
            secret_access_key: ${ACME_APP1_SECRET_001}
          - access_key_id: S3RPKEY002
            secret_access_key: ${ACME_APP1_SECRET_002}
      - name: batch
        keys:
          - access_key_id: S3RPKEY003
            secret_access_key: ${ACME_BATCH_SECRET_001}
    buckets:                         # buckets owned by this tenant
      - name: photos                 # bucket name on the front side
        created_at: 2026-01-15T09:00:00Z # reported by ListBuckets (optional; default 1970-01-01)
        backend:
          endpoint: http://ceph.internal:7480
          region: us-east-1          # default "us-east-1"
          bucket: photos-prod        # bucket name on the backend (default: same as name)
          access_key_id: ${CEPH_ACCESS_KEY_ID}
          secret_access_key: ${CEPH_SECRET_ACCESS_KEY}
          use_path_style: true       # default true
      - name: logs
        backend:
          # no endpoint: Amazon S3, resolved by the SDK from the region
          region: ap-northeast-1
          access_key_id: ${AWS_ACCESS_KEY_ID_FOR_LOGS}
          secret_access_key: ${AWS_SECRET_ACCESS_KEY_FOR_LOGS}
```

Notes:

- Bucket names and access key ids must be unique across all tenants (path-style URLs carry no tenant discriminator). User names must be unique within a tenant.
- When `backend.endpoint` is omitted, the backend is Amazon S3: the SDK resolves the endpoint from `region`, and `use_path_style` defaults to `false` (it defaults to `true` when an endpoint is set).
- When `backend.access_key_id` and `backend.secret_access_key` are omitted, the SDK default credential chain is used (environment variables, shared config, IAM roles, etc.).
- `GET /` (ListBuckets) returns the buckets of the key's tenant, with the tenant name as the owner.
- Copying (CopyObject / UploadPartCopy) resolves the source within the requesting key's tenant, so copying **from** another tenant's bucket is impossible. Copying **into** another tenant's bucket works when its policy grants `s3:PutObject` ([cross-tenant access](docs/s3-api.md#cross-tenant-access)).

### Definition store

The `s3rp` binary reads its definitions from the YAML config as above. Any other source — a database, an API — is a [`store.Store` implementation you bring](#building-a-service-on-the-gateway) when embedding the gateway; the interface is read-only on purpose, definitions are written by your control plane, not through s3rp.

## Client usage

Point any S3 client at s3rp with path-style addressing and a front-side key.

```console
$ export AWS_ACCESS_KEY_ID=S3RPKEY001
$ export AWS_SECRET_ACCESS_KEY=...
$ aws --endpoint-url http://localhost:8080 s3api put-object --bucket photos --key foo.jpg --body foo.jpg
$ aws --endpoint-url http://localhost:8080 s3api get-object --bucket photos --key foo.jpg out.jpg
$ aws --endpoint-url http://localhost:8080 s3api list-objects-v2 --bucket photos
```

## The S3 API

The API surface — the supported operations, bucket and user policies, CORS, ACLs, checksums, server-side encryption, Object Lock, presigned URLs and browser-based POST uploads — is the gateway's contract, shared by the `s3rp` binary and any service embedding `s3gw`. It is documented in **[docs/s3-api.md](docs/s3-api.md)**. Anything not implemented returns `NotImplemented` rather than being forwarded blindly (fail closed).

## Behind a TLS terminator

s3rp serves plain HTTP, and as a PoC it is not meant to face the internet at all. TLS termination and everything else that belongs in front of the gateway — proxy rules that keep SigV4 verifiable, `RemoteAddr` for IP conditions, the PROXY protocol — are covered in [Building a service on the gateway](docs/building-a-service.md#behind-a-tls-terminator).

## Building a service on the gateway

The S3 API lives in `s3gw`, separate from the parts of this repository that are only its PoC packaging — the YAML config, its store, the CLI. A real service does not fork the proxy: it implements what is genuinely its own and hands it to the gateway — a `store.Store` for where definitions come from, an `s3gw.Authorizer` for what the policies cannot express (quota, suspension), `s3gw.Interceptor`s for metering, a bandwidth-limit hook for per-tenant traffic shaping, and an `s3gw.Observer` for logging. The gateway keeps SigV4 verification, `aws-chunked` decoding and checksums, policy evaluation, CORS, and the operations themselves; it deliberately never writes definitions, so it can run with read-only credentials.

**[Building a service on the gateway](docs/building-a-service.md)** is the full guide: the minimal embedding code, store caching and policy parsing (dialects, write-time validation, temporary credentials), what belongs in front of the gateway (rate limiting, body caps), sizing the client and signer caches, how interceptors nest and what the byte counts mean, and observation.

The other packages are usable on their own: `sigv4` (server-side SigV4 verification and `aws-chunked` decoding), `policy` (AWS-style policy evaluation), `s3err`, `s3xml`, `checksum` and `cors`. `checksum`, `policy` and `s3xml` depend only on the standard library.

## Limitations

API-level limitations — headers that break verification, why lifecycle and other bucket-configuration writes are `NotImplemented` — are listed in [docs/s3-api.md](docs/s3-api.md#limitations). What follows is specific to the bundled binary:

- Definitions are read from the store on every request and nothing is cached, so the store is on the hot path. Caching belongs to a store implementation, which is the only thing that knows when a key is revoked.
- Every request is logged synchronously. At any real request rate that write dominates the request path — it roughly doubled the time of a small GET when measured — so a deployment would want the log buffered or sampled.

## Development

The S3 API itself is the `s3gw` package, built on leaf packages (`sigv4`, `policy`, `s3err`, `s3xml`, `checksum`, `cors`) over the `store` contract; the root package is only the config, its store, the HTTP server and the CLI.

Unit tests run without any backend:

```console
$ go test -race ./...
```

The auth path is fuzzed: the aws-chunked decoder, SigV4 header and presigned verification, and POST policy verification carry property-based targets in `sigv4/`. Their seed corpora and every crasher found so far replay as regressions in the command above; exploring for new inputs is a nightly workflow, or locally:

```console
$ go test ./sigv4 -run '^$' -fuzz FuzzVerifyHeaderRoundtrip -fuzztime 60s
```

Signature verification and policy evaluation run on every request, so when changing either, check the benchmarks that guard them — watch allocations as well as time, since a regression usually shows there first:

```console
$ go test ./policy -bench . -benchmem     # policy evaluation, incl. the worst case the size caps allow
$ go test ./s3gw -bench VerifyKeyDiversity -benchmem   # SigV4 verification across many access keys
```

The integration test suite runs against a real S3-compatible backend, selected by environment variables. Two backends are provided in `compose.yml`:

```console
# versitygw (lightweight, default)
$ docker compose up -d --wait versitygw
$ S3RP_TEST_BACKEND_ENDPOINT=http://localhost:7070 go test -race -run TestIntegration ./...

# Ceph RGW (heavyweight, compatibility check)
$ docker compose up -d --wait ceph
$ S3RP_TEST_BACKEND_ENDPOINT=http://127.0.0.1:7480 go test -race -run TestIntegration ./...
```

Note: access Ceph RGW via `127.0.0.1`, not `localhost` — RGW resolves Host names that do not match its `rgw dns name` as virtual-hosted bucket names. CI runs the integration suite against both backends as a matrix.

The [ceph/s3-tests](https://github.com/ceph/s3-tests) compatibility suite can be run against s3rp with `./s3tests/run.sh`, which wraps the gateway in a test-only harness providing the CreateBucket/DeleteBucket the suite needs (see [s3tests/README.md](s3tests/README.md)). The classified results live in [docs/s3-tests.md](docs/s3-tests.md).

## LICENSE

MIT

## Author

fujiwara
