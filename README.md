# s3rp

s3rp is a multi-tenant S3 API gateway: an S3-compatible endpoint that authenticates tenants with their own access keys and forwards each operation to a per-bucket backend under the backend's own credentials.

> [!WARNING]
> This is a **proof of concept**. Its goal is to validate an architecture, not to be a product. **Do not use it for any other purpose.** It is not production-ready: definitions can live in a static YAML file or a sqlite database, the config/schema may change without notice, and no security review has been done.

```
S3 client --(SigV4, tenant keys)--> s3rp --(SigV4, backend keys)--> S3-compatible backend
                                      │
                            reads definitions from
                                      ▼
                        store (YAML / sqlite), read-only
```

## What this PoC validates

s3rp is not an object storage implementation — it stores no data itself. It explores the **data plane of a managed, multi-tenant S3 service** that sits in front of existing S3-compatible storage (Ceph RGW, versitygw, Amazon S3, ...). The questions it answers, and the design decisions behind them:

**Why a reverse proxy?** A managed service needs one identity/authorization plane over heterogeneous backends. Tenants get their own keys and never see the backend's credentials, endpoints, or even the real bucket names; the operator can place a tenant's bucket on any backend and move it without the tenant noticing. The proxy is where per-tenant authentication, authorization (bucket policies), metering points, and a uniform API surface naturally live.

**Why reconstruct operations with aws-sdk-go-v2 instead of forwarding the HTTP request?** A transparent SigV4-resigning proxy would be less code, but a multi-tenant service must *understand* each request, not just relay bytes:

- **Authorization by operation** — every request maps to an `s3:*` action evaluated against the bucket policy; an allow-list of implemented operations means unsupported/dangerous ones fail closed rather than leaking through.
- **Namespace virtualization** — the front bucket name is rewritten to the backend bucket, and (crucially) rewritten *back* in responses (`ListBucketResult`, multipart results, error `Resource`), which a byte-forwarding proxy cannot do without parsing and rebuilding responses anyway.
- **Uniform behavior** — the SDK absorbs backend quirks (endpoint resolution, retries, checksum negotiation), so the tenant-facing contract is owned by s3rp, not by whichever backend happens to serve a bucket.

The cost is that each operation is implemented explicitly; that trade-off, and its edge cases (SigV4 verification, aws-chunked decoding, checksum pass-through), are much of what this PoC exercises.

**What is data plane vs. control plane?** This repo is the **data plane**: it only ever *reads* definitions (tenants, users, keys, buckets, policies), through a small read-only [`store.Store`](store/store.go) interface. All *writes* are deliberately out of scope — in a managed offering they belong to a separate control plane API (create tenants, issue/rotate keys, place buckets) with its own credentials and audit trail. The read/write split is enforced here so the boundary is real, not aspirational:

- the proxy links only SELECT queries and opens the database read-only (`mode=ro`);
- all writes go through a **separate `s3rp-admin` binary** (schema migration and importing definitions), so the proxy deployment contains no write code or write credentials.

`s3rp-admin` is a stand-in for that future control plane, just enough to populate the store. Building the control plane itself (authn, quotas, billing, self-service) is conventional CRUD work with no architectural uncertainty, so it is intentionally left unimplemented.

## Install

### Binary

