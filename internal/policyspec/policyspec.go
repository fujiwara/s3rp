// Package policyspec asks the Lean specification of the policy semantics
// (policy/spec), built as a program, what it decides. It is for tests that
// compare the Go code with the specification.
//
// The executable reads one case a line and answers one line. What the
// specification does not define is turned into its terms here, and is
// therefore not compared: the text of a principal name and of an address.
package policyspec

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/fujiwara/s3rp/policy"
)

// Env names the executable. A test is skipped without it, so Lean is not
// needed to run the tests.
const Env = "S3RP_POLICY_SPEC"

type Entry struct {
	Tenant string  `json:"tenant"`
	User   *string `json:"user"` // nil: tenant/*
}

type Range struct {
	Addr string `json:"addr"`
	Bits int    `json:"bits"`
}

type Statement struct {
	Effect       string   `json:"effect"`
	Subject      string   `json:"subject"`
	Entries      []Entry  `json:"entries"`
	Actions      []string `json:"actions"`
	Resources    []string `json:"resources"`
	IPAddress    []Range  `json:"ipAddress"`
	NotIPAddress []Range  `json:"notIpAddress"`
}

type UserStatement struct {
	Effect  string   `json:"effect"`
	Actions []string `json:"actions"`
}

type Request struct {
	Tenant   string  `json:"tenant"`
	User     string  `json:"user"`
	Action   string  `json:"action"`
	Resource string  `json:"resource"`
	Source   *string `json:"source"` // nil: unknown
}

// Case is a bucket policy, a user policy and the requests to decide under
// them. Owner is the tenant owning the bucket, which only Visible and
// Permits depend on.
type Case struct {
	Owner      string          `json:"owner"`
	Statements []Statement     `json:"statements"`
	UserPolicy []UserStatement `json:"userPolicy"`
	Requests   []Request       `json:"requests"`
}

// Answer is what the specification decides for one request.
type Answer struct {
	Effect     string `json:"effect"` // "deny", "allow" or "none"
	Statement  int    `json:"statement"`
	Mentions   bool   `json:"mentions"`
	UserAllows bool   `json:"userAllows"`
	Visible    bool   `json:"visible"`
	Permits    bool   `json:"permits"`
}

// Need is one thing a request needs: an action on a resource.
type Need struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
}

// Copy is the object a copy reads and the action reading it needs.
type Copy struct {
	Bucket   string `json:"bucket"`
	Action   string `json:"action"`
	Resource string `json:"resource"`
}

type Bucket struct {
	Name       string      `json:"name"`
	Owner      string      `json:"owner"`
	Statements []Statement `json:"statements"`
}

// Operation is a whole request to a bucket: everything it needs there, a
// copy's source, and for a DeleteObjects each key with what deleting it
// needs.
type Operation struct {
	Tenant string   `json:"tenant"`
	User   string   `json:"user"`
	Source *string  `json:"source"` // nil: unknown
	Bucket string   `json:"bucket"`
	Needs  []Need   `json:"needs"`
	Copy   *Copy    `json:"copy,omitempty"`
	Keys   [][]Need `json:"keys,omitempty"`
}

// World is the buckets that exist, a user policy, and the operations to
// decide for its holder.
type World struct {
	Buckets    []Bucket        `json:"buckets"`
	UserPolicy []UserStatement `json:"userPolicy"`
	Operations []Operation     `json:"operations"`
}

// Verdict is what the specification decides for one operation. Deletes is
// nil when the operation has no keys, or when the whole request is refused.
type Verdict struct {
	Admits  bool   `json:"admits"`
	Deletes []bool `json:"deletes"`
}

// NewRequest is the request of a "tenant/user" principal.
func NewRequest(principal, action, resource string, source netip.Addr) Request {
	tenant, user, _ := strings.Cut(principal, "/")
	r := Request{Tenant: tenant, User: user, Action: action, Resource: resource}
	if source.IsValid() {
		r.Source = new(hex.EncodeToString(source.AsSlice()))
	}
	return r
}

