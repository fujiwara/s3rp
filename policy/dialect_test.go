package policy_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/fujiwara/s3rp/policy"
)

// The dialect changes only the surface syntax; everything parsed through one
// must evaluate exactly as the default form does.

func TestDialectPrincipalKey(t *testing.T) {
	d := &policy.Dialect{PrincipalKey: "MyService"}
	p, err := d.Parse(`{
	  "Statement": [
	    {
	      "Effect": "Deny",
	      "Principal": {"MyService": ["ta/batch"]},
	      "Action": ["s3:PutObject"],
	      "Resource": ["photos/*"]
	    }
	  ]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Evaluate("ta/batch", "s3:PutObject", "photos/a.txt", policy.RequestContext{}); got != policy.Deny {
		t.Errorf("expect Deny, got %v", got)
	}
	if got := p.Evaluate("app1", "s3:PutObject", "photos/a.txt", policy.RequestContext{}); got != policy.None {
		t.Errorf("expect None for another user, got %v", got)
	}

	// the default key is not recognized by a custom dialect
	_, err = d.Parse(`{
	  "Statement": [
	    {"Effect": "Deny", "Principal": {"S3RP": ["ta/batch"]}, "Action": ["s3:PutObject"], "Resource": ["photos/*"]}
	  ]
	}`)
	if err == nil || !strings.Contains(err.Error(), "MyService") {
		t.Errorf("expect an error naming the dialect's key, got %v", err)
	}

	// "*" and NotPrincipal work in any dialect
	p, err = d.Parse(`{
	  "Statement": [
	    {"Effect": "Deny", "Principal": "*", "Action": ["s3:DeleteObject"], "Resource": ["photos/*"]},
	    {"Effect": "Deny", "NotPrincipal": {"MyService": ["ta/admin"]}, "Action": ["s3:PutObject"], "Resource": ["photos/*"]}
	  ]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Evaluate("ta/admin", "s3:DeleteObject", "photos/a.txt", policy.RequestContext{}); got != policy.Deny {
		t.Errorf("expect Deny for everyone, got %v", got)
	}
	if got := p.Evaluate("ta/admin", "s3:PutObject", "photos/a.txt", policy.RequestContext{}); got != policy.None {
		t.Errorf("expect None for the excepted user, got %v", got)
	}
}

func TestDialectResourcePrefix(t *testing.T) {
	d := &policy.Dialect{ResourcePrefix: "arn:aws:s3:::"}
	p, err := d.Parse(`{
	  "Statement": [
	    {
	      "Effect": "Deny",
	      "Principal": {"S3RP": ["ta/batch"]},
	      "Action": ["s3:PutObject"],
	      "Resource": ["arn:aws:s3:::photos/*", "arn:aws:s3:::photos"]
	    }
	  ]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	// resources are matched in the stripped, plain-path form
	if got := p.Evaluate("ta/batch", "s3:PutObject", "photos/a.txt", policy.RequestContext{}); got != policy.Deny {
		t.Errorf("expect Deny, got %v", got)
	}
	if got := p.Evaluate("ta/batch", "s3:PutObject", "photos", policy.RequestContext{}); got != policy.Deny {
		t.Errorf("expect Deny on the bucket resource, got %v", got)
	}

	// a resource without the prefix is rejected, not silently taken as-is
	_, err = d.Parse(`{
	  "Statement": [
	    {"Effect": "Deny", "Principal": {"S3RP": ["ta/batch"]}, "Action": ["s3:PutObject"], "Resource": ["photos/*"]}
	  ]
	}`)
	if err == nil || !strings.Contains(err.Error(), "arn:aws:s3:::") {
		t.Errorf("expect an error naming the required prefix, got %v", err)
	}
}

// ParseFor's bucket-scope check runs on the normalized resources, so the
// bucket is named in the plain internal form even under an ARN dialect.
func TestDialectParseFor(t *testing.T) {
	d := &policy.Dialect{ResourcePrefix: "arn:aws:s3:::"}
	if _, err := d.ParseFor("photos", `{
	  "Statement": [
	    {"Effect": "Deny", "Principal": {"S3RP": ["ta/batch"]}, "Action": ["s3:PutObject"], "Resource": ["arn:aws:s3:::photos/*"]}
	  ]
	}`); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}
	_, err := d.ParseFor("photos", `{
	  "Statement": [
	    {"Effect": "Deny", "Principal": {"S3RP": ["ta/batch"]}, "Action": ["s3:PutObject"], "Resource": ["arn:aws:s3:::otherbucket/*"]}
	  ]
	}`)
	if err == nil || !strings.Contains(err.Error(), "does not refer to bucket") {
		t.Errorf("expect a scope error on the stripped resource, got %v", err)
	}
}

// MaxPatternLen applies to the stripped pattern, the one actually matched:
// the prefix must not eat into the tenant's budget.
func TestDialectPatternLenAfterStrip(t *testing.T) {
	d := &policy.Dialect{ResourcePrefix: "arn:aws:s3:::"}
	res := "arn:aws:s3:::" + strings.Repeat("k", policy.MaxPatternLen)
	if _, err := d.Parse(`{
	  "Statement": [
	    {"Effect": "Deny", "Principal": "*", "Action": ["s3:PutObject"], "Resource": ["` + res + `"]}
	  ]
	}`); err != nil {
		t.Errorf("expect a max-length stripped pattern to pass, got %v", err)
	}
	over := "arn:aws:s3:::" + strings.Repeat("k", policy.MaxPatternLen+1)
	if _, err := d.Parse(`{
	  "Statement": [
	    {"Effect": "Deny", "Principal": "*", "Action": ["s3:PutObject"], "Resource": ["` + over + `"]}
	  ]
	}`); err == nil {
		t.Error("expect an over-length stripped pattern to be rejected")
	}
}

// A dialect with ARN-style principals normalizes them to "tenant/user" in
// the parse pass, so a store hands Parse the tenant's original text.
func TestDialectNormalizePrincipal(t *testing.T) {
	normalize := func(s string) (string, error) {
		rest, ok := strings.CutPrefix(s, "arn:myco:iam::")
		if !ok {
			return "", fmt.Errorf("not a principal ARN")
		}
		tenant, user, ok := strings.Cut(rest, ":user/")
		if !ok {
			return "", fmt.Errorf("not a principal ARN")
		}
		return tenant + "/" + user, nil
	}
	d := &policy.Dialect{PrincipalKey: "AWS", NormalizePrincipal: normalize}
	p, err := d.Parse(`{
	  "Statement": [
	    {"Effect": "Allow", "Principal": {"AWS": ["arn:myco:iam::tb:user/bob"]}, "Action": ["s3:GetObject"], "Resource": ["photos/*"]},
	    {"Effect": "Deny", "NotPrincipal": {"AWS": ["arn:myco:iam::ta:user/admin"]}, "Action": ["s3:PutObject"], "Resource": ["photos/*"]}
	  ]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	// evaluation sees the internal form
	if got := p.Evaluate("tb/bob", "s3:GetObject", "photos/a.txt", policy.RequestContext{}); got != policy.Allow {
		t.Errorf("expect Allow for the normalized principal, got %v", got)
	}
	if got := p.Evaluate("ta/admin", "s3:PutObject", "photos/a.txt", policy.RequestContext{}); got != policy.None {
		t.Errorf("expect None for the NotPrincipal-excepted user, got %v", got)
	}
	if got := p.Statement[0].Principal.Users[0]; got != "tb/bob" {
		t.Errorf("expect the stored principal to be normalized, got %q", got)
	}

	// "*" is not passed through the normalizer
	if _, err := d.Parse(`{
	  "Statement": [
	    {"Effect": "Allow", "Principal": "*", "Action": ["s3:GetObject"], "Resource": ["photos/*"]}
	  ]
	}`); err != nil {
		t.Errorf(`"*" must not go through NormalizePrincipal: %v`, err)
	}

	// a normalizer error is reported with the statement and the original value
	_, err = d.Parse(`{
	  "Statement": [
	    {"Sid": "Bad", "Effect": "Allow", "Principal": {"AWS": ["alice"]}, "Action": ["s3:GetObject"], "Resource": ["photos/*"]}
	  ]
	}`)
	if err == nil || !strings.Contains(err.Error(), "Bad") || !strings.Contains(err.Error(), "alice") {
		t.Errorf("expect an error naming the statement and value, got %v", err)
	}

	// a normalizer result that is not the internal form is still validated
	d.NormalizePrincipal = func(string) (string, error) { return "Not Internal", nil }
	_, err = d.Parse(`{
	  "Statement": [
	    {"Effect": "Allow", "Principal": {"AWS": ["arn:myco:iam::tb:user/bob"]}, "Action": ["s3:GetObject"], "Resource": ["photos/*"]}
	  ]
	}`)
	if err == nil || !strings.Contains(err.Error(), "invalid principal") {
		t.Errorf("expect the normalized value to be validated, got %v", err)
	}
}

// A dialect whose resource syntax is not a fixed prefix (variable ARN
// fields) normalizes with a function instead of ResourcePrefix.
func TestDialectNormalizeResource(t *testing.T) {
	d := &policy.Dialect{NormalizeResource: func(s string) (string, error) {
		// arn:myco:s3:<region>:<account>:bucket/key — strip five fields
		parts := strings.SplitN(s, ":", 6)
		if len(parts) != 6 || parts[0] != "arn" || parts[1] != "myco" || parts[2] != "s3" {
			return "", fmt.Errorf("not a resource ARN")
		}
		return parts[5], nil
	}}
	p, err := d.Parse(`{
	  "Statement": [
	    {"Effect": "Deny", "Principal": "*", "Action": ["s3:PutObject"], "Resource": ["arn:myco:s3:us-east-1:acct-1:photos/thumb-*"]}
	  ]
	}`)
	if err != nil {
		t.Fatal(err)
	}
	// the wildcard survives normalization and matches in the plain form
	if got := p.Evaluate("ta/batch", "s3:PutObject", "photos/thumb-1.jpg", policy.RequestContext{}); got != policy.Deny {
		t.Errorf("expect Deny, got %v", got)
	}
	if got := p.Evaluate("ta/batch", "s3:PutObject", "photos/full-1.jpg", policy.RequestContext{}); got != policy.None {
		t.Errorf("expect None outside the pattern, got %v", got)
	}

	_, err = d.Parse(`{
	  "Statement": [
	    {"Effect": "Deny", "Principal": "*", "Action": ["s3:PutObject"], "Resource": ["photos/*"]}
	  ]
	}`)
	if err == nil || !strings.Contains(err.Error(), "photos/*") {
		t.Errorf("expect an error naming the unrecognized resource, got %v", err)
	}
}

// The zero Dialect is the default dialect: identical to Parse.
func TestDialectZeroValueIsDefault(t *testing.T) {
	text := `{
	  "Statement": [
	    {"Effect": "Deny", "Principal": {"S3RP": ["ta/batch"]}, "Action": ["s3:PutObject"], "Resource": ["photos/*"]}
	  ]
	}`
	var d policy.Dialect
	fromDialect, err := d.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	fromParse, err := policy.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []*policy.Policy{fromDialect, fromParse} {
		if got := p.Evaluate("ta/batch", "s3:PutObject", "photos/a.txt", policy.RequestContext{}); got != policy.Deny {
			t.Errorf("expect Deny, got %v", got)
		}
	}
}
