# Building a service on the gateway

This document is for implementing a **production** gateway on `s3gw`. What is PoC in this repository is the packaging — the root package that assembles a gateway from a YAML file, its config store, the CLI. The gateway and the leaf packages it stands on (`store`, `policy`, `sigv4`, `s3err`, `cors`, `checksum`, `s3xml`) are built to be used as they are: a real service does not fork the proxy, it implements what is genuinely its own — where definitions live, what to admit, what to meter, what to log — and hands those to the gateway.

Each section of this document stands on its own: find the component you are implementing in the table below and jump straight to its section. [A minimal service](#a-minimal-service) shows them assembled.

## What you provide

In the order you will meet them: what the gateway cannot run without, then what makes it visible, then your service's own rules, then what a production deployment adds in front and tunes behind.

| Component | What it is | Where |
|---|---|---|
| `store.Store` | the one component the gateway cannot start without: where definitions come from — tenants, users, access keys, and where each bucket really lives. Your control plane's read side, including policy parsing and temporary credentials. | [Your store](#your-store) |
| `s3gw.Observer` | install it next: the gateway logs nothing on its own, failures included. Once per request it reports who asked, what they asked for, what they were told and why; format, level and destination are yours. | [Observation](#observation) |
| `s3gw.Authorizer` | refusing what the policies cannot express — an exhausted quota, a suspended tenant. Consulted after the bucket and user policies have already allowed the operation. | [Hooks and metering](#hooks-and-metering) |
| `s3gw.Interceptor` | wrapping each operation: metering — the byte counts are filled in by the time `next` returns — and the per-tenant concurrency cap. | [Hooks and metering](#hooks-and-metering) |
| `SetBandwidthLimit` | pacing the streams themselves, which admission hooks cannot do. | [Hooks and metering](#hooks-and-metering) |
| the wrapping handler and listener | production exposure: TLS termination and what must run **before** verification — rate limiting, request-body caps, the global connection cap, health checks — and the path-handling rules every hop in front must obey. | [In front of the gateway](#in-front-of-the-gateway) |
| `SetClientOptions`, cache sizes | tuning: instrumenting the backend clients the gateway builds and sizing the caches they live in; the defaults are sound to start with. | [Backend clients and the gateway's caches](#backend-clients-and-the-gateways-caches) |

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

func (d *definitions) GetKey(ctx context.Context, accessKeyID, sessionToken string) (*store.Key, error) {
	// look the key up; return an error wrapping store.ErrNotFound if absent.
	// sessionToken is the (unverified) token the request presented — ignore
	// it unless you issue self-contained tokens; see Temporary credentials.
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
		Metadata: &tenantPlan{quotaBytes: 1 << 40}, // 1 TiB
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

Your `store.Store` implementation — and the control plane's write path that feeds it, where validation belongs.

- Definitions are read on **every request** and nothing is cached: `GetKey` on each authentication, `GetBucket` on each bucket resolution. Caching is your store's business — it knows when a key is revoked, which the gateway cannot.
- If your store does cache, keep the parsed `*policy.Policy` / `*policy.UserPolicy` around, not the policy JSON: the compiled match patterns live on the policy object (built lazily on its first evaluation, safe to share across concurrent requests), so a store that re-parses the text per request silently repeats that work every time. A cache keyed by the policy text invalidates itself — a changed policy is a different key.

  ```go
  // a comparable struct key: no concatenation, so a lookup does not copy
  // the policy text (up to 20 KB) on the request path
  type policyKey struct{ bucket, text string }

  var policies sync.Map // policyKey → *policy.Policy

  // inside GetBucket, after loading the row
  func (d *definitions) parsedPolicy(bucket, text string) (*policy.Policy, error) {
  	key := policyKey{bucket, text} // a changed policy is a different key
  	if p, ok := policies.Load(key); ok {
  		return p.(*policy.Policy), nil // compiled patterns reused, safe to share
  	}
  	p, err := policy.Parse(bucket, text)
  	if err != nil {
  		return nil, err // a stored policy that no longer parses is a bug upstream
  	}
  	policies.Store(key, p)
  	return p, nil
  }
  ```
- The policy syntax — the `"S3RP"` principal key, `tenant/user` principals, plain-path resources — is only the default `policy.Dialect`. If your tenants should write a different key, ARN-prefixed resources (`arn:aws:s3:::bucket/*`), or their own principal syntax, parse with your own `Dialect`: `PrincipalKey` and `ResourcePrefix` cover the fixed parts, and the `NormalizePrincipal` / `NormalizeResource` hooks rewrite each value to the internal form (`"arn:myco:iam::ta:user/alice"` → `"ta/alice"`) inside the same parse pass — no pre-parsing of the tenant's JSON — with validation and the caps applied to the normalized result. Evaluation is identical in any dialect, and so is the `Condition` syntax (the IP operators and the `aws:SourceIp` key are the same everywhere). Keep the tenant's original text in `store.Bucket.PolicyText` (returned verbatim by GetBucketPolicy) and put the `Dialect`-parsed form in `Policy` — the two fields are deliberately independent.
- Parsing takes the bucket's name — `policy.Parse(bucketName, text)` / `Dialect.Parse` — and requires every resource to refer to that bucket; there is deliberately no way to parse a bucket policy without the check. A statement naming any other bucket can never match anything — a policy is only ever evaluated against its own bucket — so accepting it would leave the author trusting a restriction that silently does nothing; AWS rejects the same mistake at PutBucketPolicy time. With a dialect the check runs on the normalized resources, so pass the plain internal bucket name.

  ```go
  // tenants write AWS-style ARNs; the gateway evaluates the internal form
  var dialect = &policy.Dialect{
  	PrincipalKey:   "AWS",
  	ResourcePrefix: "arn:aws:s3:::",
  	NormalizePrincipal: func(s string) (string, error) {
  		// "arn:myco:iam::tenant-a:user/alice" → "tenant-a/alice"
  		rest, ok := strings.CutPrefix(s, "arn:myco:iam::")
  		tenant, user, ok2 := strings.Cut(rest, ":user/")
  		if !ok || !ok2 {
  			return "", fmt.Errorf("not a principal ARN")
  		}
  		return tenant + "/" + user, nil
  	},
  }

  p, err := dialect.Parse(bucketName, text) // the plain name: the check runs post-normalization
  if err != nil {
  	return err // reject at write time, exactly as PutBucketPolicy would
  }
  b := &store.Bucket{
  	PolicyText: text, // what the tenant wrote; GetBucketPolicy returns it verbatim
  	Policy:     p,    // what the gateway evaluates
  }
  ```
- **Temporary credentials** are a store concern, not a gateway one: set `store.Key.SessionToken` on a key your control plane issues under an existing user (empty = long-lived key). A request signed with that key must present exactly that token — header auth, presigned URLs and POST uploads all carry it — and a token on a long-lived key is refused (`InvalidToken`), never dropped. Expiry is also yours: an expired key is simply not returned by `GetKey`. Authorization needs nothing new — the key belongs to a user, and a per-key `UserPolicy` narrows its actions. One issuance rule: do not return the credentials to the caller before the key is visible to the store the gateways read, or the first use races the write. The gateway caches no definitions, so this race is entirely within your store — and if the store negative-caches unknown key ids, as [In front of the gateway](#in-front-of-the-gateway) recommends, a lost race turns into that cache remembering the not-found for its TTL.
- **Self-contained tokens** are the stateless alternative: `GetKey` receives the token the request presented, so a store can authenticate the token itself (a MAC or signature by the control plane's key) and derive the whole `Key` — secret material, expiry, a session `UserPolicy` — from its contents, persisting nothing per credential and skipping the issuance-visibility race entirely.

  The token is presented in the clear — the SigV4 signature commits to it but does not hide it, and in a presigned URL it is part of what gets shared — so it must never carry the secret in any recoverable form. Derive it instead: `secret = HMAC(master_key, "secret" ‖ payload)`, computed once at issuance to hand to the caller and recomputed in `GetKey` from the presented payload. The derivation label (or key) must differ from the token's own MAC — with a shared one, the MAC embedded in the token *is* the secret. (Sealing the secret into the token with authenticated encryption also works, but buys nothing over derivation and adds a nonce and a decryption path to get wrong; one keyed hash is the whole construction.)

  Two rules. First, the token arrives **before signature verification** — it is untrusted input; verify its MAC before trusting anything in it, and return an error wrapping `store.ErrInvalidToken` when it fails (the client sees `InvalidToken`; a plain `store.ErrNotFound` would claim the *key id* is unknown). Second, return the presented token as `Key.SessionToken` — the gateway still runs the exact-match, and echoing back what you authenticated passes it trivially.

  What you give up is per-credential revocation: a persisted temporary key dies with a row delete, a self-contained token lives until it expires unless you add revocation state back (a denylist of revoked token ids, a per-user session generation the token embeds, or rotating the signing key) — so keep TTLs short. Sizing note: the token rides in the `X-Amz-Security-Token` query parameter of presigned URLs, so keep it well under a few KB (AWS's own run 0.6–2 KB and the practical URL budget is ~8 KB).
- Two tenants must not map buckets to the same physical backend bucket — the gateway cannot detect this, so validate it where definitions are written.
- Validate names and backends where definitions are written, with the store package's validators: `store.ValidateBucketName` (bucket names are what path routing and `bucket/key` policy resources are built from), `store.ValidateTenantName` / `store.ValidateUserName` (tenant and user names share the charset of policy principals — a name outside it could never be written in a `Principal` element, so its grants would be unexpressible), and `store.Backend.Validate` (endpoint and credential shape). Like `Backend.SetDefaults`, these are the store's to call; the gateway does not re-check per request.
- Validate CORS rules with `cors.Rule.Validate()` where definitions are written: a rule with no origins or methods can never match (a dead rule its author believes in) and an unsupported method could never be answered — AWS rejects the same at PutBucketCors time. The gateway deliberately does not re-check rules per request.

  Together, a control plane's write path looks like:

  ```go
  // where definitions are written — the gateway re-checks none of this
  func validateBucketDefinition(tenant string, b *BucketDefinition) error {
  	if err := store.ValidateTenantName(tenant); err != nil {
  		return err
  	}
  	if err := store.ValidateBucketName(b.Name); err != nil {
  		return err
  	}
  	if err := b.Backend.Validate(); err != nil {
  		return err
  	}
  	if b.PolicyText != "" {
  		if _, err := policy.Parse(b.Name, b.PolicyText); err != nil {
  			return err
  		}
  	}
  	for _, r := range b.CORS {
  		if err := r.Validate(); err != nil {
  			return err
  		}
  	}
  	// plus what only the whole definition set can answer: global bucket-name
  	// and access-key uniqueness, and that no other tenant maps to the same
  	// physical backend bucket — unique database constraints, typically
  	return nil
  }
  ```

**Do**

- Cache the parsed `*policy.Policy` / `*policy.UserPolicy`, keyed by the policy text.
- Parse a bucket policy with its bucket's name (`policy.Parse(bucket, text)`); with a dialect, keep the tenant's original text in `PolicyText`.
- Run `store.Validate*`, `Backend.Validate`, `Backend.SetDefaults` and `cors.Rule.Validate` where definitions are written, and enforce global uniqueness (bucket names, access key ids) and physical-backend-bucket exclusivity there too.
- Make an issued key visible to the store the gateways read **before** returning its credentials to the caller.

**Don't**

- Don't re-parse policy JSON per request — the compiled patterns live on the parsed object.
- Don't hand the gateway a `Backend` that skipped `SetDefaults`, or definitions whose `Metadata` is mutated while requests share it.
- Don't return expired keys from `GetKey` — expiry is the store's concern, the gateway has no notion of it.

## Observation

`SetObserver`: the one place every request — served or refused — is reported, and the only place a failure's cause exists.

- **Nothing is logged unless you install an observer**, including failures. The cause of a failure is not recoverable anywhere else: it never reaches the client, by design. An observer is called once per request, after the response has been written, whether or not the request ever reached an operation — a signature that did not verify or a bucket that does not exist never reaches an interceptor, but is still observed.
- `RequestInfo` keeps the identity apart from the operation: `Tenant`, `User` and `AccessKeyID` are set as soon as the signature verifies, `Op` only once routing and the policies pass. So a request refused for an unknown bucket or a denied action still records **who** asked, which is usually the point of looking. `AccessKeyID` is there for key-level accounting — a user holds several keys during rotation, so a last-used timestamp that answers "is the old key still in use?" must be kept per key, and the observer is the one place that sees every request the key actually signed (the store's `GetKey` runs *before* verification, so it would record presentation, not use — and a store cache skips it entirely). Aggregate in memory and flush coarsely; do not write per request.
- `RequestInfo` stands on its own, `Start` included, so it can be handed to a metering queue or a batch and still say when it happened — an observer that defers the work must not have to stamp the time itself. It carries snake_case JSON tags and can be emitted as it stands; the failure reason is rendered as its message, since an `error` marshals to an empty object on its own. `Op` is tagged the same way.
- Log `RequestInfo.RawQuery`, not the request's own query string: the gateway masks the presigned authentication parameters, and a presigned URL's signature is a bearer credential until it expires.

**Do**

- Install an observer — it is the only place a failure's cause exists at all.
- Keep it fast (it runs on the request path, after the response): buffer or sample at real request rates, or hand `RequestInfo` to a queue — it is self-contained, `Start` included.

**Don't**

- Don't log the request's own query string — log `RequestInfo.RawQuery`, which already masks presigned signatures.
- Don't surface the failure cause (`RequestInfo.Err`) to clients; it may name backend endpoints and buckets, which is why the gateway kept it out of the response.

## Hooks and metering

`Authorizer`, `Interceptor` and `SetBandwidthLimit`: the seams every operation runs through once the bucket and user policies have allowed it.

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
- **Concurrency limiting** — a cap on in-flight operations per tenant — belongs in an interceptor, not the `Authorizer`: counting what is in flight needs a release point, and only an interceptor sees the operation end. `Authorize` is a one-shot admission check with no completion signal, so it could count requests but never un-count them. Acquire before `next`, release after it returns:

  ```go
  var inflight sync.Map // tenant -> *semaphore.Weighted; sized from your plan data
  gw.Use(func(ctx context.Context, op *s3gw.Op, next func() error) error {
  	s, _ := inflight.LoadOrStore(op.Tenant, semaphore.NewWeighted(32))
  	sem := s.(*semaphore.Weighted)
  	if !sem.TryAcquire(1) {
  		// full: refuse rather than queue — a queued request holds a
  		// connection and its buffers for the whole wait
  		return s3err.New(http.StatusServiceUnavailable, "SlowDown",
  			"Please reduce your request rate.")
  	}
  	defer sem.Release(1)
  	return next()
  })
  ```

  `503 SlowDown` is S3's own throttling answer, and the SDKs already retry it with backoff. The keying rules are the same as for bandwidth limiters: share the semaphore at the granularity you mean to cap, and key on tenant or user, not access key ids. Refusing before `next` also bounds per-request memory, because everything an operation allocates happens inside it — the largest single allocation is the 16 MiB cap on XML request bodies (DeleteObjects, CompleteMultipartUpload); object bodies stream through in constant memory whatever their size, so what this cap bounds is operations, not gigabytes. What it cannot bound is traffic that never authenticates — a hook runs only after verification and the policies — so it needs the global backstop from [In front of the gateway](#in-front-of-the-gateway) underneath it.
- A client that disconnects mid-response cancels the request context, which also aborts the backend transfer — `next` still returns promptly, with `op.BytesOut` counting what was actually sent (a truncated download reports the status it already sent, usually 200, and no error). The trap: the `ctx` your after-`next` code holds **is that canceled context**, so metering I/O that uses it fails with `context.Canceled` precisely for disconnected clients — and only for them, since on a normal completion the context outlives the hooks. Use `context.WithoutCancel(ctx)` for your own writes, or hand the self-contained `Op`/`RequestInfo` to a queue and drop the context entirely.

  ```go
  gw.Use(func(ctx context.Context, op *s3gw.Op, next func() error) error {
  	err := next()
  	// ctx is canceled when the client disconnected mid-response; detached,
  	// the usage write succeeds for exactly the requests it would otherwise
  	// silently miss
  	recordUsage(context.WithoutCancel(ctx), op.Tenant, op.BytesIn, op.BytesOut)
  	return err
  })
  ```
- A client that retries sends a **new request**, which is verified, authorized and metered on its own. That is what the server served; whether a retry should count toward a quota or an invoice is the application's decision, not the gateway's.
- `Op.BytesIn` / `BytesOut` count bytes on the wire, so an `aws-chunked` upload includes its framing. They measure transfer, not storage: a quota over bytes at rest needs the backend's inventory (deletes, overwrites and versions carry no sizes through the hooks).
- **Bandwidth limiting** is the one thing the Authorizer/Interceptor shape cannot do — they gate admission, not the stream. `SetBandwidthLimit` installs a hook that picks pacing limiters per operation, called after the policies and the Authorizer allow it; `in` paces the request body as read off the wire (the same bytes `BytesIn` counts), `out` the response body, `nil` means unlimited. The hook only *selects* — sharing is what makes it a limit, and the keying is yours: returning one limiter per tenant caps that tenant's aggregate bandwidth across all its concurrent requests. Key on `Op.User` rather than an access key: keys rotate — two are live during a rotation — and temporary keys can be minted in any number under one user (a self-contained token is not even a row anywhere), so a per-key budget is not a cap but a multiplier the client controls. Note `Op.Tenant` is the **requester's** tenant: on a cross-tenant request, whether the bandwidth bill belongs to the requester (`KeyMetadata` side) or the bucket owner (`BucketMetadata` side) is your business rule. `*rate.Limiter` satisfies the interface; the gateway waits in chunks of at most 32 KiB, so any burst ≥ 32 KiB works, but the burst is also how far a stream runs ahead of the rate — 256 KiB to 1 MiB is a practical range. A pacing failure aborts the request rather than letting it through unpaced.

  ```go
  var limiters sync.Map // tenant -> *rate.Limiter; eviction is yours too
  gw.SetBandwidthLimit(func(op *s3gw.Op) (in, out s3gw.BandwidthLimiter) {
  	plan := op.KeyMetadata.(*Plan) // loaded by your store with the key
  	if plan.BytesPerSec == 0 {
  		return nil, nil // unlimited
  	}
  	l, _ := limiters.LoadOrStore(op.Tenant,
  		rate.NewLimiter(rate.Limit(plan.BytesPerSec), 1<<20)) // burst 1 MiB
  	lim := l.(*rate.Limiter)
  	return lim, lim // one budget for both directions
  })
  ```

**Do**

- Meter after `next()` — the byte counts are final by then, in any layer.
- Refuse a request by returning **without** calling `next`; read what the store already loaded from `Op.BucketMetadata` / `Op.KeyMetadata`.
- Cap per-tenant in-flight operations in an interceptor — acquire before `next`, release after it returns, refuse with `503 SlowDown`.
- Use `context.WithoutCancel(ctx)` for your own I/O after `next`, or hand the self-contained `Op` to a queue.
- Share bandwidth limiters at the granularity you mean to cap (per tenant/user/bucket), with a burst of 256 KiB–1 MiB.

**Don't**

- Don't query the store again in a hook for data `Metadata` already carries.
- Don't count concurrency in the `Authorizer` — it has no release point — and don't expect a hook-level cap to shield against unauthenticated floods; that layer sits in front of the gateway.
- Don't use the request context as-is for post-`next` writes — it is canceled exactly for disconnected clients, so their usage silently vanishes.
- Don't key bandwidth limiters on access key ids — keys rotate (two are live during a rotation), and temporary keys can be minted in any number, so a per-key budget is a multiplier the client controls, not a cap.
- Don't read `BytesOut` as stored bytes (it is wire transfer), and don't expect the gateway to de-duplicate client retries — each retry is a new, separately metered request.

## In front of the gateway

The handler and listener wrapping `Handler()`: everything that must run before signature verification, and the path-handling rules every hop in front must obey.

- **TLS is terminated by you, not the gateway.** `Handler()` speaks plain HTTP; serve it under TLS yourself (`http.Server.ListenAndServeTLS` around the same wrapping handler), or terminate at a fronting proxy or load balancer — a hop that then sits in the signature's verification path, with rules of its own: [Behind a TLS terminator](#behind-a-tls-terminator) below.
- Unauthenticated requests are not free. `GetKey` runs **before** signature verification — the secret is what verifies — so a well-formed `Authorization` header with a bogus access key still costs one store lookup, and a valid key with a bad signature costs the HMAC chain too. A store should negative-cache unknown key ids (weighing the TTL against how soon a freshly issued key must start working), and rate limiting belongs **outside** `Handler()`, in a wrapping handler or the fronting proxy: the `Authorizer` runs only after verification and the policies, which is too late for DoS economics.
- The same placement rule holds for the **global** concurrency cap. The per-tenant in-flight limit from [Hooks and metering](#hooks-and-metering) only sees requests that verified and passed the policies; the flood that never authenticates — bogus key ids, broken signatures, each still costing a store lookup and HMAC work — never reaches a hook. Cap total connections at the listener (`netutil.LimitListener`), or total in-flight requests with a counting wrapper in the same handler that rate-limits. The two layers do different jobs: the interceptor keeps tenants fair with each other, the front cap keeps the process alive.
- That same wrapping layer is where a health-check endpoint and a request body cap (`http.MaxBytesReader`) go. To the gateway every path is S3 — `GET /` is an authenticated ListBuckets — and it deliberately caps nothing itself, for the same reason it sets no read or write timeouts.

- **The wrapper must be a plain `http.Handler`, never `http.ServeMux`** — any version, patterns or not. ServeMux canonicalizes request paths: it collapses `//`, resolves `.` and `..` segments, and answers a non-canonical path with a **301 redirect** to the cleaned one. Both halves break S3. Object keys are opaque strings — `a//b`, `a/./b` and `a/../b` name three distinct objects, so the "cleaned" path is a *different key*. And the SigV4 signature commits to the path exactly as the client signed it, so a client that follows the redirect re-requests a path it never signed and fails verification (this is why the gateway itself routes with a single catch-all and manual dispatch, and why the nginx example in [Behind a TLS terminator](#behind-a-tls-terminator) passes the request line through untouched — the same rule binds every hop in front of the gateway, proxies included). Route the few non-S3 paths by exact comparison on `r.URL.Path` inside a plain handler, as below — or serve health, metrics and pprof on a **separate listener**, which cannot collide with tenant traffic at all.

  ```go
  s3 := gw.Handler()
  h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
  	// "_" cannot appear in a bucket name, so no tenant can own this path
  	if r.URL.Path == "/_healthz" {
  		w.WriteHeader(http.StatusOK)
  		return
  	}
  	if !limiter.Allow(clientIP(r)) { // before verification — the Authorizer is too late
  		http.Error(w, "429 too many requests", http.StatusTooManyRequests)
  		return
  	}
  	r.Body = http.MaxBytesReader(w, r.Body, 5<<30) // 5 GiB, the largest accepted upload
  	// behind a proxy, also rewrite r.RemoteAddr to the real client here:
  	// it is what the access log records and aws:SourceIp conditions evaluate
  	s3.ServeHTTP(w, r)
  })
  log.Fatal(http.ListenAndServe(":8080", h))
  ```
- The signing region of a request is taken from its credential scope, so by default **any region verifies** — the signature commits to the region either way, and a front endpoint has no inherent region. A multi-region deployment should pin each endpoint with `SetRegion`: not for signature integrity, but so a client pointed at the wrong regional endpoint fails fast with AWS's own error (`AuthorizationHeaderMalformed`, naming the expected region) instead of silently being served cross-region, and so a leaked derived signing key stays scoped to its region as SigV4 intends. The pinned region is also what GetBucketLocation and HeadBucket report, keeping region discovery consistent with what the verifier accepts.

### Behind a TLS terminator

The gateway serves plain HTTP, so a real deployment puts a reverse proxy in front of it. SigV4 signs the request itself, which makes that proxy part of the verification path: anything it rewrites, the signature no longer covers. The rules below hold for any terminator; nginx and HAProxy configurations implementing them follow.

**What breaks every request.** Verification re-signs the request as it arrived, from `RequestURI`, the `Host` header and the headers the client listed as signed:

- **Preserve `Host`.** It is signed. CloudFront sends the *origin's* hostname unless the Host header is forwarded, which fails every signature; ALB preserves it.
- **Pass the request URI byte-for-byte** — no normalization, no re-encoding, no merging of `//`. Beyond the signature, this decides what the object key *is*: `a//b` and `a/b` are different keys, and `%2F` in a key is not a separator.
- **Do not alter signed headers.** Adding headers is safe — `X-Forwarded-*` is not signed — but rewriting or dropping one the client signed is not.

**What breaks large transfers.** The gateway deliberately sets no read or write timeout so uploads and downloads are not cut off mid-stream; the proxy in front usually does, and often buffers:

- Stream request and response bodies; never spool them to the proxy's disk or memory first.
- Do not cap the body size below what you accept — a single PUT can be 5 GiB.
- Raise stream timeouts well above the slowest transfer you expect.
- Do not compress responses, and let `Expect: 100-continue` through — the AWS SDKs use it for uploads.

**What corrupts accounting.** A proxy that resends a failed request to another upstream turns one client PUT into two gateway requests — a duplicate upload, metered twice, and the gateway cannot tell them apart. Retrying a *connection* that never carried the request is fine; resending a request is not.

**What you lose.** Every request now arrives from the proxy, so the `RemoteAddr` the gateway sees is the proxy's — and `RemoteAddr` is not just the access-log field: it is the source address that bucket-policy [IP conditions](s3-api.md#ip-address-conditions) evaluate. The gateway does not interpret `X-Forwarded-For` — how many hops to trust is a property of the deployment, not of the gateway — so recovering the client address is yours, and there are two ways:

- **Prefer the PROXY protocol** when the hop in front can speak it (HAProxy's `send-proxy`, an AWS NLB, nginx's `stream` module): wrap the `net.Listener` with a PROXY-protocol-aware one (e.g. `github.com/pires/go-proxyproto`) before handing it to `http.Server.Serve`, and `RemoteAddr` carries the client address before HTTP is even parsed — the log and IP conditions need nothing further. The trust model is what makes it preferable: only the direct peer can send the header, and the listener's policy restricts which peer addresses may — one connection hop, no header parsing. **Restrict it**: a listener that accepts the PROXY header from anyone lets any direct connection spoof its source.
- **Next, a real-client-IP header your proxy overwrites** (nginx's `X-Real-IP` via `proxy_set_header X-Real-IP $remote_addr;`, Cloudflare's `CF-Connecting-IP`, CloudFront's `CloudFront-Viewer-Address`): single-valued and set by the trusted hop itself, so there is no list to reason about — read it in a handler wrapped around `gw.Handler()` and rewrite `RemoteAddr`. Two conditions make it trustworthy: the hop must **overwrite** the header unconditionally (a proxy that passes a client-supplied value through leaves it spoofable), and the handler must trust it only on requests that actually came from your proxy — it is still an HTTP header on a port anyone might reach directly.
- **`X-Forwarded-For` is the last resort**, for chains where only the appended list survives to the gateway: rewrite `RemoteAddr` from the entry appended by **your own** trusted hop — count from the right, and never trust what arrived from the client side, since any client can write this header too. That trust arithmetic is the bug class the two options above avoid.

Left unrecovered, conditions see the proxy's address and fail closed: IP-gated `Allow`s grant nothing and `NotIpAddress` `Deny`s fire for everyone — wrong, but never silently open. Also leave the `x-amz-request-id` response header alone: it is what ties a user's report to the log line explaining it.

**The hop itself.** The scheme is not part of a SigV4 signature, so terminating TLS and forwarding over plain HTTP verifies correctly — but that hop carries object payloads and the requests authenticating them, so it belongs on a trusted network or under mTLS.

**nginx.** Every rule above is an explicit directive, and the defaults sit on the wrong side of most of them: uploads are spooled to disk, `client_max_body_size` is 1 MB, timeouts are 60 s, and `proxy_next_upstream` includes `error` and `timeout` — which resends a PUT:

```nginx
server {
    listen 443 ssl;
    server_name s3.example.com;

    merge_slashes off;            # object keys may contain //
    client_max_body_size 0;       # a single PUT can be 5 GiB
    proxy_request_buffering off;  # stream uploads rather than spool them
    proxy_http_version 1.1;

    location / {
        proxy_pass http://gateway:8080;   # no URI part: the request line is passed as sent
        proxy_set_header Host $http_host;
        proxy_set_header X-Real-IP $remote_addr;  # overwritten, never forwarded from the client
        proxy_next_upstream off;       # never resend a PUT elsewhere
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }
}
```

`proxy_pass` must have **no URI part**: with one, nginx passes the normalized path instead of the request line as sent. nginx's HTTP proxying cannot speak the PROXY protocol, so the client address travels in `X-Real-IP` — overwritten here on every request, exactly the real-IP-header contract from *What you lose* — and the wrapping handler rewrites `RemoteAddr` from it.

**HAProxy.** The defaults are mostly on the right side already: the request line and `Host` pass through untouched, bodies stream without spooling, and there is no body-size cap. What needs setting is the timeouts; what must stay off is `http-request normalize-uri` (it rewrites the path the client signed) and `option http-buffer-request` (it spools uploads). Connection-failure retries are safe — HAProxy does not resend a request whose body has started flowing — but do not add response-based `retry-on` conditions for writes:

```haproxy
defaults
    mode http
    timeout connect 5s
    timeout client  1h        # longer than the slowest transfer you accept
    timeout server  1h

frontend s3
    bind :443 ssl crt /etc/haproxy/certs/s3.example.com.pem
    default_backend gateway

backend gateway
    server gw1 gateway:8080 send-proxy-v2   # PROXY protocol: RemoteAddr is the client
```

`send-proxy-v2` pairs with the PROXY-protocol-aware listener from *What you lose* above: with both in place the gateway's `RemoteAddr` is the real client, and the access log and IP conditions need nothing further.

**Do**

- Terminate TLS in your own `http.Server` or at a fronting proxy that follows the [Behind a TLS terminator](#behind-a-tls-terminator) rules; keep the plain-HTTP leg behind it on a trusted network or under mTLS.
- Rate-limit and cap request bodies (`http.MaxBytesReader`) in the wrapping handler, before verification; negative-cache unknown access key ids in the store.
- Cap total connections or in-flight requests here (`netutil.LimitListener`, or a counter in the wrapper) — the interceptor-level cap only sees authenticated traffic.
- Serve health/metrics/pprof on a path no bucket name can collide with (`_`-prefixed) or on a separate listener.
- Behind a proxy, recover the real client address — it feeds the access log and `aws:SourceIp` conditions: prefer the PROXY protocol at the listener, then a real-IP header your proxy overwrites, and `X-Forwarded-For` only as a last resort (see [Behind a TLS terminator](#behind-a-tls-terminator)).
- Pin the accepted signing region per endpoint (`SetRegion`) in a multi-region deployment.

**Don't**

- Don't route with `http.ServeMux` or anything else that cleans paths or redirects — S3 keys and their signatures do not survive it.
- Don't let any hop rewrite the request line, its escaping, or the `x-amz-request-id` response header.
- Don't put rate limiting in the `Authorizer` — it runs after verification and the policies, which is too late for DoS economics.

## Backend clients and the gateway's caches

The gateway's outbound side: `SetClientOptions` for tuning the clients it builds, one setting the backend itself needs, and the sizing of the client and signer caches.

- Backend clients are built by the gateway, but `SetClientOptions` lets you contribute `s3.Options` to every one it builds — a custom `Retryer`, an instrumented `HTTPClient` (an otelhttp transport is how you get per-backend latency and retry metrics, which the hooks cannot see), timeouts. The hook receives the backend definition, so options can differ per backend, and runs after the gateway's own settings — which are load-bearing for non-AWS backends, so override them only knowingly. Set it before serving: clients are cached per backend and keep the options they were built with.

  ```go
  // must be deterministic per backend: a cached client is reused without
  // consulting the hook again
  gw.SetClientOptions(func(b *store.Backend) []func(*s3.Options) {
  	return []func(*s3.Options){func(o *s3.Options) {
  		o.HTTPClient = &http.Client{ // per-backend latency/retry metrics live here
  			Transport: otelhttp.NewTransport(http.DefaultTransport),
  		}
  		o.Retryer = retry.AddWithMaxAttempts(retry.NewStandard(), 2)
  	}}
  })
  ```
- One backend requirement that is not a client option: **disable Nagle on the backend's frontend** — for Ceph RGW, `tcp_nodelay=1` in `rgw_frontends` (`rgw_frontend_extra_args` under cephadm). RGW's beast frontend leaves Nagle on by default and writes response headers and body separately, so any response body smaller than one MSS stalls ~40ms against the gateway's delayed ACK before its first byte is sent. The reach depends on the MSS: ~1.4KB bodies on a standard 1500 MTU, but **~9KB with jumbo frames** — on a datacenter network that is every small-object GET, at +40ms each (measured: 16KiB GET 43ms → 1.1ms once set). Go-based backends (versitygw) and AWS S3 are unaffected, as is the gateway's own front side — Go's HTTP server sets TCP_NODELAY on accepted connections.
- What the gateway does cache is derived from definitions, never a definition itself, and is bounded: backend **clients** (one per distinct endpoint/credentials, LRU, default 128 — `SetClientCacheSize`) and one SigV4 **signer** per access key (default 512 slots — `SetSignerCacheSize`). Size them to the number of distinct backends and of access keys active at once; an evicted entry is rebuilt on its next request, so undersizing costs latency, not correctness.
- Whether they *are* sized right is answerable: `ClientCacheStats` / `SignerCacheStats` return snapshots (hits, misses, evictions, len, capacity — monotonic counters, poll them from your metrics collector). A rising eviction rate with `Len` near `Capacity` means the cache is too small. The signer cache's evictions are collision displacements: a high rate with `Len` well **under** `Capacity` means hot keys sharing a slot by hash luck, which more slots make improbable.

**Do**

- Set `SetClientOptions` before serving, and keep it deterministic per backend — a cached client never consults it again.
- Instrument the backend `HTTPClient` (e.g. an otelhttp transport) for per-backend latency and retry metrics; the hooks cannot see them.
- Set `tcp_nodelay=1` on a Ceph RGW backend's frontend; without it small-object GETs stall ~40ms per request.
- Size the caches to what is active at once and poll the stats to verify.

**Don't**

- Don't override the gateway's own client settings casually — the unsigned-payload and checksum settings are load-bearing for non-AWS backends.
- Don't change client options at runtime and expect existing clients to pick them up; they keep what they were built with.

## The other packages

The other packages are usable on their own: `sigv4` (server-side SigV4 verification and `aws-chunked` decoding), `policy` (AWS-style policy evaluation), `s3err`, `s3xml`, `checksum` and `cors`. `checksum`, `policy` and `s3xml` depend only on the standard library.
