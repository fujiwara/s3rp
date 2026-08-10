package sigv4_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"maps"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fujiwara/s3rp/sigv4"
)

// The primary fixture is the worked example in the AWS documentation
// ("Example: Browser-Based Upload using HTTP POST (Using AWS Signature
// Version 4)" in the S3 API reference — cited by title, AWS's deep links
// rot), which publishes the policy, its exact base64 form, the example
// credentials and the resulting signature as a test suite for signature
// calculation code.
const (
	awsExampleAccessKeyID = "AKIAIOSFODNN7EXAMPLE"
	awsExampleSecret      = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	awsExampleSignature   = "8afdbf4008c03f22c2cd3cdb72e4afbb1f6a588f3255ac628749a66d7f09699e"
	awsExamplePolicyB64   = "eyAiZXhwaXJhdGlvbiI6ICIyMDE1LTEyLTMwVDEyOjAwOjAwLjAwMFoiLA0KICAiY29uZGl0aW9ucyI6IFsNCiAgICB7ImJ1Y2tldCI6ICJzaWd2NGV4YW1wbGVidWNrZXQifSwNCiAgICBbInN0YXJ0cy13aXRoIiwgIiRrZXkiLCAidXNlci91c2VyMS8iXSwNCiAgICB7ImFjbCI6ICJwdWJsaWMtcmVhZCJ9LA0KICAgIHsic3VjY2Vzc19hY3Rpb25fcmVkaXJlY3QiOiAiaHR0cDovL3NpZ3Y0ZXhhbXBsZWJ1Y2tldC5zMy5hbWF6b25hd3MuY29tL3N1Y2Nlc3NmdWxfdXBsb2FkLmh0bWwifSwNCiAgICBbInN0YXJ0cy13aXRoIiwgIiRDb250ZW50LVR5cGUiLCAiaW1hZ2UvIl0sDQogICAgeyJ4LWFtei1tZXRhLXV1aWQiOiAiMTQzNjUxMjM2NTEyNzQifSwNCiAgICB7IngtYW16LXNlcnZlci1zaWRlLWVuY3J5cHRpb24iOiAiQUVTMjU2In0sDQogICAgWyJzdGFydHMtd2l0aCIsICIkeC1hbXotbWV0YS10YWciLCAiIl0sDQoNCiAgICB7IngtYW16LWNyZWRlbnRpYWwiOiAiQUtJQUlPU0ZPRE5ON0VYQU1QTEUvMjAxNTEyMjkvdXMtZWFzdC0xL3MzL2F3czRfcmVxdWVzdCJ9LA0KICAgIHsieC1hbXotYWxnb3JpdGhtIjogIkFXUzQtSE1BQy1TSEEyNTYifSwNCiAgICB7IngtYW16LWRhdGUiOiAiMjAxNTEyMjlUMDAwMDAwWiIgfQ0KICBdDQp9"
)

var awsExampleTime = time.Date(2015, 12, 29, 0, 0, 1, 0, time.UTC)

func awsExampleLookup(_ context.Context, akid, _ string) (sigv4.Credential, error) {
	if akid != awsExampleAccessKeyID {
		return sigv4.Credential{}, sigv4.ErrUnknownKey
	}
	return sigv4.Credential{SecretAccessKey: awsExampleSecret}, nil
}

// awsExampleFields is the example form, as the gateway hands it to
// VerifyPost: field names lower-cased, ${filename} already substituted,
// and the target bucket added as a pseudo-field.
func awsExampleFields() map[string]string {
	return map[string]string{
		"bucket":                       "sigv4examplebucket",
		"key":                          "user/user1/MyPhoto.jpg",
		"acl":                          "public-read",
		"success_action_redirect":      "http://sigv4examplebucket.s3.amazonaws.com/successful_upload.html",
		"content-type":                 "image/jpeg",
		"x-amz-meta-uuid":              "14365123651274",
		"x-amz-server-side-encryption": "AES256",
		"x-amz-meta-tag":               "",
		"x-amz-credential":             awsExampleAccessKeyID + "/20151229/us-east-1/s3/aws4_request",
		"x-amz-algorithm":              "AWS4-HMAC-SHA256",
		"x-amz-date":                   "20151229T000000Z",
		"policy":                       awsExamplePolicyB64,
		"x-amz-signature":              awsExampleSignature,
	}
}

func postVerifier(now time.Time) *sigv4.Verifier {
	v := sigv4.NewVerifier()
	v.Now = func() time.Time { return now }
	return v
}

func TestVerifyPostAWSExample(t *testing.T) {
	r := httptest.NewRequest("POST", "http://s3.example.com/sigv4examplebucket", nil)
	got, pp, s3e := postVerifier(awsExampleTime).VerifyPost(r, awsExampleFields(), awsExampleLookup)
	if s3e != nil {
		t.Fatalf("expect the AWS documentation example to verify, got %v", s3e)
	}
	if got.AccessKeyID != awsExampleAccessKeyID || got.Region != "us-east-1" {
		t.Errorf("unexpected verified request %+v", got)
	}
	if got.PayloadHash != "UNSIGNED-PAYLOAD" {
		t.Errorf("unexpected payload hash %q", got.PayloadHash)
	}
	if want := time.Date(2015, 12, 30, 12, 0, 0, 0, time.UTC); !pp.Expiration.Equal(want) {
		t.Errorf("unexpected expiration %v", pp.Expiration)
	}
	if pp.MinLength != 0 || pp.MaxLength <= 0 {
		t.Errorf("expect an unbounded length range, got %+v", pp)
	}
}

