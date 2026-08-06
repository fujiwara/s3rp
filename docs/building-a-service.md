# Building a service on the gateway

The S3 API lives in `s3gw`, separate from the parts of this repository that are only its PoC packaging — the YAML config, its store, the CLI. A real service does not fork the proxy: it implements the two things that are genuinely its own and hands them to the gateway.

## What you provide

| | |
|---|---|
| `store.Store` | where definitions come from: tenants, users, access keys, and where each bucket really lives. This is your control plane's read side. |
| `s3gw.Authorizer` | what the policies cannot express — an exhausted quota, a suspended tenant, a rate limit. Consulted after the bucket and user policies have already allowed the operation. |
| `s3gw.Interceptor` | what you meter. It wraps the operation, so the byte counts are filled in by the time `next` returns. |
| `s3gw.Observer` | how requests are logged. The gateway does not log — once per request it reports who asked, what they asked for, what they were told and why — and leaves the format, level and destination to you. |

**What the gateway does**: SigV4 verification (header and presigned), `aws-chunked` decoding and checksums, bucket and user policy evaluation, CORS, the operations themselves, and the routing that reaches them — refusing unknown ones rather than passing them through.

**What it deliberately does not do**: create tenants, buckets or keys. Those are control-plane writes; the gateway only ever reads definitions, so it can run with a read-only database account.

## A minimal service

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

## Your store

- Definitions are read on **every request** and nothing is cached: `GetKey` on each authentication, `GetBucket` on each bucket resolution. Caching is your store's business — it knows when a key is revoked, which the gateway cannot.
- If your store does cache, keep the parsed `*policy.Policy` / `*policy.UserPolicy` around, not the policy JSON: the compiled match patterns live on the policy object (built lazily on its first evaluation, safe to share across concurrent requests), so a store that re-parses the text per request silently repeats that work every time. A cache keyed by the policy text invalidates itself — a changed policy is a different key.
- The policy syntax — the `"S3RP"` principal key, `tenant/user` principals, plain-path resources — is only the default `policy.Dialect`. If your tenants should write a different key, ARN-prefixed resources (`arn:aws:s3:::bucket/*`), or their own principal syntax, parse with your own `Dialect`: `PrincipalKey` and `ResourcePrefix` cover the fixed parts, and the `NormalizePrincipal` / `NormalizeResource` hooks rewrite each value to the internal form (`"arn:myco:iam::ta:user/alice"` → `"ta/alice"`) inside the same parse pass — no pre-parsing of the tenant's JSON — with validation and the caps applied to the normalized result. Evaluation is identical in any dialect, and so is the `Condition` syntax (the IP operators and the `aws:SourceIp` key are the same everywhere). Keep the tenant's original text in `store.Bucket.PolicyText` (returned verbatim by GetBucketPolicy) and put the `Dialect`-parsed form in `Policy` — the two fields are deliberately independent.
- Parsing takes the bucket's name — `policy.Parse(bucketName, text)` / `Dialect.Parse` — and requires every resource to refer to that bucket; there is deliberately no way to parse a bucket policy without the check. A statement naming any other bucket can never match anything — a policy is only ever evaluated against its own bucket — so accepting it would leave the author trusting a restriction that silently does nothing; AWS rejects the same mistake at PutBucketPolicy time. With a dialect the check runs on the normalized resources, so pass the plain internal bucket name.
- **Temporary credentials** are a store concern, not a gateway one: set `store.Key.SessionToken` on a key your control plane issues under an existing user (empty = long-lived key). A request signed with that key must present exactly that token — header auth, presigned URLs and POST uploads all carry it — and a token on a long-lived key is refused (`InvalidToken`), never dropped. Expiry is also yours: an expired key is simply not returned by `GetKey`. Authorization needs nothing new — the key belongs to a user, and a per-key `UserPolicy` narrows its actions. One issuance rule: do not return the credentials to the caller before the key is visible to the store the gateways read, or the first use races the write (and a stale not-found may be negative-cached).
- Two tenants must not map buckets to the same physical backend bucket — the gateway cannot detect this, so validate it where definitions are written.
- Validate names and backends where definitions are written, with the store package's validators: `store.ValidateBucketName` (bucket names are what path routing and `bucket/key` policy resources are built from), `store.ValidateTenantName` / `store.ValidateUserName` (tenant and user names share the charset of policy principals — a name outside it could never be written in a `Principal` element, so its grants would be unexpressible), and `store.Backend.Validate` (endpoint and credential shape). Like `Backend.SetDefaults`, these are the store's to call; the gateway does not re-check per request.
- Validate CORS rules with `cors.Rule.Validate()` where definitions are written: a rule with no origins or methods can never match (a dead rule its author believes in) and an unsupported method could never be answered — AWS rejects the same at PutBucketCors time. The gateway deliberately does not re-check rules per request.