Download the binaries (`s3rp` and `s3rp-admin`) from [Releases](https://github.com/fujiwara/s3rp/releases).

### go install

```console
$ go install github.com/fujiwara/s3rp/cmd/s3rp@latest
$ go install github.com/fujiwara/s3rp/cmd/s3rp-admin@latest
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

A tenant owns one or more buckets and users. A user is the stable identity within a tenant (name: `[a-z][a-z0-9_-]+`); access keys are issued per user and rotate under it — add a new key, switch clients, then remove the old one. Every key of a tenant can access all of the tenant's buckets, unless restricted by a [bucket policy](#bucket-policies).

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
- Copying (CopyObject / UploadPartCopy) resolves the source within the requesting key's tenant, so cross-tenant copying is impossible.

### Definition store

By default, tenants and buckets are defined directly in the YAML config as above. They can instead be read from a sqlite database:

```yaml
listen: ":8080"
store:
  driver: sqlite
  dsn: s3rp.db
# tenants: must not be present with the sqlite driver
```

The proxy always opens the database **read-only** (a `mode=` parameter in the DSN is rejected). All writes go through the separate `s3rp-admin` binary, so the proxy deployment carries no write code or credentials:

```console
$ s3rp-admin --dsn s3rp.db migrate                       # apply the schema (idempotent)
$ s3rp-admin --dsn s3rp.db import --config tenants.yaml  # load a tenants-form YAML into the DB
```

The schema lives in `db/schema.sql`, shared by the read side (proxy) and write side (admin) via sqlc-generated packages.

## Client usage

Point any S3 client at s3rp with path-style addressing and a front-side key.

```console
$ export AWS_ACCESS_KEY_ID=S3RPKEY001
$ export AWS_SECRET_ACCESS_KEY=...
$ aws --endpoint-url http://localhost:8080 s3api put-object --bucket photos --key foo.jpg --body foo.jpg
$ aws --endpoint-url http://localhost:8080 s3api get-object --bucket photos --key foo.jpg out.jpg
$ aws --endpoint-url http://localhost:8080 s3api list-objects-v2 --bucket photos
```

## Supported operations

Because operations are reconstructed rather than forwarded, each one is implemented explicitly. The list below is the surface the PoC covers so far — enough to exercise real clients (the AWS CLI and SDKs) end to end against real backends. Anything not listed returns `NotImplemented` (fail closed).

- GetObject
- PutObject
- HeadObject
- DeleteObject
- DeleteObjects
- CopyObject
- ListObjects
- ListObjectsV2
- HeadBucket
- GetBucketLocation
- ListBuckets
- GetObjectTagging
- PutObjectTagging
- DeleteObjectTagging
- GetBucketVersioning
- PutBucketVersioning
- ListObjectVersions
- GetBucketAcl
- GetObjectAcl
- GetBucketPolicy
- GetBucketCors
- GetObjectLockConfiguration
- PutObjectLockConfiguration
- GetObjectRetention
- PutObjectRetention
- GetObjectLegalHold
- PutObjectLegalHold
- CreateMultipartUpload
- UploadPart
- UploadPartCopy
- CompleteMultipartUpload
- AbortMultipartUpload
- ListParts
- ListMultipartUploads

Other operations return a `NotImplemented` error.

CopyObject and UploadPartCopy work between buckets served by the same backend (same endpoint, region and credentials); copying across different backends returns `NotImplemented`. The copy source bucket must belong to the requester's tenant.

The `versionId` query parameter is passed through on GetObject, HeadObject, DeleteObject, GetObjectAcl and the object tagging operations. Versioning requires a backend that supports it.

`aws-chunked` request bodies (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD` and the trailer variants), which the AWS CLI and SDKs use for uploads over plain http endpoints, are decoded and their chunk signatures are verified.

### Object Lock

Object Lock (WORM) is passed through to the backend, which enforces the retention. The object-lock configuration, per-object retention, and legal hold operations are proxied, and the `x-amz-object-lock-*` headers on uploads and `x-amz-bypass-governance-retention` on deletes are forwarded. Bucket policies gain the corresponding actions (`s3:GetObjectRetention`, `s3:PutObjectRetention`, `s3:GetObjectLegalHold`, `s3:PutObjectLegalHold`, `s3:BypassGovernanceRetention`, `s3:Get/PutBucketObjectLockConfiguration`).

Object Lock must be enabled when a bucket is created, and s3rp does not proxy CreateBucket, so the backend bucket must have been created with Object Lock enabled. The exact behavior depends on the backend: Ceph RGW and Amazon S3 support it fully, while versitygw enforces retention but does not honor governance-mode bypass.

### Checksums

`x-amz-checksum-*` checksums (CRC32, CRC32C, CRC64NVME, SHA1, SHA256) flow end-to-end:

- Precomputed checksum headers on uploads pass through to the backend, which validates and stores them.
- Trailing checksums in `aws-chunked` bodies (the SDK default) are **verified by the proxy** against the decoded payload (`BadDigest` on mismatch), and the algorithm is forwarded so the backend recomputes and stores the checksum.
- Downloads pass `x-amz-checksum-mode: ENABLED` through and return the backend's checksum headers, so client SDKs can validate response payloads. Multipart part checksums are carried through UploadPart / CompleteMultipartUpload as well.

Whether a checksum is actually stored and returned depends on the backend (versitygw and Amazon S3 do; some Ceph RGW builds do not).

### Bucket policies

A bucket may carry an AWS-style policy document, written as JSON text in the config (`buckets[].policy`) or the database. GetBucketPolicy returns it; PutBucketPolicy / DeleteBucketPolicy are not supported (policies are defined in the store, not via the S3 API).

