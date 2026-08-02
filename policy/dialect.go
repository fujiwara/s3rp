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
	// PrincipalKey is the JSON key of the Principal object holding the user
	// names (empty = DefaultPrincipalKey). The principal values stay plain
	// user names in any dialect; mapping something else onto user names is
	// the caller's business, before parsing.
	PrincipalKey string
	// ResourcePrefix, when set, is required on every Resource entry and
	// stripped during parsing (e.g. "arn:aws:s3:::"), so resources are
	// stored and matched as plain paths. MaxPatternLen applies to the
	// stripped pattern — the one actually matched — while MaxPolicyBytes
	// still caps the raw document.
	ResourcePrefix string
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
	return &Principal{Users: users}, nil
}