func entries(names []string) []Entry {
	out := make([]Entry, len(names))
	for i, name := range names {
		tenant, user, _ := strings.Cut(name, "/")
		out[i] = Entry{Tenant: tenant}
		if user != "*" {
			out[i].User = &user
		}
	}
	return out
}

func ranges(values []string) ([]Range, error) {
	out := make([]Range, len(values))
	for i, v := range values {
		addrText, bitsText, isCIDR := strings.Cut(v, "/")
		a, err := netip.ParseAddr(addrText)
		if err != nil {
			return nil, err
		}
		bits := a.BitLen()
		if isCIDR {
			if bits, err = strconv.Atoi(bitsText); err != nil {
				return nil, err
			}
		}
		out[i] = Range{Addr: hex.EncodeToString(a.AsSlice()), Bits: bits}
	}
	return out, nil
}

// Statements are a parsed policy's statements in the specification's terms.
// A nil policy has none.
func Statements(p *policy.Policy) ([]Statement, error) {
	if p == nil {
		return []Statement{}, nil
	}
	out := make([]Statement, len(p.Statement))
	for i := range p.Statement {
		st := &p.Statement[i]
		s := Statement{
			Effect:       st.Effect,
			Entries:      []Entry{},
			Actions:      st.Action,
			Resources:    st.Resource,
			IPAddress:    []Range{},
			NotIPAddress: []Range{},
		}
		switch {
		case st.NotPrincipal != nil:
			s.Subject, s.Entries = "allBut", entries(st.NotPrincipal.Users)
		case st.Principal.All:
			s.Subject = "everyone"
		default:
			s.Subject, s.Entries = "listed", entries(st.Principal.Users)
		}
		if c := st.Condition; c != nil {
			var err error
			if s.IPAddress, err = ranges(c.IPAddress); err != nil {
				return nil, fmt.Errorf("statement[%d]: %w", i, err)
			}
			if s.NotIPAddress, err = ranges(c.NotIPAddress); err != nil {
				return nil, fmt.Errorf("statement[%d]: %w", i, err)
			}
		}
		out[i] = s
	}
	return out, nil
}

// UserStatements are a user policy's statements in the specification's
// terms. A nil policy has none.
func UserStatements(up *policy.UserPolicy) []UserStatement {
	out := []UserStatement{}
	if up == nil {
		return out
	}
	for _, st := range up.Statements {
		out = append(out, UserStatement{Effect: st.Effect, Actions: st.Action})
	}
	return out
}

// Process is the running executable.
type Process struct {
	cmd   *exec.Cmd
	in    io.WriteCloser
	enc   *json.Encoder
	lines *bufio.Scanner
}

// Start runs the executable at path until ctx ends or Close is called.
func Start(ctx context.Context, path string) (*Process, error) {
	cmd := exec.CommandContext(ctx, path)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	lines := bufio.NewScanner(out)
	lines.Buffer(nil, 4*1024*1024)
	return &Process{cmd: cmd, in: in, enc: json.NewEncoder(in), lines: lines}, nil
}

// ask sends one line and decodes the line that answers it, a list of n.
func ask[T any](p *Process, in any, n int) ([]T, error) {
	if err := p.enc.Encode(in); err != nil {
		return nil, err
	}
	if !p.lines.Scan() {
		if err := p.lines.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("the specification stopped answering: %w", p.cmd.Wait())
	}
	var answers []T
	if err := json.Unmarshal(p.lines.Bytes(), &answers); err != nil {
		return nil, err
	}
	if len(answers) != n {
		return nil, fmt.Errorf("%d answers for %d requests", len(answers), n)
	}
	return answers, nil
}

// Ask returns the specification's answer to each request of the case, in
// order.
func (p *Process) Ask(c *Case) ([]Answer, error) {
	return ask[Answer](p, c, len(c.Requests))
}

// AskWorld returns the specification's verdict on each operation of the
// world, in order.
func (p *Process) AskWorld(w *World) ([]Verdict, error) {
	return ask[Verdict](p, w, len(w.Operations))
}

// Close ends the executable and reports how it exited.
func (p *Process) Close() error {
	if err := p.in.Close(); err != nil {
		return err
	}
	return p.cmd.Wait()
}