Two simplifications against AWS: principals are plain user names of the tenant under the `S3RP` key (no ARNs), and resources are plain `"bucket"` / `"bucket/prefix*"` strings (no ARNs). Action and Resource support the AWS wildcards `*` (any run of characters, including `/`) and `?` (exactly one character). As in AWS, `Action` matching is case-insensitive (so a mis-cased `Deny` cannot silently fail open), while `Resource` matching is case-sensitive since object keys are.

```yaml
buckets:
  - name: photos
    backend: { ... }
    policy: |
      {
        "Version": "2012-10-17",
        "Statement": [
          {
            "Sid": "BatchIsReadOnly",
            "Effect": "Deny",
            "Principal": {"S3RP": ["batch"]},
            "Action": ["s3:PutObject", "s3:DeleteObject"],
            "Resource": ["photos/*"]
          }
        ]
      }
```

Evaluation model: every user of a tenant has full access to the tenant's buckets by default, and explicit `Deny` statements restrict it. `Allow` statements are accepted but have no effect yet (everything is already allowed); they will become meaningful when anonymous and cross-tenant access are introduced.

Principal forms:

- `{"S3RP": ["name", ...]}` — the listed users of the tenant.
- `"*"` — all users, including ones added later. Note that the scope of `"*"` will widen when anonymous / cross-tenant access arrives (an `Allow` with `"*"` will then mean public access).
- `NotPrincipal` (exclusive with `Principal`) — everyone except the listed users. `Deny` + `NotPrincipal` expresses "only these users may ..." so that newly added users are denied by default.

Limitations: policies only cover users of the owning tenant; versioned operations use the same action names as unversioned ones (no `s3:GetObjectVersion` distinction). DeleteObjects is evaluated per object: denied keys are reported in the `Error` entries of the response. Copying evaluates `s3:GetObject` on the source and `s3:PutObject` on the destination.

### CORS

CORS is handled by the proxy itself (it is a contract between the browser and the server the browser talks to, so backend CORS settings are not passed through). Rules are defined per bucket:

```yaml
buckets:
  - name: photos
    backend: { ... }
    cors:
      - allowed_origins: ["https://app.example.com", "https://*.preview.example.com"]
        allowed_methods: [GET, PUT]   # GET, PUT, POST, DELETE, HEAD
        allowed_headers: ["*"]
        expose_headers: [ETag]
        max_age_seconds: 3600
```

Preflight `OPTIONS` requests are answered without authentication based on these rules, which makes browser-direct uploads via presigned URLs work. Actual responses carry `Access-Control-Allow-Origin` / `Access-Control-Allow-Credentials` / `Access-Control-Expose-Headers` when the request's `Origin` matches a rule. `*` in `allowed_origins` matches any characters (e.g. `https://*.example.com`).

GetBucketCors returns the configuration (`NoSuchCORSConfiguration` when absent); PutBucketCors / DeleteBucketCors are not supported (rules are defined in the store, not via the S3 API).

### ACLs

s3rp behaves like a bucket with ACLs disabled (Object Ownership = bucket owner enforced, the AWS default since 2023). GetBucketAcl / GetObjectAcl return a fixed policy granting FULL_CONTROL to the tenant; PutBucketAcl / PutObjectAcl return `AccessControlListNotSupported`, and canned ACLs other than `private` / `bucket-owner-full-control` are rejected on uploads. Use bucket policies for access control instead.

## Presigned URLs

Presigned URLs (SigV4 query string authentication) generated with front-side keys against the s3rp endpoint are supported for the operations above. Expiry (`X-Amz-Expires`, up to 7 days) is enforced.

```console
$ aws --endpoint-url http://localhost:8080 s3 presign s3://photos/foo.jpg
http://localhost:8080/photos/foo.jpg?X-Amz-Algorithm=AWS4-HMAC-SHA256&...
```

## Limitations

- The payload SHA-256 declared in `x-amz-content-sha256` is not independently verified against the request body (the signature covers the declared hash; verifying the body would require buffering it). Chunk signatures and trailing checksums of `aws-chunked` bodies are verified.
- Requests that sign the `user-agent` or other headers the AWS SDK signer ignores will fail verification. Real AWS SDK/CLI clients do not do this.

## Development

Unit tests run without any backend:

```console
$ go test -race ./...
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

## LICENSE

MIT

## Author

fujiwara
