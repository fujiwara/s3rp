package s3gw

import (
	"net/http"

	"github.com/fujiwara/s3rp/s3err"

	"github.com/fujiwara/s3rp/policy"
	"github.com/fujiwara/s3rp/store"
)

// principalFor is the identity string the bucket policy is evaluated with:
// the plain user name for the bucket's own tenant, "tenant/user" for a
// requester from another tenant. The two forms cannot collide — a user name
// never contains "/".
func principalFor(vr *verifiedRequest, b *store.Bucket) string {
	if b.Tenant == vr.Tenant {
		return vr.User
	}
	return vr.Tenant + "/" + vr.User
}

// authorize evaluates the bucket policy for the authenticated user
// performing an action on a resource ("bucket" or "bucket/key").
//
// The baseline depends on who asks. A user of the bucket's own tenant has
// full access, and an explicit Deny in the bucket policy restricts it. A
// user of another tenant has no access, and only an Allow naming their
// qualified principal ("tenant/user") grants it — Deny still wins over
// Allow, so a matching Deny cuts a cross-tenant grant too.
func (g *Gateway) authorize(vr *verifiedRequest, b *store.Bucket, action, resource string) *s3err.Error {
	// the user's identity policy gates the action first (default allow all);
	// then the bucket policy decides by tenant.
	if !vr.UserPolicy.Allows(action) {
		return s3err.AccessDenied()
	}
	if b.Tenant != vr.Tenant {
		if b.Policy == nil || b.Policy.Evaluate(principalFor(vr, b), action, resource) != policy.Allow {
			return s3err.AccessDenied()
		}
		return nil
	}
	if b.Policy != nil && b.Policy.Evaluate(vr.User, action, resource) == policy.Deny {
		return s3err.AccessDenied()
	}
	return nil
}

// perObjectAuthorizer pre-computes the resource-independent parts of
// authorization once — the user policy for the action, and the bucket
// policy's statements that match this user and action — so a per-object
// operation (DeleteObjects) evaluates only the resource for each key instead
// of re-running the whole check per object. Only the resource varies per key.
type perObjectAuthorizer struct {
	denyAll bool                 // no resource can be allowed for this action
	eval    policy.DenyEvaluator // bucket-policy resource-only deny evaluator
	// cross-tenant requesters run under a default-deny baseline: each
	// resource must additionally match an Allow statement.
	requireAllow bool
	allow        policy.AllowEvaluator
}

// perObjectAuthorizer builds the authorizer for one action, running the
// resource-independent checks a single time.
func (g *Gateway) perObjectAuthorizer(vr *verifiedRequest, b *store.Bucket, action string) perObjectAuthorizer {
	if !vr.UserPolicy.Allows(action) {
		return perObjectAuthorizer{denyAll: true}
	}
	a := perObjectAuthorizer{}
	if b.Tenant != vr.Tenant {
		if b.Policy == nil {
			return perObjectAuthorizer{denyAll: true}
		}
		principal := principalFor(vr, b)
		a.allow = b.Policy.AllowEvaluatorFor(principal, action)
		if a.allow.AlwaysDenies() {
			return perObjectAuthorizer{denyAll: true}
		}
		a.requireAllow = true
		a.eval = b.Policy.DenyEvaluatorFor(principal, action)
		return a
	}
	if b.Policy != nil {
		a.eval = b.Policy.DenyEvaluatorFor(vr.User, action)
	}
	return a
}

func (a perObjectAuthorizer) denies(resource string) bool {
	if a.denyAll {
		return true
	}
	if a.requireAllow && !a.allow.Allows(resource) {
		return true
	}
	return a.eval.Denies(resource)
}

// allowsEverything reports that no resource can be denied for this action, so
// the caller may skip the per-object check altogether. A cross-tenant
// requester never qualifies: their baseline is deny, so each resource must
// be tested against the Allow patterns.
func (a perObjectAuthorizer) allowsEverything() bool {
	return !a.denyAll && !a.requireAllow && a.eval.AlwaysAllows()
}

// getBucketPolicy returns the raw bucket policy JSON.
func (g *Gateway) getBucketPolicy(c *opCtx) error {
	w, rt := c.w, c.rt
	if rt.cfg.PolicyText == "" {
		return s3err.New(http.StatusNotFound, "NoSuchBucketPolicy",
			"The bucket policy does not exist")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(rt.cfg.PolicyText))
	return nil
}
