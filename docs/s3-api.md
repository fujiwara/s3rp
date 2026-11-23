# The S3 API

The tenant-facing S3 API of the gateway (`s3gw`): which operations exist, how bucket and user policies are evaluated, and how CORS, checksums, encryption, presigned URLs and browser uploads behave. This contract is shared by the bundled `s3rp` binary and any service [built on the gateway](building-a-service.md).

Definition examples (bucket policies, user policies, CORS rules) are shown in the bundled YAML config's syntax; a service embedding the gateway keeps the same definitions in its own [store](building-a-service.md#your-store).

## Supported operations

Because operations are reconstructed rather than forwarded, each one is implemented explicitly. The table below is the surface covered so far — enough to exercise real clients (the AWS CLI and SDKs) end to end against real backends. Anything not listed returns `NotImplemented` (fail closed).

| operation | authorized actions |
|---|---|
| ListBuckets | `s3:ListAllMyBuckets` |
| HeadBucket | `s3:ListBucket` |
| ListObjects | `s3:ListBucket` |
| ListObjectsV2 | `s3:ListBucket` |
| ListObjectVersions | `s3:ListBucketVersions` |
| ListMultipartUploads | `s3:ListBucketMultipartUploads` |
| GetBucketLocation | `s3:GetBucketLocation` |
| GetBucketAcl | `s3:GetBucketAcl` |
| GetBucketPolicy | `s3:GetBucketPolicy` |
| GetBucketPolicyStatus | `s3:GetBucketPolicyStatus` |
| GetBucketCors | `s3:GetBucketCORS` |
| GetBucketLifecycleConfiguration | `s3:GetLifecycleConfiguration` |
| GetBucketReplication | `s3:GetReplicationConfiguration` |
| GetBucketTagging | `s3:GetBucketTagging` |
| GetBucketNotificationConfiguration | `s3:GetBucketNotification` |
| GetBucketLogging | `s3:GetBucketLogging` |
| GetBucketVersioning | `s3:GetBucketVersioning` |
| GetBucketEncryption | `s3:GetEncryptionConfiguration` |
| GetBucketOwnershipControls | `s3:GetBucketOwnershipControls` |
| GetPublicAccessBlock | `s3:GetBucketPublicAccessBlock` |
| GetObjectLockConfiguration | `s3:GetBucketObjectLockConfiguration` |
| DeleteObjects | `s3:DeleteObject` per key (`s3:DeleteObjectVersion` for a version) |
| PostObject | `s3:PutObject` |
| GetObject | `s3:GetObject` (`s3:GetObjectVersion` for a version) |
| HeadObject | `s3:GetObject` (`s3:GetObjectVersion` for a version) |
| GetObjectAttributes | `s3:GetObject` (`s3:GetObjectVersion` for a version) |
| PutObject | `s3:PutObject` |
| CopyObject | `s3:PutObject`, `s3:GetObject` on the copy source (`s3:GetObjectVersion` for a version) |
| DeleteObject | `s3:DeleteObject` (`s3:DeleteObjectVersion` for a version) |
| GetObjectTagging | `s3:GetObjectTagging` (`s3:GetObjectVersionTagging` for a version) |
| PutObjectTagging | `s3:PutObjectTagging` (`s3:PutObjectVersionTagging` for a version) |
| DeleteObjectTagging | `s3:DeleteObjectTagging` (`s3:DeleteObjectVersionTagging` for a version) |
| GetObjectAcl | `s3:GetObjectAcl` (`s3:GetObjectVersionAcl` for a version) |
| GetObjectRetention | `s3:GetObjectRetention` |
| PutObjectRetention | `s3:PutObjectRetention` |
| GetObjectLegalHold | `s3:GetObjectLegalHold` |
| PutObjectLegalHold | `s3:PutObjectLegalHold` |
| CreateMultipartUpload | `s3:PutObject` |
| UploadPart | `s3:PutObject` |
| UploadPartCopy | `s3:PutObject`, `s3:GetObject` on the copy source (`s3:GetObjectVersion` for a version) |
| CompleteMultipartUpload | `s3:PutObject` |
| AbortMultipartUpload | `s3:AbortMultipartUpload` |
| ListParts | `s3:ListMultipartUploadParts` |

