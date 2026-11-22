package policy_test

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/fujiwara/s3rp/policy"
)

// Fuzz targets holding the evaluator to the reference model (model_test.go).
// The properties:
//
//   - Equivalence: Decide, the per-object Deny/Allow evaluators and
//     UserPolicy.Decide answer as the model does, deciding statement
//     included — the evaluators are the optimized paths, so this is where a
//     shortcut that changes an answer shows.
//   - Sound shortcuts: AlwaysAllows / AlwaysDenies never skip a statement
//     that applies to some resource.
//   - Visibility: a principal MentionsPrincipal does not cover is never
//     allowed anything, so the cross-tenant gate hides nothing a policy
//     grants.
//   - Validation: a document Parse accepts is one the evaluator's
//     assumptions hold for (modelWellFormed).

const fuzzBucket = "b"

// gen draws choices from the fuzz input, so every input is a policy and a
// mutation of the input is a nearby policy. An exhausted input draws zeros.
type gen struct {
	data []byte
}

func (g *gen) n(max int) int {
	if len(g.data) == 0 {
		return 0
	}
	b := g.data[0]
	g.data = g.data[1:]
	return int(b) % max
}

func (g *gen) pick(choices []string) string { return choices[g.n(len(choices))] }

func (g *gen) some(choices []string, max int) []string {
	return draw(g, choices, 1+g.n(max))
}

func draw[T any](g *gen, choices []T, n int) []T {
	out := make([]T, n)
	for i := range out {
		out[i] = choices[g.n(len(choices))]
	}
	return out
}

// text draws a short string over a small alphabet, where a pattern and a
// value have a real chance of meeting.
func (g *gen) text(alphabet []rune, max int) string {
	out := make([]rune, g.n(max+1))
	for i := range out {
		out[i] = alphabet[g.n(len(alphabet))]
	}
	return string(out)
}

// The vocabularies are small and adjacent on purpose: a tenant that is a
// prefix of another, users likewise, actions differing by case or a suffix,
// ranges in both families and in the IPv4-mapped spelling.
var (
	fuzzTenants = []string{"ta", "tb", "tab"}
	fuzzUsers   = []string{"al", "alice", "bob"}

	fuzzActionPatterns = []string{
		"s3:GetObject", "s3:getobject", "S3:PutObject", "s3:*", "s3:Get*", "s3:*Object",
		"s3:DeleteObject", "s3:DeleteObject*", "s3:DeleteObject?", "s3:???Object", "s3:ListBucket",
	}
	fuzzActions = []string{
		"s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:DeleteObjectVersion",
		"s3:GetObjectVersion", "s3:ListBucket",
	}

	fuzzPatternRunes = []rune("ab/*?é")
	fuzzKeyRunes     = []rune("ab/é*?")

	fuzzRanges = []string{
		"10.0.0.0/8", "10.1.0.0/16", "10.1.2.3", "10.1.2.3/8", "192.168.0.0/24", "0.0.0.0/0",
		"2001:db8::/32", "2001:db8::1", "::/0",
		"::ffff:10.0.0.0/104", "::ffff:10.1.2.3", "::ffff:0.0.0.0/96",
	}
	fuzzSources = []netip.Addr{
		{}, // unknown
		netip.MustParseAddr("10.1.2.3"),
		netip.MustParseAddr("::ffff:10.1.2.3"),
		netip.MustParseAddr("10.2.0.1"),
		netip.MustParseAddr("192.168.0.9"),
		netip.MustParseAddr("2001:db8::1"),
		netip.MustParseAddr("2001:db9::1"),
	}
)

func fuzzPrincipals() []string {
	var out []string
	for _, tenant := range fuzzTenants {
		for _, user := range fuzzUsers {
			out = append(out, tenant+"/"+user)
		}
	}
	return out
}

func (g *gen) principal() *policy.Principal {
	if g.n(4) == 0 {
		return &policy.Principal{All: true}
	}
	return &policy.Principal{Users: g.principalNames()}
}

func (g *gen) principalNames() []string {
	names := make([]string, 1+g.n(3))
	for i := range names {
		names[i] = g.pick(fuzzTenants) + "/" + g.pick(append([]string{"*"}, fuzzUsers...))
	}
	return names
}

func (g *gen) resource() string {
	if g.n(6) == 0 {
		return fuzzBucket
	}
	return fuzzBucket + "/" + g.text(fuzzPatternRunes, 6)
}

