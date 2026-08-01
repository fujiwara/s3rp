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
