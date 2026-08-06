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

A tenant owns one or more buckets and users. A user is the stable identity within a tenant (name: `[a-z][a-z0-9_-]+`); access keys are issued per user and rotate under it — add a new key, switch clients, then remove the old one. Every key of a tenant can access all of the tenant's buckets, unless restricted by a [bucket policy](#bucket-policies) or a [user policy](#user-policies); a bucket policy can also grant selected operations to another tenant's user ([cross-tenant access](#cross-tenant-access)). Two tenants may not map their buckets to the same physical backend bucket (endpoint + backend bucket name); this is rejected at startup to preserve tenant isolation.

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
- Copying (CopyObject / UploadPartCopy) resolves the source within the requesting key's tenant, so cross-tenant copying is impossible.

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
- PostObject (browser-based POST upload, see [below](#browser-based-uploads-post))

Other operations return a `NotImplemented` error.

CopyObject and UploadPartCopy work between buckets served by the same backend (same endpoint, region and credentials); copying across different backends returns `NotImplemented`. The copy source bucket must belong to the requester's tenant.

GetBucketLocation and HeadBucket (the `x-amz-bucket-region` header) report the gateway's own region — the value pinned with `SetRegion`, `us-east-1` when unset — never the backend's region, which stays hidden like the backend bucket name and endpoint.

Wherever a response exposes an `Owner` or `Initiator` — object and version listings, multipart listings, ACLs, ListBuckets — it is the requesting tenant, never the backend account the proxy uses.

ListBuckets answers from the store without calling any backend: the bucket names are the front names, and each `CreationDate` is the store's `created_at` for the bucket (the Unix epoch when the store does not track one).

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

### Server-side encryption

SSE-S3 (`x-amz-server-side-encryption: AES256`) and SSE-KMS (`aws:kms` + `x-amz-server-side-encryption-aws-kms-key-id`) **pass through**: the backend performs the encryption, and its result headers are returned on uploads and downloads. The KMS key id is opaque to the gateway — it is whatever the backend's KMS resolves (for example Ceph RGW handing it to a Vault-compatible key service), so the key id namespace belongs to the service, not to the backend's infrastructure. The requested mode and key id are exposed as `Op.SSE` / `Op.SSEKMSKeyID` to the [Authorizer](#building-a-service-on-the-gateway): whether a tenant may use a key — or whether a bucket's backend supports encryption at all (some ignore the request silently rather than refuse) — is the service's decision; nothing else in the path knows which tenant owns which key, since the backend's KMS request carries no tenant identity.

**SSE-C is refused** with `NotImplemented` rather than silently dropped: an ignored customer key would store the object without the encryption the client believes it requested, and later serve it back without the key.

Backend notes for Ceph RGW: SSE requests require TLS toward RGW by default (`rgw_crypt_require_ssl`) — terminate or disable it deliberately; the compose file's ceph service configures the built-in `testing` KMS backend with a static key (`testkey-1`) so the integration suite can exercise SSE-KMS without a real KMS.

### Bucket policies

A bucket may carry an AWS-style policy document, written as JSON text in the config (`buckets[].policy`). GetBucketPolicy returns it; PutBucketPolicy / DeleteBucketPolicy are not supported (policies are defined in the store, not via the S3 API).

Two simplifications against AWS: principals are plain user names under the `S3RP` key (no ARNs) — `"alice"` names a user of the bucket's own tenant, `"tenant-b/alice"` a user of another tenant — and resources are plain `"bucket"` / `"bucket/prefix*"` strings (no ARNs). Action and Resource support the AWS wildcards `*` (any run of characters, including `/`) and `?` (exactly one character). As in AWS, `Action` matching is case-insensitive (so a mis-cased `Deny` cannot silently fail open), while `Resource` matching is case-sensitive since object keys are.

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

Evaluation model: every user of a tenant has full access to the tenant's buckets by default, and explicit `Deny` statements restrict it. For the bucket's own users, `Allow` statements have no effect (everything is already allowed); they are what grants cross-tenant access (below).

Principal forms:

- `{"S3RP": ["name", ...]}` — the listed users: `"alice"` is a user of the bucket's own tenant, `"tenant-b/alice"` a user of another tenant. Writing the own tenant's users in qualified form is rejected at load time (evaluation matches them by their plain name).
- `"*"` — all users **of the bucket's tenant**, including ones added later. It never reaches other tenants' users.
- `NotPrincipal` (exclusive with `Principal`) — everyone except the listed users. `Deny` + `NotPrincipal` expresses "only these users may ..." so that newly added users are denied by default.

#### Cross-tenant access

A bucket policy may grant access to another tenant's user by listing its qualified principal:

```json
{
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {"S3RP": ["tenant-b/bob"]},
      "Action": ["s3:GetObject", "s3:ListBucket"],
      "Resource": ["photos", "photos/*"]
    }
  ]
}
```

The baseline for a foreign requester is the opposite of the own-tenant one: **deny unless an `Allow` matches**, and `Deny` still wins over `Allow`. Grants never widen across tenants implicitly — an `Allow` reaches a foreign user only when its `Principal` lists the qualified name explicitly; `"*"` and `NotPrincipal`-based `Allow` statements do not. `Deny` statements match broadly, so a blanket `Deny` (`"*"`, or `NotPrincipal`) also catches cross-tenant principals granted elsewhere in the policy.

A bucket whose policy does not mention the requester answers with the same `403 AccessDenied` a nonexistent bucket produces, so bucket names cannot be probed across tenants. Responses expose the bucket-owning tenant as `Owner`, never the requester's. CopyObject / UploadPartCopy resolve their source within the requester's own tenant, so copying **from** another tenant's bucket is not possible even with a cross-tenant read grant (copying **into** a foreign bucket with a `s3:PutObject` grant works, subject to the same-backend restriction). ListBuckets lists only the tenant's own buckets, as on AWS.

Limitations: versioned operations use the same action names as unversioned ones (no `s3:GetObjectVersion` distinction). DeleteObjects is evaluated per object: denied keys are reported in the `Error` entries of the response. Copying evaluates `s3:GetObject` on the source and `s3:PutObject` on the destination.

### User policies

Where a bucket policy attaches to a bucket and names principals, a user policy attaches to a user and only looks at the operation. It is a lightweight identity policy: a list of `Allow` / `Deny` statements over `s3:*` actions (with the same `*` / `?` wildcards, case-insensitive), with no resource — it decides which operations that user may perform at all, before the bucket policy is consulted.

```yaml
tenants:
  - name: acme
    users:
      - name: readonly
        keys:
          - { access_key_id: ..., secret_access_key: ... }
        policy:
          - effect: Allow
            action: [s3:Get*, s3:List*, s3:HeadObject, s3:HeadBucket]
          - effect: Deny
            action: [s3:GetObjectAcl]
      - name: admin          # no policy = allow s3:* (full access)
        keys:
          - { access_key_id: ..., secret_access_key: ... }
```

Evaluation is IAM-style and independent of the bucket policy's baseline-allow model:

- **No policy** (the default) means allow `s3:*` — the current full-access behavior is preserved.
- **With a policy**, only actions matching an `Allow` are permitted; anything unmatched is an implicit deny. A matching `Deny` takes precedence over any `Allow`. So `Allow [s3:Get*, s3:List*]` limits the user to reads.

A request must pass **both** layers: the user policy must allow the action **and** the bucket policy must not `Deny` it. Either denial returns `403 AccessDenied`. The check sits at the same single authorization chokepoint as bucket policies, so it covers every operation uniformly (including per-object DeleteObjects entries and the source/destination actions of a copy).

Both bucket and user policies are bounded in size: at most 20 KB per document, 20 statements per policy, 30 actions and 10 resources per statement, 128 bytes per action/resource pattern, and 100 principal users per statement. Oversized policies are rejected when loaded.

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

Preflight `OPTIONS` requests are answered without authentication based on these rules, which makes browser-direct uploads via presigned URLs work. Actual responses carry `Access-Control-Allow-Origin` / `Access-Control-Allow-Credentials` / `Access-Control-Expose-Headers` when the request's `Origin` matches a rule. `*` matches any characters in both `allowed_origins` (e.g. `https://*.example.com`) and `allowed_headers` (e.g. `x-amz-*` allows every Amazon-specific header); header matching is case-insensitive.

GetBucketCors returns the configuration (`NoSuchCORSConfiguration` when absent); PutBucketCors / DeleteBucketCors are not supported (rules are defined in the store, not via the S3 API).

### ACLs

s3rp behaves like a bucket with ACLs disabled (Object Ownership = bucket owner enforced, the AWS default since 2023). GetBucketAcl / GetObjectAcl return a fixed policy granting FULL_CONTROL to the tenant; PutBucketAcl / PutObjectAcl return `AccessControlListNotSupported`, and canned ACLs other than `private` / `bucket-owner-full-control` are rejected on uploads. Use bucket policies for access control instead.

## Presigned URLs

Presigned URLs (SigV4 query string authentication) generated with front-side keys against the s3rp endpoint are supported for the operations above. Expiry (`X-Amz-Expires`, up to 7 days) is enforced.

```console
$ aws --endpoint-url http://localhost:8080 s3 presign s3://photos/foo.jpg
http://localhost:8080/photos/foo.jpg?X-Amz-Algorithm=AWS4-HMAC-SHA256&...
```

## Browser-based uploads (POST)

`POST /bucket` with a `multipart/form-data` body and a SigV4 POST policy ("Browser-Based Uploads Using POST" in the S3 API reference) is supported: a backend service signs a policy document with a front-side key, and the browser uploads directly to s3rp with an HTML form — no credentials in the page. Combined with [CORS](#cors), this is the standard browser-direct upload flow, and forms generated by the AWS SDK presigned-POST helpers work as they are.

- Conditions: exact match (`{"field": "value"}` / `["eq", "$field", "value"]`), `["starts-with", "$field", "prefix"]` and `["content-length-range", min, max]` (enforced while the file streams; an upload that ends up under the minimum is deleted from the backend and refused). Field names are case-insensitive; `${filename}` in the `key` field is substituted before conditions are evaluated, and the target bucket is checked as the `bucket` condition.
- As on AWS, every form field must be covered by a condition (except `policy`, `x-amz-signature`, `x-ignore-*` and the file itself), the policy `expiration` is enforced, and form fields after the `file` part are ignored.
- Supported fields: `key`, `Content-Type`, `Content-MD5`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Content-Language`, `Expires`, `x-amz-storage-class`, `x-amz-tagging`, `x-amz-meta-*`, `x-amz-server-side-encryption` (+ `-aws-kms-key-id`), `success_action_status` (200/201/204) and `success_action_redirect` (303 with `bucket`/`key`/`etag` appended). `acl` follows the same rule as everywhere else (ACLs are disabled); unsupported fields are refused with `NotImplemented` rather than silently dropped.

## Behind a TLS terminator

s3rp serves plain HTTP, so a real deployment puts a reverse proxy in front of it. SigV4 signs the request itself, which makes that proxy part of the verification path: anything it rewrites, the signature no longer covers.

**What breaks every request.** Verification re-signs the request as it arrived, from `RequestURI`, the `Host` header and the headers the client listed as signed:

- **Preserve `Host`.** It is signed. nginx: `proxy_set_header Host $http_host;`. CloudFront sends the *origin's* hostname unless the Host header is forwarded, which fails every signature. ALB preserves it.
- **Pass the request URI exactly as sent** — no normalization, no re-encoding, no merging of `//`. Beyond the signature, this decides what the object key *is*: `a//b` and `a/b` are different keys, and `%2F` in a key is not a separator. nginx: `proxy_pass` **without a URI part** (with one, nginx passes the normalized form), and `merge_slashes off;`.
- **Do not alter signed headers.** Adding headers is safe — `X-Forwarded-*` is not signed — but rewriting or dropping one the client signed is not.

**What breaks large transfers.** s3rp deliberately sets no read or write timeout so uploads and downloads are not cut off mid-stream; the proxy in front usually does, and usually buffers:

- `proxy_request_buffering off;` — otherwise every upload lands on the proxy's disk first.
- `client_max_body_size 0;` — the default 1 MB refuses ordinary objects; a single PUT can be 5 GiB.
- Raise `proxy_read_timeout` / `proxy_send_timeout`; the 60 s default cuts off slow or large transfers.
- Do not compress responses, and let `Expect: 100-continue` through — the AWS SDKs use it for uploads.

**What corrupts accounting.** `proxy_next_upstream` includes `error` and `timeout` by default, so nginx may resend a **PUT** to another upstream. That is a duplicate upload, and a second request as far as s3rp is concerned — metering counts it twice, and it cannot tell the two apart. Set `proxy_next_upstream off;` or restrict it to idempotent cases.

```nginx
server {
    listen 443 ssl;
    server_name s3.example.com;

    merge_slashes off;            # object keys may contain //
    client_max_body_size 0;       # a single PUT can be 5 GiB
    proxy_request_buffering off;  # stream uploads rather than spool them
    proxy_http_version 1.1;

    location / {
        proxy_pass http://s3rp:8080;   # no URI part: the request line is passed as sent
        proxy_set_header Host $http_host;
        proxy_next_upstream off;       # never resend a PUT elsewhere
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }
}
```

**What you lose.** Every request now arrives from the proxy, so the `RemoteAddr` in the access log is the proxy's. s3rp does not interpret `X-Forwarded-For` — how many hops to trust is a property of the deployment, not of the gateway. A service that needs the client address should rewrite `RemoteAddr` in a handler wrapped around `gw.Handler()`, which the gateway reads for logging and uses for nothing else. Also leave the `x-amz-request-id` response header alone: it is what ties a user's report to the log line explaining it.

**The hop itself.** The scheme is not part of a SigV4 signature, so terminating TLS and forwarding over plain HTTP verifies correctly — but that hop carries object payloads and the requests authenticating them, so it belongs on a trusted network or under mTLS.

## Building a service on the gateway

The S3 API lives in `s3gw`, separate from the parts of this repository that are only its PoC packaging — the YAML config, its store, the CLI. A real service does not fork the proxy: it implements the two things that are genuinely its own and hands them to the gateway.

**What you provide**

| | |
|---|---|
| `store.Store` | where definitions come from: tenants, users, access keys, and where each bucket really lives. This is your control plane's read side. |
| `s3gw.Authorizer` | what the policies cannot express — an exhausted quota, a suspended tenant, a rate limit. Consulted after the bucket and user policies have already allowed the operation. |
| `s3gw.Interceptor` | what you meter. It wraps the operation, so the byte counts are filled in by the time `next` returns. |
| `s3gw.Observer` | how requests are logged. The gateway does not log — once per request it reports who asked, what they asked for, what they were told and why — and leaves the format, level and destination to you. |

**What the gateway does**: SigV4 verification (header and presigned), `aws-chunked` decoding and checksums, bucket and user policy evaluation, CORS, the operations themselves, and the routing that reaches them — refusing unknown ones rather than passing them through.

**What it deliberately does not do**: create tenants, buckets or keys. Those are control-plane writes; the gateway only ever reads definitions, so it can run with a read-only database account.

```go
package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"

	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/store"
)

// definitions is your control plane: which tenants and users exist, which
// access keys they hold, and where each bucket really lives.
type definitions struct{ /* your database */ }

func (d *definitions) GetKey(ctx context.Context, accessKeyID string) (*store.Key, error) {
	// look the key up; return an error wrapping store.ErrNotFound if absent
	return &store.Key{
		AccessKeyID: accessKeyID, SecretAccessKey: "…",
		Tenant: "acme", User: "app1",
	}, nil
}

func (d *definitions) GetBucket(ctx context.Context, tenant, bucket string) (*store.Bucket, error) {
	return &store.Bucket{
		Tenant: tenant, Name: bucket,
		Backend: &store.Backend{
			Endpoint: "https://rgw.internal:7480", Region: "us-east-1",
			Bucket:      "acme-photos-a1b2", // the real name, never shown to the client
			AccessKeyID: "…", SecretAccessKey: "…",
		},
		// Metadata is opaque to the gateway and comes back on Op in your
		// hooks — attach what this lookup already loaded instead of
		// querying it again there. store.Key has the same field.
		Metadata: &tenantPlan{quotaBytes: 1 << 40},
	}, nil
}

// GetBucketByName resolves a bucket without a tenant, for unauthenticated
// CORS preflights. Bucket names are globally unique for this reason.
func (d *definitions) GetBucketByName(ctx context.Context, bucket string) (*store.Bucket, error) { /* … */ }

// ListBuckets returns light entries (name and creation date) so a listing
// does not have to materialize full definitions, credentials included.
func (d *definitions) ListBuckets(ctx context.Context, tenant string) ([]store.BucketEntry, error) { /* … */ }

// tenantPlan is what the store loads alongside a bucket; the gateway hands
// it back untouched on Op.BucketMetadata.
type tenantPlan struct{ quotaBytes int64 }

// quota refuses what a bucket policy cannot express.
type quota struct{}

func (quota) Authorize(ctx context.Context, op *s3gw.Op) error {
	plan, _ := op.BucketMetadata.(*tenantPlan) // attached by GetBucket above
	if op.Action == "s3:PutObject" && plan != nil && usedBytes(op.Tenant) > plan.quotaBytes {
		// any error refuses the request; an *s3err.Error chooses what the
		// client is told, and the backend is never reached
		return s3err.New(http.StatusForbidden, "QuotaExceeded", "Quota exceeded")
	}
	return nil
}

func main() {
	gw := s3gw.New(&definitions{})
	gw.SetAuthorizer(quota{})
	gw.Use(func(ctx context.Context, op *s3gw.Op, next func() error) error {
		err := next()
		meter(op) // op.BytesIn and op.BytesOut are filled in by now
		return err
	})

	// the gateway does not log; this is where a request is recorded, in
	// whatever form your service already uses
	gw.SetObserver(func(ctx context.Context, info *s3gw.RequestInfo) {
		if info.Err != nil {
			// only place the reason for a failure can be seen; the client was
			// told no more than info.Code
			slog.ErrorContext(ctx, "request failed", "code", info.Code,
				"error", info.Err, "request_id", info.RequestID)
		}
		attrs := []any{
			"method", info.Method, "path", info.Path,
			"query", info.RawQuery, // presigned signature already masked
			"tenant", info.Tenant, "user", info.User, // empty if unverified
			"status", info.Status, "bytes_out", info.BytesOut,
			"duration", info.Duration, "request_id", info.RequestID,
		}
		if info.Op != nil { // nil when the request never reached an operation
			attrs = append(attrs, "action", info.Op.Action, "bucket", info.Op.Bucket)
		}
		slog.InfoContext(ctx, "request", attrs...)
	})

	log.Fatal(http.ListenAndServe(":8080", gw.Handler()))
}
```

**Things worth knowing before you build on it**

*Your store*

- Definitions are read on **every request** and nothing is cached: `GetKey` on each authentication, `GetBucket` on each bucket resolution. Caching is your store's business — it knows when a key is revoked, which the gateway cannot.
- If your store does cache, keep the parsed `*policy.Policy` / `*policy.UserPolicy` around, not the policy JSON: the compiled match patterns live on the policy object (built lazily on its first evaluation, safe to share across concurrent requests), so a store that re-parses the text per request silently repeats that work every time. A cache keyed by the policy text invalidates itself — a changed policy is a different key.
- The policy syntax — the `"S3RP"` principal key, plain-path resources — is only the default `policy.Dialect`. If your tenants should write a different key or ARN-prefixed resources (`arn:aws:s3:::bucket/*`), parse with your own `Dialect`: it normalizes at parse time, in one pass, and evaluation is identical. Keep the tenant's original text in `store.Bucket.PolicyText` (returned verbatim by GetBucketPolicy) and put the `Dialect`-parsed form in `Policy` — the two fields are deliberately independent.
- **Temporary credentials** are a store concern, not a gateway one: set `store.Key.SessionToken` on a key your control plane issues under an existing user (empty = long-lived key). A request signed with that key must present exactly that token — header auth, presigned URLs and POST uploads all carry it — and a token on a long-lived key is refused (`InvalidToken`), never dropped. Expiry is also yours: an expired key is simply not returned by `GetKey`. Authorization needs nothing new — the key belongs to a user, and a per-key `UserPolicy` narrows its actions. One issuance rule: do not return the credentials to the caller before the key is visible to the store the gateways read, or the first use races the write (and a stale not-found may be negative-cached).
- Two tenants must not map buckets to the same physical backend bucket — the gateway cannot detect this, so validate it where definitions are written.

*In front of the gateway*

- Unauthenticated requests are not free. `GetKey` runs **before** signature verification — the secret is what verifies — so a well-formed `Authorization` header with a bogus access key still costs one store lookup, and a valid key with a bad signature costs the HMAC chain too. A store should negative-cache unknown key ids (weighing the TTL against how soon a freshly issued key must start working), and rate limiting belongs **outside** `Handler()`, in a wrapping handler or the fronting proxy: the `Authorizer` runs only after verification and the policies, which is too late for DoS economics.
- That same wrapping layer is where a health-check endpoint and a request body cap (`http.MaxBytesReader`) go. To the gateway every path is S3 — `GET /` is an authenticated ListBuckets — and it deliberately caps nothing itself, for the same reason it sets no read or write timeouts.
- The signing region of a request is taken from its credential scope, so by default **any region verifies** — the signature commits to the region either way, and a front endpoint has no inherent region. A multi-region deployment should pin each endpoint with `SetRegion`: not for signature integrity, but so a client pointed at the wrong regional endpoint fails fast with AWS's own error (`AuthorizationHeaderMalformed`, naming the expected region) instead of silently being served cross-region, and so a leaked derived signing key stays scoped to its region as SigV4 intends. The pinned region is also what GetBucketLocation and HeadBucket report, keeping region discovery consistent with what the verifier accepts.

*Backend clients and the gateway's caches*

- Backend clients are built by the gateway, but `SetClientOptions` lets you contribute `s3.Options` to every one it builds — a custom `Retryer`, an instrumented `HTTPClient` (an otelhttp transport is how you get per-backend latency and retry metrics, which the hooks cannot see), timeouts. The hook receives the backend definition, so options can differ per backend, and runs after the gateway's own settings — which are load-bearing for non-AWS backends, so override them only knowingly. Set it before serving: clients are cached per backend and keep the options they were built with.
- What the gateway does cache is derived from definitions, never a definition itself, and is bounded: backend **clients** (one per distinct endpoint/credentials, LRU, default 128 — `SetClientCacheSize`) and one SigV4 **signer** per access key (default 512 slots — `SetSignerCacheSize`). Size them to the number of distinct backends and of access keys active at once; an evicted entry is rebuilt on its next request, so undersizing costs latency, not correctness.
- Whether they *are* sized right is answerable: `ClientCacheStats` / `SignerCacheStats` return snapshots (hits, misses, evictions, len, capacity — monotonic counters, poll them from your metrics collector). A rising eviction rate with `Len` near `Capacity` means the cache is too small. The signer cache's evictions are collision displacements: a high rate with `Len` well **under** `Capacity` means hot keys sharing a slot by hash luck, which more slots make improbable.

*Hooks and metering*

- `store.Bucket.Metadata` and `store.Key.Metadata` are opaque to the gateway and come back on `Op` (`BucketMetadata` / `KeyMetadata`), so the hooks get what the store's lookups already loaded — a quota, a suspension flag — without querying it again. They are excluded from `Op`'s JSON on purpose: whether that data belongs in a log is your decision. A store that shares definitions across requests must make the values safe for concurrent reads.
- Interceptors bracket **one inbound request** and nest like middleware: the first `Use` is the outermost layer, and `next()` runs everything inside it — down to the handler, which both calls the backend and writes the response to the client. With `gw.Use(A); gw.Use(B)`:

  ```
  verification → routing → policies → Authorizer
    A, before its next()
      B, before its next()
        handler: backend call (incl. SDK-internal retries),
                 response streamed to the client
        ...op.BytesIn / op.BytesOut are final from here on
      B, after its next()
    A, after its next()
  observer (RequestInfo, exactly once)
  ```

  Returning without calling `next` refuses the request — nothing inside runs, the backend is never contacted. An error returned by an inner layer is the outer layer's `next()` result, and whatever the outermost returns decides what the client is told. The byte counts are set at the innermost point, so metering is code placed after `next` in **any** layer and the counts are identical; a *duration* measured in A includes B, though, as in any middleware stack.
- A client that disconnects mid-response cancels the request context, which also aborts the backend transfer — `next` still returns promptly, with `op.BytesOut` counting what was actually sent (a truncated download reports the status it already sent, usually 200, and no error). The trap: the `ctx` your after-`next` code holds **is that canceled context**, so metering I/O that uses it fails with `context.Canceled` precisely for disconnected clients — and only for them, since on a normal completion the context outlives the hooks. Use `context.WithoutCancel(ctx)` for your own writes, or hand the self-contained `Op`/`RequestInfo` to a queue and drop the context entirely.
- A client that retries sends a **new request**, which is verified, authorized and metered on its own. That is what the server served; whether a retry should count toward a quota or an invoice is the application's decision, not the gateway's.
- `Op.BytesIn` / `BytesOut` count bytes on the wire, so an `aws-chunked` upload includes its framing. They measure transfer, not storage: a quota over bytes at rest needs the backend's inventory (deletes, overwrites and versions carry no sizes through the hooks).

*Observation*

- **Nothing is logged unless you install an observer**, including failures. The cause of a failure is not recoverable anywhere else: it never reaches the client, by design. An observer is called once per request, after the response has been written, whether or not the request ever reached an operation — a signature that did not verify or a bucket that does not exist never reaches an interceptor, but is still observed.
- `RequestInfo` keeps the identity apart from the operation: `Tenant` and `User` are set as soon as the signature verifies, `Op` only once routing and the policies pass. So a request refused for an unknown bucket or a denied action still records **who** asked, which is usually the point of looking.
- `RequestInfo` stands on its own, `Start` included, so it can be handed to a metering queue or a batch and still say when it happened — an observer that defers the work must not have to stamp the time itself. It carries snake_case JSON tags and can be emitted as it stands; the failure reason is rendered as its message, since an `error` marshals to an empty object on its own. `Op` is tagged the same way.
- Log `RequestInfo.RawQuery`, not the request's own query string: the gateway masks the presigned authentication parameters, and a presigned URL's signature is a bearer credential until it expires.

**The other packages** are usable on their own: `sigv4` (server-side SigV4 verification and `aws-chunked` decoding), `policy` (AWS-style policy evaluation), `s3err`, `s3xml`, `checksum` and `cors`. `checksum`, `policy` and `s3xml` depend only on the standard library.

## Limitations

- Requests that sign the `user-agent` or other headers the AWS SDK signer ignores will fail verification. Real AWS SDK/CLI clients do not do this.
- Bucket lifecycle configuration (expiration, transitions) is deliberately **not** exposed via the S3 API — `?lifecycle` gets the same loud `NotImplemented` as every unsupported subresource. Enforcement is the backend's job, so rules must live on the backend bucket, and they are written there by the control plane with backend credentials — the same split as bucket policies and CORS, whose `Put*` are also not proxied. This is a deliberate choice beyond consistency: a bucket holds exactly **one** lifecycle configuration, so letting tenants `PutBucketLifecycleConfiguration` would let one request replace the operator's baseline rules (such as aborting stale multipart uploads); exposing it would require a merge layer, not a pass-through. For experiments, Ceph RGW's `rgw_lc_debug_interval` shortens expiry to seconds.
- Definitions are read from the store on every request and nothing is cached, so the store is on the hot path. Caching belongs to a store implementation, which is the only thing that knows when a key is revoked.
- Every request is logged synchronously. At any real request rate that write dominates the request path — it roughly doubled the time of a small GET when measured — so a deployment would want the log buffered or sampled.

## Development

The S3 API itself is the `s3gw` package, built on leaf packages (`sigv4`, `policy`, `s3err`, `s3xml`, `checksum`, `cors`) over the `store` contract; the root package is only the config, its store, the HTTP server and the CLI.

Unit tests run without any backend:

```console
$ go test -race ./...
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

## LICENSE

MIT

## Author

fujiwara
