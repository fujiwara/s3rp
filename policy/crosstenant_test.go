package policy_test

import (
	"strings"
	"testing"

	"github.com/fujiwara/s3rp/policy"
)

// Principals are always tenant-qualified: "tenant/user" one user,
// "tenant/*" every user of a tenant, "*" every authenticated user of any
// tenant. The requester's tenant no longer changes how a statement matches —
// cross-tenant behavior comes solely from the gateway's baseline (deny
// unless allowed), not from special matching rules.

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
		{"listed principal is allowed", "tb/bob", "s3:GetObject", "shared/a.txt", policy.Allow},
		{"unlisted principal matches nothing", "tb/carol", "s3:GetObject", "shared/a.txt", policy.None},
		{"unlisted action matches nothing", "tb/bob", "s3:PutObject", "shared/a.txt", policy.None},
		{"blanket Deny catches the listed principal", "tb/bob", "s3:GetObject", "shared/secret/x", policy.Deny},
		{"blanket Deny catches any principal", "ta/alice", "s3:GetObject", "shared/secret/x", policy.Deny},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.Evaluate(tc.principal, tc.action, tc.resource, policy.RequestContext{}); got != tc.want {
				t.Errorf("Evaluate(%q, %q, %q) = %v, want %v", tc.principal, tc.action, tc.resource, got, tc.want)
			}
		})
	}
}

func TestEvaluateWildcardPrincipals(t *testing.T) {
	t.Run("star matches every authenticated principal", func(t *testing.T) {
		p, err := policy.Parse(`{
			"Statement": [
				{"Effect": "Allow", "Principal": "*", "Action": "s3:GetObject", "Resource": "open/*"}
			]
		}`)
		if err != nil {
			t.Fatal(err)
		}
		for _, principal := range []string{"ta/alice", "tb/bob"} {
			if got := p.Evaluate(principal, "s3:GetObject", "open/a.txt", policy.RequestContext{}); got != policy.Allow {
				t.Errorf("Allow * must match %q: %v", principal, got)
			}
		}
	})
	t.Run("tenant wildcard matches only that tenant", func(t *testing.T) {
		p, err := policy.Parse(`{
			"Statement": [
				{"Effect": "Allow", "Principal": {"S3RP": ["tb/*"]}, "Action": "s3:GetObject", "Resource": "shared/*"}
			]
		}`)
		if err != nil {
			t.Fatal(err)
		}
		for _, principal := range []string{"tb/bob", "tb/carol"} {
			if got := p.Evaluate(principal, "s3:GetObject", "shared/a.txt", policy.RequestContext{}); got != policy.Allow {
				t.Errorf("tb/* must match %q: %v", principal, got)
			}
		}
		if got := p.Evaluate("ta/alice", "s3:GetObject", "shared/a.txt", policy.RequestContext{}); got != policy.None {
			t.Errorf("tb/* must not match ta/alice: %v", got)
		}
		// the wildcard is a whole-name form, not a prefix: "tb" the tenant,
		// not tenants starting with "tb"
		if got := p.Evaluate("tbx/bob", "s3:GetObject", "shared/a.txt", policy.RequestContext{}); got != policy.None {
			t.Errorf("tb/* must not match tbx/bob: %v", got)
		}
	})
	t.Run("tenant wildcard works in Deny and NotPrincipal", func(t *testing.T) {
		p, err := policy.Parse(`{
			"Statement": [
				{"Effect": "Deny", "NotPrincipal": {"S3RP": ["ta/*"]}, "Action": "s3:PutObject", "Resource": "b/*"}
			]
		}`)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Evaluate("tb/bob", "s3:PutObject", "b/k", policy.RequestContext{}); got != policy.Deny {
			t.Errorf("everyone outside ta must be denied: %v", got)
		}
		if got := p.Evaluate("ta/alice", "s3:PutObject", "b/k", policy.RequestContext{}); got != policy.None {
			t.Errorf("ta users are excluded from the Deny: %v", got)
		}
	})
}

func TestNotPrincipalDenyOnly(t *testing.T) {
	_, err := policy.Parse(`{
		"Statement": [
			{"Effect": "Allow", "NotPrincipal": {"S3RP": ["ta/alice"]}, "Action": "s3:GetObject", "Resource": "b/*"}
		]
	}`)
	if err == nil || !strings.Contains(err.Error(), "NotPrincipal is only allowed with Effect Deny") {
		t.Errorf("Allow + NotPrincipal must be rejected, got %v", err)
	}
}

func TestParseQualifiedPrincipal(t *testing.T) {
	valid := []string{"tb/bob", "tenant-b/some_user", "t2/u2", "tb/*"}
	for _, u := range valid {
		if _, err := policy.Parse(`{
			"Statement": [
				{"Effect": "Allow", "Principal": {"S3RP": ["` + u + `"]}, "Action": "s3:GetObject", "Resource": "b/*"}
			]
		}`); err != nil {
			t.Errorf("principal %q must parse: %v", u, err)
		}
	}
	invalid := []string{"bob", "tb/tb/bob", "/bob", "tb/", "Tb/bob", "tb/B0b", "*/bob", "tb/b*"}
	for _, u := range invalid {
		_, err := policy.Parse(`{
			"Statement": [
				{"Effect": "Allow", "Principal": {"S3RP": ["` + u + `"]}, "Action": "s3:GetObject", "Resource": "b/*"}
			]
		}`)
		if err == nil || !strings.Contains(err.Error(), "invalid principal") {
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
	e := p.AllowEvaluatorFor("tb/bob", "s3:DeleteObject", policy.RequestContext{})
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
	d := p.DenyEvaluatorFor("tb/bob", "s3:DeleteObject", policy.RequestContext{})
	if !d.Denies("shared/tmp/pinned/a.txt") {
		t.Error("Deny must still win inside the allowed prefix")
	}

	unmatched := p.AllowEvaluatorFor("tb/carol", "s3:DeleteObject", policy.RequestContext{})
	if !unmatched.AlwaysDenies() {
		t.Error("no Allow matches this principal, AlwaysDenies must be true")
	}
	if unmatched.Allows("shared/tmp/a.txt") {
		t.Error("an empty evaluator must not allow anything")
	}
	wrongAction := p.AllowEvaluatorFor("tb/bob", "s3:PutObject", policy.RequestContext{})
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
	// "tenant/*" and "*" grants cover principals they match
	wildcards, err := policy.Parse(`{
		"Statement": [
			{"Effect": "Allow", "Principal": {"S3RP": ["tb/*"]}, "Action": "s3:GetObject", "Resource": "b/*"},
			{"Effect": "Allow", "Principal": "*", "Action": "s3:ListBucket", "Resource": "b"}
		]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if !wildcards.MentionsPrincipal("tb/carol") {
		t.Error("tb/* must mention tb/carol")
	}
	if !wildcards.MentionsPrincipal("tc/dave") {
		t.Error(`Allow "*" must mention every authenticated principal`)
	}
}
