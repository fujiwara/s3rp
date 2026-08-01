package s3gw

import (
	"net/http"

	"github.com/fujiwara/s3rp/s3err"

	"github.com/fujiwara/s3rp/policy"
	"github.com/fujiwara/s3rp/store"
)

// authorize evaluates the bucket policy for the authenticated user
// performing an action on a resource ("bucket" or "bucket/key").
//
// The baseline is that every user of a tenant has full access to the
// tenant's buckets; an explicit Deny in the bucket policy restricts it.
// Allow statements are accepted but have no effect yet — they will become
// meaningful with anonymous and cross-tenant access.
func (g *Gateway) authorize(vr *verifiedRequest, b *store.Bucket, action, resource string) *s3err.Error {
	// the user's identity policy gates the action first (default allow all);
	// then the bucket policy may add a resource-specific Deny.
	if !vr.UserPolicy.Allows(action) {
		return s3err.AccessDenied()
	}
	if b.Policy != nil && b.Policy.Evaluate(vr.User, action, resource) == policy.Deny {
		return s3err.AccessDenied()
	}
	return nil
}

// perObjectAuthorizer pre-computes the resource-independent parts of
// authorization once — the user policy for the action, and the bucket
// policy's Deny statements that match this user and action — so a per-object
// operation (DeleteObjects) evaluates only the resource for each key instead
// of re-running the whole check per object. Only the resource varies per key.
type perObjectAuthorizer struct {
	denyAll bool                 // user policy denies the action for every resource
	eval    policy.DenyEvaluator // bucket-policy resource-only deny evaluator
}

// perObjectAuthorizer builds the authorizer for one action, running the
// resource-independent checks a single time.
func (g *Gateway) perObjectAuthorizer(vr *verifiedRequest, b *store.Bucket, action string) perObjectAuthorizer {
	if !vr.UserPolicy.Allows(action) {
		return perObjectAuthorizer{denyAll: true}
	}
	a := perObjectAuthorizer{}
	if b.Policy != nil {
		a.eval = b.Policy.DenyEvaluatorFor(vr.User, action)
	}
	return a
}

// denies reports whether the given resource is denied for this action.
func (a perObjectAuthorizer) denies(resource string) bool {
	return a.denyAll || a.eval.Denies(resource)
}

// allowsEverything reports that no resource can be denied for this action, so
// the caller may skip the per-object check altogether.
func (a perObjectAuthorizer) allowsEverything() bool {
	return !a.denyAll && a.eval.AlwaysAllows()
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
