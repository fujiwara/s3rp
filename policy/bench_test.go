package policy_test

import (
	"strings"
	"testing"

	"github.com/fujiwara/s3rp/policy"
)

// The benchmarks below measure the worst case allowed by the structural caps
// (adversarial patterns that force a full object-key scan), so a change that
// slows the matcher, defeats the one-time pattern precompilation, or breaks
// the DeleteObjects de-amplification shows up as a visible regression. They
// derive their sizes from policy.Max* so they track the caps automatically.

// worstKey is a ~1 KB object key (near the S3 limit) of a single repeated
// character, the costly input for a leading-"*" adversarial pattern.
func worstKey() string {
	return "photos/" + strings.Repeat("a", 1000)
}

// maxBucketPolicy is the most expensive bucket policy the caps allow to
// evaluate: MaxStatements Deny statements, each with MaxResourcesPerStatement
// adversarial resource patterns ("*zb" hunts a char absent from the key,
// forcing a full scan), all matching the requester's principal and action.
func maxBucketPolicy(tb testing.TB) *policy.Policy {
	res := make([]string, policy.MaxResourcesPerStatement)
	for i := range res {
		res[i] = `"*zb"`
	}
	stmt := `{"Effect":"Deny","Principal":"*","Action":["s3:*Object*"],"Resource":[` + strings.Join(res, ",") + `]}`
	stmts := make([]string, policy.MaxStatements)
	for i := range stmts {
		stmts[i] = stmt
	}
	p, err := policy.Parse(`{"Statement":[` + strings.Join(stmts, ",") + `]}`)
	if err != nil {
		tb.Fatal(err)
	}
	return p
}

// benignBucketPolicy denies a different action, so it never matches
// s3:DeleteObject: the common case the de-amplification must keep O(1) per key.
func benignBucketPolicy(tb testing.TB) *policy.Policy {
	p, err := policy.Parse(`{"Statement":[{"Effect":"Deny","Principal":{"S3RP":["someone"]},"Action":"s3:PutObject","Resource":"b/*"}]}`)
	if err != nil {
		tb.Fatal(err)
	}
	return p
}

// maxUserPolicy is the most expensive user policy the caps allow.
func maxUserPolicy() *policy.UserPolicy {
	actionPat := "s3:" + strings.Repeat("z*", (policy.MaxPatternLen-4)/2) + "q"
	stmts := make([]policy.ActionStatement, policy.MaxStatements)
	for i := range stmts {
		acts := make([]string, policy.MaxActionsPerStatement)
		for j := range acts {
			acts[j] = actionPat
		}
		stmts[i] = policy.ActionStatement{Effect: "Allow", Action: acts}
	}
	stmts[0].Action[0] = "s3:*" // so s3:DeleteObject passes
	return &policy.UserPolicy{Statements: stmts}
}

// BenchmarkEvaluate is the worst single bucket-policy evaluation.
func BenchmarkEvaluate(b *testing.B) {
	p := maxBucketPolicy(b)
	key := worstKey()
	b.ReportAllocs()
	for b.Loop() {
		p.Evaluate("someuser", "s3:DeleteObject", key)
	}
}

// BenchmarkAllows is the worst user-policy evaluation.
func BenchmarkAllows(b *testing.B) {
	up := maxUserPolicy()
	b.ReportAllocs()
	for b.Loop() {
		up.Allows("s3:DeleteObject")
	}
}

// BenchmarkDeleteObjects mirrors the proxy's per-request path: resolve the
// resource-independent parts once, then test only the resource for each of
// 1000 keys. The benign case must stay O(1) per key (AlwaysAllows); the
// adversarial case is bounded by the resource-pattern count cap.
func BenchmarkDeleteObjects(b *testing.B) {
	up := maxUserPolicy()
	keys := make([]string, 1000)
	for i := range keys {
		keys[i] = worstKey()
	}
	run := func(b *testing.B, p *policy.Policy) {
		b.ReportAllocs()
		for b.Loop() {
			if !up.Allows("s3:DeleteObject") {
				continue
			}
			e := p.DenyEvaluatorFor("someuser", "s3:DeleteObject")
			for _, k := range keys {
				e.Denies(k)
			}
		}
	}
	b.Run("benign", func(b *testing.B) { run(b, benignBucketPolicy(b)) })
	b.Run("adversarial", func(b *testing.B) { run(b, maxBucketPolicy(b)) })
}
