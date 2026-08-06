package policy_test

import (
	"strings"
	"testing"

	"github.com/fujiwara/s3rp/policy"
)

func TestParse(t *testing.T) {
	t.Run("string and array forms", func(t *testing.T) {
		p, err := policy.Parse(`{
			"Version": "2012-10-17",
			"Statement": [
				{
					"Effect": "Deny",
					"Principal": {"S3RP": "ta/batch"},
					"Action": "s3:PutObject",
					"Resource": "photos/*"
				},
				{
					"Effect": "Deny",
					"Principal": {"S3RP": ["ta/batch", "ta/app1"]},
					"Action": ["s3:PutObject", "s3:DeleteObject"],
					"Resource": ["photos", "photos/*"]
				}
			]
		}`)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Statement) != 2 {
			t.Fatalf("expect 2 statements, got %d", len(p.Statement))
		}
		if len(p.Statement[0].Action) != 1 || p.Statement[0].Action[0] != "s3:PutObject" {
			t.Errorf("unexpected action %v", p.Statement[0].Action)
		}
		if len(p.Statement[1].Principal.Users) != 2 {
			t.Errorf("unexpected principal %+v", p.Statement[1].Principal)
		}
	})
	t.Run("wildcard principal", func(t *testing.T) {
		p, err := policy.Parse(`{
			"Statement": [
				{"Effect": "Deny", "Principal": "*", "Action": "s3:DeleteObject", "Resource": "photos/*"}
			]
		}`)
		if err != nil {
			t.Fatal(err)
		}
		if !p.Statement[0].Principal.All {
			t.Error("expect All principal")
		}
	})
	t.Run("not principal", func(t *testing.T) {
		p, err := policy.Parse(`{
			"Statement": [
				{"Effect": "Deny", "NotPrincipal": {"S3RP": ["ta/admin"]}, "Action": "s3:PutObject", "Resource": "photos/*"}
			]
		}`)
		if err != nil {
			t.Fatal(err)
		}
		if p.Statement[0].NotPrincipal == nil || p.Statement[0].NotPrincipal.Users[0] != "ta/admin" {
			t.Errorf("unexpected NotPrincipal %+v", p.Statement[0].NotPrincipal)
		}
	})

	errCases := []struct {
		name   string
		text   string
		errStr string
	}{
		{"malformed json", `{`, "malformed policy JSON"},
		{"no statements", `{"Statement": []}`, "at least one statement"},
		{
			"invalid effect",
			`{"Statement": [{"Effect": "Maybe", "Principal": "*", "Action": "s3:GetObject", "Resource": "b"}]}`,
			"effect must be Allow or Deny",
		},
		{
			"both principal and not principal",
			`{"Statement": [{"Effect": "Deny", "Principal": "*", "NotPrincipal": {"S3RP": ["a1"]}, "Action": "s3:GetObject", "Resource": "b"}]}`,
			"exactly one of Principal or NotPrincipal",
		},
		{
			"neither principal nor not principal",
			`{"Statement": [{"Effect": "Deny", "Action": "s3:GetObject", "Resource": "b"}]}`,
			"exactly one of Principal or NotPrincipal",
		},
		{
			"wildcard not principal",
			`{"Statement": [{"Effect": "Deny", "NotPrincipal": "*", "Action": "s3:GetObject", "Resource": "b"}]}`,
			"matches nobody",
		},
		{
			"invalid principal",
			`{"Statement": [{"Effect": "Deny", "Principal": {"S3RP": ["Bad_User"]}, "Action": "s3:GetObject", "Resource": "b"}]}`,
			"invalid principal",
		},
		{
			"invalid principal shape",
			`{"Statement": [{"Effect": "Deny", "Principal": "everyone", "Action": "s3:GetObject", "Resource": "b"}]}`,
			"principal must be",
		},
		{
			"non-s3 action",
			`{"Statement": [{"Effect": "Deny", "Principal": "*", "Action": "iam:PassRole", "Resource": "b"}]}`,
			"must start with s3:",
		},
		{
			"no resource",
			`{"Statement": [{"Effect": "Deny", "Principal": "*", "Action": "s3:GetObject", "Resource": []}]}`,
			"at least one resource",
		},
		{
			"empty action array",
			`{"Statement": [{"Effect": "Deny", "Principal": "*", "Action": [], "Resource": "b"}]}`,
			"at least one action",
		},
		{
			"unsupported version",
			`{"Version": "2010-05-08", "Statement": [{"Effect": "Deny", "Principal": "*", "Action": "s3:GetObject", "Resource": "b"}]}`,
			"unsupported policy version",
		},
		{
			// the wildcard principal is the string "*", not a user named "*"
			"wildcard user in principal array",
			`{"Statement": [{"Effect": "Deny", "Principal": {"S3RP": ["*"]}, "Action": "s3:GetObject", "Resource": "b"}]}`,
			"invalid principal",
		},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := policy.Parse(tc.text)
			if err == nil {
				t.Fatal("expect error")
			}
			if !strings.Contains(err.Error(), tc.errStr) {
				t.Errorf("expect error containing %q, got %q", tc.errStr, err)
			}
		})
	}
}

