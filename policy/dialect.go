package policy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
)

// DefaultPrincipalKey is the JSON key of the Principal object in the default
// dialect.
const DefaultPrincipalKey = "S3RP"

// Dialect selects the surface syntax a policy document is written in. The
// zero value is the default dialect: principals under the "S3RP" key and
// resources as plain "bucket" or "bucket/prefix*" paths. Parsing normalizes
// a document to that internal form, so evaluation — and everything tuned to
// it, the structural caps and the precompiled matching — is
// dialect-independent. A service whose tenants write a different syntax
// parses with its own Dialect and keeps the original text in
// store.Bucket.PolicyText, which is what GetBucketPolicy returns.
// Condition syntax (the operators and the "aws:SourceIp" key) is the same
// in every dialect.
type Dialect struct {
	// PrincipalKey is the JSON key of the Principal object holding the
	// names (empty = DefaultPrincipalKey).
	PrincipalKey string
	// ResourcePrefix, when set, is required on every Resource entry and
	// stripped during parsing (e.g. "arn:aws:s3:::"), so resources are
	// stored and matched as plain paths. MaxPatternLen applies to the
	// stripped pattern — the one actually matched — while MaxPolicyBytes
	// still caps the raw document.
	ResourcePrefix string
	// NormalizePrincipal, when set, rewrites each Principal/NotPrincipal
	// entry to the internal "tenant/user" (or "tenant/*") form during
	// parsing — e.g. "arn:myco:iam::tenant-a:user/alice" → "tenant-a/alice"
	// — so a dialect store hands Parse the original text instead of
	// rewriting the JSON itself. It is not applied to "*" (every dialect
	// spells everyone the same way). Validation runs on the normalized
	// value, so a result that is not the internal form is rejected.
	NormalizePrincipal func(string) (string, error)
	// NormalizeResource, when set, rewrites each Resource entry during
	// parsing, after ResourcePrefix stripping, for dialects whose resource
	// syntax is not a fixed prefix (e.g. ARNs with variable middle fields).
	// Entries are glob patterns, not literals: rewrite only the literal
	// prefix you recognize and pass the rest — wildcards included —
	// through untouched, or return an error. The caps apply to the
	// normalized pattern.
	NormalizeResource func(string) (string, error)
}

func (d *Dialect) principalKey() string {
	if d.PrincipalKey == "" {
		return DefaultPrincipalKey
	}
	return d.PrincipalKey
}

