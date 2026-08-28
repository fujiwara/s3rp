package policy_test

import (
	"strings"
	"testing"

	"github.com/fujiwara/s3rp/policy"
)

func TestDecide(t *testing.T) {
	p, err := policy.Parse("photos", `{"Statement": [
		{"Sid": "ReadAll", "Effect": "Allow", "Principal": "*", "Action": "s3:GetObject", "Resource": "photos/*"},
		{"Effect": "Allow", "Principal": {"S3RP": "tb/*"}, "Action": "s3:GetObject", "Resource": "photos/public/*"},
		{"Sid": "NoLogs", "Effect": "Deny", "Principal": "*", "Action": "s3:*", "Resource": "photos/logs/*"}
	]}`)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		principal string
		action    string
		resource  string
		effect    policy.Effect
		statement int
		named     string
	}{
		{"allow by sid", "ta/u", "s3:GetObject", "photos/a", policy.Allow, 0, `statement[0] "ReadAll"`},
		{"first allow wins the name", "tb/u", "s3:GetObject", "photos/public/a", policy.Allow, 0, `statement[0] "ReadAll"`},
		{"deny over allow", "ta/u", "s3:GetObject", "photos/logs/a", policy.Deny, 2, `statement[2] "NoLogs"`},
		{"none", "ta/u", "s3:PutObject", "photos/a", policy.None, -1, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := p.Decide(tc.principal, tc.action, tc.resource, policy.RequestContext{})
			if d.Effect != tc.effect || d.Statement != tc.statement {
				t.Errorf("got %+v, want effect %v statement %d", d, tc.effect, tc.statement)
			}
			if got := d.StatementName(p); got != tc.named {
				t.Errorf("name %q, want %q", got, tc.named)
			}
			if p.Evaluate(tc.principal, tc.action, tc.resource, policy.RequestContext{}) != tc.effect {
				t.Error("Evaluate disagrees with Decide")
			}
		})
	}
	t.Run("index without sid", func(t *testing.T) {
		one := policy.Decision{Effect: policy.Allow, Statement: 1}
		if got := one.StatementName(p); got != "statement[1]" {
			t.Errorf("name %q", got)
		}
	})
}

func TestDenyingStatement(t *testing.T) {
	p, err := policy.Parse("photos", `{"Statement": [
		{"Sid": "NoLogs", "Effect": "Deny", "Principal": "*", "Action": "s3:DeleteObject", "Resource": ["photos/logs/*", "photos/audit/*"]},
		{"Sid": "NoTmp", "Effect": "Deny", "Principal": "*", "Action": "s3:DeleteObject", "Resource": "photos/tmp/*"},
		{"Sid": "Other", "Effect": "Deny", "Principal": {"S3RP": "tb/*"}, "Action": "s3:DeleteObject", "Resource": "photos/*"}
	]}`)
	if err != nil {
		t.Fatal(err)
	}
	e := p.DenyEvaluatorFor("ta/u", "s3:DeleteObject", policy.RequestContext{})
	for res, want := range map[string]int{
		"photos/logs/a":  0,
		"photos/audit/a": 0, // second pattern of the same statement
		"photos/tmp/a":   1,
		"photos/a":       -1, // the tb-only statement was not pre-matched
	} {
		if got := e.DenyingStatement([]rune(res)); got != want {
			t.Errorf("%s: statement %d, want %d", res, got, want)
		}
		if e.Denies(res) != (want >= 0) {
			t.Errorf("%s: Denies disagrees", res)
		}
	}
}

func TestUserPolicyDecide(t *testing.T) {
	up := &policy.UserPolicy{Statements: []policy.ActionStatement{
		{Effect: "Allow", Action: []string{"s3:Get*", "s3:List*"}},
		{Effect: "Allow", Action: []string{"s3:GetObject"}},
		{Effect: "Deny", Action: []string{"s3:GetObjectTagging"}},
	}}
	tests := []struct {
		action    string
		effect    policy.Effect
		statement int
	}{
		{"s3:GetObject", policy.Allow, 0},
		{"s3:ListBucket", policy.Allow, 0},
		{"s3:GetObjectTagging", policy.Deny, 2},
		{"s3:PutObject", policy.Deny, -1}, // implicit
	}
	for _, tc := range tests {
		d := up.Decide(tc.action)
		if d.Effect != tc.effect || d.Statement != tc.statement {
			t.Errorf("%s: got %+v, want %v/%d", tc.action, d, tc.effect, tc.statement)
		}
	}
	var none *policy.UserPolicy
	if d := none.Decide("s3:PutObject"); d.Effect != policy.Allow || d.Statement != -1 {
		t.Errorf("nil policy: %+v", d)
	}
}

func TestParseDuplicateSid(t *testing.T) {
	_, err := policy.Parse("photos", `{"Statement": [
		{"Sid": "A", "Effect": "Deny", "Principal": "*", "Action": "s3:PutObject", "Resource": "photos/*"},
		{"Effect": "Deny", "Principal": "*", "Action": "s3:PutObject", "Resource": "photos/*"},
		{"Sid": "A", "Effect": "Deny", "Principal": "*", "Action": "s3:DeleteObject", "Resource": "photos/*"}
	]}`)
	if err == nil || !strings.Contains(err.Error(), `duplicate Sid "A" (statement[0] and statement[2])`) {
		t.Errorf("expect the duplicate Sid error, got %v", err)
	}
	// two Sid-less statements are fine
	if _, err := policy.Parse("photos", `{"Statement": [
		{"Effect": "Deny", "Principal": "*", "Action": "s3:PutObject", "Resource": "photos/*"},
		{"Effect": "Deny", "Principal": "*", "Action": "s3:DeleteObject", "Resource": "photos/*"}
	]}`); err != nil {
		t.Errorf("Sid-less statements must be accepted: %v", err)
	}
}