The actions are what bucket and user policies match ([Bucket policies](#bucket-policies), [User policies](#user-policies)); some headers add one ([Actions a header adds](#actions-a-header-adds)). ListBuckets has no bucket policy to consult, so `s3:ListAllMyBuckets` is decided by the user policy alone; it lists only the requester's own buckets. PostObject is the browser-based POST upload ([below](#browser-based-uploads-post)).

These operations are recognized and always refused — `NotImplemented`, or `AccessControlListNotSupported` for the ACL writes — and recorded under their name on `Op.Operation` so a service can count attempts: CreateBucket, DeleteBucket, PutBucketAcl, PutBucketPolicy, DeleteBucketPolicy, PutBucketCors, DeleteBucketCors, PutBucketLifecycleConfiguration, DeleteBucketLifecycle, PutBucketReplication, DeleteBucketReplication, PutBucketTagging, DeleteBucketTagging, PutBucketNotificationConfiguration, PutBucketLogging, PutBucketVersioning, PutObjectLockConfiguration, PutBucketEncryption, DeleteBucketEncryption, PutBucketOwnershipControls, DeleteBucketOwnershipControls, PutPublicAccessBlock, DeletePublicAccessBlock, PutObjectAcl. Any other request is `NotImplemented` as well. The S3 operations that fall under that are listed by feature in [Unsupported features](#unsupported-features).

The same catalog is machine-readable: [`s3op/operations.json`](../s3op/operations.json), exposed to Go as package `s3op` (`Operations`, `Lookup`, `Unsupported`, `Actions`, `CheckActionPattern` and generated `Op*` / `Action*` constants), for a control plane validating the policies it stores or a console listing what a policy can grant.

CopyObject and UploadPartCopy work between buckets served by the same backend (same endpoint, region and credentials); copying across different backends returns `NotImplemented`. The copy source bucket must belong to the requester's tenant; the destination may be another tenant's bucket when its policy grants `s3:PutObject` ([cross-tenant access](#cross-tenant-access)).

GetBucketPolicyStatus, GetBucketOwnershipControls and GetPublicAccessBlock answer from the gateway's own model without consulting the backend: anonymous access does not exist, so `IsPublic` is always `false`; ACLs are disabled (see [ACLs](#acls)), so ownership is `BucketOwnerEnforced` and every public-access block is `true`. Tools that inspect a bucket (consoles, Terraform, audit scanners) read these alongside the ACL and treat a `NotImplemented` as a broken bucket. GetBucketEncryption is proxied like GetBucketVersioning — the default encryption is backend bucket configuration, and the key id it reports is the same opaque name SSE-KMS requests carry. Their `Put`/`Delete` counterparts are `NotImplemented` like every bucket-configuration write (see [Limitations](#limitations)).

GetBucketLocation and HeadBucket (the `x-amz-bucket-region` header) report the gateway's own region — the value pinned with `SetRegion`, `us-east-1` when unset — never the backend's region, which stays hidden like the backend bucket name and endpoint.

Wherever a response exposes an `Owner` or `Initiator` — object and version listings, multipart listings, ACLs, ListBuckets — it is the bucket-owning tenant (which differs from the requester's on a [cross-tenant request](#cross-tenant-access)), never the backend account the proxy uses.

A backend the gateway has stopped calling — its circuit breaker is open after repeated failures, when the service enables one (`circuit_breaker` in the bundled config, `SetBreaker` when embedding) — answers `503 ServiceUnavailable` immediately instead of waiting on the backend; clients treat it like any throttling response and retry later.

ListBuckets answers from the store without calling any backend: the bucket names are the front names, and each `CreationDate` is the store's `created_at` for the bucket (the Unix epoch when the store does not track one).

The `versionId` query parameter is passed through on GetObject, HeadObject, DeleteObject, GetObjectAcl and the object tagging operations. GetObject and HeadObject also accept `partNumber` to read a single part of a multipart object, returning `x-amz-mp-parts-count`; both answer `206` with the part's `Content-Range`, as they do for a `Range`. Versioning requires a backend that supports it. The versioning **state** is bucket configuration: PutBucketVersioning is not proxied (see [Limitations](#limitations)), so it is set on the backend bucket by whoever created it; GetBucketVersioning reports it.

ListBuckets supports pagination (`max-buckets` / `continuation-token`) and the `prefix` filter, served from the store's listing; `bucket-region` is not supported. `max-keys`, which is not a ListBuckets parameter but which WinSCP sends when it connects, is accepted and ignored, as on S3.

Conditional requests are forwarded to the backend: `If-Match` / `If-None-Match` / `If-Modified-Since` / `If-Unmodified-Since` on reads, and the write preconditions — `If-Match` / `If-None-Match` on PutObject and CompleteMultipartUpload, `If-Match` plus `x-amz-if-match-last-modified-time` / `x-amz-if-match-size` on DeleteObject, and the per-object `ETag` / `LastModifiedTime` / `Size` members of DeleteObjects. Whether a precondition is enforced is the backend's business; the gateway's job is to never drop one silently.

`aws-chunked` request bodies (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD` and the trailer variants), which the AWS CLI and SDKs use for uploads over plain http endpoints, are decoded and their chunk signatures are verified.

### Object Lock

Object Lock (WORM) is passed through to the backend, which enforces the retention. The per-object retention and legal hold operations are proxied, and the `x-amz-object-lock-*` headers on uploads (PutObject, CopyObject, CreateMultipartUpload) and `x-amz-bypass-governance-retention` on deletes are forwarded — each requiring the corresponding action in addition to the operation's own, as on AWS (see [Actions a header adds](#actions-a-header-adds)). Bucket policies gain the corresponding actions (`s3:GetObjectRetention`, `s3:PutObjectRetention`, `s3:GetObjectLegalHold`, `s3:PutObjectLegalHold`, `s3:BypassGovernanceRetention`, `s3:GetBucketObjectLockConfiguration`). The bucket-level configuration is readable (GetObjectLockConfiguration) but not writable through the gateway: the default retention is bucket configuration, written where the bucket is created (see [Limitations](#limitations)).

Object Lock must be enabled when a bucket is created, and the gateway does not proxy CreateBucket, so the backend bucket must have been created with Object Lock enabled. The exact behavior depends on the backend: Ceph RGW, RustFS and Amazon S3 support it fully, while versitygw enforces retention but does not honor governance-mode bypass.

### Checksums

Which checksum algorithms are offered, and how far, is one setting of the service, the same for every bucket whatever backend serves it (`SetChecksumSupport`, [Checksum algorithms](building-a-service.md#checksum-algorithms)). The default, which the bundled `s3rp` binary uses:

| algorithm | support |
| --- | --- |
| `CRC32` | verified |
| `CRC32C` | verified |
| `CRC64NVME` | verified |
| `SHA1` | verified |
| `SHA256` | verified |
| `SHA512` | refused |
| `MD5` | refused |
| `XXHASH64` | refused |
| `XXHASH3` | refused |
| `XXHASH128` | refused |

- **verified**: the value header and `x-amz-checksum-algorithm` pass to the backend, and an `aws-chunked` trailer is verified by the gateway itself.
- **forwarded**: the value header and `x-amz-checksum-algorithm` pass to the backend, which verifies and stores the checksum. A trailer is refused, since the gateway does not verify it.
- **refused**: the value header, a trailer, `x-amz-checksum-algorithm` and a value in a CompleteMultipartUpload part are all refused with `501 NotImplemented`, and a checksum of the algorithm the backend reports is not shown (responses and listings alike). An algorithm S3 does not define is `400 InvalidRequest`.

Beyond that, checksums flow end-to-end:

- Precomputed checksum headers on uploads pass through to the backend, which validates and stores them.
- Trailing checksums in `aws-chunked` bodies (the SDK default over https) are **verified by the proxy** against the decoded payload (`BadDigest` on mismatch). When the backend is reached over https the algorithm is forwarded so the backend recomputes and stores the checksum; over a plain-http backend it is not (the SDK can only recompute it over an unseekable body as a trailer, which it sends over https only), so the upload is still verified but the backend stores no checksum. A trailer the proxy cannot verify — an algorithm that is not verified (e.g. SHA512, which the SDKs send as a trailer over https), a non-checksum trailer, more than one — is refused with `501 NotImplemented` rather than dropped, and so is a declared trailer on a payload that carries none (`400 InvalidRequest`).
- Downloads pass `x-amz-checksum-mode: ENABLED` through and return the backend's checksum headers, so client SDKs can validate response payloads. Multipart part checksums are carried through UploadPart / CompleteMultipartUpload as well.
- Listings carry what the backend reports: each entry's `ChecksumAlgorithm` / `ChecksumType` in ListObjects (V1/V2), ListObjectVersions and ListMultipartUploads, and ListParts the upload's algorithm and type plus each part's checksum. What is reported varies: versitygw reports all of them, Ceph RGW (tentacle) only the part checksums, RustFS none.
- CopyObject forwards `x-amz-checksum-algorithm`, so the backend computes the destination's checksum with the requested algorithm (how S3 changes an object's checksum algorithm), and CopyObject / UploadPartCopy results carry the checksums the backend reports, so a client sees whether the algorithm was applied. Ceph RGW (tentacle) ignores it and keeps the source's checksum; a service on such a backend can refuse it instead ([Backend clients](building-a-service.md#backend-clients-and-the-gateways-caches)).

Whether a checksum is actually stored and returned depends on the backend (versitygw, RustFS and Amazon S3 do; some Ceph RGW builds do not).

**Limitation — backends reached over plain http store no trailing checksum.** A checksum sent as an `aws-chunked` trailer, which is how the SDKs and the AWS CLI send it on every upload over https, is verified by the gateway whatever the backend's scheme, but only an https backend is asked to store it: the SDK the gateway uses toward the backend can recompute a checksum over the streamed body only as a trailer of its own, which it sends over https only. For every algorithm alike, then, an upload through a gateway whose clients use https and whose backend is http carries no stored checksum: GetObject / HeadObject with `x-amz-checksum-mode` return none, and the PutObject response carries none either — the gateway deliberately does not report the value it verified, since that would present a check of what the gateway received as a checksum of what the backend stored.

The hop between the gateway and an http backend is itself **not integrity-checked**: the body goes out as `UNSIGNED-PAYLOAD` with no checksum and no TLS, so corruption on that hop, or in the backend before the data is written, goes undetected, and with no stored checksum it cannot be detected later either. What the gateway does guarantee on either scheme is that it never completes a body it has not verified: the trailer is checked before the final bytes are handed to the backend, and a mismatch aborts the backend request short of its `Content-Length`, so nothing is stored. A checksum sent precomputed in a header is forwarded to the backend, which verifies and stores it over either scheme, so that form is checked end to end even over http.

A multipart upload is where this is more than a missing checksum. CreateMultipartUpload forwards the checksum algorithm the client names, and the parts' trailing checksums are not forwarded to an http backend, so a backend that holds the upload to its declared algorithm refuses the parts (versitygw: `400 InvalidRequest`). The transfer managers of the AWS SDKs for Python, Go and Java upload this way by default, so through a gateway on an http backend their multipart uploads fail.

**Reach backends over https in production.** Over https, TLS protects the hop and the backend verifies and stores the checksum the gateway's SDK recomputes, so every client's checksum is checked end to end and returned on later reads. Plain http backends are for development and tests.

### Server-side encryption

SSE-S3 (`x-amz-server-side-encryption: AES256`) and SSE-KMS (`aws:kms` + `x-amz-server-side-encryption-aws-kms-key-id`) **pass through**: the backend performs the encryption, and its result headers are returned on uploads and downloads. The KMS key id is opaque to the gateway — it is whatever the backend's KMS resolves (for example Ceph RGW handing it to a Vault-compatible key service), so the key id namespace belongs to the service, not to the backend's infrastructure. The requested mode and key id are exposed as `Op.Request.SSE` / `Op.Request.SSEKMSKeyID` to the [Authorizer](building-a-service.md#hooks-and-metering): whether a tenant may use a key — or whether a bucket's backend supports encryption at all (some ignore the request silently rather than refuse) — is the service's decision; nothing else in the path knows which tenant owns which key, since the backend's KMS request carries no tenant identity. A service can also install a `KMSKeyMapper` that resolves the key names tenants use to the backend KMS's ids on writes and maps them back on every response that reports one (GetBucketEncryption included), so the backend's key namespace never appears in the API ([KMS key ids](building-a-service.md#kms-key-ids)); the bundled `s3rp` binary installs none.

**SSE-C is refused** with `NotImplemented` rather than silently dropped: an ignored customer key would store the object without the encryption the client believes it requested, and later serve it back without the key.

### Storage class

`x-amz-storage-class` on uploads (PutObject, POST upload, CopyObject, CreateMultipartUpload) is forwarded to the backend as is, and the class the backend reports is returned as is (the `x-amz-storage-class` header of GetObject/HeadObject, the `StorageClass` element of the listings, GetObjectAttributes and ListParts) — **unless the service decides otherwise**. A service built on the gateway can install a `StorageClassMapper` that chooses the backend class of every write itself (from the object size, the tenant, the bucket) and decides what class the client is shown, so that the backend's class names never appear in the API ([Hooks and metering](building-a-service.md#hooks-and-metering)); the bundled `s3rp` binary installs none. Whether a tenant may name a class at all is a service decision made on `Op.Request.StorageClass` in the Authorizer. Note that a backend does not verify a class it does not define — Ceph RGW refuses the write with `InvalidArgument`, versitygw stores the object as `STANDARD` — and no S3 write returns the class the object landed in.

Backend notes for Ceph RGW: SSE requests require TLS toward RGW by default (`rgw_crypt_require_ssl`) — terminate or disable it deliberately; the compose file's ceph service configures the built-in `testing` KMS backend with a static key (`testkey-1`) so the integration suite can exercise SSE-KMS without a real KMS.

### Request validation

A few request values are checked by the gateway itself, before the backend sees them, and refused with Amazon S3's own code and message. Backends answer each of these differently — some accept what S3 refuses — and a backend's wording is never relayed, so without the check a client would learn only `Invalid Argument`.

- **Object tags** (`x-amz-tagging`, the POST field, PutObjectTagging): more than 10 tags is `400 BadRequest` "Object tags cannot be greater than 10"; a key longer than 128 or a value longer than 256 characters, or a character outside letters, numbers, spaces and `+ - = . _ : / @`, is `400 InvalidTag`; a key given twice is `400 InvalidTag` in PutObjectTagging and `400 InvalidArgument` inline.
- **`x-amz-tagging-directive`, `x-amz-metadata-directive`**: anything but `COPY` and `REPLACE` is `400 InvalidArgument`.
- **`x-amz-object-annotation-directive`**: `EXCLUDE` is accepted and changes nothing, since objects have no annotations; `COPY` is `501 NotImplemented`.
- **`x-amz-object-attributes`**: a name S3 does not define is `400 InvalidArgument`.
- **`x-amz-optional-object-attributes`** (ListObjects, ListObjectsV2, ListObjectVersions): `RestoreStatus` is forwarded and each entry carries the restore status the backend reports, if any; another value is `501 NotImplemented`.
- **CompleteMultipartUpload**: a part number lower than the one before it is `400 InvalidPartOrder`.

Storage class names are deliberately not checked: which classes exist is the backend's and the service's to define ([Storage class](#storage-class)).

### Bucket policies

A bucket may carry an AWS-style policy document, written as JSON text in the store (in the bundled YAML config, `buckets[].policy`). GetBucketPolicy returns it; PutBucketPolicy / DeleteBucketPolicy are not supported (policies are defined in the store, not via the S3 API).

Two simplifications against AWS: principals are `"tenant/user"` names under the `S3RP` key — always tenant-qualified, the short form of an ARN's account/user pair — and resources are plain `"bucket"` / `"bucket/prefix*"` strings (no ARNs). Action and Resource support the AWS wildcards `*` (any run of characters, including `/`) and `?` (exactly one character). There is no escaping: a `*` or `?` in a pattern is always a wildcard, so it also matches a key containing that character. As in AWS, `Action` matching is case-insensitive (so a mis-cased `Deny` cannot silently fail open), while `Resource` matching is case-sensitive since object keys are. An action is ASCII — a pattern with any other character is rejected when the policy is loaded — and only `A`–`Z` and `a`–`z` are the same letters, so no character outside ASCII can stand for one in an action name. Every `Resource` entry must refer to the bucket the policy is attached to — the bucket name itself or `bucket/...` — anything else (most likely a typo) could never match a request and is rejected when the policy is loaded, the same mistake AWS refuses at PutBucketPolicy time ("Policy has invalid resource"). The grammar of the document is [policy.schema.json](policy.schema.json), a JSON Schema an editor or a pipeline can validate a policy against before it is loaded; what a schema cannot say — the resources' bucket, a `Sid` given twice, whether a condition value is an address, the sizes of a pattern and of the document in bytes — is only checked at load. A document is read strictly: an element that is not one of `Version`, `Id` and `Statement`, or in a statement `Sid`, `Effect`, `Principal`, `NotPrincipal`, `Action`, `Resource` and `Condition`, is rejected when the policy is loaded — a misspelled `Condition`, or a `NotAction` / `NotResource`, which are not supported — and so are an element name in another case (`effect`), an element given twice, and one that is `null`. None of these is ignored, since the policy would then mean something other than what it says. `Id` is accepted and has no effect. `Sid` is optional but must be unique within a policy when given (as in IAM): it is how the operator's log names the statement that refused a request, so setting one on every statement makes a denial explainable — the client itself is only ever told `AccessDenied`.

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
            "Principal": {"S3RP": ["acme/batch"]},
            "Action": ["s3:PutObject", "s3:DeleteObject"],
            "Resource": ["photos/*"]
          }
        ]
      }
```

Evaluation model: every user of a tenant has full access to the tenant's own buckets by default, and explicit `Deny` statements restrict it. For the bucket's own users, `Allow` statements have no effect (everything is already allowed); they are what grants cross-tenant access (below).

Principal forms:

- `{"S3RP": ["tenant/user", ...]}` — the listed users, always tenant-qualified (the bucket's own users included).
- `{"S3RP": ["tenant/*", ...]}` — every user of the named tenant, including ones added later.
- `"*"` — **every authenticated user of any tenant**. There is no anonymous access, so this means "anyone with valid credentials", not "public". An `Allow` with `"*"` opens the bucket to all tenants — write `"mytenant/*"` when you mean your own users.
- `NotPrincipal` (exclusive with `Principal`, `Deny` only) — everyone except the listed names. `Deny` + `NotPrincipal` expresses "only these users may ..." so that newly added users — and every other tenant's users — are denied by default. `Allow` + `NotPrincipal` would be a grant to everyone-but, which never crosses anyone's mind on purpose; it is rejected at load time.

#### IP address conditions

A statement may carry a `Condition` restricting it to requests from certain source addresses, with the same spelling as AWS (and Ceph RGW, MinIO — a policy written for them works unchanged):

```json
{
  "Sid": "WritesOnlyFromTheOffice",
  "Effect": "Deny",
  "Principal": "*",
  "Action": ["s3:PutObject", "s3:DeleteObject"],
  "Resource": ["photos/*"],
  "Condition": {"NotIpAddress": {"aws:SourceIp": ["203.0.113.0/24", "2001:db8::/32"]}}
}
```

- The supported operators are `IpAddress` and `NotIpAddress`, and the only key is `aws:SourceIp` (key names are case-insensitive, as on AWS), given once per operator. Anything else is rejected at load time rather than ignored — a restriction the author intended is never silently dropped.
- Values are CIDR prefixes or plain addresses (a plain address is a `/32` or `/128`). Values within one operator are ORed; the operators of one `Condition` are ANDed. IPv4 and IPv6 are distinct families — an IPv4 prefix never matches an IPv6 source (IPv4-mapped IPv6 counts as IPv4, in a source and in a value alike: `::ffff:10.0.0.0/104` is `10.0.0.0/8`).
- The source address is the connection's `RemoteAddr`. When the request's source is unknown, conditions fail **closed**: `IpAddress` does not match (an `Allow` gated on it grants nothing) and `NotIpAddress` matches (a `Deny` gated on it applies). Behind a reverse proxy, `RemoteAddr` is the proxy's address — see [Behind a TLS terminator](building-a-service.md#behind-a-tls-terminator).
- A source address is weak evidence: it can be spoofed or mangled by a misconfigured proxy chain. Use IP conditions to *restrict* (`Deny` + `NotIpAddress`, as above) as defense in depth, not as the sole basis for widening access.

#### Cross-tenant access

A bucket policy may grant access to another tenant's users — one by name, a whole tenant, or every authenticated user:

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

The baseline for a foreign requester is the opposite of the own-tenant one: **deny unless an `Allow` matches**, and `Deny` still wins over `Allow`. Principal matching itself does not depend on who asks — the same statement forms (`tenant/user`, `tenant/*`, `"*"`) match the same principals under either baseline, so a blanket `Deny` also catches cross-tenant principals granted elsewhere in the policy.

A bucket whose policy grants the requester nothing answers with the same `403 AccessDenied` a nonexistent bucket produces, so bucket names cannot be probed across tenants (a bucket with an `Allow` for `"*"` is consequently visible to every authenticated user). Responses expose the bucket-owning tenant as `Owner`, never the requester's. ListBuckets lists only the tenant's own buckets, as on AWS.

Copying across tenants works in one direction only:

- **Into** another tenant's bucket — supported: the destination goes through the normal authorization path, so an `s3:PutObject` grant (plus the same-backend restriction) is all it takes; the source read is authorized within your own tenant as usual.
- **From** another tenant's bucket — not supported, even with an `s3:GetObject` grant: the `x-amz-copy-source` bucket always resolves within the requester's own tenant. A server-side copy never streams through the proxy, so the source owner's request hooks would see nothing of the read; fetching with GetObject (which the grant does allow) and re-uploading achieves the same result with both sides authorized and observable.

A request naming an object version — a `versionId` query parameter, a `versionId` in `x-amz-copy-source`, a `VersionId` on a DeleteObjects entry — is authorized for the version action **instead of** the plain one, as on AWS (the operations table lists each pair): a `Deny` on `s3:DeleteObject` does not stop `DeleteObject` with a `versionId`, so a statement meant to protect objects should name both (`s3:DeleteObject`, `s3:DeleteObjectVersion`) or a wildcard such as `s3:DeleteObject*`. Object Lock actions (`s3:GetObjectRetention`, `s3:PutObjectLegalHold`, ...) are the same with or without a version, also as on AWS. DeleteObjects is evaluated per object: denied keys are reported in the `Error` entries of the response. Copying evaluates `s3:GetObject` (`s3:GetObjectVersion` for a source version) on the source and `s3:PutObject` on the destination.

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
            action: [s3:Get*, s3:List*]   # HeadObject is s3:GetObject, HeadBucket s3:ListBucket
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

#### Actions a header adds

Some request headers require an action beyond the operation's own, exactly as on Amazon S3, so a policy can refuse what the header does without the gateway inspecting values:

| header | on | additional action |
|---|---|---|
| `x-amz-tagging` | PutObject, CopyObject, CreateMultipartUpload, POST upload | `s3:PutObjectTagging` |
| `x-amz-object-lock-mode`, `x-amz-object-lock-retain-until-date` | PutObject, CopyObject, CreateMultipartUpload | `s3:PutObjectRetention` |
| `x-amz-object-lock-legal-hold` | PutObject, CopyObject, CreateMultipartUpload | `s3:PutObjectLegalHold` |
| `x-amz-bypass-governance-retention: true` | DeleteObject, DeleteObjects, PutObjectRetention | `s3:BypassGovernanceRetention` |

A user policy of `Allow [s3:*]` + `Deny [s3:PutObjectTagging]` therefore refuses tags everywhere they can be written — the dedicated PutObjectTagging operation and an upload carrying `x-amz-tagging` alike — and a bucket policy `Deny` on `s3:PutObjectRetention` keeps a tenant from locking objects on upload. A copy also needs `s3:GetObject` on its source.

#### Unknown actions

A policy action that matches none of the actions in the [operations table](#supported-operations) — a typo, an AWS action for an operation s3rp does not serve through the S3 API such as `s3:PutBucketPolicy`, or one it does not distinguish such as `s3:GetObjectAttributes` — can never match a request, so its statement silently does nothing (a `Deny` on it protects nothing). The bundled binary logs a warning for each one at startup; with `strict_actions: true` in the config it refuses to start instead. A service with its own store checks the actions it stores by passing `s3op.CheckActionPattern` to `policy.Parse` / `policy.ValidateUserPolicy`; the errors wrap `s3op.ErrUnknownAction`.

Both bucket and user policies are bounded in size: at most 20 KB per document, 20 statements per policy, 30 actions and 10 resources per statement, 128 bytes per action/resource pattern, 100 principal users per statement, and 50 condition values per operator. Oversized policies are rejected when loaded.

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

### Lifecycle configuration

GetBucketLifecycleConfiguration returns the lifecycle configuration recorded in the bucket's definition, as it is (`NoSuchLifecycleConfiguration` when there is none). It is answered from the store, like the bucket policy; the backend is not asked. `withUpdatedAt`, which minio-go (and so `mc ilm rule ls`) adds to the request to ask MinIO for the time of the last change, is accepted and ignored: the time is not reported.

```yaml
buckets:
  - name: logs
    backend: { ... }
    lifecycle: |
      <LifecycleConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
        <Rule>
          <ID>expire-logs</ID>
          <Filter><Prefix>logs/</Prefix></Filter>
          <Status>Enabled</Status>
          <Expiration><Days>30</Days></Expiration>
        </Rule>
      </LifecycleConfiguration>
```

What is recorded is what a tenant is shown, and the gateway neither reads it nor applies it: expiration is the backend's work, on the rules the control plane wrote to the backend bucket. In a service the control plane records the tenant's own rules here when it writes them there; in the bundled config it is a declaration the operator keeps in step by hand. PutBucketLifecycleConfiguration and DeleteBucketLifecycle are refused ([Limitations](#limitations)), and `x-amz-expiration` is not returned on object responses.

### Replication configuration

GetBucketReplication returns the replication configuration recorded in the bucket's definition, as it is (`ReplicationConfigurationNotFoundError` when there is none), from the store and without asking the backend, like the [lifecycle configuration](#lifecycle-configuration).

```yaml
buckets:
  - name: photos
    backend: { ... }
    replication: |
      <ReplicationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
        <Role>replication</Role>
        <Rule>
          <ID>to-backup</ID>
          <Priority>1</Priority>
          <Status>Enabled</Status>
          <Filter><Prefix></Prefix></Filter>
          <DeleteMarkerReplication><Status>Disabled</Status></DeleteMarkerReplication>
          <Destination><Bucket>arn:aws:s3:::photos-backup</Bucket></Destination>
        </Rule>
      </ReplicationConfiguration>
```

The gateway replicates nothing and does not read the document: replication is something a service provides through its backend, configured by its control plane, which records here what the tenant is to be shown. Whatever the document names is shown as written, so a destination is a front bucket — S3's schema writes it as `arn:aws:s3:::bucket`, which is what clients expect to read — and never a backend's. PutBucketReplication and DeleteBucketReplication are refused like every bucket-configuration write, and `x-amz-replication-status` is not returned on object responses.

### Bucket tags

GetBucketTagging returns the tags recorded in the bucket's definition (`NoSuchTagSet` when there are none), sorted by key. They are answered from the store; a backend bucket's own tags are not shown.

```yaml
buckets:
  - name: photos
    backend: { ... }
    tags:
      env: prod
      team: storage
```

The gateway shows the tags and uses them for nothing: no policy reads them, and none is sent to the backend. S3's limits apply where they are written (50 tags; a key of up to 128 characters that does not start with `aws:`; a value of up to 256; letters, numbers, spaces and `+ - = . _ : / @` only). PutBucketTagging and DeleteBucketTagging are refused like every bucket-configuration write.

### Notification configuration

GetBucketNotificationConfiguration returns the notification configuration recorded in the bucket's definition, as it is, from the store and without asking the backend. A bucket without one answers with an empty configuration, not an error, as on S3.

```yaml
buckets:
  - name: photos
    backend: { ... }
    notification: |
      <NotificationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
        <TopicConfiguration>
          <Id>on-create</Id>
          <Topic>arn:aws:sns:us-east-1:000000000000:photo-events</Topic>
          <Event>s3:ObjectCreated:*</Event>
        </TopicConfiguration>
      </NotificationConfiguration>
```

The gateway emits no event and does not read the document. A service that notifies — from the gateway's hooks, or through its backend — configures that in its control plane and records here what the tenant is to be shown; a destination is shown as written, so it is the service's name for it and never a backend's. PutBucketNotificationConfiguration is refused like every bucket-configuration write.

### Server access logging

GetBucketLogging returns the logging status recorded in the bucket's definition, as it is, from the store and without asking the backend. A bucket without one answers with an empty status — logging off — not an error, as on S3.

```yaml
buckets:
  - name: photos
    backend: { ... }
    logging: |
      <BucketLoggingStatus xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
        <LoggingEnabled>
          <TargetBucket>audit</TargetBucket>
          <TargetPrefix>access/photos/</TargetPrefix>
        </LoggingEnabled>
      </BucketLoggingStatus>
```

The gateway delivers no log and does not read the document. It reports every request to the service's observer; a service that turns those into access logs in a tenant's bucket configures that in its control plane and records here what the tenant is to be shown, the target being a front bucket. PutBucketLogging is refused like every bucket-configuration write.

### ACLs

The gateway behaves like a bucket with ACLs disabled (Object Ownership = bucket owner enforced, the AWS default since 2023). GetBucketAcl / GetObjectAcl return a fixed policy granting FULL_CONTROL to the tenant; PutBucketAcl / PutObjectAcl return `AccessControlListNotSupported`, and canned ACLs other than `private` / `bucket-owner-full-control` — and any explicit `x-amz-grant-*` header — are rejected on uploads. Use bucket policies for access control instead.

## Addressing

Buckets are addressed path-style (`/bucket/key`) and, when the gateway is given its host name (`virtual_host_suffix` in the bundled config, `SetVirtualHostSuffix` when embedding), virtual-hosted-style as well: a request whose `Host` is `bucket.s3.example.com` targets that bucket and its whole path is the key. Both forms are served at once, as on Amazon S3; a `Host` that is not a direct subdomain of the suffix (the bare endpoint, another name, a dotted label) is path-style. Bucket names contain no dot, so a bucket is always exactly one DNS label — that is why the name charset excludes it. The `Location` a multipart completion or a POST upload returns mirrors the form the client used. Signature verification is unaffected either way: the signed `Host` is whatever the client sent.

## Presigned URLs

Presigned URLs (SigV4 query string authentication) generated with front-side keys against the gateway's endpoint are supported for the operations above. Expiry (`X-Amz-Expires`, up to 7 days) is enforced.

Every `x-amz-*` request header must be covered by the signature, exactly as Amazon S3 requires. A request carrying an `x-amz-*` header outside `SignedHeaders` (for either header or presigned authentication) is refused with `403 AccessDenied`: the signature does not commit to such a header, so honoring it would let a presigned-URL holder attach storage class, object-lock retention, an SSE mode and key, a copy source, tagging or metadata the URL grantor never signed. Standard headers left unsigned (for example `Content-Type` on a presigned PUT) are unaffected, as on AWS.

```console
$ aws --endpoint-url http://localhost:8080 s3 presign s3://photos/foo.jpg
http://localhost:8080/photos/foo.jpg?X-Amz-Algorithm=AWS4-HMAC-SHA256&...
```

## Browser-based uploads (POST)

`POST /bucket` with a `multipart/form-data` body and a SigV4 POST policy ("Browser-Based Uploads Using POST" in the S3 API reference) is supported: a backend service signs a policy document with a front-side key, and the browser uploads directly to the gateway with an HTML form — no credentials in the page. Combined with [CORS](#cors), this is the standard browser-direct upload flow, and forms generated by the AWS SDK presigned-POST helpers work as they are.

- Conditions: exact match (`{"field": "value"}` / `["eq", "$field", "value"]`), `["starts-with", "$field", "prefix"]` and `["content-length-range", min, max]` (enforced while the file streams; an upload that ends up under the minimum is deleted from the backend and refused). Field names are case-insensitive; `${filename}` in the `key` field is substituted before conditions are evaluated, and the target bucket is checked as the `bucket` condition.
- As on AWS, every form field must be covered by a condition (except `policy`, `x-amz-signature`, `x-ignore-*` and the file itself), the policy `expiration` is enforced, and form fields after the `file` part are ignored. The policy document is read strictly: a member other than `expiration` and `conditions`, one given twice, or a field given twice in one condition object is `InvalidPolicyDocument`. Beyond AWS, which documents no such bound, a form signed (`x-amz-date`) more than seven days ago is refused whatever its `expiration` — the same bound as a presigned URL, so that a policy's lifetime is not entirely up to whoever signs it.
- Supported fields: `key`, `Content-Type`, `Content-MD5`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Content-Language`, `Expires`, `x-amz-storage-class`, `x-amz-tagging`, `x-amz-meta-*`, `x-amz-server-side-encryption` (+ `-aws-kms-key-id`), `success_action_status` (200/201/204) and `success_action_redirect` (303 with `bucket`/`key`/`etag` appended). `acl` follows the same rule as everywhere else (ACLs are disabled); unsupported fields are refused with `NotImplemented` rather than silently dropped.

## Unsupported features

The parts of the S3 API the gateway does not implement, with the operations each consists of. None of them has a route of its own. A request for one carries a query parameter or header the gateway does not know and is answered `501 NotImplemented` naming it, like any request it does not recognize; unlike the refused operations above, it is not recorded under its name. A request that matches an existing route is handled by that route: ListDirectoryBuckets is a `GET /` on the wire, which is ListBuckets, and is answered as one. Together with the operations above, the list accounts for every operation of the S3 API as the AWS SDK for Go defines it: a test fails when an SDK update brings one that is in neither.

They are grouped by what they are, which is also what supporting one would take. Why a feature is absent is given only where it is the gateway's own reason: what Amazon or a backend does with a feature today is not recorded here.

### Object operations

Operations on objects. A backend that implements one would be passed the request, as for the operations supported now.

| feature | operations | |
|---|---|---|
| Object annotations | GetObjectAnnotation, PutObjectAnnotation, DeleteObjectAnnotation, ListObjectAnnotations | CopyObject accepts x-amz-object-annotation-directive: EXCLUDE and nothing else |
| Restoring archived objects | RestoreObject | A listing relays the restore status the backend reports (x-amz-optional-object-attributes: RestoreStatus) |
| Changing an object's encryption in place | UpdateObjectEncryption |  |
| S3 Select | SelectObjectContent |  |
| BitTorrent | GetObjectTorrent |  |

### Bucket configuration

Configuration of a bucket, which the control plane writes and a data-plane key must not ([Limitations](#limitations)). A read would be answered from the store's definition of the bucket, and the function itself is the service's to provide.

| feature | operations | |
|---|---|---|
| Analytics, inventory, metrics and Intelligent-Tiering configurations | GetBucketAnalyticsConfiguration, PutBucketAnalyticsConfiguration, DeleteBucketAnalyticsConfiguration, ListBucketAnalyticsConfigurations, GetBucketInventoryConfiguration, PutBucketInventoryConfiguration, DeleteBucketInventoryConfiguration, ListBucketInventoryConfigurations, GetBucketMetricsConfiguration, PutBucketMetricsConfiguration, DeleteBucketMetricsConfiguration, ListBucketMetricsConfigurations, GetBucketIntelligentTieringConfiguration, PutBucketIntelligentTieringConfiguration, DeleteBucketIntelligentTieringConfiguration, ListBucketIntelligentTieringConfigurations |  |
| Attribute-based access control | GetBucketAbac, PutBucketAbac | authorization is the gateway's own policy evaluation |

### Outside the gateway's model

Another bucket type, anonymous access, or a function of Amazon's own infrastructure.

| feature | operations | |
|---|---|---|
| Static website hosting | GetBucketWebsite, PutBucketWebsite, DeleteBucketWebsite | needs anonymous access, which does not exist |
| S3 Metadata tables | CreateBucketMetadataConfiguration, GetBucketMetadataConfiguration, DeleteBucketMetadataConfiguration, CreateBucketMetadataTableConfiguration, GetBucketMetadataTableConfiguration, DeleteBucketMetadataTableConfiguration, UpdateBucketMetadataAnnotationTableConfiguration, UpdateBucketMetadataInventoryTableConfiguration, UpdateBucketMetadataJournalTableConfiguration | tables Amazon maintains about a bucket's objects |
| Transfer Acceleration | GetBucketAccelerateConfiguration, PutBucketAccelerateConfiguration | a function of Amazon's network, not of the API |
| Requester Pays | GetBucketRequestPayment, PutBucketRequestPayment | the tenant is billed by the service |
| Directory buckets | CreateSession, ListDirectoryBuckets, RenameObject | another bucket type, with session-based authentication of its own |
| S3 Object Lambda | WriteGetObjectResponse | transforms responses with AWS Lambda functions |

The list is in the same catalog (`unsupported` in [`s3op/operations.json`](../s3op/operations.json), `s3op.Unsupported` in Go). Request headers the gateway does not support are in [Request headers](request-headers.md#not-supported).

## Limitations

- An `x-amz-*` request header that no supported operation reads is refused with `501 NotImplemented` naming the header, never ignored — the same rule as for an unknown query subresource and an unknown POST form field. Ignoring a header the client signed would apply less than the client believes: `x-amz-server-side-encryption-context`, `x-amz-website-redirect-location`, `x-amz-request-payer`, `x-amz-expected-bucket-owner` are the ones real SDK options produce. Headers an operation reads are accepted on every operation (a `x-amz-tagging` on a GET is ignored, as on AWS); `x-amz-meta-*` is always accepted. The full header-level view — supported, refused by name, not supported — is [Request headers](request-headers.md).
- Requests whose query string is not canonically percent-encoded (raw reserved characters in values, signed as sent) fail verification — as they do against AWS itself and every implementation measured; duplicate query keys are accepted in exactly one value ordering. Reachable only from hand-rolled SigV4 clients; see [SigV4 query canonicalization](sigv4-canonicalization.md).
- Bucket lifecycle configuration (expiration, transitions) cannot be **written** via the S3 API: PutBucketLifecycleConfiguration and DeleteBucketLifecycle are `NotImplemented`. Enforcement is the backend's job, so rules must live on the backend bucket, and they are written there by the control plane with backend credentials — the same split as bucket policies and CORS, whose `Put*` are also not proxied. The read is served from the bucket's definition ([Lifecycle configuration](#lifecycle-configuration)). This is a deliberate choice beyond consistency: a bucket holds exactly **one** lifecycle configuration, so letting tenants `PutBucketLifecycleConfiguration` would let one request replace the operator's baseline rules (such as aborting stale multipart uploads); exposing it would require a merge layer, not a pass-through. For experiments, Ceph RGW's `rgw_lc_debug_interval` shortens expiry to seconds.
- The same rule covers every bucket-configuration write: PutBucketVersioning, PutObjectLockConfiguration, PutBucketEncryption, PutBucketOwnershipControls and PutPublicAccessBlock (and their `Delete`s) are also `NotImplemented`. Bucket configuration is written where the bucket is created — the control plane — and a data-plane access key must not be able to overwrite it: suspending versioning changes what the bucket retains from then on, and rewriting the Object Lock default retention would let any key of the tenant undo what the bucket was provisioned with. The reads (GetBucketVersioning, GetObjectLockConfiguration, GetBucketEncryption) stay proxied.
