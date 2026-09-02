# The S3 API

The tenant-facing S3 API of the gateway (`s3gw`): which operations exist, how bucket and user policies are evaluated, and how CORS, checksums, encryption, presigned URLs and browser uploads behave. This contract is shared by the bundled `s3rp` binary and any service [built on the gateway](building-a-service.md).

Definition examples (bucket policies, user policies, CORS rules) are shown in the bundled YAML config's syntax; a service embedding the gateway keeps the same definitions in its own [store](building-a-service.md#your-store).

## Supported operations

Because operations are reconstructed rather than forwarded, each one is implemented explicitly. The list below is the surface covered so far — enough to exercise real clients (the AWS CLI and SDKs) end to end against real backends. Anything not listed returns `NotImplemented` (fail closed).

- GetObject
- PutObject
- HeadObject
- GetObjectAttributes
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
- GetBucketEncryption
- GetBucketPolicyStatus
- GetBucketOwnershipControls
- GetPublicAccessBlock
- ListObjectVersions
- GetBucketAcl
- GetObjectAcl
- GetBucketPolicy
- GetBucketCors
- GetObjectLockConfiguration
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

CopyObject and UploadPartCopy work between buckets served by the same backend (same endpoint, region and credentials); copying across different backends returns `NotImplemented`. The copy source bucket must belong to the requester's tenant; the destination may be another tenant's bucket when its policy grants `s3:PutObject` ([cross-tenant access](#cross-tenant-access)).

GetBucketPolicyStatus, GetBucketOwnershipControls and GetPublicAccessBlock answer from the gateway's own model without consulting the backend: anonymous access does not exist, so `IsPublic` is always `false`; ACLs are disabled (see [ACLs](#acls)), so ownership is `BucketOwnerEnforced` and every public-access block is `true`. Tools that inspect a bucket (consoles, Terraform, audit scanners) read these alongside the ACL and treat a `NotImplemented` as a broken bucket. GetBucketEncryption is proxied like GetBucketVersioning — the default encryption is backend bucket configuration, and the key id it reports is the same opaque name SSE-KMS requests carry. Their `Put`/`Delete` counterparts are `NotImplemented` like every bucket-configuration write (see [Limitations](#limitations)).

GetBucketLocation and HeadBucket (the `x-amz-bucket-region` header) report the gateway's own region — the value pinned with `SetRegion`, `us-east-1` when unset — never the backend's region, which stays hidden like the backend bucket name and endpoint.

Wherever a response exposes an `Owner` or `Initiator` — object and version listings, multipart listings, ACLs, ListBuckets — it is the bucket-owning tenant (which differs from the requester's on a [cross-tenant request](#cross-tenant-access)), never the backend account the proxy uses.

A backend the gateway has stopped calling — its circuit breaker is open after repeated failures, when the service enables one (`circuit_breaker` in the bundled config, `SetBreaker` when embedding) — answers `503 ServiceUnavailable` immediately instead of waiting on the backend; clients treat it like any throttling response and retry later.

ListBuckets answers from the store without calling any backend: the bucket names are the front names, and each `CreationDate` is the store's `created_at` for the bucket (the Unix epoch when the store does not track one).

The `versionId` query parameter is passed through on GetObject, HeadObject, DeleteObject, GetObjectAcl and the object tagging operations. GetObject and HeadObject also accept `partNumber` to read a single part of a multipart object, returning `x-amz-mp-parts-count`. Versioning requires a backend that supports it. The versioning **state** is bucket configuration: PutBucketVersioning is not proxied (see [Limitations](#limitations)), so it is set on the backend bucket by whoever created it; GetBucketVersioning reports it.

ListBuckets supports pagination (`max-buckets` / `continuation-token`), served from the store's listing.

Conditional requests are forwarded to the backend: `If-Match` / `If-None-Match` / `If-Modified-Since` / `If-Unmodified-Since` on reads, and the write preconditions — `If-Match` / `If-None-Match` on PutObject and CompleteMultipartUpload, `If-Match` plus `x-amz-if-match-last-modified-time` / `x-amz-if-match-size` on DeleteObject, and the per-object `ETag` / `LastModifiedTime` / `Size` members of DeleteObjects. Whether a precondition is enforced is the backend's business; the gateway's job is to never drop one silently.

`aws-chunked` request bodies (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD` and the trailer variants), which the AWS CLI and SDKs use for uploads over plain http endpoints, are decoded and their chunk signatures are verified.

### Object Lock

Object Lock (WORM) is passed through to the backend, which enforces the retention. The per-object retention and legal hold operations are proxied, and the `x-amz-object-lock-*` headers on uploads (PutObject, CopyObject, CreateMultipartUpload) and `x-amz-bypass-governance-retention` on deletes are forwarded — each requiring the corresponding action in addition to the operation's own, as on AWS (see [Actions a header adds](#actions-a-header-adds)). Bucket policies gain the corresponding actions (`s3:GetObjectRetention`, `s3:PutObjectRetention`, `s3:GetObjectLegalHold`, `s3:PutObjectLegalHold`, `s3:BypassGovernanceRetention`, `s3:GetBucketObjectLockConfiguration`). The bucket-level configuration is readable (GetObjectLockConfiguration) but not writable through the gateway: the default retention is bucket configuration, written where the bucket is created (see [Limitations](#limitations)).

Object Lock must be enabled when a bucket is created, and the gateway does not proxy CreateBucket, so the backend bucket must have been created with Object Lock enabled. The exact behavior depends on the backend: Ceph RGW and Amazon S3 support it fully, while versitygw enforces retention but does not honor governance-mode bypass.

### Checksums

`x-amz-checksum-*` checksums (CRC32, CRC32C, CRC64NVME, SHA1, SHA256) flow end-to-end:

- Precomputed checksum headers on uploads pass through to the backend, which validates and stores them.
- Trailing checksums in `aws-chunked` bodies (the SDK default over https) are **verified by the proxy** against the decoded payload (`BadDigest` on mismatch). When the backend is reached over https the algorithm is forwarded so the backend recomputes and stores the checksum; over a plain-http backend it is not (the SDK can only recompute it over an unseekable body as a trailer, which it sends over https only), so the upload is still verified but the backend stores no checksum.
- Downloads pass `x-amz-checksum-mode: ENABLED` through and return the backend's checksum headers, so client SDKs can validate response payloads. Multipart part checksums are carried through UploadPart / CompleteMultipartUpload as well.

Whether a checksum is actually stored and returned depends on the backend (versitygw and Amazon S3 do; some Ceph RGW builds do not).

### Server-side encryption

SSE-S3 (`x-amz-server-side-encryption: AES256`) and SSE-KMS (`aws:kms` + `x-amz-server-side-encryption-aws-kms-key-id`) **pass through**: the backend performs the encryption, and its result headers are returned on uploads and downloads. The KMS key id is opaque to the gateway — it is whatever the backend's KMS resolves (for example Ceph RGW handing it to a Vault-compatible key service), so the key id namespace belongs to the service, not to the backend's infrastructure. The requested mode and key id are exposed as `Op.SSE` / `Op.SSEKMSKeyID` to the [Authorizer](building-a-service.md#hooks-and-metering): whether a tenant may use a key — or whether a bucket's backend supports encryption at all (some ignore the request silently rather than refuse) — is the service's decision; nothing else in the path knows which tenant owns which key, since the backend's KMS request carries no tenant identity.

**SSE-C is refused** with `NotImplemented` rather than silently dropped: an ignored customer key would store the object without the encryption the client believes it requested, and later serve it back without the key.

Backend notes for Ceph RGW: SSE requests require TLS toward RGW by default (`rgw_crypt_require_ssl`) — terminate or disable it deliberately; the compose file's ceph service configures the built-in `testing` KMS backend with a static key (`testkey-1`) so the integration suite can exercise SSE-KMS without a real KMS.

### Bucket policies

A bucket may carry an AWS-style policy document, written as JSON text in the store (in the bundled YAML config, `buckets[].policy`). GetBucketPolicy returns it; PutBucketPolicy / DeleteBucketPolicy are not supported (policies are defined in the store, not via the S3 API).

Two simplifications against AWS: principals are `"tenant/user"` names under the `S3RP` key — always tenant-qualified, the short form of an ARN's account/user pair — and resources are plain `"bucket"` / `"bucket/prefix*"` strings (no ARNs). Action and Resource support the AWS wildcards `*` (any run of characters, including `/`) and `?` (exactly one character). As in AWS, `Action` matching is case-insensitive (so a mis-cased `Deny` cannot silently fail open), while `Resource` matching is case-sensitive since object keys are. Every `Resource` entry must refer to the bucket the policy is attached to — the bucket name itself or `bucket/...` — anything else (most likely a typo) could never match a request and is rejected when the policy is loaded, the same mistake AWS refuses at PutBucketPolicy time ("Policy has invalid resource"). `Sid` is optional but must be unique within a policy when given (as in IAM): it is how the operator's log names the statement that refused a request, so setting one on every statement makes a denial explainable — the client itself is only ever told `AccessDenied`.

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

- The supported operators are `IpAddress` and `NotIpAddress`, and the only key is `aws:SourceIp` (key names are case-insensitive, as on AWS). Anything else is rejected at load time rather than ignored — a restriction the author intended is never silently dropped.
- Values are CIDR prefixes or plain addresses (a plain address is a `/32` or `/128`). Values within one operator are ORed; the operators of one `Condition` are ANDed. IPv4 and IPv6 are distinct families — an IPv4 prefix never matches an IPv6 source (IPv4-mapped IPv6 sources count as IPv4).
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

#### Actions a header adds

Some request headers require an action beyond the operation's own, exactly as on Amazon S3, so a policy can refuse what the header does without the gateway inspecting values:

| header | on | additional action |
|---|---|---|
| `x-amz-tagging` | PutObject, CopyObject, CreateMultipartUpload, POST upload | `s3:PutObjectTagging` |
| `x-amz-object-lock-mode`, `x-amz-object-lock-retain-until-date` | PutObject, CopyObject, CreateMultipartUpload | `s3:PutObjectRetention` |
| `x-amz-object-lock-legal-hold` | PutObject, CopyObject, CreateMultipartUpload | `s3:PutObjectLegalHold` |
| `x-amz-bypass-governance-retention: true` | DeleteObject, DeleteObjects, PutObjectRetention | `s3:BypassGovernanceRetention` |

A user policy of `Allow [s3:*]` + `Deny [s3:PutObjectTagging]` therefore refuses tags everywhere they can be written — the dedicated PutObjectTagging operation and an upload carrying `x-amz-tagging` alike — and a bucket policy `Deny` on `s3:PutObjectRetention` keeps a tenant from locking objects on upload. A copy also needs `s3:GetObject` on its source.

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

### ACLs

The gateway behaves like a bucket with ACLs disabled (Object Ownership = bucket owner enforced, the AWS default since 2023). GetBucketAcl / GetObjectAcl return a fixed policy granting FULL_CONTROL to the tenant; PutBucketAcl / PutObjectAcl return `AccessControlListNotSupported`, and canned ACLs other than `private` / `bucket-owner-full-control` are rejected on uploads. Use bucket policies for access control instead.

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
- As on AWS, every form field must be covered by a condition (except `policy`, `x-amz-signature`, `x-ignore-*` and the file itself), the policy `expiration` is enforced, and form fields after the `file` part are ignored.
- Supported fields: `key`, `Content-Type`, `Content-MD5`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Content-Language`, `Expires`, `x-amz-storage-class`, `x-amz-tagging`, `x-amz-meta-*`, `x-amz-server-side-encryption` (+ `-aws-kms-key-id`), `success_action_status` (200/201/204) and `success_action_redirect` (303 with `bucket`/`key`/`etag` appended). `acl` follows the same rule as everywhere else (ACLs are disabled); unsupported fields are refused with `NotImplemented` rather than silently dropped.

## Limitations

- Requests that sign the `user-agent` or other headers the AWS SDK signer ignores will fail verification. Real AWS SDK/CLI clients do not do this.
- Requests whose query string is not canonically percent-encoded (raw reserved characters in values, signed as sent) fail verification — as they do against AWS itself and every implementation measured; duplicate query keys are accepted in exactly one value ordering. Reachable only from hand-rolled SigV4 clients; see [SigV4 query canonicalization](sigv4-canonicalization.md).
- Bucket lifecycle configuration (expiration, transitions) is deliberately **not** exposed via the S3 API — `?lifecycle` gets the same loud `NotImplemented` as every unsupported subresource. Enforcement is the backend's job, so rules must live on the backend bucket, and they are written there by the control plane with backend credentials — the same split as bucket policies and CORS, whose `Put*` are also not proxied. This is a deliberate choice beyond consistency: a bucket holds exactly **one** lifecycle configuration, so letting tenants `PutBucketLifecycleConfiguration` would let one request replace the operator's baseline rules (such as aborting stale multipart uploads); exposing it would require a merge layer, not a pass-through. For experiments, Ceph RGW's `rgw_lc_debug_interval` shortens expiry to seconds.
- The same rule covers every bucket-configuration write: PutBucketVersioning, PutObjectLockConfiguration, PutBucketEncryption, PutBucketOwnershipControls and PutPublicAccessBlock (and their `Delete`s) are also `NotImplemented`. Bucket configuration is written where the bucket is created — the control plane — and a data-plane access key must not be able to overwrite it: suspending versioning changes what the bucket retains from then on, and rewriting the Object Lock default retention would let any key of the tenant undo what the bucket was provisioned with. The reads (GetBucketVersioning, GetObjectLockConfiguration, GetBucketEncryption) stay proxied.
