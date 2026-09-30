// Package s3op is the catalog of the S3 operations the gateway knows and the
// s3:* actions each one authorizes, for a service built on it: a control
// plane validating the policies tenants write, a console listing what a
// policy can grant, hooks matching Op.Operation and Op.Actions.
//
// operations.json is the source of the catalog; the Go data and the
// Op*/Action* constants are generated from it (go generate ./s3op). A
// service written in another language reads the JSON file directly.
package s3op

//go:generate go run ./internal/s3opgen/cmd/s3opgen

import (
	"errors"
	"fmt"
	"strings"

	"github.com/fujiwara/s3rp/policy"
)

// Scope is what an operation's request addresses.
type Scope string

const (
	ScopeService Scope = "service" // no bucket (ListBuckets)
	ScopeBucket  Scope = "bucket"
	ScopeObject  Scope = "object"
)

// Support is whether the gateway performs an operation.
type Support string

const (
	Supported Support = "supported"
	// Refused operations are recognized, recorded on Op.Operation under
	// their name and always refused (NotImplemented, or
	// AccessControlListNotSupported for ACL writes).
	Refused Support = "refused"
)

// Resource is the resource an action is authorized against.
type Resource string

const (
	// OnTarget is the bucket or object the request addresses.
	OnTarget Resource = "target"
	// OnCopySource is the source object of a copy.
	OnCopySource Resource = "copy-source"
	// OnEachKey is every object a multi-object request names, authorized
	// one by one (DeleteObjects).
	OnEachKey Resource = "each-key"
)

// Authorization is one action an operation is authorized for.
type Authorization struct {
	Action string
	On     Resource
	// VersionAction, when set, is authorized instead of Action when the
	// request names an object version — a versionId query parameter, a
	// versionId in x-amz-copy-source, a VersionId on a DeleteObjects entry —
	// as on Amazon S3: a policy on Action alone does not cover the versioned
	// request.
	VersionAction string
	// Header, when set, makes the authorization conditional: it is required
	// only when the request carries this header (for PostObject, the form
	// field of that name).
	Header string
}

// Operation is one S3 operation. Name is the Op.Operation value the gateway
// records for it.
type Operation struct {
	Name           string
	Scope          Scope
	Support        Support
	Authorizations []Authorization
	Note           string
}

// Operations returns every operation in the catalog, in catalog order.
func Operations() []Operation {
	out := make([]Operation, len(operations))
	for i, op := range operations {
		op.Authorizations = append([]Authorization(nil), op.Authorizations...)
		out[i] = op
	}
	return out
}

// Lookup returns the operation named name.
func Lookup(name string) (Operation, bool) {
	for _, op := range operations {
		if op.Name == name {
			op.Authorizations = append([]Authorization(nil), op.Authorizations...)
			return op, true
		}
	}
	return Operation{}, false
}

// Actions returns every action some operation authorizes, sorted.
func Actions() []string {
	return append([]string(nil), actions...)
}

// ErrUnknownAction is the error an UnknownActionError unwraps to.
var ErrUnknownAction = errors.New("matches no action the gateway authorizes")

// UnknownActionError reports a policy action pattern that matches no action
// the gateway ever authorizes: a statement using it never takes effect.
type UnknownActionError struct {
	Pattern string
}

func (e *UnknownActionError) Error() string {
	return fmt.Sprintf("action %q %s", e.Pattern, ErrUnknownAction)
}

func (e *UnknownActionError) Unwrap() error { return ErrUnknownAction }

// CheckActionPattern returns an *UnknownActionError when the policy action
// pattern p matches none of Actions(). Matching is case-insensitive with the
// "*" and "?" wildcards, as in policy evaluation.
func CheckActionPattern(p string) error {
	lp := strings.ToLower(p)
	for _, a := range actions {
		if policy.Match(lp, strings.ToLower(a)) {
			return nil
		}
	}
	return &UnknownActionError{Pattern: p}
}
