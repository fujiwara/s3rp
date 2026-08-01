package s3rp_test

import (
	"github.com/fujiwara/s3rp/cors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/fujiwara/s3rp"
	"github.com/google/go-cmp/cmp"
)

// newCORSTestProxy builds a proxy with a CORS-enabled bucket and a plain one.
func newCORSTestProxy(t *testing.T) (*httptest.Server, *s3rp.S3RP) {
	t.Helper()
	cfg := &s3rp.Config{
		Tenants: []*s3rp.TenantConfig{
			{
				Name: "corstenant",
				Users: []*s3rp.UserConfig{
					{Name: "app1", Keys: []*s3rp.KeyConfig{{AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey}}},
				},
				Buckets: []*s3rp.BucketConfig{
					{
						Name: "webbucket",
						Backend: &s3rp.BackendConfig{
							Endpoint: "http://backend.invalid", AccessKeyID: "bk", SecretAccessKey: "bs",
						},
						CORS: []*cors.Rule{
							{
								AllowedOrigins: []string{"https://app.example.com", "https://*.preview.example.com"},
								AllowedMethods: []string{"GET", "PUT"},
								AllowedHeaders: []string{"*"},
								ExposeHeaders:  []string{"ETag"},
								MaxAgeSeconds:  3600,
							},
						},
					},
					{
						Name: "plainbucket",
						Backend: &s3rp.BackendConfig{
							Endpoint: "http://backend.invalid", AccessKeyID: "bk", SecretAccessKey: "bs",
						},
					},
				},
			},
		},
	}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	app, err := s3rp.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	stub := &stubBackend{
		getOut: &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("x")), ETag: aws.String(`"e"`)},
		putOut: &s3.PutObjectOutput{ETag: aws.String(`"e"`)},
	}
	app.SetBackend("webbucket", stub)
	app.SetBackend("plainbucket", stub)
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(ts.Close)
	return ts, app
}

func preflight(t *testing.T, url, origin, method, headers string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodOptions, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", method)
	if headers != "" {
		req.Header.Set("Access-Control-Request-Headers", headers)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestCORSPreflight(t *testing.T) {
	ts, _ := newCORSTestProxy(t)

	t.Run("allowed origin and method", func(t *testing.T) {
		resp := preflight(t, ts.URL+"/webbucket/key.txt", "https://app.example.com", "PUT", "content-type, x-amz-date")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expect 200, got %d", resp.StatusCode)
		}
		want := map[string]string{
			"Access-Control-Allow-Origin":      "https://app.example.com",
			"Access-Control-Allow-Methods":     "GET, PUT",
			"Access-Control-Allow-Headers":     "content-type, x-amz-date",
			"Access-Control-Allow-Credentials": "true",
			"Access-Control-Max-Age":           "3600",
		}
		got := map[string]string{}
		for k := range want {
			got[k] = resp.Header.Get(k)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("header mismatch (-want +got):\n%s", diff)
		}
	})
	t.Run("wildcard origin pattern", func(t *testing.T) {
		resp := preflight(t, ts.URL+"/webbucket/key.txt", "https://pr-42.preview.example.com", "GET", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expect 200, got %d", resp.StatusCode)
		}
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://pr-42.preview.example.com" {
			t.Errorf("unexpected allow-origin %q", got)
		}
	})
	t.Run("origin not allowed", func(t *testing.T) {
		resp := preflight(t, ts.URL+"/webbucket/key.txt", "https://evil.example.net", "GET", "")
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("expect 403, got %d", resp.StatusCode)
		}
	})
	t.Run("method not allowed", func(t *testing.T) {
		resp := preflight(t, ts.URL+"/webbucket/key.txt", "https://app.example.com", "DELETE", "")
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("expect 403, got %d", resp.StatusCode)
		}
	})
	t.Run("bucket without cors", func(t *testing.T) {
		resp := preflight(t, ts.URL+"/plainbucket/key.txt", "https://app.example.com", "GET", "")
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("expect 403, got %d", resp.StatusCode)
		}
	})
	t.Run("unknown bucket", func(t *testing.T) {
		resp := preflight(t, ts.URL+"/nosuchbucket/key.txt", "https://app.example.com", "GET", "")
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("expect 403, got %d", resp.StatusCode)
		}
	})
	t.Run("missing origin", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodOptions, ts.URL+"/webbucket/key.txt", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expect 400, got %d", resp.StatusCode)
		}
	})
}

// TestCORSActualRequest exercises the browser flow: a presigned URL fetched
// with an Origin header must carry CORS headers on the actual response.
func TestCORSActualRequest(t *testing.T) {
	ts, _ := newCORSTestProxy(t)
	client := newS3Client(t, ts.URL, testAccessKeyID, testSecretAccessKey)
	presigner := s3.NewPresignClient(client)

	req, err := presigner.PresignGetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("webbucket"),
		Key:    aws.String("key.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	hr, err := http.NewRequestWithContext(t.Context(), http.MethodGet, req.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	hr.Header.Set("Origin", "https://app.example.com")
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expect 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("unexpected allow-origin %q", got)
	}
	if got := resp.Header.Get("Access-Control-Expose-Headers"); got != "ETag" {
		t.Errorf("unexpected expose-headers %q", got)
	}

	// a disallowed origin gets no CORS headers (but the request itself succeeds)
	hr2, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, req.URL, nil)
	hr2.Header.Set("Origin", "https://evil.example.net")
	resp2, err := http.DefaultClient.Do(hr2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if got := resp2.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("expect no allow-origin, got %q", got)
	}
}

func TestGetBucketCors(t *testing.T) {
	ts, _ := newCORSTestProxy(t)
	client := newS3Client(t, ts.URL, testAccessKeyID, testSecretAccessKey)
	out, err := client.GetBucketCors(t.Context(), &s3.GetBucketCorsInput{
		Bucket: aws.String("webbucket"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.CORSRules) != 1 {
		t.Fatalf("expect 1 rule, got %d", len(out.CORSRules))
	}
	rule := out.CORSRules[0]
	if len(rule.AllowedOrigins) != 2 || rule.AllowedOrigins[0] != "https://app.example.com" {
		t.Errorf("unexpected origins %v", rule.AllowedOrigins)
	}
	if aws.ToInt32(rule.MaxAgeSeconds) != 3600 {
		t.Errorf("unexpected max age %v", rule.MaxAgeSeconds)
	}

	if _, err := client.GetBucketCors(t.Context(), &s3.GetBucketCorsInput{
		Bucket: aws.String("plainbucket"),
	}); err == nil {
		t.Error("expect NoSuchCORSConfiguration")
	}
}
