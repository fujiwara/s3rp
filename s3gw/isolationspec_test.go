package s3gw_test

import (
	"fmt"
	"strings"

	"github.com/fujiwara/s3rp/policy"
	"github.com/fujiwara/s3rp/s3op"
)

// The tenant isolation specification: who may do what to whose bucket,
// written over a deliberately small policy language — a pattern is exact or
// one prefix followed by "*" — so that it needs no policy evaluator of its
// own and can be checked by reading. The tests enumerate worlds in that
// language and hold the gateway to this file.
//
//   - A bucket is visible to its owning tenant's users, and to a user of
//     another tenant only if an Allow statement of its policy names them.
//   - On a visible bucket, a user of the owning tenant may do anything no
//     Deny statement applies to; a user of another tenant only what an Allow
//     statement applies to and no Deny does.
//   - Whatever the bucket says, the user's own policy must allow the action.
//   - A copy reads its source from a bucket of the requester's own tenant,
//     never another's, whatever that bucket grants.

type isoUser struct {
	tenant, name string
	policy       []policy.ActionStatement
}

func (u isoUser) principal() string { return u.tenant + "/" + u.name }

// isoStatement is one statement of the small language. principal is "*",
// "tenant/user" or "tenant/*"; action and resource are exact or a prefix
// followed by "*".
type isoStatement struct {
	effect       string
	principal    string
	notPrincipal bool
	action       string
	resource     string
}

func (st isoStatement) json() string {
	principal := `{"S3RP": "` + st.principal + `"}`
	if st.principal == "*" {
		principal = `"*"`
	}
	key := "Principal"
	if st.notPrincipal {
		key = "NotPrincipal"
	}
	return fmt.Sprintf(`{"Effect": %q, %q: %s, "Action": %q, "Resource": %q}`,
		st.effect, key, principal, st.action, st.resource)
}

func isoPolicyText(statements []isoStatement) string {
	if len(statements) == 0 {
		return ""
	}
	parts := make([]string, len(statements))
	for i, st := range statements {
		parts[i] = st.json()
	}
	return `{"Statement": [` + strings.Join(parts, ", ") + `]}`
}

type isoBucket struct {
	owner  string
	policy []isoStatement
}

// isoWorld is the buckets that exist, by front name.
type isoWorld map[string]isoBucket

func specPattern(pattern, value string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(value, prefix)
	}
	return pattern == value
}

func specNames(entry string, u isoUser) bool {
	switch tenant, user, _ := strings.Cut(entry, "/"); {
	case entry == "*":
		return true
	case user == "*":
		return tenant == u.tenant
	default:
		return entry == u.principal()
	}
}

func (st isoStatement) applies(u isoUser, action, resource string) bool {
	return specNames(st.principal, u) != st.notPrincipal &&
		specPattern(st.action, action) && specPattern(st.resource, resource)
}

func specAny(statements []isoStatement, effect string, u isoUser, action, resource string) bool {
	for _, st := range statements {
		if st.effect == effect && st.applies(u, action, resource) {
			return true
		}
	}
	return false
}

func specUserAllows(u isoUser, action string) bool {
	if len(u.policy) == 0 {
		return true
	}
	allowed := false
	for _, st := range u.policy {
		for _, a := range st.Action {
			if !specPattern(a, action) {
				continue
			}
			if st.Effect == "Deny" {
				return false
			}
			allowed = true
		}
	}
	return allowed
}

func (w isoWorld) visible(u isoUser, bucket string) bool {
	b, ok := w[bucket]
	if !ok {
		return false
	}
	if b.owner == u.tenant {
		return true
	}
	for _, st := range b.policy {
		if st.effect == "Allow" && !st.notPrincipal && specNames(st.principal, u) {
			return true
		}
	}
	return false
}

func (w isoWorld) permits(u isoUser, bucket, action, resource string) bool {
	if !w.visible(u, bucket) || !specUserAllows(u, action) {
		return false
	}
	b := w[bucket]
	if specAny(b.policy, "Deny", u, action, resource) {
		return false
	}
	return b.owner == u.tenant || specAny(b.policy, "Allow", u, action, resource)
}

// isoRequest is one request to the bucket under test: an operation of the
// catalog and what about the request changes the actions it needs.
type isoRequest struct {
	op      s3op.Operation
	key     string // "" for a bucket-level request
	version bool   // names an object version
	header  string // the one action-bearing header it carries, if any
	// a copy's source
	copyBucket  string
	copyVersion bool
}

func (r isoRequest) String() string {
	s := r.op.Name
	if r.key != "" {
		s += " " + r.key
	}
	if r.version {
		s += " versionId"
	}
	if r.header != "" {
		s += " " + r.header
	}
	if r.copyBucket != "" {
		s += " from " + r.copyBucket
		if r.copyVersion {
			s += " versionId"
		}
	}
	return s
}

func versionedAction(a s3op.Authorization, version bool) string {
	if version && a.VersionAction != "" {
		return a.VersionAction
	}
	return a.Action
}

// targetActions are the actions the catalog says the request needs on the
// bucket or object it addresses.
func (r isoRequest) targetActions() []string {
	var actions []string
	for _, a := range r.op.Authorizations {
		if a.On == s3op.OnTarget && (a.Header == "" || a.Header == r.header) {
			actions = append(actions, versionedAction(a, r.version))
		}
	}
	return actions
}

func (r isoRequest) copyAction() string {
	for _, a := range r.op.Authorizations {
		if a.On == s3op.OnCopySource {
			return versionedAction(a, r.copyVersion)
		}
	}
	return ""
}

const isoCopyKey = "src/k"

// admitted reports whether the request, addressed to bucket, passes
// authorization: every action it needs is permitted, and a copy's source is
// a readable object of the requester's own tenant.
func (w isoWorld) admitted(u isoUser, bucket string, r isoRequest) bool {
	if !w.visible(u, bucket) {
		return false
	}
	resource := bucket
	if r.key != "" {
		resource += "/" + r.key
	}
	for _, action := range r.targetActions() {
		if !w.permits(u, bucket, action, resource) {
			return false
		}
	}
	if action := r.copyAction(); action != "" {
		src, ok := w[r.copyBucket]
		if !ok || src.owner != u.tenant || !w.permits(u, r.copyBucket, action, r.copyBucket+"/"+isoCopyKey) {
			return false
		}
	}
	return true
}
