package policy

import (
	"encoding/json"
	"fmt"
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

// rawStatement defers principal decoding to the dialect; the other fields
// are dialect-independent.
type rawStatement struct {
	Sid          string          `json:"Sid"`
	Effect       string          `json:"Effect"`
	Principal    json.RawMessage `json:"Principal"`
	NotPrincipal json.RawMessage `json:"NotPrincipal"`
	Action       StringOrSlice   `json:"Action"`
	Resource     StringOrSlice   `json:"Resource"`
}

// Parse parses and validates a policy document written in this dialect. The
// returned Policy is in the internal form regardless of the dialect.
func (d *Dialect) Parse(text string) (*Policy, error) {
	if len(text) > MaxPolicyBytes {
		return nil, fmt.Errorf("policy is %d bytes, at most %d are allowed", len(text), MaxPolicyBytes)
	}
	var doc struct {
		Version   string         `json:"Version"`
		Statement []rawStatement `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("malformed policy JSON: %w", err)
	}
	p := &Policy{Version: doc.Version, Statement: make([]Statement, len(doc.Statement))}
	for i, rs := range doc.Statement {
		name := rs.Sid
		if name == "" {
			name = fmt.Sprintf("statement[%d]", i)
		}
		st := Statement{Sid: rs.Sid, Effect: rs.Effect, Action: rs.Action, Resource: rs.Resource}
		var err error
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
	return p, nil
}

// decodePrincipal decodes a Principal/NotPrincipal element under the
// dialect's key. Absent and JSON null both mean no principal, matching how a
// null decodes into a pointer field.
func (d *Dialect) decodePrincipal(data json.RawMessage) (*Principal, error) {
	if len(data) == 0 || string(data) == "null" {
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
	var obj map[string]StringOrSlice
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("malformed principal: %w", err)
	}
	users := obj[key]
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
