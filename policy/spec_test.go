package policy_test

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/fujiwara/s3rp/policy"
)

// The Lean specification (policy/spec) built as a program answers the same
// requests as the evaluator. S3RP_POLICY_SPEC names the executable; without
// it the test is skipped, so Lean is not needed to run the package's tests.
//
// The executable reads one case a line and answers one line. What the
// specification does not define is turned into its terms here, and is
// therefore not compared: the text of a principal name and of an address.
const (
	specEnv      = "S3RP_POLICY_SPEC"
	specSeedEnv  = "S3RP_POLICY_SPEC_SEED"
	specCasesEnv = "S3RP_POLICY_SPEC_CASES"

	specDefaultCases = 20000
)

type specEntry struct {
	Tenant string  `json:"tenant"`
	User   *string `json:"user"` // nil: tenant/*
}

type specRange struct {
	Addr string `json:"addr"`
	Bits int    `json:"bits"`
}

type specStatement struct {
	Effect       string      `json:"effect"`
	Subject      string      `json:"subject"`
	Entries      []specEntry `json:"entries"`
	Actions      []string    `json:"actions"`
	Resources    []string    `json:"resources"`
	IPAddress    []specRange `json:"ipAddress"`
	NotIPAddress []specRange `json:"notIpAddress"`
}

type specUserStatement struct {
	Effect  string   `json:"effect"`
	Actions []string `json:"actions"`
}

type specRequest struct {
	Tenant   string  `json:"tenant"`
	User     string  `json:"user"`
	Action   string  `json:"action"`
	Resource string  `json:"resource"`
	Source   *string `json:"source"` // nil: unknown

	principal string
	src       netip.Addr
}

type specCase struct {
	Statements []specStatement     `json:"statements"`
	UserPolicy []specUserStatement `json:"userPolicy"`
	Requests   []specRequest       `json:"requests"`

	impl *policy.Policy
	user *policy.UserPolicy
	text string
}

type specAnswer struct {
	Effect     string `json:"effect"`
	Statement  int    `json:"statement"`
	Mentions   bool   `json:"mentions"`
	UserAllows bool   `json:"userAllows"`
}

func specEntries(names []string) []specEntry {
	out := make([]specEntry, len(names))
	for i, name := range names {
		tenant, user, _ := strings.Cut(name, "/")
		out[i] = specEntry{Tenant: tenant}
		if user != "*" {
			out[i].User = &user
		}
	}
	return out
}

func specAddr(a netip.Addr) string { return hex.EncodeToString(a.AsSlice()) }

func specRanges(t *testing.T, values []string) []specRange {
	out := make([]specRange, len(values))
	for i, v := range values {
		addrText, bitsText, isCIDR := strings.Cut(v, "/")
		a, err := netip.ParseAddr(addrText)
		if err != nil {
			t.Fatal(err)
		}
		bits := a.BitLen()
		if isCIDR {
			if bits, err = strconv.Atoi(bitsText); err != nil {
				t.Fatal(err)
			}
		}
		out[i] = specRange{Addr: specAddr(a), Bits: bits}
	}
	return out
}

func specStatements(t *testing.T, p *policy.Policy) []specStatement {
	out := make([]specStatement, len(p.Statement))
	for i := range p.Statement {
		st := &p.Statement[i]
		s := specStatement{
			Effect:       st.Effect,
			Entries:      []specEntry{},
			Actions:      st.Action,
			Resources:    st.Resource,
			IPAddress:    []specRange{},
			NotIPAddress: []specRange{},
		}
		switch {
		case st.NotPrincipal != nil:
			s.Subject, s.Entries = "allBut", specEntries(st.NotPrincipal.Users)
		case st.Principal.All:
			s.Subject = "everyone"
		default:
			s.Subject, s.Entries = "listed", specEntries(st.Principal.Users)
		}
		if c := st.Condition; c != nil {
			s.IPAddress, s.NotIPAddress = specRanges(t, c.IPAddress), specRanges(t, c.NotIPAddress)
		}
		out[i] = s
	}
	return out
}