// TestDenyEvaluator verifies the per-request evaluator (which pre-matches
// principal and action) agrees with Evaluate on the Deny decision, and that
// AlwaysAllows short-circuits when no Deny statement matches.
func TestDenyEvaluator(t *testing.T) {
	p, err := policy.Parse(`{
		"Statement": [
			{"Effect": "Deny", "Principal": {"S3RP": ["ta/batch"]}, "Action": ["s3:DeleteObject", "s3:PutObject"], "Resource": ["photos/*", "photos/2026/*"]},
			{"Effect": "Deny", "Principal": "*", "Action": "s3:DeleteObject", "Resource": "photos/archive/*"},
			{"Effect": "Allow", "Principal": {"S3RP": ["ta/batch"]}, "Action": "s3:GetObject", "Resource": "photos/*"}
		]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		principal, action, resource string
	}{
		{"ta/batch", "s3:DeleteObject", "photos/a.jpg"},
		{"ta/batch", "s3:DeleteObject", "logs/a.jpg"},
		{"ta/batch", "s3:DeleteObject", "photos/archive/x"},
		{"ta/app1", "s3:DeleteObject", "photos/a.jpg"},
		{"ta/app1", "s3:DeleteObject", "photos/archive/x"},
		{"ta/batch", "s3:GetObject", "photos/a.jpg"}, // only an inert Allow matches
		{"ta/batch", "s3:PutObject", "photos/a.jpg"},
	}
	for _, tc := range cases {
		want := p.Evaluate(tc.principal, tc.action, tc.resource) == policy.Deny
		eval := p.DenyEvaluatorFor(tc.principal, tc.action)
		if got := eval.Denies(tc.resource); got != want {
			t.Errorf("Denies(%s,%s,%s)=%v, Evaluate says deny=%v", tc.principal, tc.action, tc.resource, got, want)
		}
		if eval.AlwaysAllows() && eval.Denies(tc.resource) {
			t.Errorf("AlwaysAllows but Denies(%s)", tc.resource)
		}
	}
	// no Deny statement matches this action -> AlwaysAllows, per-object check skippable
	if !p.DenyEvaluatorFor("ta/batch", "s3:GetObject").AlwaysAllows() {
		t.Error("expect AlwaysAllows when only an inert Allow matches")
	}
	if !p.DenyEvaluatorFor("ta/app1", "s3:PutObject").AlwaysAllows() {
		t.Error("expect AlwaysAllows when no statement matches the principal")
	}
	// a matching Deny is not AlwaysAllows
	if p.DenyEvaluatorFor("ta/batch", "s3:DeleteObject").AlwaysAllows() {
		t.Error("expect not AlwaysAllows when a Deny matches")
	}
}

func TestEvaluate(t *testing.T) {
	p, err := policy.Parse(`{
		"Statement": [
			{
				"Sid": "BatchReadOnly",
				"Effect": "Deny",
				"Principal": {"S3RP": ["ta/batch"]},
				"Action": ["s3:PutObject", "s3:DeleteObject"],
				"Resource": ["photos/*"]
			},
			{
				"Sid": "NoDeleteForAnyone",
				"Effect": "Deny",
				"Principal": "*",
				"Action": "s3:DeleteObject",
				"Resource": "photos/archive/*"
			},
			{
				"Sid": "AllowIsNoOp",
				"Effect": "Allow",
				"Principal": {"S3RP": ["ta/app1"]},
				"Action": "s3:GetObject",
				"Resource": "photos/*"
			}
		]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name                        string
		principal, action, resource string
		want                        policy.Effect
	}{
		{"batch denied put", "ta/batch", "s3:PutObject", "photos/a.jpg", policy.Deny},
		{"batch denied delete", "ta/batch", "s3:DeleteObject", "photos/a.jpg", policy.Deny},
		{"batch allowed get", "ta/batch", "s3:GetObject", "photos/a.jpg", policy.None},
		{"app1 put not denied", "ta/app1", "s3:PutObject", "photos/a.jpg", policy.None},
		{"everyone denied archive delete", "ta/app1", "s3:DeleteObject", "photos/archive/x", policy.Deny},
		{"future user denied archive delete", "newuser", "s3:DeleteObject", "photos/archive/x", policy.Deny},
		{"allow statement matches", "ta/app1", "s3:GetObject", "photos/a.jpg", policy.Allow},
		{"wildcard crosses slash", "ta/batch", "s3:PutObject", "photos/deep/nested/key", policy.Deny},
		{"bucket-level resource not matched by prefix", "ta/batch", "s3:PutObject", "otherbucket/a", policy.None},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.Evaluate(tc.principal, tc.action, tc.resource); got != tc.want {
				t.Errorf("expect %v, got %v", tc.want, got)
			}
		})
	}
}

