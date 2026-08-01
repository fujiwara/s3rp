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
					"Principal": {"S3RP": "batch"},
					"Action": "s3:PutObject",
					"Resource": "photos/*"
				},
				{
					"Effect": "Deny",
					"Principal": {"S3RP": ["batch", "app1"]},
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
				{"Effect": "Deny", "NotPrincipal": {"S3RP": ["admin"]}, "Action": "s3:PutObject", "Resource": "photos/*"}
			]
		}`)
		if err != nil {
			t.Fatal(err)
		}
		if p.Statement[0].NotPrincipal == nil || p.Statement[0].NotPrincipal.Users[0] != "admin" {
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
			"invalid principal user name",
			`{"Statement": [{"Effect": "Deny", "Principal": {"S3RP": ["Bad_User"]}, "Action": "s3:GetObject", "Resource": "b"}]}`,
			"invalid principal user name",
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
			"invalid principal user name",
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

func TestEvaluate(t *testing.T) {
	p, err := policy.Parse(`{
		"Statement": [
			{
				"Sid": "BatchReadOnly",
				"Effect": "Deny",
				"Principal": {"S3RP": ["batch"]},
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
				"Principal": {"S3RP": ["app1"]},
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
		{"batch denied put", "batch", "s3:PutObject", "photos/a.jpg", policy.Deny},
		{"batch denied delete", "batch", "s3:DeleteObject", "photos/a.jpg", policy.Deny},
		{"batch allowed get", "batch", "s3:GetObject", "photos/a.jpg", policy.None},
		{"app1 put not denied", "app1", "s3:PutObject", "photos/a.jpg", policy.None},
		{"everyone denied archive delete", "app1", "s3:DeleteObject", "photos/archive/x", policy.Deny},
		{"future user denied archive delete", "newuser", "s3:DeleteObject", "photos/archive/x", policy.Deny},
		{"allow statement matches", "app1", "s3:GetObject", "photos/a.jpg", policy.Allow},
		{"wildcard crosses slash", "batch", "s3:PutObject", "photos/deep/nested/key", policy.Deny},
		{"bucket-level resource not matched by prefix", "batch", "s3:PutObject", "otherbucket/a", policy.None},
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
				"NotPrincipal": {"S3RP": ["admin"]},
				"Action": ["s3:PutObject", "s3:DeleteObject"],
				"Resource": ["photos", "photos/*"]
			}
		]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Evaluate("admin", "s3:PutObject", "photos/a"); got != policy.None {
		t.Errorf("admin must not be denied, got %v", got)
	}
	// any other user, including ones added later, is denied
	for _, u := range []string{"batch", "newuser"} {
		if got := p.Evaluate(u, "s3:PutObject", "photos/a"); got != policy.Deny {
			t.Errorf("%s must be denied, got %v", u, got)
		}
	}
	if got := p.Evaluate("batch", "s3:GetObject", "photos/a"); got != policy.None {
		t.Errorf("read must not be denied, got %v", got)
	}
}

func TestEvaluateActionWildcard(t *testing.T) {
	p, err := policy.Parse(`{
		"Statement": [
			{"Effect": "Deny", "Principal": {"S3RP": ["batch"]}, "Action": "s3:Put*", "Resource": "b/*"},
			{"Effect": "Deny", "Principal": {"S3RP": ["batch2"]}, "Action": "s3:*", "Resource": "b/*"}
		]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Evaluate("batch", "s3:PutObject", "b/k"); got != policy.Deny {
		t.Errorf("expect Deny for s3:Put*, got %v", got)
	}
	if got := p.Evaluate("batch", "s3:PutObjectTagging", "b/k"); got != policy.Deny {
		t.Errorf("expect Deny for s3:Put*, got %v", got)
	}
	if got := p.Evaluate("batch", "s3:GetObject", "b/k"); got != policy.None {
		t.Errorf("expect None for get, got %v", got)
	}
	if got := p.Evaluate("batch2", "s3:GetObject", "b/k"); got != policy.Deny {
		t.Errorf("expect Deny for s3:*, got %v", got)
	}
}

// TestEvaluateActionMiddleWildcard covers wildcards in the middle of an
// action pattern (e.g. s3:*Object*), not just a trailing s3:Get*.
func TestEvaluateActionMiddleWildcard(t *testing.T) {
	p, err := policy.Parse(`{
		"Statement": [
			{"Effect": "Deny", "Principal": {"S3RP": ["user1"]}, "Action": ["s3:*Object*", "s3:*Multipart*"], "Resource": "b/*"}
		]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	deny := []string{"s3:GetObject", "s3:PutObjectTagging", "s3:ListMultipartUploadParts"}
	for _, a := range deny {
		if got := p.Evaluate("user1", a, "b/k"); got != policy.Deny {
			t.Errorf("Evaluate(%q) = %v, want Deny", a, got)
		}
	}
	if got := p.Evaluate("user1", "s3:ListBucket", "b/k"); got != policy.None {
		t.Errorf("s3:ListBucket should not match, got %v", got)
	}
}

// TestEvaluateResourceWildcard covers resource patterns: multi-segment
// wildcards, prefix wildcards, and bucket-only vs object patterns.
func TestEvaluateResourceWildcard(t *testing.T) {
	p, err := policy.Parse(`{
		"Statement": [
			{"Effect": "Deny", "Principal": {"S3RP": ["user1"]}, "Action": "s3:GetObject",
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
		if got := p.Evaluate("user1", "s3:GetObject", tc.resource); got != tc.want {
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
			{"Effect": "Allow", "Principal": {"S3RP": ["user1"]}, "Action": "s3:GetObject", "Resource": "photos/public/*"}
		]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	// the ".." key matches the prefix literally (it is not collapsed)
	if got := p.Evaluate("user1", "s3:GetObject", "photos/public/../private.txt"); got != policy.Allow {
		t.Errorf("literal .. under the prefix should match, got %v", got)
	}
	// a sibling key outside the prefix is not matched
	if got := p.Evaluate("user1", "s3:GetObject", "photos/private.txt"); got != policy.None {
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
			{"Effect": "Deny", "Principal": {"S3RP": ["batch"]}, "Action": "s3:???Object", "Resource": "b/log-????"}
		]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Evaluate("batch", "s3:PutObject", "b/log-2026"); got != policy.Deny {
		t.Errorf("expect Deny, got %v", got)
	}
	if got := p.Evaluate("batch", "s3:GetObject", "b/log-2026"); got != policy.Deny {
		t.Errorf("expect Deny (s3:???Object matches Get), got %v", got)
	}
	// resource with the wrong length does not match the ???? pattern
	if got := p.Evaluate("batch", "s3:PutObject", "b/log-12345"); got != policy.None {
		t.Errorf("expect None (log-???? needs 4 chars), got %v", got)
	}
}

// TestEvaluateActionCaseInsensitive verifies that actions match regardless
// of case (as in AWS), so a mis-cased Deny cannot silently fail open, while
// resources stay case-sensitive.
func TestEvaluateActionCaseInsensitive(t *testing.T) {
	p, err := policy.Parse(`{
		"Statement": [
			{"Effect": "Deny", "Principal": {"S3RP": ["batch"]}, "Action": ["s3:putobject", "S3:DeleteObject", "s3:Get*"], "Resource": "photos/*"}
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
		if got := p.Evaluate("batch", tc.action, tc.resource); got != tc.want {
			t.Errorf("Evaluate(%q, %q) = %v, want %v", tc.action, tc.resource, got, tc.want)
		}
	}
}
