// Package policy implements AWS-style bucket policy documents for s3rp.
//
// The document structure follows the AWS policy language (Version /
// Statement / Effect / Principal / Action / Resource), with two
// simplifications: principals are plain user names (no ARNs) under the
// "S3RP" key — "tenant/user" names another tenant's user, a plain name a
// user of the bucket's own tenant — and resources are plain "bucket" or
// "bucket/prefix*" strings (no ARNs). Action and Resource support the AWS
// wildcards "*" (any run of characters) and "?" (a single character).
//
// Those two syntax choices are the default Dialect; a service can accept a
// different principal key or ARN-prefixed resources by parsing with its own
// Dialect. The internal form — and evaluation — is the same either way.
package policy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Effect is the result of evaluating a policy.
type Effect int

const (
	// None means no statement matched.
	None Effect = iota
	// Allow means an Allow statement matched and no Deny did.
	Allow
	// Deny means a Deny statement matched.
	Deny
)

// principalRegexp accepts a plain user name ("alice", a user of the bucket's
// own tenant) or a tenant-qualified one ("tenant-b/alice", another tenant's
// user). Both parts use the user-name charset.
var principalRegexp = regexp.MustCompile(`^([a-z][a-z0-9_-]+/)?[a-z][a-z0-9_-]+$`)

// Structural limits on a policy document. Policies are tenant-authored, so
// they are untrusted input, and authorization runs on every request (per
// object for DeleteObjects). The dominant worst-case cost is the number of
// resource patterns matched (each adversarial pattern can force a full scan
// of a ~1 KB object key), i.e. MaxStatements*MaxResourcesPerStatement; action
// matching is against a short action string and stays cheap regardless of
// count. These caps sit well above what any real policy needs while keeping a
// worst-case evaluation to a few hundred microseconds, and are checked at
// parse/validate time.
const (
	// MaxPolicyBytes caps the raw size of a policy document, checked before
	// parsing so an oversized document is rejected without being unmarshaled
	// into memory. Matches the AWS bucket-policy size limit (20 KB).
	MaxPolicyBytes = 20 * 1024
	// MaxPrincipalUsers caps the user names in one statement's Principal or
	// NotPrincipal, bounding the per-statement principal-match cost.
	MaxPrincipalUsers = 100
	// MaxStatements caps the statements in one policy document.
	MaxStatements = 20
	// MaxActionsPerStatement caps the Action entries in one statement. Action
	// matching is cheap (short string), so this is generous.
	MaxActionsPerStatement = 30
	// MaxResourcesPerStatement caps the Resource entries in one statement.
	// Resource patterns are matched against the object key, so the total
	// (MaxStatements*MaxResourcesPerStatement) bounds the worst-case cost.
	MaxResourcesPerStatement = 10
	// MaxPatternLen caps the length (in bytes) of a single action or
	// resource pattern, bounding the per-pattern glob-match cost.
	MaxPatternLen = 128
)

// Policy is a bucket policy document.
type Policy struct {
	Version   string      `json:"Version,omitempty"`
	Statement []Statement `json:"Statement"`

	compileOnce sync.Once // precompiles statement patterns to runes on first Evaluate
}

// Statement is a single statement of a policy.
type Statement struct {
	Sid          string        `json:"Sid,omitempty"`
	Effect       string        `json:"Effect"`
	Principal    *Principal    `json:"Principal,omitempty"`
	NotPrincipal *Principal    `json:"NotPrincipal,omitempty"`
	Action       StringOrSlice `json:"Action"`
	Resource     StringOrSlice `json:"Resource"`

	// precompiled by Policy.compile: action patterns are lower-cased for
	// case-insensitive matching, resource patterns kept as-is.
	actionRunes   [][]rune
	resourceRunes [][]rune
}

// Principal is either "*" (all users of the bucket's tenant) or
// {"S3RP": user names}, where a name is plain ("alice", the bucket's own
// tenant) or tenant-qualified ("tenant-b/alice", another tenant's user).
type Principal struct {
	All   bool
	Users []string
}