func TestVerifyPostRejects(t *testing.T) {
	modify := func(mod func(map[string]string)) map[string]string {
		f := maps.Clone(awsExampleFields())
		mod(f)
		return f
	}
	cases := []struct {
		name   string
		now    time.Time
		fields map[string]string
		region string // pinned region, "" = unpinned
		code   string
	}{
		{
			name:   "tampered signature",
			fields: modify(func(f map[string]string) { f["x-amz-signature"] = strings.Repeat("0", 64) }),
			code:   "SignatureDoesNotMatch",
		},
		{
			name: "tampered policy",
			fields: modify(func(f map[string]string) {
				f["policy"] = base64.StdEncoding.EncodeToString([]byte(`{"expiration":"2999-01-01T00:00:00Z","conditions":[]}`))
			}),
			code: "SignatureDoesNotMatch",
		},
		{
			// the original signed form, with only the clock moved past the
			// policy's expiration
			name:   "expired policy",
			now:    time.Date(2015, 12, 30, 12, 0, 1, 0, time.UTC),
			fields: awsExampleFields(),
			code:   "AccessDenied",
		},
		{
			name:   "eq condition failed",
			fields: modify(func(f map[string]string) { f["acl"] = "private" }),
			code:   "AccessDenied",
		},
		{
			name:   "starts-with condition failed",
			fields: modify(func(f map[string]string) { f["key"] = "other/user2/photo.jpg" }),
			code:   "AccessDenied",
		},
		{
			name:   "uncovered extra field",
			fields: modify(func(f map[string]string) { f["x-amz-meta-extra"] = "sneaky" }),
			code:   "AccessDenied",
		},
		{
			name:   "unknown access key",
			fields: modify(func(f map[string]string) { f["x-amz-credential"] = "NOSUCHKEY/20151229/us-east-1/s3/aws4_request" }),
			code:   "InvalidAccessKeyId",
		},
		{
			name:   "missing policy",
			fields: modify(func(f map[string]string) { delete(f, "policy") }),
			code:   "InvalidArgument",
		},
		{
			name:   "wrong algorithm",
			fields: modify(func(f map[string]string) { f["x-amz-algorithm"] = "AWS4-HMAC-SHA1" }),
			code:   "InvalidArgument",
		},
		{
			name:   "pinned region mismatch",
			fields: awsExampleFields(),
			region: "ap-northeast-1",
			code:   "InvalidArgument",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := tc.now
			if now.IsZero() {
				now = awsExampleTime
			}
			v := postVerifier(now)
			if tc.region != "" {
				v.SetRegion(tc.region)
			}
			r := httptest.NewRequest("POST", "http://s3.example.com/sigv4examplebucket", nil)
			_, _, s3e := v.VerifyPost(r, tc.fields, awsExampleLookup)
			if s3e == nil {
				t.Fatal("expect an error")
			}
			if s3e.Code != tc.code {
				t.Errorf("expect %s, got %s (%s)", tc.code, s3e.Code, s3e.Message)
			}
		})
	}
}

// signPostPolicy signs a policy the way a service backend does, computed
// here from first principles (raw HMAC chain) so the test does not depend
// on the package's own key derivation.
func signPostPolicy(secret, date, region, policyB64 string) string {
	h := func(key []byte, s string) []byte {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(s))
		return m.Sum(nil)
	}
	k := h([]byte("AWS4"+secret), date)
	k = h(k, region)
	k = h(k, "s3")
	k = h(k, "aws4_request")
	return hex.EncodeToString(h(k, policyB64))
}

func TestVerifyPostContentLengthRange(t *testing.T) {
	policy := `{"expiration":"2026-08-02T00:00:00Z","conditions":[` +
		`{"bucket":"testbucket"},{"key":"a.txt"},` +
		`{"x-amz-credential":"` + testAccessKeyID + `/20260801/us-east-1/s3/aws4_request"},` +
		`{"x-amz-algorithm":"AWS4-HMAC-SHA256"},{"x-amz-date":"20260801T120000Z"},` +
		`["content-length-range", 10, 1000],["content-length-range", 5, 500]]}`
	b64 := base64.StdEncoding.EncodeToString([]byte(policy))
	fields := map[string]string{
		"bucket":           "testbucket",
		"key":              "a.txt",
		"x-amz-credential": testAccessKeyID + "/20260801/us-east-1/s3/aws4_request",
		"x-amz-algorithm":  "AWS4-HMAC-SHA256",
		"x-amz-date":       "20260801T120000Z",
		"policy":           b64,
		"x-amz-signature":  signPostPolicy(testSecret, "20260801", "us-east-1", b64),
	}
	r := httptest.NewRequest("POST", "http://s3.example.com/testbucket", nil)
	_, pp, s3e := postVerifier(testTime).VerifyPost(r, fields, lookup)
	if s3e != nil {
		t.Fatalf("expect success, got %v", s3e)
	}
	// repeated ranges intersect
	if pp.MinLength != 10 || pp.MaxLength != 500 {
		t.Errorf("expect the intersected range [10, 500], got %+v", pp)
	}
}
