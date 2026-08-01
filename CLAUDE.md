# CLAUDE.md

Guidance for working on s3rp. See README.md for user-facing documentation.

## What this is

s3rp is a PoC toward a multi-tenant S3-compatible object storage service: an S3 API reverse proxy that authenticates clients by SigV4 with tenant/user access keys and re-executes operations via aws-sdk-go-v2 against per-bucket backends (Ceph RGW, versitygw, Amazon S3, ...).

It deliberately reconstructs operations through the SDK instead of transparently forwarding requests: a multi-tenant service needs to understand every operation anyway (authorization, metering, quota), must present a uniform API surface regardless of backend, and must hide backend bucket names in responses.

## Architecture

- Root `package s3rp` is the proxy; `cmd/s3rp/` is a thin main (kong CLI, slog JSON logging).
- `store/` is the read-only contract for tenant/user/bucket/backend definitions (`store.Store` interface plus domain types `Key`, `Bucket`, `Backend`, `Password`, `CORSRule`). Two implementations: the YAML config (`configStore` in the root package) and sqlite (`store/rdb`, selected via `store.driver` in the config). Bucket policies and CORS rules are carried on `store.Bucket` (policy as raw JSON text + parsed form) and stored as JSON text columns in the DB.
- `db/` holds the shared schema (`schema.sql`, source of truth) and sqlc config. sqlc generates **two packages from one schema**: `store/rdb/readdb` (SELECT only, linked by the proxy) and `db/writedb` (writes, linked only by `cmd/s3rp-admin`). Regenerate with `make gen` after editing schema or queries; the sqlc version is pinned in `mise.toml` (`mise install` to get it). The proxy must never import `db` or `db/writedb`; `store/rdb.Open` force-appends `mode=ro` (the sqlite analog of a SELECT-only DB account). Never add write statements to `db/queries/read.sql`. `db.Import` marshals backends via a plain-string DTO because `store.Password` masks itself on `json.Marshal` — reusing `json.Marshal(store.Backend)` for storage silently destroys secrets.
- `policy/` parses and evaluates AWS-style policy JSON. Leaf package, also usable by a DB store.
- Request flow: verify signature (auth.go) → resolve bucket via Store → evaluate bucket policy (bucketpolicy.go) → dispatch (handler.go) → operation (proxy.go, multipart.go, copy.go, tagging.go, versioning.go, acl.go, cors.go).
- Backend clients are built lazily and cached by backend identity (endpoint/region/credentials/path-style); clients are bucket-agnostic.

## Domain model decisions (do not regress these)

