package s3gw

import (
	"net/http"

	"github.com/fujiwara/s3rp/s3err"

	"github.com/fujiwara/s3rp/policy"
	"github.com/fujiwara/s3rp/store"
)

// authorize evaluates the bucket policy for the authenticated user
// performing an action on a resource ("bucket" or "bucket/key"). The
// principal is always the qualified "tenant/user" form (vr.principal).
//
// The baseline depends on who asks. A user of the bucket's own tenant has
// full access, and an explicit Deny in the bucket policy restricts it. A
// user of another tenant has no access, and only a matching Allow grants
// it — Deny still wins over Allow, so a matching Deny cuts a cross-tenant
// grant too.
//
// Every refusal carries a *DenyReason as its cause, for the observer only;
// the allow path builds nothing.
func (g *Gateway) authorize(vr *verifiedRequest, b *store.Bucket, action, resource string) *s3err.Error {
	// the user's identity policy gates the action first (default allow all);
	// then the bucket policy decides by tenant.
	if d := vr.UserPolicy.Decide(action); d.Effect != policy.Allow {
		return s3err.AccessDenied().WithCause(userPolicyReason(vr.principal(), action, d))
	}
	if b.Tenant != vr.Tenant {
		if b.Policy == nil {
			return s3err.AccessDenied().WithCause(bucketPolicyReason(nil, vr.principal(), action, resource, policy.Decision{Statement: -1}))
		}
		if d := b.Policy.Decide(vr.principal(), action, resource, vr.requestContext()); d.Effect != policy.Allow {
			return s3err.AccessDenied().WithCause(bucketPolicyReason(b.Policy, vr.principal(), action, resource, d))
		}
		return nil
	}
	if b.Policy != nil {
		if d := b.Policy.Decide(vr.principal(), action, resource, vr.requestContext()); d.Effect == policy.Deny {
			return s3err.AccessDenied().WithCause(bucketPolicyReason(b.Policy, vr.principal(), action, resource, d))
		}
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

	// for explaining a denial (denyReason), never consulted by denies
	principal, action string
	policy            *policy.Policy
	userDecision      policy.Decision // the user policy's verdict on the action
}

// perObjectAuthorizer builds the authorizer for one action, running the
// resource-independent checks a single time.
func (g *Gateway) perObjectAuthorizer(vr *verifiedRequest, b *store.Bucket, action string) perObjectAuthorizer {
	a := perObjectAuthorizer{principal: vr.principal(), action: action, policy: b.Policy}
	a.userDecision = vr.UserPolicy.Decide(action)
	if a.userDecision.Effect != policy.Allow {
		a.denyAll = true
		return a
	}
	rc := vr.requestContext()
	if b.Tenant != vr.Tenant {
		a.requireAllow = true
		if b.Policy == nil {
			a.denyAll = true
			return a
		}
		a.allow = b.Policy.AllowEvaluatorFor(vr.principal(), action, rc)
		if a.allow.AlwaysDenies() {
			a.denyAll = true
			return a
		}
		a.eval = b.Policy.DenyEvaluatorFor(vr.principal(), action, rc)
		return a
	}
	if b.Policy != nil {
		a.eval = b.Policy.DenyEvaluatorFor(vr.principal(), action, rc)
	}
	return a
}

func (a perObjectAuthorizer) denies(resource string) bool {
	if a.denyAll {
		return true
	}
	if !a.requireAllow {
		return a.eval.Denies(resource)
	}
	// testing both the Allow and the Deny side: convert the resource once
	r := []rune(resource)
	return !a.allow.AllowsRunes(r) || a.eval.DeniesRunes(r)
}

// denyReason explains why denies(resource) was true. It is a second scan of
// the Deny patterns, paid only for a denied key, so the per-object path
// stays a single scan for keys that go through.
func (a perObjectAuthorizer) denyReason(resource string) *DenyReason {
	if a.userDecision.Effect != policy.Allow {
		return userPolicyReason(a.principal, a.action, a.userDecision)
	}
	if i := a.eval.DenyingStatement([]rune(resource)); i >= 0 {
		return denyStatementReason(a.policy, a.principal, a.action, resource, i)
	}
	// nothing denied it explicitly, so a cross-tenant baseline did
	return &DenyReason{Layer: LayerCrossTenant, Statement: -1, Principal: a.principal, Action: a.action, Resource: resource}
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
