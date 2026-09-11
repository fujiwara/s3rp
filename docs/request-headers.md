# Request headers

Which request headers the gateway honors, which it refuses by name, and what happens to the rest. The operations themselves are listed in [The S3 API](s3-api.md); this page is the header-level view of the same surface.

## The rules

- **Every `x-amz-*` header must be signed.** One outside `SignedHeaders` is refused with `403 AccessDenied`, as on Amazon S3 — the signature does not commit to it, so honoring it would let a presigned-URL holder attach what the grantor never signed.
- **An `x-amz-*` header no operation reads is refused with `501 NotImplemented`**, naming the header, never ignored. Ignoring a signed header would apply less than the client believes (an encryption context, a redirect, a grant). The same rule already covers unknown query subresources and unknown POST form fields.
- **The list of known headers is global.** A header some operation reads is accepted on every operation and ignored where it has no meaning (`x-amz-tagging` on a GET), as on AWS. `x-amz-meta-*` is always accepted.
- **Standard headers are not gated.** S3 requires only `x-amz-*` to be signed, and presigners leave `Content-Type` off a presigned PUT, so a standard header is honored signed or not — which is why a standard header only ever affects the object's own attributes or the shape of a read, never authorization.

## Supported `x-amz-*` headers

| Header | Operations | Notes |
|---|---|---|
| `x-amz-date`, `x-amz-content-sha256`, `x-amz-security-token`, `x-amz-decoded-content-length`, `x-amz-trailer` | all | consumed by SigV4 verification and `aws-chunked` decoding |
| `x-amz-user-agent` | all | sent by the browser SDKs; accepted, unused |
| `x-amz-meta-*` | PutObject, CopyObject, CreateMultipartUpload, POST upload | user metadata, stored and returned on reads |
| `x-amz-storage-class` | PutObject, CopyObject, CreateMultipartUpload, POST upload | forwarded, unless a service installs a `StorageClassMapper` ([Storage class](s3-api.md#storage-class)) |
| `x-amz-tagging` | PutObject, CopyObject, CreateMultipartUpload, POST upload | requires `s3:PutObjectTagging` in addition to the operation's action |
| `x-amz-tagging-directive`, `x-amz-metadata-directive` | CopyObject | |
| `x-amz-copy-source` | PutObject → CopyObject, UploadPart → UploadPartCopy | source resolved within the requester's tenant; same backend only |
| `x-amz-copy-source-if-match`, `-if-none-match`, `-if-modified-since`, `-if-unmodified-since` | CopyObject | |
| `x-amz-copy-source-range` | UploadPartCopy | |
| `x-amz-server-side-encryption`, `x-amz-server-side-encryption-aws-kms-key-id` | PutObject, CopyObject, CreateMultipartUpload, POST upload | `AES256` or `aws:kms`; the key id is forwarded as an opaque name, unless a service installs a `KMSKeyMapper` ([Server-side encryption](s3-api.md#server-side-encryption)) |
| `x-amz-object-lock-mode`, `x-amz-object-lock-retain-until-date` | PutObject, CopyObject, CreateMultipartUpload | require `s3:PutObjectRetention` |
| `x-amz-object-lock-legal-hold` | PutObject, CopyObject, CreateMultipartUpload | requires `s3:PutObjectLegalHold` |
| `x-amz-bypass-governance-retention` | DeleteObject, DeleteObjects, PutObjectRetention | requires `s3:BypassGovernanceRetention` |
| `x-amz-checksum-crc32`, `-crc32c`, `-crc64nvme`, `-sha1`, `-sha256`, `x-amz-sdk-checksum-algorithm` | PutObject, UploadPart, CompleteMultipartUpload | precomputed checksums, forwarded ([Checksums](s3-api.md#checksums)) |
| `x-amz-checksum-algorithm` | CreateMultipartUpload | |
| `x-amz-checksum-type` | CreateMultipartUpload, CompleteMultipartUpload | |
| `x-amz-checksum-mode` | GetObject, HeadObject | `ENABLED` returns the stored checksum headers |
| `x-amz-mp-object-size` | CompleteMultipartUpload | |
| `x-amz-if-match-size`, `x-amz-if-match-last-modified-time` | DeleteObject | conditional delete, with `If-Match` |
| `x-amz-object-attributes`, `x-amz-max-parts`, `x-amz-part-number-marker` | GetObjectAttributes | |
| `x-amz-acl` | PutObject, CreateMultipartUpload, POST upload (`acl` field) | only `private` and `bucket-owner-full-control` are accepted — see below |

## Refused by name

These are recognized precisely so they can be refused with a specific answer instead of the generic 501:

| Header | Answer | Why |
|---|---|---|
| `x-amz-server-side-encryption-customer-algorithm`, `x-amz-copy-source-server-side-encryption-customer-algorithm` (SSE-C) | `501 NotImplemented` | dropping the customer key would store the object unprotected while the client believes otherwise. The `-customer-key` / `-customer-key-md5` headers on their own meet the generic 501 |
| `x-amz-acl` other than `private` / `bucket-owner-full-control` | `400 AccessControlListNotSupported` | ACLs are disabled ([ACLs](s3-api.md#acls)) |
| `x-amz-grant-read`, `-write`, `-read-acp`, `-write-acp`, `-full-control` | `400 AccessControlListNotSupported` | an explicit grant is an ACL write whatever the syntax |

## Not supported

Every other `x-amz-*` header is `501 NotImplemented`. The ones real SDK options produce:

| Header | SDK option | Why not |
|---|---|---|
| `x-amz-server-side-encryption-context`, `x-amz-server-side-encryption-bucket-key-enabled` | `SSEKMSEncryptionContext`, `BucketKeyEnabled` | KMS is the backend's; the gateway forwards a key id only |
| `x-amz-website-redirect-location` | `WebsiteRedirectLocation` | static website hosting is not part of the surface |
| `x-amz-request-payer` | `RequestPayer` | no requester-pays model; the tenant is billed by the service |
| `x-amz-expected-bucket-owner`, `x-amz-source-expected-bucket-owner` | `ExpectedBucketOwner`, `ExpectedSourceBucketOwner` | bucket ownership is the tenant's; there is no account id to compare |
| `x-amz-write-offset-bytes` | `WriteOffsetBytes` | append writes (S3 Express) are not supported |
| `x-amz-optional-object-attributes` | `OptionalObjectAttributes` | restore status in listings; RestoreObject is not implemented |
| `x-amz-object-ownership`, `x-amz-bucket-object-lock-enabled` | CreateBucket options | CreateBucket is not proxied; bucket configuration is written by the control plane |

## Standard headers

| Header | Operations | Notes |
|---|---|---|
| `Host` | all | bucket addressing (path-style, or virtual-hosted-style under the configured suffix) |
| `Content-Length`, `Content-MD5` | PutObject, UploadPart, POST upload | length required (`411 MissingContentLength` otherwise); an invalid `Content-MD5` is `400 InvalidDigest` |
| `Content-Type`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Content-Language`, `Expires` | PutObject, CreateMultipartUpload, POST upload; `Content-Type` also CopyObject | stored as the object's attributes; `aws-chunked` is stripped from `Content-Encoding` |
| `If-Match`, `If-None-Match`, `If-Modified-Since`, `If-Unmodified-Since` | GetObject, HeadObject | conditional reads, forwarded |
| `If-Match`, `If-None-Match` | PutObject, CompleteMultipartUpload; `If-Match` on DeleteObject | conditional writes, forwarded; enforcement is the backend's |
| `Range` | GetObject, HeadObject | forwarded |
| `Origin`, `Access-Control-Request-*` | all, and unauthenticated `OPTIONS` | CORS ([CORS](s3-api.md#cors)) |
| `Expect: 100-continue` | uploads | handled by the Go HTTP server |
| `User-Agent` and the other headers the AWS SDK signer ignores | all | must **not** be signed, or verification fails; real SDKs and CLIs do not sign them |

Unknown standard headers are ignored, as they are by any HTTP server.

## Response headers not relayed

What the backend answers is reconstructed, not forwarded, so a few backend-side response headers never reach the client: `x-amz-expiration` (would name the operator's lifecycle rules), `x-amz-restore`, `x-amz-replication-status`, and the backend's own `x-amz-request-id` / `x-amz-id-2` (the client gets the gateway's request id, which is what the observer logs). `x-amz-storage-class` and the SSE headers are relayed, subject to the mapping a service installs (`StorageClassMapper`, `KMSKeyMapper`).
