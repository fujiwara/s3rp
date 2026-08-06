package policy_test

import (
	"strings"
	"testing"

	"github.com/fujiwara/s3rp/policy"
)

// A qualified principal ("tenant/user") names another tenant's user. It is
// granted only by an explicit Principal listing — "*" and NotPrincipal never
// widen an Allow across tenants — while Deny statements match it broadly.

const crossTenantPolicy = `{
	"Statement": [
		{
			"Effect": "Allow",
			"Principal": {"S3RP": ["tb/bob"]},
			"Action": "s3:GetObject",
			"Resource": "shared/*"
		},
		{
			"Effect": "Deny",
			"Principal": "*",
			"Action": "s3:GetObject",
			"Resource": "shared/secret/*"
		}
	]
}`

func TestEvaluateQualifiedPrincipal(t *testing.T) {
	p, err := policy.Parse(crossTenantPolicy)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		principal string
		action    string
		resource  string
		want      policy.Effect
	}{
		{"listed qualified principal is allowed", "tb/bob", "s3:GetObject", "shared/a.txt", policy.Allow},
		{"other qualified principal matches nothing", "tb/carol", "s3:GetObject", "shared/a.txt", policy.None},
		{"plain name does not match the qualified listing", "bob", "s3:GetObject", "shared/a.txt", policy.None},
		{"unlisted action matches nothing", "tb/bob", "s3:PutObject", "shared/a.txt", policy.None},
		{"blanket Deny catches the qualified principal", "tb/bob", "s3:GetObject", "shared/secret/x", policy.Deny},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.Evaluate(tc.principal, tc.action, tc.resource); got != tc.want {
				t.Errorf("Evaluate(%q, %q, %q) = %v, want %v", tc.principal, tc.action, tc.resource, got, tc.want)
			}
		})
	}
}

func TestQualifiedPrincipalNeverWidens(t *testing.T) {
	t.Run("wildcard Allow does not reach qualified principals", func(t *testing.T) {
		p, err := policy.Parse(`{
			"Statement": [
				{"Effect": "Allow", "Principal": "*", "Action": "s3:GetObject", "Resource": "open/*"}
			]
		}`)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Evaluate("tb/bob", "s3:GetObject", "open/a.txt"); got != policy.None {
			t.Errorf("qualified principal matched Allow *: %v", got)
		}
		if got := p.Evaluate("alice", "s3:GetObject", "open/a.txt"); got != policy.Allow {
			t.Errorf("plain principal must match Allow *: %v", got)
		}
	})
	t.Run("NotPrincipal Allow does not reach qualified principals", func(t *testing.T) {
		p, err := policy.Parse(`{
			"Statement": [
				{"Effect": "Allow", "NotPrincipal": {"S3RP": ["alice"]}, "Action": "s3:GetObject", "Resource": "open/*"}
			]
		}`)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Evaluate("tb/bob", "s3:GetObject", "open/a.txt"); got != policy.None {
			t.Errorf("qualified principal matched NotPrincipal Allow: %v", got)
		}
		if got := p.Evaluate("carol", "s3:GetObject", "open/a.txt"); got != policy.Allow {
			t.Errorf("plain principal must match NotPrincipal Allow: %v", got)
		}
	})
	t.Run("NotPrincipal Deny catches qualified principals", func(t *testing.T) {
		p, err := policy.Parse(`{
			"Statement": [
				{"Effect": "Deny", "NotPrincipal": {"S3RP": ["tb/bob"]}, "Action": "s3:DeleteObject", "Resource": "shared/*"}
			]
		}`)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Evaluate("tb/carol", "s3:DeleteObject", "shared/a.txt"); got != policy.Deny {
			t.Errorf("excluded-only Deny must catch other qualified principals: %v", got)
		}
		if got := p.Evaluate("tb/bob", "s3:DeleteObject", "shared/a.txt"); got != policy.None {
			t.Errorf("the excluded qualified principal must not be denied: %v", got)
		}
	})
}

func TestParseQualifiedPrincipal(t *testing.T) {
	valid := []string{"tb/bob", "tenant-b/some_user", "t2/u2"}
	for _, u := range valid {
		if _, err := policy.Parse(`{
			"Statement": [
				{"Effect": "Allow", "Principal": {"S3RP": ["` + u + `"]}, "Action": "s3:GetObject", "Resource": "b/*"}
			]
		}`); err != nil {
			t.Errorf("principal %q must parse: %v", u, err)
		}
	}
	invalid := []string{"tb/tb/bob", "/bob", "tb/", "Tb/bob", "tb/B0b"}
	for _, u := range invalid {
		_, err := policy.Parse(`{
			"Statement": [
				{"Effect": "Allow", "Principal": {"S3RP": ["` + u + `"]}, "Action": "s3:GetObject", "Resource": "b/*"}
			]
		}`)
		if err == nil || !strings.Contains(err.Error(), "invalid principal user name") {
			t.Errorf("principal %q must be rejected, got %v", u, err)
		}
	}
}

func TestAllowEvaluator(t *testing.T) {
	p, err := policy.Parse(`{
		"Statement": [
			{"Effect": "Allow", "Principal": {"S3RP": ["tb/bob"]}, "Action": "s3:DeleteObject", "Resource": "shared/tmp/*"},
			{"Effect": "Deny", "Principal": {"S3RP": ["tb/bob"]}, "Action": "s3:DeleteObject", "Resource": "shared/tmp/pinned/*"}
		]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	e := p.AllowEvaluatorFor("tb/bob", "s3:DeleteObject")
	if e.AlwaysDenies() {
		t.Fatal("an Allow statement matched, AlwaysDenies must be false")
	}
	if !e.Allows("shared/tmp/a.txt") {
		t.Error("resource inside the Allow pattern must be allowed")
	}
	if e.Allows("shared/keep/a.txt") {
		t.Error("resource outside the Allow pattern must not be allowed")
	}
	// the Deny side is a separate evaluator, as in the gateway
	d := p.DenyEvaluatorFor("tb/bob", "s3:DeleteObject")
	if !d.Denies("shared/tmp/pinned/a.txt") {
		t.Error("Deny must still win inside the allowed prefix")
	}

	unmatched := p.AllowEvaluatorFor("tb/carol", "s3:DeleteObject")
	if !unmatched.AlwaysDenies() {
		t.Error("no Allow matches this principal, AlwaysDenies must be true")
	}
	if unmatched.Allows("shared/tmp/a.txt") {
		t.Error("an empty evaluator must not allow anything")
	}
	wrongAction := p.AllowEvaluatorFor("tb/bob", "s3:PutObject")
	if !wrongAction.AlwaysDenies() {
		t.Error("no Allow matches this action, AlwaysDenies must be true")
	}
}

func TestMentionsPrincipal(t *testing.T) {
	p, err := policy.Parse(crossTenantPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if !p.MentionsPrincipal("tb/bob") {
		t.Error("a principal listed in an Allow must be mentioned")
	}
	if p.MentionsPrincipal("tb/carol") {
		t.Error("an unlisted principal must not be mentioned")
	}
	// a Deny listing is not a mention: it grants nothing, so it must not
	// make the bucket visible
	denyOnly, err := policy.Parse(`{
		"Statement": [
			{"Effect": "Deny", "Principal": {"S3RP": ["tb/bob"]}, "Action": "s3:GetObject", "Resource": "b/*"}
		]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if denyOnly.MentionsPrincipal("tb/bob") {
		t.Error("a Deny-only listing must not count as a mention")
	}
}
