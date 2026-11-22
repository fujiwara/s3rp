package policy_test

import (
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/fujiwara/s3rp/policy"
)

// The reference model: the policy semantics written as their definition,
// with no precompilation, no early exit and no shared code with the
// evaluator — it reads only the exported fields of a Policy. The fuzz
// targets hold every evaluation path (Decide, the Deny/Allow evaluators,
// MentionsPrincipal, UserPolicy.Decide) to it. Keep it naive: its value is
// that it can be checked by reading, so an optimization belongs in the
// evaluator and never here.

// modelGlob is the wildcard definition as a recurrence over runes: "*"
// matches any run of characters, "?" exactly one, anything else itself.
func modelGlob(pattern, value string) bool {
	p, v := []rune(pattern), []rune(value)
	// m[i][j]: p[i:] matches v[j:]
	m := make([][]bool, len(p)+1)
	for i := range m {
		m[i] = make([]bool, len(v)+1)
	}
	m[len(p)][len(v)] = true
	for i, v0 := range slices.Backward(p) {
		for j := len(v); j >= 0; j-- {
			switch {
			case v0 == '*':
				m[i][j] = m[i+1][j] || (j < len(v) && m[i][j+1])
			case j < len(v) && (v0 == '?' || v0 == v[j]):
				m[i][j] = m[i+1][j+1]
			}
		}
	}
	return m[0][0]
}

func modelAnyGlob(patterns []string, value string, fold bool) bool {
	for _, p := range patterns {
		if fold {
			p, value = strings.ToLower(p), strings.ToLower(value)
		}
		if modelGlob(p, value) {
			return true
		}
	}
	return false
}

// modelCovers: an entry names the principal when the tenants are equal and
// the entry's user is the principal's or "*".
func modelCovers(entries []string, principal string) bool {
	tenant, user, _ := strings.Cut(principal, "/")
	for _, e := range entries {
		et, eu, _ := strings.Cut(e, "/")
		if et == tenant && (eu == "*" || eu == user) {
			return true
		}
	}
	return false
}

func modelPrincipal(st *policy.Statement, principal string) bool {
	if st.Principal != nil {
		return st.Principal.All || modelCovers(st.Principal.Users, principal)
	}
	return !modelCovers(st.NotPrincipal.Users, principal)
}

// modelContains reports whether a condition value — an address, or a CIDR
// block — includes src. An IPv4-mapped IPv6 value or source denotes its
// IPv4 addresses; otherwise the families never meet.
func modelContains(value string, src netip.Addr) (bool, error) {
	addrText, bitsText, isCIDR := strings.Cut(value, "/")
	a, err := netip.ParseAddr(addrText)
	if err != nil {
		return false, err
	}
	bits := a.BitLen()
	if isCIDR {
		if bits, err = strconv.Atoi(bitsText); err != nil {
			return false, err
		}
	}
	if a.Is4In6() {
		if bits < 96 {
			return false, fmt.Errorf("%q is wider than the IPv4-mapped range", value)
		}
		a, bits = a.Unmap(), bits-96
	}
	src = src.Unmap()
	if !src.IsValid() || a.Is4() != src.Is4() {
		return false, nil
	}
	as, ss := a.AsSlice(), src.AsSlice()
	for i := range bits {
		mask := byte(0x80) >> (i % 8)
		if as[i/8]&mask != ss[i/8]&mask {
			return false, nil
		}
	}
	return true, nil
}

func modelAnyContains(t testing.TB, values []string, src netip.Addr) bool {
	for _, v := range values {
		in, err := modelContains(v, src)
		if err != nil {
			t.Fatalf("condition value the model cannot read: %v", err)
		}
		if in {
			return true
		}
	}
	return false
}

// modelCondition: every operator present must hold. An unknown source is in
// no range, and still fails closed: IpAddress does not hold, NotIpAddress
// does.
func modelCondition(t testing.TB, c *policy.Condition, src netip.Addr) bool {
	if c == nil {
		return true
	}
	known := src.IsValid()
	if len(c.IPAddress) > 0 && !(known && modelAnyContains(t, c.IPAddress, src)) {
		return false
	}
	if len(c.NotIPAddress) > 0 && known && modelAnyContains(t, c.NotIPAddress, src) {
		return false
	}
	return true
}

type modelRequest struct {
	principal, action, resource string
	src                         netip.Addr
}

func (r modelRequest) String() string {
	return fmt.Sprintf("principal=%q action=%q resource=%q source=%v", r.principal, r.action, r.resource, r.src)
}

func modelApplies(t testing.TB, st *policy.Statement, r modelRequest) bool {
	return modelPrincipal(st, r.principal) &&
		modelCondition(t, st.Condition, r.src) &&
		modelAnyGlob(st.Action, r.action, true) &&
		modelAnyGlob(st.Resource, r.resource, false)
}

// modelFirst is the index of the first statement of the effect that applies
// to the request, -1 when none does.
func modelFirst(t testing.TB, p *policy.Policy, effect string, r modelRequest) int {
	for i := range p.Statement {
		if p.Statement[i].Effect == effect && modelApplies(t, &p.Statement[i], r) {
			return i
		}
	}
	return -1
}