- A tenant owns buckets and users. A **user** (`[a-z][a-z0-9_-]+`, unique per tenant) is the rotation-stable identity; access keys rotate under it. **Never use access key ids as policy principals.**
- Bucket names and access key ids are globally unique across tenants (path-style URLs carry no tenant discriminator; `Store.GetBucketByName` relies on this for unauthenticated CORS preflights).
- Bucket policy: AWS-style JSON text; principals are plain user names under the `"S3RP"` key, `"*"` = all users, `NotPrincipal` = everyone-except; resources are plain `bucket/prefix*` (no ARNs anywhere — explicit owner preference). Semantics: tenant users have full access as the baseline; explicit Deny restricts. Allow is inert until anonymous / cross-tenant access exist.
- Policies are tenant-authored and evaluated on every request (per object for DeleteObjects), so they are untrusted input with structural caps enforced at parse/validate time (`policy.Max*`: 20 KB total document, 20 statements, 30 actions, 10 resources, 128-byte patterns, 100 principal users; the byte cap is checked before unmarshal so an oversized document is rejected without being parsed into memory). The worst-case cost is dominated by the total resource-pattern count (`MaxStatements*MaxResourcesPerStatement`) — each adversarial resource pattern can force a full scan of the object key — not by pattern length or the byte cap; action matching is against a short string and stays cheap, so `MaxActionsPerStatement` is generous. — the glob matcher is O(n·m) (no ReDoS), but unbounded size would still let one authorization do unbounded work. Perf: patterns are compiled to `[]rune` once (lazily via `sync.Once` on first `Evaluate`/`Allows`, so `Policy`/`UserPolicy` carry a lock and must be passed by pointer — `db.marshalUserPolicy` marshals `&UserPolicy`), and the matched value is converted to runes once per call rather than per pattern; together this keeps a maxed-out policy's evaluation in the low microseconds. DeleteObjects authorizes per object but resolves the resource-independent parts once via `app.perObjectAuthorizer` / `policy.DenyEvaluatorFor` (user policy + the bucket policy's matching Deny statements), so only the resource is tested per key; when no Deny matches (the common case), `DenyEvaluator.AlwaysAllows` skips the per-key check entirely — de-amplifying the ×1000 to O(1). Only a policy with resource-discriminating Deny rules for the action pays per key, bounded by the resource-count cap.
- User policy (`policy.UserPolicy`, carried on `store.Key`): action-only identity policy attached to a **user**, no resource/principal. Evaluation is IAM-style and separate from the bucket-policy baseline: no policy = allow `s3:*`; with a policy only `Allow`-matched actions pass (unmatched = implicit deny), `Deny` wins. `app.authorize` checks it **before** the bucket policy — a request must pass both layers (user policy allows AND bucket policy does not Deny), so it lives at the same single chokepoint and covers every operation. Stored as JSON text in the users table (`users.policy`).
- ACLs mimic ACL-disabled buckets (AWS default since 2023): GET returns a fixed tenant-FULL_CONTROL stub, writes get `AccessControlListNotSupported`. Do not proxy ACLs to backends.
- CORS is handled by the proxy (not the backend): preflight `OPTIONS` is unauthenticated and handled before signature verification. Required for browser-direct presigned uploads.
- Object Lock is passed through to the backend (WORM enforcement is the backend's job). Bucket must be created with Object Lock enabled on the backend (s3rp does not proxy CreateBucket). Backend behavior differs — Ceph RGW/S3 honor governance bypass, versitygw does not — so the integration ObjectLock bypass-delete is best-effort, not asserted.
- CopyObject / UploadPartCopy work only between buckets on the same backend; the source resolves within the requester's tenant, making cross-tenant copies impossible by construction.
- Responses must expose the **front** bucket name (ListObjectsV2 `Name`, multipart results, error `Resource`), never the backend bucket rename.

## SigV4 verification (auth.go) — the trickiest part

Verification re-signs a clone of the request with the SDK's own `v4.Signer` and compares signatures (constant-time). Non-obvious details:

- `SignerOptions.DisableURIPathEscaping = true` (S3 mode) or keys containing `/` break.
- The signing region is parsed from the client's credential scope, not configured.
- The signer includes `content-length` in canonical headers based on `Request.ContentLength`, not the header map — the clone sets the field when the header was signed.
- The clone preserves the raw request escaping via `url.ParseRequestURI(r.RequestURI)`.
- Presigned URLs verify the same way via `PresignHTTP`; auth query params are stripped first (the signer re-adds them). S3's presigner disables header hoisting (signed `x-amz-meta-*` must be sent by the uploader) and strips Content-Type from presigned PUTs.
- aws-chunked bodies (`STREAMING-AWS4-HMAC-SHA256-PAYLOAD` and trailer variants) are mandatory to decode — the aws CLI uses them for every upload over http. chunked.go verifies the per-chunk HMAC chain seeded by the request signature; AWS docs known-answer vectors are the test fixtures.

## Checksums (checksum.go)

`x-amz-checksum-*` flows end-to-end: precomputed headers pass through; trailing checksums in aws-chunked bodies are verified by the proxy (chunked.go, `BadDigest` on mismatch) and the algorithm is forwarded as `ChecksumAlgorithm` so the backend SDK recomputes and stores it — an explicit `ChecksumAlgorithm` forces calculation even under `RequestChecksumCalculationWhenRequired`. Downloads forward `x-amz-checksum-mode` and return the backend's checksum headers. Whether checksums exist depends on the backend (versitygw yes; the ceph/demo RGW build does not store them — the integration Checksum subtest skips there). CRC64NVME uses `crc64.MakeTable(0x9A6C9329AC4BC9B5)`; values are base64 of the big-endian sum.

## Backend client construction (backend.go) — real failure modes

Two settings are load-bearing for non-AWS backends; removing either breaks PutObject:

- `v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware`: the request body is unseekable, so the SDK cannot compute a payload hash over plain http.
- `RequestChecksumCalculationWhenRequired` (+ response equivalent): SDK-default CRC32 trailer checksums re-introduce aws-chunked encoding, which S3-compatible backends may reject.

## Routing (handler.go)

- A single catch-all route with manual dispatch; `http.ServeMux` patterns are unusable (path cleaning + 301 redirects break S3 keys and signatures).
- Unknown query subresources return 501 loudly (never silently ignore — SDK operations must fail visibly). The `x-id` query param (SDK operation hint) and `x-amz-*` query keys (presigned auth params, hoisted headers) are exempt.
- Each dispatch branch checks the allowed `paramSet` and calls `authorize` with the mapped `s3:*` action. DeleteObjects authorizes per object; copies authorize `s3:GetObject` on the source and `s3:PutObject` on the destination.

## Testing

- stdlib `testing` + `google/go-cmp` only; no testify. External test package (`s3rp_test`) with `export_test.go` for internals. Table tests, `t.Context()`.
- The main e2e pattern: `httptest.NewServer(app.Handler())` with a **real aws-sdk-go-v2 client** using front credentials (genuine client-side SigV4) and a `stubBackend` injected via `SetBackend` (which pre-warms the backend client cache). This covers verification → routing → response mapping without any real backend.
- Integration tests are env-gated: `S3RP_TEST_BACKEND_ENDPOINT` (+ optional key env vars). Backends in compose.yml:
  - versitygw (default, fast): `docker compose up -d --wait versitygw`, endpoint `http://localhost:7070`. The `--versioning-dir` flag must precede the positional data dir (urfave/cli stops parsing flags after positionals).
  - Ceph RGW (`ceph` profile, heavyweight): `docker compose up -d --wait ceph`, endpoint `http://127.0.0.1:7480`. **Use 127.0.0.1, never localhost** — RGW resolves Host names not matching `rgw dns name` as virtual-hosted bucket names. Keep the image on a non-EOL Ceph release (currently Squid; Reef is EOL).
  - **Never use MinIO** — its OSS edition is effectively unmaintained.
- CI runs unit tests (go 1.25/1.26, `-race`) and the integration suite as a backend matrix (versitygw / ceph, `fail-fast: false`).
- Policy evaluation is on the hot path of every request (per object for DeleteObjects), so it is performance-sensitive. `policy/bench_test.go` measures the worst case the caps allow (adversarial patterns, `BenchmarkEvaluate` / `BenchmarkAllows` / `BenchmarkDeleteObjects`). When changing anything under `policy/` or the authorization path (`bucketpolicy.go`, the DeleteObjects loop), **always run `go test ./policy -bench . -benchmem` and confirm no unintended regression** (watch both ns/op and allocs/op — an alloc jump usually means the pattern precompilation or the DeleteObjects de-amplification was defeated).

## Conventions

- Branch per change; never commit directly to main. Run `go fmt ./...` and `go fix ./...` before committing; `git add <file>`, not `-A`. Concise English commit messages and PR descriptions.
- Error responses: S3 XML via `S3Error` (`fromSDKError` preserves backend code/status; `wellKnownErrorStatus` maps code-only errors). HEAD and 304 responses must not carry a body. Every response gets `x-amz-request-id`.
- Secrets use `store.Password` (masked on JSON/YAML marshal). The local `s3rp.yml` is an untracked personal config — never commit it.
- Update README.md when adding or changing operations; the supported-operations list and the Limitations section must stay accurate.
