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
// adversarial resource patterns ("photos/*zb" — the mandatory bucket prefix,
// then a wildcard hunting a char absent from the key, forcing a full scan),
// all matching the requester's principal and action.
// Every statement also carries a condition that the benchmark's source
// (10.0.0.1) satisfies only at the last value, so the full prefix lists are
// scanned and every statement still reaches the resource scan. The lists
// hold 20 values per operator, not MaxConditionValues: MaxPolicyBytes is
// the binding constraint here — a document cannot carry max-length
// adversarial resources and maxed-out conditions in every statement at
// once, and the resource scans are what dominate the cost.
func maxBucketPolicy(tb testing.TB) *policy.Policy {
	res := make([]string, policy.MaxResourcesPerStatement)
	for i := range res {
		res[i] = `"photos/*zb"`
	}
	ips := make([]string, 20)
	notIPs := make([]string, 20)
	for i := range ips {
		ips[i] = `"192.0.2.1"`
		notIPs[i] = `"198.51.100.1"`
	}
	ips[len(ips)-1] = `"10.0.0.0/8"` // the only value containing the source
	cond := `{"IpAddress":{"aws:SourceIp":[` + strings.Join(ips, ",") + `]},"NotIpAddress":{"aws:SourceIp":[` + strings.Join(notIPs, ",") + `]}}`
	stmt := `{"Effect":"Deny","Principal":"*","Action":["s3:*Object*"],"Resource":[` + strings.Join(res, ",") + `],"Condition":` + cond + `}`
	stmts := make([]string, policy.MaxStatements)
	for i := range stmts {
		stmts[i] = stmt
	}
	p, err := policy.Parse("photos", `{"Statement":[`+strings.Join(stmts, ",")+`]}`)
	if err != nil {
		tb.Fatal(err)
	}
	return p
}

// benignBucketPolicy denies a different action, so it never matches
// s3:DeleteObject: the common case the de-amplification must keep O(1) per key.
func benignBucketPolicy(tb testing.TB) *policy.Policy {
	p, err := policy.Parse("b", `{"Statement":[{"Effect":"Deny","Principal":{"S3RP":["ta/someone"]},"Action":"s3:PutObject","Resource":"b/*"}]}`)
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
	rc := from("10.0.0.1")
	b.ReportAllocs()
	for b.Loop() {
		p.Evaluate("ta/someuser", "s3:DeleteObject", key, rc)
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

// BenchmarkDeleteObjects mirrors the proxy's per-request path (see the
// DeleteObjects loop in proxy.go): resolve the resource-independent parts
// once, skip the per-object work entirely when nothing can deny, and
// otherwise test only the resource for each of 1000 keys. The benign case
// must stay O(1) for the whole batch — if the AlwaysAllows skip regresses it
// jumps by three orders of magnitude — and the adversarial case is bounded by
// the resource-pattern count cap.
func BenchmarkDeleteObjects(b *testing.B) {
	up := maxUserPolicy()
	const bucket = "photos"
	keys := make([]string, 1000)
	for i := range keys {
		keys[i] = worstKey()
	}
	rc := from("10.0.0.1")
	run := func(b *testing.B, p *policy.Policy) {
		b.ReportAllocs()
		for b.Loop() {
			if !up.Allows("s3:DeleteObject") {
				continue
			}
			e := p.DenyEvaluatorFor("ta/someuser", "s3:DeleteObject", rc)
			if e.AlwaysAllows() {
				continue // the proxy skips the per-object loop entirely
			}
			for _, k := range keys {
				e.Denies(bucket + "/" + k)
			}
		}
	}
	b.Run("benign", func(b *testing.B) { run(b, benignBucketPolicy(b)) })
	b.Run("adversarial", func(b *testing.B) { run(b, maxBucketPolicy(b)) })
}