// members reads a JSON object's members as they are written. encoding/json
// would do three things a policy must not be read with: skip a member no
// field is named for (a misspelled Condition, a NotResource), take "effect"
// for Effect, and let the last of two members of one name win. Each makes a
// document mean something other than what it says — to its author, or to
// another parser that validated it.
func members(data []byte, what string, allowed ...string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil {
		return nil, err
	} else if tok != json.Delim('{') {
		return nil, fmt.Errorf("%s must be an object", what)
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name := tok.(string) // a member name is all the grammar allows here
		if allowed != nil && !slices.Contains(allowed, name) {
			return nil, fmt.Errorf("unknown %s element %q", what, name)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("duplicate %s element %q", what, name)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		if string(value) == "null" {
			return nil, fmt.Errorf("%s element %q is null", what, name)
		}
		out[name] = value
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("data after the %s", what)
	}
	return out, nil
}

// member decodes the named member into v, if it is there.
func member(m map[string]json.RawMessage, name string, v any) error {
	raw, ok := m[name]
	if !ok {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// rawStatement defers principal decoding to the dialect; the other fields
// are dialect-independent.
type rawStatement struct {
	Sid          string
	Effect       string
	Principal    json.RawMessage
	NotPrincipal json.RawMessage
	Action       StringOrSlice
	Resource     StringOrSlice
	Condition    *Condition
}

func decodeStatement(data []byte) (rs rawStatement, err error) {
	m, err := members(data, "statement", "Sid", "Effect", "Principal", "NotPrincipal", "Action", "Resource", "Condition")
	if err != nil {
		return rs, err
	}
	rs.Principal, rs.NotPrincipal = m["Principal"], m["NotPrincipal"]
	for name, v := range map[string]any{
		"Sid": &rs.Sid, "Effect": &rs.Effect, "Action": &rs.Action, "Resource": &rs.Resource, "Condition": &rs.Condition,
	} {
		if err := member(m, name, v); err != nil {
			return rs, err
		}
	}
	return rs, nil
}

// Parse parses and validates the named bucket's policy document written in
// this dialect. The returned Policy is in the internal form regardless of
// the dialect, and every resource must refer to the bucket
// (ValidateResourcesFor, run after normalization — bucket is the plain
// internal name even when the surface syntax is ARN-prefixed). check names
// the actions that exist (see ActionChecker); nil skips that check.
func (d *Dialect) Parse(bucket, text string, check ActionChecker) (*Policy, error) {
	if len(text) > MaxPolicyBytes {
		return nil, fmt.Errorf("policy is %d bytes, at most %d are allowed", len(text), MaxPolicyBytes)
	}
	// Id is a label AWS policies carry; it is accepted and means nothing
	m, err := members([]byte(text), "policy", "Version", "Id", "Statement")
	if err != nil {
		return nil, fmt.Errorf("malformed policy JSON: %w", err)
	}
	var doc struct {
		Version, Id string
		Statement   []json.RawMessage
	}
	for name, v := range map[string]any{"Version": &doc.Version, "Id": &doc.Id, "Statement": &doc.Statement} {
		if err := member(m, name, v); err != nil {
			return nil, fmt.Errorf("malformed policy JSON: %w", err)
		}
	}
	p := &Policy{Version: doc.Version, Statement: make([]Statement, len(doc.Statement))}
	for i, raw := range doc.Statement {
		rs, err := decodeStatement(raw)
		if err != nil {
			return nil, fmt.Errorf("malformed policy JSON: statement[%d]: %w", i, err)
		}
		name := statementName(rs.Sid, i)
		st := Statement{Sid: rs.Sid, Effect: rs.Effect, Action: rs.Action, Resource: rs.Resource, Condition: rs.Condition}
		if st.Principal, err = d.decodePrincipal(rs.Principal); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if st.NotPrincipal, err = d.decodePrincipal(rs.NotPrincipal); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if prefix := d.ResourcePrefix; prefix != "" {
			for j, res := range st.Resource {
				if !strings.HasPrefix(res, prefix) {
					return nil, fmt.Errorf("%s: resource %q must start with %q", name, res, prefix)
				}
				st.Resource[j] = res[len(prefix):]
			}
		}
		if d.NormalizeResource != nil {
			for j, res := range st.Resource {
				n, err := d.NormalizeResource(res)
				if err != nil {
					return nil, fmt.Errorf("%s: resource %q: %w", name, res, err)
				}
				st.Resource[j] = n
			}
		}
		p.Statement[i] = st
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	if err := p.ValidateResourcesFor(bucket); err != nil {
		return nil, err
	}
	if err := checkActions(check, len(p.Statement),
		func(i int) []string { return p.Statement[i].Action },
		func(i int) string { return p.Statement[i].Sid }); err != nil {
		return nil, err
	}
	return p, nil
}

// decodePrincipal decodes a Principal/NotPrincipal element under the
// dialect's key. An absent one is no principal.
func (d *Dialect) decodePrincipal(data json.RawMessage) (*Principal, error) {
	if len(data) == 0 {
		return nil, nil
	}
	key := d.principalKey()
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		if s != "*" {
			return nil, fmt.Errorf("principal must be %q or an object with the %s key", "*", key)
		}
		return &Principal{All: true}, nil
	}
	// the one key: a principal of another kind beside it ("AWS") would be
	// dropped, and the statement would be about fewer or more than it says
	obj, err := members(data, "principal", key)
	if err != nil {
		return nil, fmt.Errorf("principal must be %q or an object with the %s key: %w", "*", key, err)
	}
	var users StringOrSlice
	if err := member(obj, key, &users); err != nil {
		return nil, fmt.Errorf("malformed principal: %w", err)
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("principal must be %q or an object with the %s key", "*", key)
	}
	if d.NormalizePrincipal != nil {
		for i, u := range users {
			n, err := d.NormalizePrincipal(u)
			if err != nil {
				return nil, fmt.Errorf("principal %q: %w", u, err)
			}
			users[i] = n
		}
	}
	return &Principal{Users: users}, nil
}