## In front of the gateway

- Unauthenticated requests are not free. `GetKey` runs **before** signature verification — the secret is what verifies — so a well-formed `Authorization` header with a bogus access key still costs one store lookup, and a valid key with a bad signature costs the HMAC chain too. A store should negative-cache unknown key ids (weighing the TTL against how soon a freshly issued key must start working), and rate limiting belongs **outside** `Handler()`, in a wrapping handler or the fronting proxy: the `Authorizer` runs only after verification and the policies, which is too late for DoS economics.
- That same wrapping layer is where a health-check endpoint and a request body cap (`http.MaxBytesReader`) go. To the gateway every path is S3 — `GET /` is an authenticated ListBuckets — and it deliberately caps nothing itself, for the same reason it sets no read or write timeouts.
- The signing region of a request is taken from its credential scope, so by default **any region verifies** — the signature commits to the region either way, and a front endpoint has no inherent region. A multi-region deployment should pin each endpoint with `SetRegion`: not for signature integrity, but so a client pointed at the wrong regional endpoint fails fast with AWS's own error (`AuthorizationHeaderMalformed`, naming the expected region) instead of silently being served cross-region, and so a leaked derived signing key stays scoped to its region as SigV4 intends. The pinned region is also what GetBucketLocation and HeadBucket report, keeping region discovery consistent with what the verifier accepts.

## Backend clients and the gateway's caches

- Backend clients are built by the gateway, but `SetClientOptions` lets you contribute `s3.Options` to every one it builds — a custom `Retryer`, an instrumented `HTTPClient` (an otelhttp transport is how you get per-backend latency and retry metrics, which the hooks cannot see), timeouts. The hook receives the backend definition, so options can differ per backend, and runs after the gateway's own settings — which are load-bearing for non-AWS backends, so override them only knowingly. Set it before serving: clients are cached per backend and keep the options they were built with.
- What the gateway does cache is derived from definitions, never a definition itself, and is bounded: backend **clients** (one per distinct endpoint/credentials, LRU, default 128 — `SetClientCacheSize`) and one SigV4 **signer** per access key (default 512 slots — `SetSignerCacheSize`). Size them to the number of distinct backends and of access keys active at once; an evicted entry is rebuilt on its next request, so undersizing costs latency, not correctness.
- Whether they *are* sized right is answerable: `ClientCacheStats` / `SignerCacheStats` return snapshots (hits, misses, evictions, len, capacity — monotonic counters, poll them from your metrics collector). A rising eviction rate with `Len` near `Capacity` means the cache is too small. The signer cache's evictions are collision displacements: a high rate with `Len` well **under** `Capacity` means hot keys sharing a slot by hash luck, which more slots make improbable.

## Hooks and metering

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

## Observation

- **Nothing is logged unless you install an observer**, including failures. The cause of a failure is not recoverable anywhere else: it never reaches the client, by design. An observer is called once per request, after the response has been written, whether or not the request ever reached an operation — a signature that did not verify or a bucket that does not exist never reaches an interceptor, but is still observed.
- `RequestInfo` keeps the identity apart from the operation: `Tenant` and `User` are set as soon as the signature verifies, `Op` only once routing and the policies pass. So a request refused for an unknown bucket or a denied action still records **who** asked, which is usually the point of looking.
- `RequestInfo` stands on its own, `Start` included, so it can be handed to a metering queue or a batch and still say when it happened — an observer that defers the work must not have to stamp the time itself. It carries snake_case JSON tags and can be emitted as it stands; the failure reason is rendered as its message, since an `error` marshals to an empty object on its own. `Op` is tagged the same way.
- Log `RequestInfo.RawQuery`, not the request's own query string: the gateway masks the presigned authentication parameters, and a presigned URL's signature is a bearer credential until it expires.

## The other packages

The other packages are usable on their own: `sigv4` (server-side SigV4 verification and `aws-chunked` decoding), `policy` (AWS-style policy evaluation), `s3err`, `s3xml`, `checksum` and `cors`. `checksum`, `policy` and `s3xml` depend only on the standard library.
