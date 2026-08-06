package policy_test

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"

	"github.com/fujiwara/s3rp/policy"
)

// from builds the request context of a client at addr ("" = unknown source).
func from(addr string) policy.RequestContext {
	if addr == "" {
		return policy.RequestContext{}
	}
	return policy.RequestContext{SourceIP: netip.MustParseAddr(addr)}
}

func TestParseCondition(t *testing.T) {
	t.Run("case-insensitive key, string and array values", func(t *testing.T) {
		p, err := policy.Parse("b", `{
			"Statement": [
				{"Effect": "Deny", "Principal": "*", "Action": "s3:PutObject", "Resource": "b/*",
				 "Condition": {"IpAddress": {"AWS:sourceip": "10.0.0.0/8"}}},
				{"Effect": "Deny", "Principal": "*", "Action": "s3:DeleteObject", "Resource": "b/*",
				 "Condition": {"NotIpAddress": {"aws:SourceIp": ["192.0.2.0/24", "2001:db8::/32"]}}}
			]
		}`)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Statement[0].Condition.IPAddress; len(got) != 1 || got[0] != "10.0.0.0/8" {
			t.Errorf("unexpected IPAddress %v", got)
		}
		if got := p.Statement[1].Condition.NotIPAddress; len(got) != 2 {
			t.Errorf("unexpected NotIPAddress %v", got)
		}
	})

	errCases := []struct {
		name   string
		cond   string
		errStr string
	}{
		{"unknown operator", `{"StringEquals": {"aws:SourceIp": "10.0.0.0/8"}}`, "unknown condition operator"},
		{"unknown key", `{"IpAddress": {"aws:username": "alice"}}`, "unknown condition key"},
		{"bad CIDR", `{"IpAddress": {"aws:SourceIp": "10.0.0.0/33"}}`, "invalid source IP value"},
		{"not an address", `{"IpAddress": {"aws:SourceIp": "office"}}`, "invalid source IP value"},
		{"zoned address", `{"IpAddress": {"aws:SourceIp": "fe80::1%eth0"}}`, "invalid source IP value"},
		{"empty condition", `{}`, "at least one operator"},
		{"operator without keys", `{"IpAddress": {}}`, "has no values"},
		{"operator with empty values", `{"IpAddress": {"aws:SourceIp": []}}`, "has no values"},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			text := `{"Statement": [{"Effect": "Deny", "Principal": "*", "Action": "s3:PutObject", "Resource": "b/*", "Condition": ` + tc.cond + `}]}`
			if _, err := policy.Parse("b", text); err == nil || !strings.Contains(err.Error(), tc.errStr) {
				t.Errorf("expect error containing %q, got %v", tc.errStr, err)
			}
		})
	}

	t.Run("unmarshal resets a reused receiver", func(t *testing.T) {
		var c policy.Condition
		if err := json.Unmarshal([]byte(`{"IpAddress": {"aws:SourceIp": "10.0.0.0/8"}, "NotIpAddress": {"aws:SourceIp": "192.0.2.0/24"}}`), &c); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(`{"IpAddress": {"aws:SourceIp": "172.16.0.0/12"}}`), &c); err != nil {
			t.Fatal(err)
		}
		if len(c.IPAddress) != 1 || c.IPAddress[0] != "172.16.0.0/12" {
			t.Errorf("expect only the second document's IPAddress, got %v", c.IPAddress)
		}
		if len(c.NotIPAddress) != 0 {
			t.Errorf("expect NotIPAddress cleared, got %v", c.NotIPAddress)
		}
	})
}

