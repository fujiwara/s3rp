// Package policy implements AWS-style bucket policy documents for s3rp.
//
// The document structure follows the AWS policy language (Version /
// Statement / Effect / Principal / Action / Resource), with two
// simplifications: principals are plain user names (no ARNs) under the
// "S3RP" key, and resources are plain "bucket" or "bucket/prefix*" strings
// (no ARNs).
package policy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
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

var userNameRegexp = regexp.MustCompile(`^[a-z][a-z0-9_-]+$`)

// Policy is a bucket policy document.
type Policy struct {
	Version   string      `json:"Version,omitempty"`
	Statement []Statement `json:"Statement"`
}

// Statement is a single statement of a policy.
type Statement struct {
	Sid          string        `json:"Sid,omitempty"`
	Effect       string        `json:"Effect"`
	Principal    *Principal    `json:"Principal,omitempty"`
	NotPrincipal *Principal    `json:"NotPrincipal,omitempty"`
	Action       StringOrSlice `json:"Action"`
	Resource     StringOrSlice `json:"Resource"`
}

// Principal is either "*" (all users) or {"S3RP": user names}.
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

// Parse parses and validates a policy document.
func Parse(text string) (*Policy, error) {
	var p Policy
	if err := json.Unmarshal([]byte(text), &p); err != nil {
		return nil, fmt.Errorf("malformed policy JSON: %w", err)
	}
	if len(p.Statement) == 0 {
		return nil, fmt.Errorf("policy must contain at least one statement")
	}
	for i, st := range p.Statement {
		name := st.Sid
		if name == "" {
			name = fmt.Sprintf("statement[%d]", i)
		}
		if st.Effect != "Allow" && st.Effect != "Deny" {
			return nil, fmt.Errorf("%s: effect must be Allow or Deny", name)
		}
		if (st.Principal == nil) == (st.NotPrincipal == nil) {
			return nil, fmt.Errorf("%s: exactly one of Principal or NotPrincipal is required", name)
		}
		if st.NotPrincipal != nil && st.NotPrincipal.All {
			return nil, fmt.Errorf("%s: NotPrincipal %q matches nobody", name, "*")
		}
		for _, pr := range []*Principal{st.Principal, st.NotPrincipal} {
			if pr == nil {
				continue
			}
			for _, u := range pr.Users {
				if !userNameRegexp.MatchString(u) {
					return nil, fmt.Errorf("%s: invalid principal user name %q", name, u)
				}
			}
		}
		if len(st.Action) == 0 {
			return nil, fmt.Errorf("%s: at least one action is required", name)
		}
		for _, a := range st.Action {
			// the action prefix is case-insensitive, matching evaluation
			if !strings.HasPrefix(strings.ToLower(a), "s3:") {
				return nil, fmt.Errorf("%s: action %q must start with s3:", name, a)
			}
		}
		if len(st.Resource) == 0 {
			return nil, fmt.Errorf("%s: at least one resource is required", name)
		}
	}
	return &p, nil
}

// Evaluate evaluates the policy for a principal (user name) performing an
// action on a resource ("bucket" or "bucket/key"). Deny takes precedence
// over Allow; None means no statement matched.
func (p *Policy) Evaluate(principal, action, resource string) Effect {
	// AWS treats the Action element as case-insensitive; comparing it
	// case-sensitively would let a mis-cased Deny silently fail open. The
	// Resource is an object key and stays case-sensitive.
	action = strings.ToLower(action)
	result := None
	for _, st := range p.Statement {
		if !st.matchPrincipal(principal) {
			continue
		}
		if !matchAnyFold(st.Action, action) {
			continue
		}
		if !matchAny(st.Resource, resource) {
			continue
		}
		if st.Effect == "Deny" {
			return Deny
		}
		result = Allow
	}
	return result
}

func (st *Statement) matchPrincipal(principal string) bool {
	if st.Principal != nil {
		return st.Principal.All || slices.Contains(st.Principal.Users, principal)
	}
	// NotPrincipal: matches everyone except the listed users
	return !slices.Contains(st.NotPrincipal.Users, principal)
}

func matchAny(patterns []string, value string) bool {
	for _, p := range patterns {
		if Match(p, value) {
			return true
		}
	}
	return false
}

// matchAnyFold matches value (already lower-cased) against patterns
// case-insensitively, used for AWS-style case-insensitive actions.
func matchAnyFold(patterns []string, value string) bool {
	for _, p := range patterns {
		if Match(strings.ToLower(p), value) {
			return true
		}
	}
	return false
}

// Match matches value against a pattern where "*" matches any sequence of
// characters, including "/" (as in AWS policies).
func Match(pattern, value string) bool {
	segments := strings.Split(pattern, "*")
	if len(segments) == 1 {
		return pattern == value
	}
	if !strings.HasPrefix(value, segments[0]) {
		return false
	}
	value = value[len(segments[0]):]
	for _, seg := range segments[1 : len(segments)-1] {
		i := strings.Index(value, seg)
		if i < 0 {
			return false
		}
		value = value[i+len(seg):]
	}
	return strings.HasSuffix(value, segments[len(segments)-1])
}
