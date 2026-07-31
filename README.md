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

```yaml
listen: ":8080"
buckets:
  - name: photos                     # bucket name on the front side
    backend:
      endpoint: http://ceph.internal:7480
      region: us-east-1              # default "us-east-1"
      bucket: photos-prod            # bucket name on the backend (default: same as name)
      access_key_id: ${CEPH_ACCESS_KEY_ID}
      secret_access_key: ${CEPH_SECRET_ACCESS_KEY}
      use_path_style: true           # default true
    keys:                            # front-side access keys (multiple allowed)
      - access_key_id: S3RPKEY001
        secret_access_key: ${FRONT_SECRET_001}
      - access_key_id: S3RPKEY002
        secret_access_key: ${FRONT_SECRET_002}
  - name: logs
    backend:
      # no endpoint: Amazon S3, resolved by the SDK from the region
      region: ap-northeast-1
      access_key_id: ${AWS_ACCESS_KEY_ID_FOR_LOGS}
      secret_access_key: ${AWS_SECRET_ACCESS_KEY_FOR_LOGS}
    keys:
      - access_key_id: S3RPKEY001    # the same key id may be used for multiple buckets
        secret_access_key: ${FRONT_SECRET_001}
```

Notes:

- When `backend.endpoint` is omitted, the backend is Amazon S3: the SDK resolves the endpoint from `region`, and `use_path_style` defaults to `false` (it defaults to `true` when an endpoint is set).
- When `backend.access_key_id` and `backend.secret_access_key` are omitted, the SDK default credential chain is used (environment variables, shared config, IAM roles, etc.).
- The same front access key id may appear under multiple buckets, but its secret must be identical everywhere.
- `GET /` (ListBuckets) returns the buckets accessible by the authenticated key.

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
- CreateMultipartUpload
- UploadPart
- UploadPartCopy
- CompleteMultipartUpload
- AbortMultipartUpload
- ListParts
- ListMultipartUploads

Other operations return a `NotImplemented` error.

CopyObject and UploadPartCopy work between buckets served by the same backend (same endpoint, region and credentials); copying across different backends returns `NotImplemented`. The copy source bucket must be accessible by the requesting access key.

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