func TestEvaluateCondition(t *testing.T) {
	p, err := policy.Parse("b", `{
		"Statement": [
			{
				"Sid": "AllowFromOffice",
				"Effect": "Allow",
				"Principal": {"S3RP": ["tb/bob"]},
				"Action": "s3:GetObject",
				"Resource": "b/*",
				"Condition": {"IpAddress": {"aws:SourceIp": ["10.0.0.0/8", "2001:db8::/32"]}}
			},
			{
				"Sid": "DenyOutsideOffice",
				"Effect": "Deny",
				"Principal": {"S3RP": ["ta/batch"]},
				"Action": "s3:PutObject",
				"Resource": "b/*",
				"Condition": {"NotIpAddress": {"aws:SourceIp": "192.0.2.0/24"}}
			},
			{
				"Sid": "BothOperators",
				"Effect": "Deny",
				"Principal": {"S3RP": ["ta/carol"]},
				"Action": "s3:DeleteObject",
				"Resource": "b/*",
				"Condition": {
					"IpAddress": {"aws:SourceIp": "203.0.113.0/24"},
					"NotIpAddress": {"aws:SourceIp": "203.0.113.7"}
				}
			},
			{
				"Sid": "PlainAddresses",
				"Effect": "Deny",
				"Principal": {"S3RP": ["ta/dave"]},
				"Action": "s3:GetObject",
				"Resource": "b/*",
				"Condition": {"IpAddress": {"aws:SourceIp": ["198.51.100.7", "2001:db8::1"]}}
			}
		]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		principal string
		action    string
		source    string
		want      policy.Effect
	}{
		{"allow inside range", "tb/bob", "s3:GetObject", "10.1.2.3", policy.Allow},
		{"allow outside range", "tb/bob", "s3:GetObject", "11.0.0.1", policy.None},
		{"allow unknown source fails closed", "tb/bob", "s3:GetObject", "", policy.None},
		{"allow inside IPv6 range", "tb/bob", "s3:GetObject", "2001:db8::1", policy.Allow},
		{"allow outside IPv6 range", "tb/bob", "s3:GetObject", "2001:db9::1", policy.None},
		{"4-in-6 source matches IPv4 prefix", "tb/bob", "s3:GetObject", "::ffff:10.1.2.3", policy.Allow},
		{"deny lifted inside range", "ta/batch", "s3:PutObject", "192.0.2.5", policy.None},
		{"deny outside range", "ta/batch", "s3:PutObject", "198.51.100.1", policy.Deny},
		{"deny unknown source fails closed", "ta/batch", "s3:PutObject", "", policy.Deny},
		{"both operators, in range and not excluded", "ta/carol", "s3:DeleteObject", "203.0.113.5", policy.Deny},
		{"both operators, excluded by NotIpAddress", "ta/carol", "s3:DeleteObject", "203.0.113.7", policy.None},
		{"both operators, outside IpAddress", "ta/carol", "s3:DeleteObject", "198.51.100.1", policy.None},
		{"plain IPv4 is /32", "ta/dave", "s3:GetObject", "198.51.100.7", policy.Deny},
		{"plain IPv4 excludes neighbor", "ta/dave", "s3:GetObject", "198.51.100.8", policy.None},
		{"plain IPv6 is /128", "ta/dave", "s3:GetObject", "2001:db8::1", policy.Deny},
		{"plain IPv6 excludes neighbor", "ta/dave", "s3:GetObject", "2001:db8::2", policy.None},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.Evaluate(tc.principal, tc.action, "b/k", from(tc.source)); got != tc.want {
				t.Errorf("Evaluate(%q, %q, from %q) = %v, want %v", tc.principal, tc.action, tc.source, got, tc.want)
			}
		})
	}
}

// TestEvaluatorConditions verifies that conditions are resolved when an
// evaluator is built: a statement excluded by its condition contributes no
// resource patterns, so the AlwaysAllows/AlwaysDenies fast paths still fire.
func TestEvaluatorConditions(t *testing.T) {
	p, err := policy.Parse("b", `{
		"Statement": [
			{
				"Effect": "Deny",
				"Principal": "*",
				"Action": "s3:DeleteObject",
				"Resource": "b/*",
				"Condition": {"NotIpAddress": {"aws:SourceIp": "127.0.0.0/8"}}
			},
			{
				"Effect": "Allow",
				"Principal": {"S3RP": ["tb/bob"]},
				"Action": "s3:DeleteObject",
				"Resource": "b/*",
				"Condition": {"IpAddress": {"aws:SourceIp": "127.0.0.0/8"}}
			}
		]
	}`)
	if err != nil {
		t.Fatal(err)
	}

	if e := p.DenyEvaluatorFor("ta/alice", "s3:DeleteObject", from("127.0.0.1")); !e.AlwaysAllows() {
		t.Error("deny lifted inside the range: expect AlwaysAllows")
	}
	e := p.DenyEvaluatorFor("ta/alice", "s3:DeleteObject", from("10.0.0.1"))
	if e.AlwaysAllows() {
		t.Error("deny applies outside the range: expect not AlwaysAllows")
	}
	if !e.Denies("b/k") {
		t.Error("expect b/k denied outside the range")
	}
	if e := p.DenyEvaluatorFor("ta/alice", "s3:DeleteObject", from("")); e.AlwaysAllows() {
		t.Error("unknown source fails closed: expect the Deny to apply")
	}

	a := p.AllowEvaluatorFor("tb/bob", "s3:DeleteObject", from("127.0.0.1"))
	if a.AlwaysDenies() || !a.Allows("b/k") {
		t.Error("expect the Allow to apply inside the range")
	}
	if a := p.AllowEvaluatorFor("tb/bob", "s3:DeleteObject", from("10.0.0.1")); !a.AlwaysDenies() {
		t.Error("expect AlwaysDenies outside the range")
	}
}

// TestHandBuiltConditionFailsClosed covers a policy constructed without
// Parse: an uncompilable condition must never widen access — the gated
// Allow grants nothing and the gated Deny always applies.
func TestHandBuiltConditionFailsClosed(t *testing.T) {
	allow := &policy.Policy{Statement: []policy.Statement{{
		Effect:    "Allow",
		Principal: &policy.Principal{All: true},
		Action:    policy.StringOrSlice{"s3:GetObject"},
		Resource:  policy.StringOrSlice{"b/*"},
		Condition: &policy.Condition{IPAddress: policy.StringOrSlice{"garbage"}},
	}}}
	if got := allow.Evaluate("ta/alice", "s3:GetObject", "b/k", from("10.0.0.1")); got != policy.None {
		t.Errorf("invalid condition on Allow: expect None, got %v", got)
	}
	deny := &policy.Policy{Statement: []policy.Statement{{
		Effect:    "Deny",
		Principal: &policy.Principal{All: true},
		Action:    policy.StringOrSlice{"s3:GetObject"},
		Resource:  policy.StringOrSlice{"b/*"},
		Condition: &policy.Condition{IPAddress: policy.StringOrSlice{"garbage"}},
	}}}
	if got := deny.Evaluate("ta/alice", "s3:GetObject", "b/k", from("10.0.0.1")); got != policy.Deny {
		t.Errorf("invalid condition on Deny: expect Deny, got %v", got)
	}
}

func TestConditionMarshalRoundTrip(t *testing.T) {
	text := `{
		"Statement": [
			{
				"Effect": "Deny",
				"Principal": "*",
				"Action": "s3:PutObject",
				"Resource": "b/*",
				"Condition": {
					"IpAddress": {"aws:SourceIp": ["203.0.113.0/24"]},
					"NotIpAddress": {"aws:SourceIp": ["203.0.113.7"]}
				}
			}
		]
	}`
	p, err := policy.Parse("b", text)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	rp, err := policy.Parse("b", string(data))
	if err != nil {
		t.Fatalf("re-parse of %s: %v", data, err)
	}
	for _, tc := range []struct {
		source string
		want   policy.Effect
	}{
		{"203.0.113.5", policy.Deny},
		{"203.0.113.7", policy.None},
		{"198.51.100.1", policy.None},
	} {
		if got := rp.Evaluate("ta/alice", "s3:PutObject", "b/k", from(tc.source)); got != tc.want {
			t.Errorf("re-parsed Evaluate(from %q) = %v, want %v", tc.source, got, tc.want)
		}
	}
}