// modelDecision: an applying Deny decides, else an applying Allow, else
// nothing does. deny and allow are the first applying statement of each
// effect (modelFirst).
func modelDecision(deny, allow int) policy.Decision {
	switch {
	case deny >= 0:
		return policy.Decision{Effect: policy.Deny, Statement: deny}
	case allow >= 0:
		return policy.Decision{Effect: policy.Allow, Statement: allow}
	}
	return policy.Decision{Effect: policy.None, Statement: -1}
}

// modelMentions: some Allow statement's Principal covers the principal,
// whatever its action, resource and condition.
func modelMentions(p *policy.Policy, principal string) bool {
	for i := range p.Statement {
		st := &p.Statement[i]
		if st.Effect == "Allow" && st.Principal != nil &&
			(st.Principal.All || modelCovers(st.Principal.Users, principal)) {
			return true
		}
	}
	return false
}

// modelUserDecide: an empty policy allows everything; otherwise a matching
// Deny decides, else a matching Allow, else the action is implicitly denied.
func modelUserDecide(up *policy.UserPolicy, action string) policy.Decision {
	if up == nil || len(up.Statements) == 0 {
		return policy.Decision{Effect: policy.Allow, Statement: -1}
	}
	for _, effect := range []string{"Deny", "Allow"} {
		for i, st := range up.Statements {
			if st.Effect == effect && modelAnyGlob(st.Action, action, true) {
				if effect == "Deny" {
					return policy.Decision{Effect: policy.Deny, Statement: i}
				}
				return policy.Decision{Effect: policy.Allow, Statement: i}
			}
		}
	}
	return policy.Decision{Effect: policy.Deny, Statement: -1}
}