func (p *Principal) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		if s != "*" {
			return fmt.Errorf("principal must be %q or an object with the S3RP key", "*")
		}
		p.All = true
		return nil
	}
	var obj struct {
		S3RP StringOrSlice `json:"S3RP"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("malformed principal: %w", err)
	}
	if len(obj.S3RP) == 0 {
		return fmt.Errorf("principal must be %q or an object with the S3RP key", "*")
	}
	p.Users = obj.S3RP
	return nil
}

func (p *Principal) MarshalJSON() ([]byte, error) {
	if p.All {
		return json.Marshal("*")
	}
	return json.Marshal(map[string][]string{"S3RP": p.Users})
}

// StringOrSlice accepts both a JSON string and an array of strings.
type StringOrSlice []string

func (s *StringOrSlice) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*s = []string{single}
		return nil
	}
	var multi []string
	if err := json.Unmarshal(data, &multi); err != nil {
		return err
	}
	*s = multi
	return nil
}

// Parse parses and validates a policy document in the default dialect
// (principals under the "S3RP" key, resources as plain paths). A service
// whose tenants write a different surface syntax parses with a Dialect.
func Parse(text string) (*Policy, error) {
	var d Dialect
	return d.Parse(text)
}

// validate checks the document structure and the caps. It runs on the
// normalized (dialect-independent) form.
func (p *Policy) validate() error {
	// Version is optional, but if present it must be a recognized value
	// (AWS accepts only these two); a typo would otherwise pass silently.
	if p.Version != "" && p.Version != "2012-10-17" && p.Version != "2008-10-17" {
		return fmt.Errorf("unsupported policy version %q", p.Version)
	}
	if len(p.Statement) == 0 {
		return fmt.Errorf("policy must contain at least one statement")
	}
	if len(p.Statement) > MaxStatements {
		return fmt.Errorf("policy has %d statements, at most %d are allowed", len(p.Statement), MaxStatements)
	}
	for i, st := range p.Statement {
		name := st.Sid
		if name == "" {
			name = fmt.Sprintf("statement[%d]", i)
		}
		if st.Effect != "Allow" && st.Effect != "Deny" {
			return fmt.Errorf("%s: effect must be Allow or Deny", name)
		}
		if (st.Principal == nil) == (st.NotPrincipal == nil) {
			return fmt.Errorf("%s: exactly one of Principal or NotPrincipal is required", name)
		}
		if st.NotPrincipal != nil && st.NotPrincipal.All {
			return fmt.Errorf("%s: NotPrincipal %q matches nobody", name, "*")
		}
		for _, pr := range []*Principal{st.Principal, st.NotPrincipal} {
			if pr == nil {
				continue
			}
			if len(pr.Users) > MaxPrincipalUsers {
				return fmt.Errorf("%s: %d principal users, at most %d are allowed", name, len(pr.Users), MaxPrincipalUsers)
			}
			for _, u := range pr.Users {
				if !principalRegexp.MatchString(u) {
					return fmt.Errorf("%s: invalid principal user name %q", name, u)
				}
			}
		}
		if len(st.Action) == 0 {
			return fmt.Errorf("%s: at least one action is required", name)
		}
		if len(st.Action) > MaxActionsPerStatement {
			return fmt.Errorf("%s: %d actions, at most %d are allowed", name, len(st.Action), MaxActionsPerStatement)
		}
		for _, a := range st.Action {
			if err := validateAction(a); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		if len(st.Resource) == 0 {
			return fmt.Errorf("%s: at least one resource is required", name)
		}
		if len(st.Resource) > MaxResourcesPerStatement {
			return fmt.Errorf("%s: %d resources, at most %d are allowed", name, len(st.Resource), MaxResourcesPerStatement)
		}
		for _, res := range st.Resource {
			if len(res) > MaxPatternLen {
				return fmt.Errorf("%s: resource pattern too long (%d bytes, max %d)", name, len(res), MaxPatternLen)
			}
		}
	}
	return nil
}

// validateAction checks an action element. The s3: prefix is
// case-insensitive, matching evaluation.
func validateAction(a string) error {
	if !strings.HasPrefix(strings.ToLower(a), "s3:") {
		return fmt.Errorf("action %q must start with s3:", a)
	}
	if len(a) > MaxPatternLen {
		return fmt.Errorf("action pattern too long (%d bytes, max %d)", len(a), MaxPatternLen)
	}
	return nil
}

// UserPolicy is a resource-agnostic identity policy attached to a user: it
// grants or denies operations by action pattern, ignoring the resource. A
// nil or empty policy allows every action (the default: allow s3:*).
type UserPolicy struct {
	Statements []ActionStatement `yaml:"policy" json:"policy"`

	compileOnce sync.Once // precompiles action patterns to runes on first Allows
}

// ActionStatement is one allow/deny rule of a UserPolicy.
type ActionStatement struct {
	Effect string   `yaml:"effect" json:"effect"`
	Action []string `yaml:"action" json:"action"`

	// precompiled by UserPolicy.compile: lower-cased action patterns.
	actionRunes [][]rune
}

// Allows reports whether the user may perform action. With no statements it
// allows everything; otherwise the action must match an Allow and no Deny
// (Deny takes precedence, unmatched actions are implicitly denied), as in
// an AWS identity policy.
func (up *UserPolicy) Allows(action string) bool {
	if up == nil || len(up.Statements) == 0 {
		return true
	}
	up.compileOnce.Do(up.compile)
	// Convert the action to runes once; every pattern is matched against it.
	actionRunes := []rune(strings.ToLower(action))
	allowed := false
	for i := range up.Statements {
		st := &up.Statements[i]
		if !matchAnyRunes(st.actionRunes, actionRunes) {
			continue
		}
		// Only the two well-known effects have meaning; anything else is
		// ignored so a malformed effect cannot silently grant access.
		switch st.Effect {
		case "Deny":
			return false
		case "Allow":
			allowed = true
		}
	}
	return allowed
}

// compile precomputes the rune form of every action pattern so Allows does
// not reconvert them on each request.
func (up *UserPolicy) compile() {
	for i := range up.Statements {
		up.Statements[i].actionRunes = lowerPatternRunes(up.Statements[i].Action)
	}
}

// ValidateUserPolicy checks a user policy's effects and actions.
func ValidateUserPolicy(up *UserPolicy) error {
	if up == nil {
		return nil
	}
	if len(up.Statements) > MaxStatements {
		return fmt.Errorf("policy has %d statements, at most %d are allowed", len(up.Statements), MaxStatements)
	}
	for i, st := range up.Statements {
		if st.Effect != "Allow" && st.Effect != "Deny" {
			return fmt.Errorf("statement[%d]: effect must be Allow or Deny", i)
		}
		if len(st.Action) == 0 {
			return fmt.Errorf("statement[%d]: at least one action is required", i)
		}
		if len(st.Action) > MaxActionsPerStatement {
			return fmt.Errorf("statement[%d]: %d actions, at most %d are allowed", i, len(st.Action), MaxActionsPerStatement)
		}
		for _, a := range st.Action {
			if err := validateAction(a); err != nil {
				return fmt.Errorf("statement[%d]: %w", i, err)
			}
		}
	}
	return nil
}

// MarshalUserPolicy serializes a user policy for storage and enforces
// MaxPolicyBytes on the result. The byte cap applies to the serialized form,
// so it lives here rather than in ValidateUserPolicy: callers that already
// hold the serialized policy (the DB read path) check the raw string
// directly, and ValidateUserPolicy stays allocation-free for the request
// hot path.
func MarshalUserPolicy(up *UserPolicy) (string, error) {
	data, err := json.Marshal(up)
	if err != nil {
		return "", err
	}
	if len(data) > MaxPolicyBytes {
		return "", fmt.Errorf("policy is %d bytes, at most %d are allowed", len(data), MaxPolicyBytes)
	}
	return string(data), nil
}

// Evaluate evaluates the policy for a principal performing an action on a
// resource ("bucket" or "bucket/key"). The principal is a plain user name
// for the bucket's own tenant, or "tenant/user" for another tenant's user;
// see matchPrincipal for how the two are matched. Deny takes precedence
// over Allow; None means no statement matched.
func (p *Policy) Evaluate(principal, action, resource string) Effect {
	// AWS treats the Action element as case-insensitive; comparing it
	// case-sensitively would let a mis-cased Deny silently fail open. The
	// Resource is an object key and stays case-sensitive.
	p.compileOnce.Do(p.compile)
	// Convert the action and resource to runes once, up front, rather than
	// on every Match call: with the same value reused across every statement
	// and pattern, this is the dominant cost for a large policy or a long key.
	actionRunes := []rune(strings.ToLower(action))
	resourceRunes := []rune(resource)
	qualified := qualifiedPrincipal(principal)
	result := None
	for i := range p.Statement {
		st := &p.Statement[i]
		if !st.matchPrincipal(principal, qualified) {
			continue
		}
		if !matchAnyRunes(st.actionRunes, actionRunes) {
			continue
		}
		if !matchAnyRunes(st.resourceRunes, resourceRunes) {
			continue
		}
		if st.Effect == "Deny" {
			return Deny
		}
		result = Allow
	}
	return result
}

// DenyEvaluator answers whether a resource is denied for a principal/action
// pair that has already been matched. It is built once per request (via
// DenyEvaluatorFor) so that a per-object operation like DeleteObjects does
// not re-match principal and action for every key — only the resource, which
// is the only part that varies per object.
type DenyEvaluator struct {
	// resource patterns of every Deny statement whose principal and action
	// matched; a resource is denied iff it matches any of them.
	denyResources [][]rune
}

// DenyEvaluatorFor pre-matches principal and action and returns an evaluator
// over the resource alone. Only Deny statements matter: bucket-policy Allow
// statements are inert (the baseline already allows), so a matching Deny is
// the only thing that can restrict access.
func (p *Policy) DenyEvaluatorFor(principal, action string) DenyEvaluator {
	p.compileOnce.Do(p.compile)
	actionRunes := []rune(strings.ToLower(action))
	qualified := qualifiedPrincipal(principal)
	var e DenyEvaluator
	for i := range p.Statement {
		st := &p.Statement[i]
		if st.Effect != "Deny" {
			continue
		}
		if !st.matchPrincipal(principal, qualified) {
			continue
		}
		if !matchAnyRunes(st.actionRunes, actionRunes) {
			continue
		}
		e.denyResources = append(e.denyResources, st.resourceRunes...)
	}
	return e
}

// AllowEvaluator answers whether a resource is allowed for a principal/action
// pair that has already been matched, the Allow-side counterpart of
// DenyEvaluator. A per-object operation under a default-deny baseline
// (a cross-tenant DeleteObjects) needs both: each key must match an Allow
// and no Deny.
type AllowEvaluator struct {
	// resource patterns of every Allow statement whose principal and action
	// matched; a resource is allowed iff it matches any of them.
	allowResources [][]rune
}

// AllowEvaluatorFor pre-matches principal and action over the Allow
// statements and returns an evaluator over the resource alone.
func (p *Policy) AllowEvaluatorFor(principal, action string) AllowEvaluator {
	p.compileOnce.Do(p.compile)
	actionRunes := []rune(strings.ToLower(action))
	qualified := qualifiedPrincipal(principal)
	var e AllowEvaluator
	for i := range p.Statement {
		st := &p.Statement[i]
		if st.Effect != "Allow" {
			continue
		}
		if !st.matchPrincipal(principal, qualified) {
			continue
		}
		if !matchAnyRunes(st.actionRunes, actionRunes) {
			continue
		}
		e.allowResources = append(e.allowResources, st.resourceRunes...)
	}
	return e
}

// AlwaysDenies reports that no Allow statement matched the principal/action,
// so under a default-deny baseline every resource is denied and the
// per-object check can be skipped.
func (e AllowEvaluator) AlwaysDenies() bool {
	return len(e.allowResources) == 0
}

// Allows reports whether the resource is allowed.
func (e AllowEvaluator) Allows(resource string) bool {
	if len(e.allowResources) == 0 {
		return false
	}
	return matchAnyRunes(e.allowResources, []rune(resource))
}

// MentionsPrincipal reports whether any Allow statement's Principal lists
// the principal explicitly. It is the cross-tenant visibility gate: another
// tenant's bucket is reachable only for principals its policy names, so
// every other requester gets the same answer a nonexistent bucket produces
// and bucket names cannot be probed across tenants.
func (p *Policy) MentionsPrincipal(principal string) bool {
	for i := range p.Statement {
		st := &p.Statement[i]
		if st.Effect == "Allow" && st.Principal != nil && slices.Contains(st.Principal.Users, principal) {
			return true
		}
	}
	return false
}

// AlwaysAllows reports that no Deny statement matched the principal/action,
// so every resource is allowed and the per-object check can be skipped.
func (e DenyEvaluator) AlwaysAllows() bool {
	return len(e.denyResources) == 0
}

// Denies reports whether the resource is denied.
func (e DenyEvaluator) Denies(resource string) bool {
	if len(e.denyResources) == 0 {
		return false
	}
	return matchAnyRunes(e.denyResources, []rune(resource))
}

// compile precomputes the rune form of every action and resource pattern so
// Evaluate does not reconvert them on each request. Action patterns are
// lower-cased for AWS-style case-insensitive matching.
func (p *Policy) compile() {
	for i := range p.Statement {
		p.Statement[i].actionRunes = lowerPatternRunes(p.Statement[i].Action)
		p.Statement[i].resourceRunes = patternRunes(p.Statement[i].Resource)
	}
}

// matchPrincipal reports whether the statement applies to the principal.
// qualified marks a tenant-qualified principal ("tenant/user" — another
// tenant's user); callers compute it once per evaluation.
//
// A qualified principal is matched by an Allow statement only when its
// Principal lists it explicitly: "*" and NotPrincipal widen a statement to
// principals the author never named, and a grant must not widen across
// tenants. Deny statements match broadly for the same safety reason — a
// blanket "Deny *" must also catch cross-tenant principals the policy
// allowed elsewhere.
func (st *Statement) matchPrincipal(principal string, qualified bool) bool {
	if st.Principal != nil {
		if st.Principal.All {
			return !qualified || st.Effect == "Deny"
		}
		return slices.Contains(st.Principal.Users, principal)
	}
	// NotPrincipal: matches everyone except the listed users
	if qualified && st.Effect != "Deny" {
		return false
	}
	return !slices.Contains(st.NotPrincipal.Users, principal)
}

// qualifiedPrincipal reports whether the principal is tenant-qualified
// ("tenant/user"): another tenant's user, as opposed to a plain user name of
// the bucket's own tenant.
func qualifiedPrincipal(principal string) bool {
	return strings.ContainsRune(principal, '/')
}

func matchAnyRunes(patterns [][]rune, value []rune) bool {
	for _, p := range patterns {
		if matchRunes(p, value) {
			return true
		}
	}
	return false
}

func patternRunes(patterns []string) [][]rune {
	out := make([][]rune, len(patterns))
	for i, p := range patterns {
		out[i] = []rune(p)
	}
	return out
}

// lowerPatternRunes converts patterns to lower-cased rune form, for
// case-insensitive action matching.
func lowerPatternRunes(patterns []string) [][]rune {
	out := make([][]rune, len(patterns))
	for i, p := range patterns {
		out[i] = []rune(strings.ToLower(p))
	}
	return out
}

// Match matches value against a pattern where "*" matches any sequence of
// characters (including "/") and "?" matches exactly one character, as in
// AWS policies. Both wildcards operate on runes, and there is no escaping:
// a literal "*"/"?" in a key cannot be matched literally, matching AWS.
func Match(pattern, value string) bool {
	return matchRunes([]rune(pattern), []rune(value))
}

// matchRunes is the rune-based core of Match. Callers that reuse the same
// value against many patterns convert it once and call this directly, so the
// value conversion is not repeated per pattern.
func matchRunes(pat, val []rune) bool {
	var p, v int
	// last position where a '*' was seen, and the value index it started at,
	// so we can backtrack and let the '*' consume one more character
	star, starV := -1, 0
	for v < len(val) {
		switch {
		case p < len(pat) && (pat[p] == '?' || pat[p] == val[v]):
			p++
			v++
		case p < len(pat) && pat[p] == '*':
			star, starV = p, v
			p++
		case star >= 0:
			p = star + 1
			starV++
			v = starV
		default:
			return false
		}
	}
	for p < len(pat) && pat[p] == '*' {
		p++
	}
	return p == len(pat)
}
