package policy

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
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

// unmarshal reads a policy document, or a part of one, as it is written. It
// is json.Unmarshal — which refuses a repeated member name and matches a
// name exactly — refusing also a member no field is named for (a misspelled
// Condition, a NotResource) and a null, which would read as the member being
// absent. Each of those makes a document mean something other than what it
// says, to its author or to another parser that validated it.
func unmarshal(data []byte, v any) error {
	dec := jsontext.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.ReadToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if tok.Kind() == 'n' {
			return fmt.Errorf("null at %s", dec.StackPointer())
		}
	}
	return describe(json.Unmarshal(data, v, json.RejectUnknownMembers(true)))
}

// describe words a decoding error by where it is in the document; the
// package's own wording names the Go type it was decoding into.
func describe(err error) error {
	var se *json.SemanticError
	if !errors.As(err, &se) {
		return err
	}
	at := string(se.JSONPointer)
	if at == "" {
		at = "the top"
	}
	switch {
	case errors.Is(se.Err, json.ErrUnknownName):
		return fmt.Errorf("unknown element %s", at)
	case se.Err != nil:
		return fmt.Errorf("%s: %w", at, se.Err)
	}
	return fmt.Errorf("unexpected %s at %s", se.JSONKind, at)
}

// rawStatement defers principal decoding to the dialect; the other fields
// are dialect-independent.
type rawStatement struct {
	Sid          string
	Effect       string
	Principal    jsontext.Value
	NotPrincipal jsontext.Value
	Action       StringOrSlice
	Resource     StringOrSlice
	Condition    *Condition
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
	var doc struct {
		Version   *string
		Id        string
		Statement []rawStatement
	}
	if err := unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("malformed policy JSON: %w", err)
	}
	// Policy.validate takes an empty Version for an absent one
	version := ""
	if doc.Version != nil {
		if version = *doc.Version; version == "" {
			return nil, fmt.Errorf("unsupported policy version %q", version)
		}
	}
	p := &Policy{Version: version, Statement: make([]Statement, len(doc.Statement))}
	for i, rs := range doc.Statement {
		var err error
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
func (d *Dialect) decodePrincipal(data jsontext.Value) (*Principal, error) {
	if len(data) == 0 {
		return nil, nil
	}
	key := d.principalKey()
	if data.Kind() == '"' {
		var s string
		if err := unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("malformed principal: %w", err)
		}
		if s != "*" {
			return nil, fmt.Errorf("principal must be %q or an object with the %s key", "*", key)
		}
		return &Principal{All: true}, nil
	}
	var obj map[string]StringOrSlice
	if err := unmarshal(data, &obj); err != nil {
		return nil, fmt.Errorf("principal must be %q or an object with the %s key: %w", "*", key, err)
	}
	// the one key: a principal of another kind beside it ("AWS") would be
	// dropped, and the statement would be about fewer or more than it says
	for name := range obj {
		if name != key {
			return nil, fmt.Errorf("principal must be %q or an object with the %s key: unknown principal element %q", "*", key, name)
		}
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
