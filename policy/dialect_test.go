package policy_test

import (
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
	if got := p.Evaluate("ta/batch", "s3:PutObject", "photos/a.txt"); got != policy.Deny {
		t.Errorf("expect Deny, got %v", got)
	}
	if got := p.Evaluate("app1", "s3:PutObject", "photos/a.txt"); got != policy.None {
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
	if got := p.Evaluate("ta/admin", "s3:DeleteObject", "photos/a.txt"); got != policy.Deny {
		t.Errorf("expect Deny for everyone, got %v", got)
	}
	if got := p.Evaluate("ta/admin", "s3:PutObject", "photos/a.txt"); got != policy.None {
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
	if got := p.Evaluate("ta/batch", "s3:PutObject", "photos/a.txt"); got != policy.Deny {
		t.Errorf("expect Deny, got %v", got)
	}
	if got := p.Evaluate("ta/batch", "s3:PutObject", "photos"); got != policy.Deny {
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
		if got := p.Evaluate("ta/batch", "s3:PutObject", "photos/a.txt"); got != policy.Deny {
			t.Errorf("expect Deny, got %v", got)
		}
	}
}