func (g *gen) condition() *policy.Condition {
	var c policy.Condition
	switch g.n(6) {
	case 0:
		c.IPAddress = g.some(fuzzRanges, 3)
	case 1:
		c.NotIPAddress = g.some(fuzzRanges, 3)
	case 2:
		c.IPAddress = g.some(fuzzRanges, 3)
		c.NotIPAddress = g.some(fuzzRanges, 3)
	default:
		return nil
	}
	return &c
}

func (g *gen) policy() *policy.Policy {
	p := &policy.Policy{Statement: make([]policy.Statement, 1+g.n(6))}
	for i := range p.Statement {
		st := &p.Statement[i]
		if g.n(2) == 0 {
			st.Sid = fmt.Sprintf("s%d", i)
		}
		switch g.n(5) {
		case 0, 1:
			st.Effect, st.Principal = "Allow", g.principal()
		case 2, 3:
			st.Effect, st.Principal = "Deny", g.principal()
		default:
			st.Effect, st.NotPrincipal = "Deny", &policy.Principal{Users: g.principalNames()}
		}
		st.Action = g.some(fuzzActionPatterns, 3)
		st.Resource = make([]string, 1+g.n(3))
		for j := range st.Resource {
			st.Resource[j] = g.resource()
		}
		st.Condition = g.condition()
	}
	return p
}

func (g *gen) resources(n int) []string {
	out := make([]string, n)
	for i := range out {
		if g.n(8) == 0 {
			out[i] = fuzzBucket
			continue
		}
		out[i] = fuzzBucket + "/" + g.text(fuzzKeyRunes, 8)
	}
	return out
}

// FuzzDecide: a generated policy, serialized and parsed as a tenant's would
// be, evaluates as the model says on requests drawn from the vocabulary.
func FuzzDecide(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("\x02\x00\x00\x01\x00\x01\x00\x00\x02\x01\x01\x03\x05\x02\x00\x04\x01\x00\x01\x03\x03"))
	f.Add([]byte("the quick brown fox jumps over the lazy dog, twice over the lazy dog"))
	f.Fuzz(func(t *testing.T, data []byte) {
		g := &gen{data: data}
		spec := g.policy()
		text, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		impl, err := policy.Parse(fuzzBucket, string(text), nil)
		if err != nil {
			t.Fatalf("generated policy refused: %v\n%s", err, text)
		}
		t.Logf("policy: %s", text)
		// the requests are drawn too: the product of whole vocabularies
		// costs more per input than the inputs it would save
		checkAgainstModel(t, impl, spec, probes{
			principals: draw(g, fuzzPrincipals(), 3),
			actions:    draw(g, fuzzActions, 2),
			resources:  g.resources(4),
			sources:    draw(g, fuzzSources, 3),
		})
	})
}

// instantiate turns a pattern into a value it matches, so a document's own
// patterns supply requests that reach its statements.
func instantiate(pattern string) string {
	return strings.NewReplacer("*", "x", "?", "y").Replace(pattern)
}

// probesOf derives requests from the policy itself: its principals, actions
// and resources with the wildcards filled in, its ranges' own addresses.
func probesOf(p *policy.Policy) probes {
	pr := probes{
		principals: []string{"ta/alice", "zz/nobody"},
		actions:    []string{"s3:GetObject"},
		resources:  []string{fuzzBucket, fuzzBucket + "/key"},
		sources:    []netip.Addr{{}, netip.MustParseAddr("10.1.2.3"), netip.MustParseAddr("2001:db8::1")},
	}
	for i := range p.Statement {
		st := &p.Statement[i]
		for _, who := range []*policy.Principal{st.Principal, st.NotPrincipal} {
			if who == nil {
				continue
			}
			for _, u := range who.Users {
				pr.principals = append(pr.principals, strings.TrimSuffix(u, "*")+"u1")
				if !strings.HasSuffix(u, "*") {
					pr.principals = append(pr.principals, u)
				}
			}
		}
		for _, a := range st.Action {
			pr.actions = append(pr.actions, instantiate(a))
		}
		for _, r := range st.Resource {
			pr.resources = append(pr.resources, instantiate(r), r)
		}
		if c := st.Condition; c != nil {
			for _, v := range append(append([]string{}, c.IPAddress...), c.NotIPAddress...) {
				addr, _, _ := strings.Cut(v, "/")
				if a, err := netip.ParseAddr(addr); err == nil {
					pr.sources = append(pr.sources, a)
				}
			}
		}
	}
	// a document may repeat one pattern up to the caps; the comparison is
	// the product of the four lists
	pr.principals = firstDistinct(pr.principals, 8)
	pr.actions = firstDistinct(pr.actions, 8)
	pr.resources = firstDistinct(pr.resources, 8)
	pr.sources = firstDistinct(pr.sources, 6)
	return pr
}

