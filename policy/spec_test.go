package policy_test

import (
	"encoding/json/v2"
	"math/rand/v2"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/fujiwara/s3rp/internal/policyspec"
	"github.com/fujiwara/s3rp/policy"
)

// The Lean specification (policy/spec) built as a program answers the same
// requests as the evaluator.
const (
	specSeedEnv  = "S3RP_POLICY_SPEC_SEED"
	specCasesEnv = "S3RP_POLICY_SPEC_CASES"

	specDefaultCases = 20000
)

type specRequest struct {
	principal, action, resource string
	src                         netip.Addr
}

type specCase struct {
	policyspec.Case

	requests []specRequest
	impl     *policy.Policy
	user     *policy.UserPolicy
	text     string
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
	for i := range up.Statements {
		up.Statements[i] = policy.ActionStatement{
			Effect: g.pick([]string{"Allow", "Deny"}),
			Action: g.some(fuzzActionPatterns, 3),
		}
	}
	if err := policy.ValidateUserPolicy(up, nil); err != nil {
		t.Fatalf("generated user policy refused: %v", err)
	}
	statements, err := policyspec.Statements(impl)
	if err != nil {
		t.Fatal(err)
	}
	c := &specCase{
		Statements: statements, UserPolicy: policyspec.UserStatements(up),
		impl: impl,
		user: up,
		text: string(text),
	}
	for _, principal := range draw(g, fuzzPrincipals(), 2) {
		for _, action := range draw(g, fuzzActions, 2) {
			// the action's case is the requester's to choose
			if g.n(4) == 0 {
				action = strings.ToUpper(action)
			}
			for _, resource := range g.resources(2) {
				src := fuzzSources[g.n(len(fuzzSources))]
				c.requests = append(c.requests, specRequest{principal, action, resource, src})
				c.Requests = append(c.Requests, policyspec.NewRequest(principal, action, resource, src))
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
	path := os.Getenv(policyspec.Env)
	if path == "" {
		t.Skipf("%s is not set", policyspec.Env)
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

	spec, err := policyspec.Start(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(seed, 0))
	requests, differences := 0, 0
	for i := range n {
		// more than the largest case draws: a generator out of input
		// draws zeros, and its requests would all be the same one
		data := make([]byte, 512)
		for j := range data {
			data[j] = byte(rng.UintN(256))
		}
		c := specGenerate(t, data)
		answers, err := spec.Ask(&c.Case)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		for j, r := range c.requests {
			requests++
			want := answers[j]
			d := c.impl.Decide(r.principal, r.action, r.resource, policy.RequestContext{SourceIP: r.src})
			// Visible and Permits are the gateway's, compared in s3gw
			got := policyspec.Answer{
				Effect:     specEffect(d.Effect),
				Statement:  d.Statement,
				Mentions:   c.impl.MentionsPrincipal(r.principal),
				UserAllows: c.user.Decide(r.action).Effect == policy.Allow,
				Visible:    want.Visible,
				Permits:    want.Permits,
			}
			if got != want {
				differences++
				if differences <= 5 {
					t.Errorf("case %d: principal=%q action=%q resource=%q source=%v\nevaluator:     %+v\nspecification: %+v\npolicy: %s\nuser policy: %+v",
						i, r.principal, r.action, r.resource, r.src, got, want, c.text, c.user.Statements)
				}
			}
		}
	}
	if err := spec.Close(); err != nil {
		t.Fatal(err)
	}
	if differences > 0 {
		t.Errorf("%d of %d requests differ", differences, requests)
	}
	t.Logf("%d requests agree", requests-differences)
}