// specGenerate draws a case as FuzzDecide and FuzzUserPolicy draw theirs.
func specGenerate(t *testing.T, data []byte) *specCase {
	g := &gen{data: data}
	text, err := json.Marshal(g.policy())
	if err != nil {
		t.Fatal(err)
	}
	impl, err := policy.Parse(fuzzBucket, string(text), nil)
	if err != nil {
		t.Fatalf("generated policy refused: %v\n%s", err, text)
	}
	up := &policy.UserPolicy{Statements: make([]policy.ActionStatement, g.n(4))}
	c := &specCase{
		Statements: specStatements(t, impl),
		UserPolicy: make([]specUserStatement, len(up.Statements)),
		impl:       impl,
		user:       up,
		text:       string(text),
	}
	for i := range up.Statements {
		st := policy.ActionStatement{
			Effect: g.pick([]string{"Allow", "Deny"}),
			Action: g.some(fuzzActionPatterns, 3),
		}
		up.Statements[i] = st
		c.UserPolicy[i] = specUserStatement{Effect: st.Effect, Actions: st.Action}
	}
	if err := policy.ValidateUserPolicy(up, nil); err != nil {
		t.Fatalf("generated user policy refused: %v", err)
	}
	for _, principal := range draw(g, fuzzPrincipals(), 2) {
		for _, action := range draw(g, fuzzActions, 2) {
			// the action's case is the requester's to choose
			if g.n(4) == 0 {
				action = strings.ToUpper(action)
			}
			for _, resource := range g.resources(2) {
				src := fuzzSources[g.n(len(fuzzSources))]
				tenant, user, _ := strings.Cut(principal, "/")
				r := specRequest{
					Tenant: tenant, User: user, Action: action, Resource: resource,
					principal: principal, src: src,
				}
				if src.IsValid() {
					r.Source = new(specAddr(src))
				}
				c.Requests = append(c.Requests, r)
			}
		}
	}
	return c
}

func specEffect(e policy.Effect) string {
	switch e {
	case policy.Deny:
		return "deny"
	case policy.Allow:
		return "allow"
	}
	return "none"
}

func TestSpec(t *testing.T) {
	path := os.Getenv(specEnv)
	if path == "" {
		t.Skipf("%s is not set", specEnv)
	}
	seed := rand.Uint64()
	if s := os.Getenv(specSeedEnv); s != "" {
		var err error
		if seed, err = strconv.ParseUint(s, 10, 64); err != nil {
			t.Fatal(err)
		}
	}
	n := specDefaultCases
	if s := os.Getenv(specCasesEnv); s != "" {
		var err error
		if n, err = strconv.Atoi(s); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%s=%d, %d cases", specSeedEnv, seed, n)

	rng := rand.New(rand.NewPCG(seed, 0))
	cases := make([]*specCase, n)
	for i := range cases {
		data := make([]byte, 96)
		for j := range data {
			data[j] = byte(rng.UintN(256))
		}
		cases[i] = specGenerate(t, data)
	}

	cmd := exec.CommandContext(t.Context(), path)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	writeErr := make(chan error, 1)
	go func() {
		w := bufio.NewWriter(stdin)
		enc := json.NewEncoder(w)
		for _, c := range cases {
			if err := enc.Encode(c); err != nil {
				writeErr <- err
				return
			}
		}
		if err := w.Flush(); err != nil {
			writeErr <- err
			return
		}
		writeErr <- stdin.Close()
	}()

	requests, differences := 0, 0
	lines := bufio.NewScanner(stdout)
	lines.Buffer(nil, 1024*1024)
	for i, c := range cases {
		if !lines.Scan() {
			t.Fatalf("no answer for case %d: %v", i, lines.Err())
		}
		var answers []specAnswer
		if err := json.Unmarshal(lines.Bytes(), &answers); err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if len(answers) != len(c.Requests) {
			t.Fatalf("case %d: %d answers for %d requests", i, len(answers), len(c.Requests))
		}
		for j, r := range c.Requests {
			requests++
			want := answers[j]
			d := c.impl.Decide(r.principal, r.Action, r.Resource, policy.RequestContext{SourceIP: r.src})
			got := specAnswer{
				Effect:     specEffect(d.Effect),
				Statement:  d.Statement,
				Mentions:   c.impl.MentionsPrincipal(r.principal),
				UserAllows: c.user.Decide(r.Action).Effect == policy.Allow,
			}
			if got != want {
				differences++
				if differences <= 5 {
					t.Errorf("case %d: %s\nevaluator:     %+v\nspecification: %+v\npolicy: %s\nuser policy: %+v",
						i, fmt.Sprintf("principal=%q action=%q resource=%q source=%v", r.principal, r.Action, r.Resource, r.src),
						got, want, c.text, c.user.Statements)
				}
			}
		}
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if differences > 0 {
		t.Errorf("%d of %d requests differ", differences, requests)
	}
	t.Logf("%d requests agree", requests-differences)
}