func firstDistinct[T comparable](in []T, max int) []T {
	seen := map[T]bool{}
	var out []T
	for _, v := range in {
		if !seen[v] && len(out) < max {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// FuzzParse: Parse survives any text, and a document it accepts is
// well-formed and evaluates as the model reads it — shapes the generator of
// FuzzDecide does not write included.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		`{"Statement": [{"Effect": "Allow", "Principal": "*", "Action": "s3:GetObject", "Resource": "b/*"}]}`,
		`{"Version": "2012-10-17", "Statement": [
			{"Sid": "Read", "Effect": "Allow", "Principal": {"S3RP": ["tb/bob", "tc/*"]}, "Action": ["s3:Get*", "s3:ListBucket"], "Resource": ["b", "b/pub/*"]},
			{"Sid": "NoLogs", "Effect": "Deny", "Principal": "*", "Action": "s3:*", "Resource": "b/logs/*"}
		]}`,
		`{"Statement": [{"Effect": "Deny", "NotPrincipal": {"S3RP": "ta/alice"}, "Action": "s3:DeleteObject*", "Resource": "b/?/*",
			"Condition": {"NotIpAddress": {"aws:SourceIp": ["10.0.0.0/8", "2001:db8::/32"]}}}]}`,
		`{"Statement": [{"Effect": "Allow", "Principal": {"S3RP": "ta/*"}, "Action": "S3:PUTOBJECT", "Resource": "b/*",
			"Condition": {"IpAddress": {"AWS:sourceip": "::ffff:10.1.2.3"}, "NotIpAddress": {"aws:SourceIp": "10.1.2.4"}}}]}`,
		`{"Statement": []}`,
		`{"Statement": [{"Effect": "Allow", "NotPrincipal": {"S3RP": "ta/alice"}, "Action": "s3:*", "Resource": "b/*"}]}`,
		`{"Statement": [{"Effect": "Allow", "Principal": "*", "Action": "s3:*", "Resource": "other/*"}]}`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		p, err := policy.Parse(fuzzBucket, text, nil)
		if err != nil {
			return
		}
		if len(text) > policy.MaxPolicyBytes {
			t.Fatalf("accepted a document of %d bytes", len(text))
		}
		if err := modelWellFormed(p, fuzzBucket); err != nil {
			t.Fatalf("accepted a malformed policy: %v", err)
		}
		checkAgainstModel(t, p, p, probesOf(p))
	})
}

// FuzzUserPolicy: a user policy decides an action as the model says.
func FuzzUserPolicy(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("\x02\x00\x01\x03\x01\x00\x06"))
	f.Add([]byte("\x03\x01\x01\x04\x00\x02\x02\x05\x01\x00\x00"))
	f.Fuzz(func(t *testing.T, data []byte) {
		g := &gen{data: data}
		up := &policy.UserPolicy{Statements: make([]policy.ActionStatement, g.n(6))}
		for i := range up.Statements {
			up.Statements[i] = policy.ActionStatement{
				Effect: g.pick([]string{"Allow", "Deny"}),
				Action: g.some(fuzzActionPatterns, 3),
			}
		}
		if err := policy.ValidateUserPolicy(up, nil); err != nil {
			t.Fatalf("generated user policy refused: %v", err)
		}
		for _, action := range fuzzActions {
			want := modelUserDecide(up, action)
			for _, a := range []string{action, strings.ToUpper(action)} {
				if got := up.Decide(a); got != want {
					t.Errorf("Decide(%q) = %+v, model says %+v\npolicy: %+v", a, got, want, up.Statements)
				}
			}
			if got := up.Allows(action); got != (want.Effect == policy.Allow) {
				t.Errorf("Allows(%q) = %v, model says %+v", action, got, want)
			}
		}
	})
}

// FuzzMatch: the backtracking matcher agrees with the wildcard definition.
func FuzzMatch(f *testing.F) {
	for _, seed := range [][2]string{
		{"", ""}, {"*", "anything"}, {"a?c", "abc"}, {"b/*.jpg", "b/x/y.jpg"},
		{"*a*b*a*b", "aabbaabbab"}, {"a**?*b", "aéb"}, {"\xff*", "\xffx"},
		{"b/*", "b/*x"}, {"b/?*", "b/?*x"},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, pattern, value string) {
		// the model's table is quadratic; real patterns are capped far lower
		if len(pattern) > 256 || len(value) > 1024 {
			t.Skip()
		}
		if got, want := policy.Match(pattern, value), modelGlob(pattern, value); got != want {
			t.Errorf("Match(%q, %q) = %v, the definition says %v", pattern, value, got, want)
		}
	})
}