func TestEvaluateNotPrincipal(t *testing.T) {
	p, err := policy.Parse(`{
		"Statement": [
			{
				"Sid": "OnlyAdminWrites",
				"Effect": "Deny",
				"NotPrincipal": {"S3RP": ["ta/admin"]},
				"Action": ["s3:PutObject", "s3:DeleteObject"],
				"Resource": ["photos", "photos/*"]
			}
		]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Evaluate("ta/admin", "s3:PutObject", "photos/a"); got != policy.None {
		t.Errorf("admin must not be denied, got %v", got)
	}
	// any other user, including ones added later, is denied
	for _, u := range []string{"ta/batch", "newuser"} {
		if got := p.Evaluate(u, "s3:PutObject", "photos/a"); got != policy.Deny {
			t.Errorf("%s must be denied, got %v", u, got)
		}
	}
	if got := p.Evaluate("ta/batch", "s3:GetObject", "photos/a"); got != policy.None {
		t.Errorf("read must not be denied, got %v", got)
	}
}

func TestEvaluateActionWildcard(t *testing.T) {
	p, err := policy.Parse(`{
		"Statement": [
			{"Effect": "Deny", "Principal": {"S3RP": ["ta/batch"]}, "Action": "s3:Put*", "Resource": "b/*"},
			{"Effect": "Deny", "Principal": {"S3RP": ["ta/batch2"]}, "Action": "s3:*", "Resource": "b/*"}
		]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Evaluate("ta/batch", "s3:PutObject", "b/k"); got != policy.Deny {
		t.Errorf("expect Deny for s3:Put*, got %v", got)
	}
	if got := p.Evaluate("ta/batch", "s3:PutObjectTagging", "b/k"); got != policy.Deny {
		t.Errorf("expect Deny for s3:Put*, got %v", got)
	}
	if got := p.Evaluate("ta/batch", "s3:GetObject", "b/k"); got != policy.None {
		t.Errorf("expect None for get, got %v", got)
	}
	if got := p.Evaluate("ta/batch2", "s3:GetObject", "b/k"); got != policy.Deny {
		t.Errorf("expect Deny for s3:*, got %v", got)
	}
}

// TestEvaluateActionMiddleWildcard covers wildcards in the middle of an
// action pattern (e.g. s3:*Object*), not just a trailing s3:Get*.
func TestEvaluateActionMiddleWildcard(t *testing.T) {
	p, err := policy.Parse(`{
		"Statement": [
			{"Effect": "Deny", "Principal": {"S3RP": ["ta/user1"]}, "Action": ["s3:*Object*", "s3:*Multipart*"], "Resource": "b/*"}
		]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	deny := []string{"s3:GetObject", "s3:PutObjectTagging", "s3:ListMultipartUploadParts"}
	for _, a := range deny {
		if got := p.Evaluate("ta/user1", a, "b/k"); got != policy.Deny {
			t.Errorf("Evaluate(%q) = %v, want Deny", a, got)
		}
	}
	if got := p.Evaluate("ta/user1", "s3:ListBucket", "b/k"); got != policy.None {
		t.Errorf("s3:ListBucket should not match, got %v", got)
	}
}

// TestEvaluateResourceWildcard covers resource patterns: multi-segment
// wildcards, prefix wildcards, and bucket-only vs object patterns.
func TestEvaluateResourceWildcard(t *testing.T) {
	p, err := policy.Parse(`{
		"Statement": [
			{"Effect": "Deny", "Principal": {"S3RP": ["ta/user1"]}, "Action": "s3:GetObject",
			 "Resource": ["b/*/*", "b/2026-*"]}
		]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		resource string
		want     policy.Effect
	}{
		{"b/a/x", policy.Deny},             // b/*/*
		{"b/deep/nested/key", policy.Deny}, // * spans /
		{"b/a", policy.None},               // b/*/* needs two segments, b/2026-* no
		{"b/2026-01", policy.Deny},         // b/2026-* prefix wildcard, single segment
		{"b/2025-01", policy.None},         // wrong prefix, single segment
	}
	for _, tc := range cases {
		if got := p.Evaluate("ta/user1", "s3:GetObject", tc.resource); got != tc.want {
			t.Errorf("Evaluate(resource=%q) = %v, want %v", tc.resource, got, tc.want)
		}
	}
}

// TestEvaluateLiteralDotSegments locks in that dot segments in object keys
// are matched literally: S3 keys are opaque strings, so "photos/public/*"
// does not grant access to a key literally named "photos/private.txt", and
// a key containing ".." is a distinct object, not a traversal.
func TestEvaluateLiteralDotSegments(t *testing.T) {
	p, err := policy.Parse(`{
		"Statement": [
			{"Effect": "Allow", "Principal": {"S3RP": ["ta/user1"]}, "Action": "s3:GetObject", "Resource": "photos/public/*"}
		]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	// the ".." key matches the prefix literally (it is not collapsed)
	if got := p.Evaluate("ta/user1", "s3:GetObject", "photos/public/../private.txt"); got != policy.Allow {
		t.Errorf("literal .. under the prefix should match, got %v", got)
	}
	// a sibling key outside the prefix is not matched
	if got := p.Evaluate("ta/user1", "s3:GetObject", "photos/private.txt"); got != policy.None {
		t.Errorf("key outside the prefix must not match, got %v", got)
	}
}

// TestMatchWildcards exercises the Match function directly for both the
// "*" (any run) and "?" (single character) wildcards, including
// combinations and rune handling.
func TestMatchWildcards(t *testing.T) {
	cases := []struct {
		pattern, value string
		want           bool
	}{
		// exact and star
		{"s3:GetObject", "s3:GetObject", true},
		{"s3:GetObject", "s3:PutObject", false},
		{"s3:Get*", "s3:GetObject", true},
		{"s3:*Object*", "s3:PutObjectTagging", true},
		{"b/*", "b/deep/nested/key", true}, // star spans /
		{"b/*/*", "b/a/x", true},
		{"b/*/*", "b/a", false},
		// single-character ?
		{"s3:Get?bject", "s3:GetObject", true},   // ? matches O
		{"s3:Get??bject", "s3:GetObject", false}, // two ? overshoot
		{"s3:Get?", "s3:Get1", true},
		{"s3:Get?", "s3:Get12", false},
		{"a?c", "abc", true},
		{"a?c", "ac", false}, // ? requires exactly one char
		{"a?c", "a/c", true}, // ? matches any char, including /
		// combinations
		{"foo/???/bar", "foo/qux/bar", true},
		{"foo/???/bar", "foo/quxx/bar", false},
		{"pre*mid?end", "preXXXmidYend", true},
		{"pre*mid?end", "preXXXmidend", false},
		// no escaping: a literal ? in the value needs a ? or * in the pattern
		{"a?c", "a?c", true},
		{"abc", "a?c", false},
		// rune-aware: ? matches one multibyte rune
		{"a?c", "aあc", true},
	}
	for _, tc := range cases {
		if got := policy.Match(tc.pattern, tc.value); got != tc.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.pattern, tc.value, got, tc.want)
		}
	}
}

// TestEvaluateQuestionWildcard verifies "?" works through policy evaluation,
// so a Deny using it does not silently fail open.
func TestEvaluateQuestionWildcard(t *testing.T) {
	p, err := policy.Parse(`{
		"Statement": [
			{"Effect": "Deny", "Principal": {"S3RP": ["ta/batch"]}, "Action": "s3:???Object", "Resource": "b/log-????"}
		]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Evaluate("ta/batch", "s3:PutObject", "b/log-2026"); got != policy.Deny {
		t.Errorf("expect Deny, got %v", got)
	}
	if got := p.Evaluate("ta/batch", "s3:GetObject", "b/log-2026"); got != policy.Deny {
		t.Errorf("expect Deny (s3:???Object matches Get), got %v", got)
	}
	// resource with the wrong length does not match the ???? pattern
	if got := p.Evaluate("ta/batch", "s3:PutObject", "b/log-12345"); got != policy.None {
		t.Errorf("expect None (log-???? needs 4 chars), got %v", got)
	}
}

func TestUserPolicyAllows(t *testing.T) {
	// nil and empty policies allow everything (the default)
	if !(*policy.UserPolicy)(nil).Allows("s3:DeleteObject") {
		t.Error("nil policy must allow all")
	}
	if !(&policy.UserPolicy{}).Allows("s3:DeleteObject") {
		t.Error("empty policy must allow all")
	}

	up := &policy.UserPolicy{Statements: []policy.ActionStatement{
		{Effect: "Allow", Action: []string{"s3:Get*", "s3:List*", "s3:HeadObject"}},
		{Effect: "Deny", Action: []string{"s3:GetObjectAcl"}},
	}}
	cases := []struct {
		action string
		want   bool
	}{
		{"s3:GetObject", true},     // Allow s3:Get*
		{"s3:ListBucket", true},    // Allow s3:List*
		{"s3:HeadObject", true},    // exact allow
		{"s3:PutObject", false},    // not allowed -> implicit deny
		{"s3:DeleteObject", false}, // not allowed
		{"s3:GetObjectAcl", false}, // Allow s3:Get* but explicit Deny wins
		{"S3:getobject", true},     // case-insensitive
	}
	for _, tc := range cases {
		if got := up.Allows(tc.action); got != tc.want {
			t.Errorf("Allows(%q) = %v, want %v", tc.action, got, tc.want)
		}
	}
}

// TestUserPolicyDenyOnly documents that a policy with only Deny statements
// denies everything else (no Allow -> implicit deny), matching AWS.
func TestUserPolicyDenyOnly(t *testing.T) {
	up := &policy.UserPolicy{Statements: []policy.ActionStatement{
		{Effect: "Deny", Action: []string{"s3:DeleteObject"}},
	}}
	if up.Allows("s3:GetObject") {
		t.Error("a Deny-only policy implicitly denies unmatched actions")
	}
	if up.Allows("s3:DeleteObject") {
		t.Error("explicitly denied action must be denied")
	}
}

// TestUserPolicyUnknownEffect locks in that a statement whose effect is
// neither Allow nor Deny (e.g. a typo that skipped validation because it was
// written straight into the store) is ignored rather than granting access.
func TestUserPolicyUnknownEffect(t *testing.T) {
	// a mistyped Deny must not fail open into an allow
	up := &policy.UserPolicy{Statements: []policy.ActionStatement{
		{Effect: "Denyy", Action: []string{"s3:DeleteObject"}},
	}}
	if up.Allows("s3:DeleteObject") {
		t.Error("an unrecognized effect must not grant access")
	}
	// even alongside a real Allow, the garbage statement grants nothing extra
	up = &policy.UserPolicy{Statements: []policy.ActionStatement{
		{Effect: "Allow", Action: []string{"s3:GetObject"}},
		{Effect: "allow", Action: []string{"s3:PutObject"}}, // wrong case
	}}
	if !up.Allows("s3:GetObject") {
		t.Error("valid Allow should still grant")
	}
	if up.Allows("s3:PutObject") {
		t.Error("lower-case effect must not grant")
	}
}

func TestValidateUserPolicy(t *testing.T) {
	if err := policy.ValidateUserPolicy(nil); err != nil {
		t.Errorf("nil policy is valid: %v", err)
	}
	valid := &policy.UserPolicy{Statements: []policy.ActionStatement{
		{Effect: "Allow", Action: []string{"s3:Get*"}},
	}}
	if err := policy.ValidateUserPolicy(valid); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	errCases := []struct {
		name   string
		up     *policy.UserPolicy
		errStr string
	}{
		{"bad effect", &policy.UserPolicy{Statements: []policy.ActionStatement{{Effect: "Maybe", Action: []string{"s3:Get*"}}}}, "effect must be Allow or Deny"},
		{"empty action", &policy.UserPolicy{Statements: []policy.ActionStatement{{Effect: "Allow"}}}, "at least one action"},
		{"non-s3 action", &policy.UserPolicy{Statements: []policy.ActionStatement{{Effect: "Allow", Action: []string{"iam:PassRole"}}}}, "must start with s3:"},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			err := policy.ValidateUserPolicy(tc.up)
			if err == nil || !strings.Contains(err.Error(), tc.errStr) {
				t.Errorf("expect error containing %q, got %v", tc.errStr, err)
			}
		})
	}
}

// TestPolicyLimits verifies that oversized policies are rejected at
// parse/validate time. Policies are tenant-authored, so unbounded size would
// let a single authorization do unbounded glob work (amplified per object by
// DeleteObjects).
func TestPolicyLimits(t *testing.T) {
	longPat := "s3:" + strings.Repeat("x", policy.MaxPatternLen)

	// bucket policy limits
	tooManyStmts := make([]string, policy.MaxStatements+1)
	for i := range tooManyStmts {
		tooManyStmts[i] = `{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"b/*"}`
	}
	// a valid statement padded past the byte cap with whitespace
	oversizeText := `{"Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"b/*"}]}` +
		strings.Repeat(" ", policy.MaxPolicyBytes)
	tooManyPrincipals := make([]string, policy.MaxPrincipalUsers+1)
	for i := range tooManyPrincipals {
		tooManyPrincipals[i] = `"user"`
	}

	bucketCases := []struct {
		name, text, errStr string
	}{
		{"policy too large", oversizeText, "at most"},
		{"too many principal users", `{"Statement":[{"Effect":"Deny","Principal":{"S3RP":[` + strings.Join(tooManyPrincipals, ",") + `]},"Action":"s3:GetObject","Resource":"b/*"}]}`, "principal users"},
		{"too many statements", `{"Statement":[` + strings.Join(tooManyStmts, ",") + `]}`, "at most"},
		{"too many actions", `{"Statement":[{"Effect":"Deny","Principal":"*","Action":[` + repeatQuoted(`"s3:GetObject"`, policy.MaxActionsPerStatement+1) + `],"Resource":"b/*"}]}`, "at most"},
		{"too many resources", `{"Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":[` + repeatQuoted(`"b/*"`, policy.MaxResourcesPerStatement+1) + `]}]}`, "at most"},
		{"action pattern too long", `{"Statement":[{"Effect":"Deny","Principal":"*","Action":"` + longPat + `","Resource":"b/*"}]}`, "too long"},
		{"resource pattern too long", `{"Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"` + strings.Repeat("a", policy.MaxPatternLen+1) + `"}]}`, "too long"},
	}
	for _, tc := range bucketCases {
		t.Run("bucket/"+tc.name, func(t *testing.T) {
			if _, err := policy.Parse(tc.text); err == nil || !strings.Contains(err.Error(), tc.errStr) {
				t.Errorf("expect error containing %q, got %v", tc.errStr, err)
			}
		})
	}

	// user policy limits
	overStmts := make([]policy.ActionStatement, policy.MaxStatements+1)
	for i := range overStmts {
		overStmts[i] = policy.ActionStatement{Effect: "Allow", Action: []string{"s3:GetObject"}}
	}
	overActions := make([]string, policy.MaxActionsPerStatement+1)
	for i := range overActions {
		overActions[i] = "s3:GetObject"
	}
	// structurally valid but marshals over MaxPolicyBytes (statements/actions
	// within caps, each action pattern near the max length)
	bigActions := make([]string, policy.MaxActionsPerStatement)
	for i := range bigActions {
		bigActions[i] = "s3:" + strings.Repeat("z", policy.MaxPatternLen-4)
	}
	bigStmts := make([]policy.ActionStatement, policy.MaxStatements)
	for i := range bigStmts {
		bigStmts[i] = policy.ActionStatement{Effect: "Allow", Action: bigActions}
	}
	userCases := []struct {
		name   string
		up     *policy.UserPolicy
		errStr string
	}{
		{"too many statements", &policy.UserPolicy{Statements: overStmts}, "at most"},
		{"too many actions", &policy.UserPolicy{Statements: []policy.ActionStatement{{Effect: "Allow", Action: overActions}}}, "at most"},
		{"action pattern too long", &policy.UserPolicy{Statements: []policy.ActionStatement{{Effect: "Allow", Action: []string{longPat}}}}, "too long"},
	}
	for _, tc := range userCases {
		t.Run("user/"+tc.name, func(t *testing.T) {
			if err := policy.ValidateUserPolicy(tc.up); err == nil || !strings.Contains(err.Error(), tc.errStr) {
				t.Errorf("expect error containing %q, got %v", tc.errStr, err)
			}
		})
	}

	// The byte cap applies to the serialized form, so it is enforced by
	// MarshalUserPolicy rather than the structural validation: a policy within
	// every structural cap can still be too large once marshaled.
	t.Run("user/policy too large", func(t *testing.T) {
		up := &policy.UserPolicy{Statements: bigStmts}
		if err := policy.ValidateUserPolicy(up); err != nil {
			t.Fatalf("structural validation should pass: %v", err)
		}
		if _, err := policy.MarshalUserPolicy(up); err == nil || !strings.Contains(err.Error(), "at most") {
			t.Errorf("expect marshal to reject an oversized policy, got %v", err)
		}
	})
}

func repeatQuoted(s string, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = s
	}
	return strings.Join(parts, ",")
}

// TestEvaluateActionCaseInsensitive verifies that actions match regardless
// of case (as in AWS), so a mis-cased Deny cannot silently fail open, while
// resources stay case-sensitive.
func TestEvaluateActionCaseInsensitive(t *testing.T) {
	p, err := policy.Parse(`{
		"Statement": [
			{"Effect": "Deny", "Principal": {"S3RP": ["ta/batch"]}, "Action": ["s3:putobject", "S3:DeleteObject", "s3:Get*"], "Resource": "photos/*"}
		]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	// the proxy always passes the canonical action name
	cases := []struct {
		action   string
		resource string
		want     policy.Effect
	}{
		{"s3:PutObject", "photos/a", policy.Deny},    // policy lower-cased
		{"s3:DeleteObject", "photos/a", policy.Deny}, // policy S3: prefix
		{"s3:GetObject", "photos/a", policy.Deny},    // wildcard, mixed case
		{"s3:HeadObject", "photos/a", policy.None},   // not covered
		{"s3:PutObject", "Photos/a", policy.None},    // resource case-sensitive
	}
	for _, tc := range cases {
		if got := p.Evaluate("ta/batch", tc.action, tc.resource); got != tc.want {
			t.Errorf("Evaluate(%q, %q) = %v, want %v", tc.action, tc.resource, got, tc.want)
		}
	}
}
