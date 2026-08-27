package s3gw

import (
	"fmt"

	"github.com/fujiwara/s3rp/policy"
	"github.com/fujiwara/s3rp/store"
)

// DenyReason is the cause behind an AccessDenied the gateway decided
// itself: which layer refused and, when a statement did, which one. It
// reaches the observer as RequestInfo.Err (errors.As) and is rendered by
// its Error text; the client is told only AccessDenied, as on AWS — naming
// the statement would show a cross-tenant requester the shape of a policy
// it cannot read.
type DenyReason struct {
	// Layer is where the refusal was decided: one of the Layer* constants.
	Layer string `json:"layer"`
	// Statement is the index of the deciding statement in that layer's
	// policy, -1 when none matched — the implicit deny of a user policy,
	// or a cross-tenant request no Allow covers.
	Statement int `json:"statement"`
	// Sid is the deciding statement's Sid when it has one (bucket policies
	// only; unique within a policy).
	Sid       string `json:"sid,omitempty"`
	Principal string `json:"principal"`
	Action    string `json:"action,omitempty"`
	Resource  string `json:"resource,omitempty"`

	err error // wrapped store error, for the copy-source layer
}

const (
	// LayerUserPolicy: the user's identity policy denied the action, by a
	// Deny statement or by matching no Allow.
	LayerUserPolicy = "user-policy"
	// LayerBucketPolicy: a Deny statement of the bucket policy matched.
	LayerBucketPolicy = "bucket-policy"
	// LayerCrossTenant: another tenant's bucket, and no Allow statement
	// covered the principal, action and resource (the default-deny
	// baseline for cross-tenant requests).
	LayerCrossTenant = "cross-tenant"
	// LayerVisibility: another tenant's bucket whose policy never mentions
	// the principal — refused before any operation, indistinguishable from
	// a nonexistent bucket to the client.
	LayerVisibility = "visibility"
	// LayerCopySource: a copy source that does not resolve within the
	// requester's own tenant (copying from another tenant's bucket is
	// impossible by construction).
	LayerCopySource = "copy-source"
)

func (r *DenyReason) Error() string {
	stmt := fmt.Sprintf("statement[%d]", r.Statement)
	if r.Sid != "" {
		stmt += fmt.Sprintf(" %q", r.Sid)
	}
	switch r.Layer {
	case LayerUserPolicy:
		if r.Statement < 0 {
			return fmt.Sprintf("user policy allows nothing for %s (%s)", r.Action, r.Principal)
		}
		return fmt.Sprintf("user policy %s denies %s (%s)", stmt, r.Action, r.Principal)
	case LayerBucketPolicy:
		return fmt.Sprintf("bucket policy %s denies %s on %s (%s)", stmt, r.Action, r.Resource, r.Principal)
	case LayerCrossTenant:
		return fmt.Sprintf("bucket policy grants %s nothing for %s on %s (cross-tenant)", r.Principal, r.Action, r.Resource)
	case LayerVisibility:
		return fmt.Sprintf("bucket %s belongs to another tenant and its policy never mentions %s", r.Resource, r.Principal)
	case LayerCopySource:
		return fmt.Sprintf("copy source %s does not resolve in tenant of %s: %v", r.Resource, r.Principal, r.err)
	}
	return fmt.Sprintf("%s denies %s on %s (%s)", r.Layer, r.Action, r.Resource, r.Principal)
}

// Unwrap exposes the store error behind a copy-source refusal, so
// errors.Is(err, store.ErrNotFound) keeps working.
func (r *DenyReason) Unwrap() error { return r.err }

// userPolicyReason explains a user-policy refusal from its decision.
func userPolicyReason(principal, action string, d policy.Decision) *DenyReason {
	return &DenyReason{Layer: LayerUserPolicy, Statement: d.Statement, Principal: principal, Action: action}
}

// bucketPolicyReason explains a bucket-policy refusal: the Deny statement
// that matched, or — for a cross-tenant request — the absence of an Allow.
func bucketPolicyReason(p *policy.Policy, principal, action, resource string, d policy.Decision) *DenyReason {
	if d.Effect != policy.Deny {
		return &DenyReason{Layer: LayerCrossTenant, Statement: -1, Principal: principal, Action: action, Resource: resource}
	}
	return denyStatementReason(p, principal, action, resource, d.Statement)
}

// denyStatementReason explains a refusal by the bucket policy's Deny
// statement at index i.
func denyStatementReason(p *policy.Policy, principal, action, resource string, i int) *DenyReason {
	r := &DenyReason{Layer: LayerBucketPolicy, Statement: i, Principal: principal, Action: action, Resource: resource}
	if p != nil && i >= 0 && i < len(p.Statement) {
		r.Sid = p.Statement[i].Sid
	}
	return r
}

func visibilityReason(principal, bucket string) *DenyReason {
	return &DenyReason{Layer: LayerVisibility, Statement: -1, Principal: principal, Resource: bucket}
}

func copySourceReason(principal, resource string, err error) *DenyReason {
	if err == nil {
		err = store.ErrNotFound
	}
	return &DenyReason{Layer: LayerCopySource, Statement: -1, Principal: principal, Action: "s3:GetObject", Resource: resource, err: err}
}

// Denial is one deciding statement's share of the per-key refusals inside
// a DeleteObjects that otherwise succeeded: how many keys it denied, with
// the first of them as the reason's Resource.
type Denial struct {
	Reason DenyReason `json:"reason"`
	Keys   int        `json:"keys"`
}

// addDenial folds one per-key refusal into the op's summary, one entry per
// (layer, statement) — bounded by the policy's statement cap, not by the
// number of keys.
func (op *Op) addDenial(r *DenyReason) {
	if op == nil {
		return
	}
	for i := range op.Denials {
		d := &op.Denials[i]
		if d.Reason.Layer == r.Layer && d.Reason.Statement == r.Statement {
			d.Keys++
			return
		}
	}
	op.Denials = append(op.Denials, Denial{Reason: *r, Keys: 1})
}