var modelPrincipalName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]+/([a-z0-9][a-z0-9_-]+|\*)$`)

// modelWellFormed is what the evaluator and the model assume of a parsed
// policy, stated independently of the validation code: a document Parse
// accepts must satisfy it.
func modelWellFormed(p *policy.Policy, bucket string) error {
	if n := len(p.Statement); n < 1 || n > policy.MaxStatements {
		return fmt.Errorf("%d statements", n)
	}
	sids := map[string]bool{}
	for i := range p.Statement {
		st := &p.Statement[i]
		fail := func(format string, args ...any) error {
			return fmt.Errorf("statement[%d]: %s", i, fmt.Sprintf(format, args...))
		}
		if st.Sid != "" {
			if sids[st.Sid] {
				return fail("duplicate Sid %q", st.Sid)
			}
			sids[st.Sid] = true
		}
		if st.Effect != "Allow" && st.Effect != "Deny" {
			return fail("effect %q", st.Effect)
		}
		if (st.Principal == nil) == (st.NotPrincipal == nil) {
			return fail("not exactly one of Principal and NotPrincipal")
		}
		if np := st.NotPrincipal; np != nil && (st.Effect != "Deny" || np.All || len(np.Users) == 0) {
			return fail("NotPrincipal %+v with effect %s", *np, st.Effect)
		}
		for _, pr := range []*policy.Principal{st.Principal, st.NotPrincipal} {
			if pr == nil {
				continue
			}
			if pr.All == (len(pr.Users) > 0) || len(pr.Users) > policy.MaxPrincipalUsers {
				return fail("principal %+v", *pr)
			}
			for _, u := range pr.Users {
				if !modelPrincipalName.MatchString(u) {
					return fail("principal name %q", u)
				}
			}
		}
		if n := len(st.Action); n < 1 || n > policy.MaxActionsPerStatement {
			return fail("%d actions", n)
		}
		for _, a := range st.Action {
			if len(a) > policy.MaxPatternLen || !strings.HasPrefix(strings.ToLower(a), "s3:") {
				return fail("action %q", a)
			}
		}
		if n := len(st.Resource); n < 1 || n > policy.MaxResourcesPerStatement {
			return fail("%d resources", n)
		}
		for _, r := range st.Resource {
			if len(r) > policy.MaxPatternLen || (r != bucket && !strings.HasPrefix(r, bucket+"/")) {
				return fail("resource %q", r)
			}
		}
		if c := st.Condition; c != nil {
			if len(c.IPAddress)+len(c.NotIPAddress) == 0 ||
				len(c.IPAddress) > policy.MaxConditionValues || len(c.NotIPAddress) > policy.MaxConditionValues {
				return fail("condition with %d and %d values", len(c.IPAddress), len(c.NotIPAddress))
			}
			for _, v := range append(append([]string{}, c.IPAddress...), c.NotIPAddress...) {
				if _, err := modelContains(v, netip.Addr{}); err != nil {
					return fail("condition value: %v", err)
				}
			}
		}
	}
	return nil
}

// probes are the requests one policy is compared on: every combination of
// the four lists.
type probes struct {
	principals, actions, resources []string
	sources                        []netip.Addr
}

// checkAgainstModel holds every evaluation path of impl to the model's
// reading of spec — the same policy, as generated or as parsed. It stops at
// the first difference: one counterexample is the finding.
func checkAgainstModel(t *testing.T, impl, spec *policy.Policy, pr probes) {
	t.Helper()
	for _, principal := range pr.principals {
		mentions := modelMentions(spec, principal)
		if got := impl.MentionsPrincipal(principal); got != mentions {
			t.Fatalf("MentionsPrincipal(%q) = %v, model says %v", principal, got, mentions)
		}
		for _, action := range pr.actions {
			for _, src := range pr.sources {
				rc := policy.RequestContext{SourceIP: src}
				deny := impl.DenyEvaluatorFor(principal, action, rc)
				allow := impl.AllowEvaluatorFor(principal, action, rc)
				for _, resource := range pr.resources {
					r := modelRequest{principal, action, resource, src}
					wantDeny := modelFirst(t, spec, "Deny", r)
					wantAllow := modelFirst(t, spec, "Allow", r)
					want := modelDecision(wantDeny, wantAllow)
					if got := impl.Decide(principal, action, resource, rc); got != want {
						t.Fatalf("Decide(%v) = %+v, model says %+v", r, got, want)
					}
					// the action is case-insensitive as a whole
					if got := impl.Decide(principal, strings.ToUpper(action), resource, rc); got != want {
						t.Fatalf("Decide(%v) with the action upper-cased = %+v, model says %+v", r, got, want)
					}
					if want.Effect == policy.Allow && !mentions {
						t.Fatalf("%v is allowed to a principal the visibility gate hides the bucket from", r)
					}

					if got := deny.Denies(resource); got != (wantDeny >= 0) {
						t.Fatalf("DenyEvaluator.Denies(%v) = %v, model's first Deny is %d", r, got, wantDeny)
					}
					if got := deny.DenyingStatement([]rune(resource)); got != wantDeny {
						t.Fatalf("DenyEvaluator.DenyingStatement(%v) = %d, model says %d", r, got, wantDeny)
					}
					if wantDeny >= 0 && deny.AlwaysAllows() {
						t.Fatalf("DenyEvaluator.AlwaysAllows skips the Deny of %v (statement %d)", r, wantDeny)
					}

					if got := allow.Allows(resource); got != (wantAllow >= 0) {
						t.Fatalf("AllowEvaluator.Allows(%v) = %v, model's first Allow is %d", r, got, wantAllow)
					}
					if wantAllow >= 0 && allow.AlwaysDenies() {
						t.Fatalf("AllowEvaluator.AlwaysDenies refuses the Allow of %v (statement %d)", r, wantAllow)
					}
				}
			}
		}
	}
}

// The model is only worth comparing against if it is right; these are the
// answers the documentation gives.
func TestModel(t *testing.T) {
	globs := []struct {
		pattern, value string
		want           bool
	}{
		{"", "", true},
		{"", "a", false},
		{"*", "", true},
		{"*", "a/b", true},
		{"?", "", false},
		{"?", "é", true},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"b/*.jpg", "b/x/y.jpg", true},
		{"b/*.jpg", "b/x/y.png", false},
		{"*a*b", "xaxbxb", true},
		{"*a*b", "xbxa", false},
		{"a*", "a", true},
		{"a**", "a", true},
		{"a*?", "a", false},
	}
	for _, tc := range globs {
		if got := modelGlob(tc.pattern, tc.value); got != tc.want {
			t.Errorf("modelGlob(%q, %q) = %v, want %v", tc.pattern, tc.value, got, tc.want)
		}
	}

	ranges := []struct {
		value, src string
		want       bool
	}{
		{"10.0.0.0/8", "10.1.2.3", true},
		{"10.0.0.0/8", "11.1.2.3", false},
		{"10.0.0.0/8", "::ffff:10.1.2.3", true},
		{"10.1.2.3", "10.1.2.3", true},
		{"10.1.2.3", "10.1.2.4", false},
		{"0.0.0.0/0", "2001:db8::1", false},
		{"::/0", "10.1.2.3", false},
		{"2001:db8::/32", "2001:db8::1", true},
		{"2001:db8::/32", "2001:db9::1", false},
		{"::ffff:10.0.0.0/104", "10.1.2.3", true},
		{"::ffff:10.0.0.0/104", "11.1.2.3", false},
		{"::ffff:10.1.2.3", "10.1.2.3", true},
	}
	for _, tc := range ranges {
		got, err := modelContains(tc.value, netip.MustParseAddr(tc.src))
		if err != nil || got != tc.want {
			t.Errorf("modelContains(%q, %s) = %v, %v, want %v", tc.value, tc.src, got, err, tc.want)
		}
	}

	covers := []struct {
		entry, principal string
		want             bool
	}{
		{"ta/alice", "ta/alice", true},
		{"ta/alice", "ta/al", false},
		{"ta/al", "ta/alice", false},
		{"ta/*", "ta/alice", true},
		{"ta/*", "tab/alice", false},
		{"tab/*", "ta/alice", false},
	}
	for _, tc := range covers {
		if got := modelCovers([]string{tc.entry}, tc.principal); got != tc.want {
			t.Errorf("modelCovers(%q, %q) = %v, want %v", tc.entry, tc.principal, got, tc.want)
		}
	}
}
