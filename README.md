# s3rp

s3rp is an S3 API reverse proxy with SigV4 re-signing.

Clients access s3rp using the S3 API (path-style) with front-side access keys issued per bucket. s3rp verifies the SigV4 signature of incoming requests, then executes the operations against per-bucket backends (any S3-compatible server: Ceph, versitygw, Amazon S3, etc.) using the backend's own credentials.

```
S3 client --(SigV4, front keys)--> s3rp --(SigV4, backend keys)--> S3-compatible backend
```

Use cases:

- Issue access keys for buckets on backends that make key management hard.
- Hide the backend credentials and endpoints from clients.
- Route buckets on a single endpoint to different backends.

## Install

### Homebrew

```console
$ brew install fujiwara/tap/s3rp
```

### Binary

Download the binary from [Releases](https://github.com/fujiwara/s3rp/releases).

### go install

```console
$ go install github.com/fujiwara/s3rp/cmd/s3rp@latest
```

## Usage

```
Usage: s3rp [flags]

S3 API reverse proxy with SigV4 re-signing

Flags:
  -h, --help                Show context-sensitive help.
      --config="s3rp.yaml"  config file path ($S3RP_CONFIG)
      --listen=STRING       listen address (overrides config) ($S3RP_LISTEN)
      --log-level="info"    log level ($S3RP_LOG_LEVEL)
      --version             show version
```

## Configuration

The config file is YAML. Environment variables in the file are expanded (`${VAR}` or `$VAR`).

A tenant owns one or more buckets and users. A user is the stable identity within a tenant (name: `[a-z][a-z0-9_-]+`); access keys are issued per user and rotate under it — add a new key, switch clients, then remove the old one. Every key of a tenant can access all of the tenant's buckets.

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
- CreateMultipartUpload
- UploadPart
- UploadPartCopy
- CompleteMultipartUpload
- AbortMultipartUpload
- ListParts
- ListMultipartUploads

Other operations return a `NotImplemented` error.

CopyObject and UploadPartCopy work between buckets served by the same backend (same endpoint, region and credentials); copying across different backends returns `NotImplemented`. The copy source bucket must be accessible by the requesting access key.

The `versionId` query parameter is passed through on GetObject, HeadObject, DeleteObject and the object tagging operations. Versioning requires a backend that supports it.

### Bucket policies

A bucket may carry an AWS-style policy document, written as JSON text in the config (`buckets[].policy`). GetBucketPolicy returns it; PutBucketPolicy / DeleteBucketPolicy are not supported (policies are defined in the config).

Two simplifications against AWS: principals are plain user names of the tenant under the `S3RP` key (no ARNs), and resources are plain `"bucket"` / `"bucket/prefix*"` strings (no ARNs). `*` in Action / Resource matches any characters including `/`.

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

### ACLs

s3rp behaves like a bucket with ACLs disabled (Object Ownership = bucket owner enforced, the AWS default since 2023). GetBucketAcl / GetObjectAcl return a fixed policy granting FULL_CONTROL to the tenant; PutBucketAcl / PutObjectAcl return `AccessControlListNotSupported`, and canned ACLs other than `private` / `bucket-owner-full-control` are rejected on uploads. Use tenant keys for access control instead.

`aws-chunked` request bodies (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD` and the trailer variants), which the AWS CLI and SDKs use for uploads over plain http endpoints, are decoded and their chunk signatures are verified.

## Presigned URLs

Presigned URLs (SigV4 query string authentication) generated with front-side keys against the s3rp endpoint are supported for the operations above. Expiry (`X-Amz-Expires`, up to 7 days) is enforced.

```console
$ aws --endpoint-url http://localhost:8080 s3 presign s3://photos/foo.jpg
http://localhost:8080/photos/foo.jpg?X-Amz-Algorithm=AWS4-HMAC-SHA256&...
```

## Limitations

- The payload SHA-256 declared in `x-amz-content-sha256` is not independently verified against the request body (the signature covers the declared hash; verifying the body would require buffering it). Chunk signatures of `aws-chunked` bodies are verified.
- Requests that sign the `user-agent` or other headers the AWS SDK signer ignores will fail verification. Real AWS SDK/CLI clients do not do this.

## LICENSE

MIT

## Author

fujiwara
