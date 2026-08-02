package s3gw_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/google/go-cmp/cmp"
)

func TestPresignedGetObject(t *testing.T) {
	stub := &stubBackend{
		getOut: &s3.GetObjectOutput{
			Body:          io.NopCloser(strings.NewReader("presigned content")),
			ContentLength: aws.Int64(17),
			ContentType:   aws.String("text/plain"),
		},
	}
	client, _ := newTestProxy(t, stub)
	presigner := s3.NewPresignClient(client)
	req, err := presigner.PresignGetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("testbucket"),
		Key:    aws.String("dir/presigned.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(req.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expect 200, got %d: %s", resp.StatusCode, body)
	}
	if string(body) != "presigned content" {
		t.Errorf("unexpected body %q", body)
	}
	if aws.ToString(stub.getIn.Key) != "dir/presigned.txt" {
		t.Errorf("unexpected key %v", stub.getIn.Key)
	}
}

func TestPresignedPutObject(t *testing.T) {
	stub := &stubBackend{
		putOut: &s3.PutObjectOutput{ETag: aws.String(`"presigned-etag"`)},
	}
	client, _ := newTestProxy(t, stub)
	presigner := s3.NewPresignClient(client)
	req, err := presigner.PresignPutObject(t.Context(), &s3.PutObjectInput{
		Bucket:   aws.String("testbucket"),
		Key:      aws.String("dir/uploaded.txt"),
		Metadata: map[string]string{"origin": "presign-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	content := "presigned upload body"
	hr, err := http.NewRequestWithContext(t.Context(), req.Method, req.URL, strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	// signed headers (x-amz-meta-* etc.) must be sent as presigned
	for k, vs := range req.SignedHeader {
		for _, v := range vs {
			hr.Header.Set(k, v)
		}
	}
	// non-x-amz headers may be sent unsigned with a presigned URL
	hr.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expect 200, got %d: %s", resp.StatusCode, body)
	}
	if string(stub.putBody) != content {
		t.Errorf("unexpected body %q", stub.putBody)
	}
	if aws.ToString(stub.putIn.ContentType) != "text/plain" {
		t.Errorf("unexpected content type %v", stub.putIn.ContentType)
	}
	// metadata is hoisted into the query by the presigner and must be
	// promoted back into the backend request
	if diff := cmp.Diff(map[string]string{"origin": "presign-test"}, stub.putIn.Metadata); diff != "" {
		t.Errorf("metadata mismatch (-want +got):\n%s", diff)
	}
}

func TestPresignedErrors(t *testing.T) {
	newProxyAndURL := func(t *testing.T, akid, secret string, expires time.Duration) (*s3gw.Gateway, string) {
		t.Helper()
		gw := newTestGateway(t)
		if err := gw.SetBackend("testbucket", &stubBackend{
			getOut: &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("x"))},
		}); err != nil {
			t.Fatal(err)
		}
		ts := newTestServer(t, gw)
		cfg, err := awsconfig.LoadDefaultConfig(t.Context(),
			awsconfig.WithRegion("us-east-1"),
			awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(akid, secret, "")),
		)
		if err != nil {
			t.Fatal(err)
		}
		client := s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(ts)
			o.UsePathStyle = true
		})
		req, err := s3.NewPresignClient(client).PresignGetObject(t.Context(), &s3.GetObjectInput{
			Bucket: aws.String("testbucket"),
			Key:    aws.String("key.txt"),
		}, func(po *s3.PresignOptions) {
			po.Expires = expires
		})
		if err != nil {
			t.Fatal(err)
		}
		return gw, req.URL
	}

	get := func(t *testing.T, url string) (int, string) {
		t.Helper()
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	t.Run("expired", func(t *testing.T) {
		app, u := newProxyAndURL(t, testAccessKeyID, testSecretAccessKey, time.Minute)
		app.SetNow(func() time.Time { return time.Now().Add(2 * time.Hour) })
		status, body := get(t, u)
		if status != http.StatusForbidden || !strings.Contains(body, "Request has expired") {
			t.Errorf("expect 403 expired, got %d: %s", status, body)
		}
	})
	t.Run("tampered signature", func(t *testing.T) {
		_, u := newProxyAndURL(t, testAccessKeyID, testSecretAccessKey, time.Minute)
		u = strings.Replace(u, "X-Amz-Signature=", "X-Amz-Signature=0000", 1)
		status, body := get(t, u)
		if status != http.StatusForbidden || !strings.Contains(body, "SignatureDoesNotMatch") {
			t.Errorf("expect 403 SignatureDoesNotMatch, got %d: %s", status, body)
		}
	})
	t.Run("tampered key", func(t *testing.T) {
		_, u := newProxyAndURL(t, testAccessKeyID, testSecretAccessKey, time.Minute)
		u = strings.Replace(u, "key.txt", "other.txt", 1)
		status, body := get(t, u)
		if status != http.StatusForbidden || !strings.Contains(body, "SignatureDoesNotMatch") {
			t.Errorf("expect 403 SignatureDoesNotMatch, got %d: %s", status, body)
		}
	})
	t.Run("unknown access key", func(t *testing.T) {
		_, u := newProxyAndURL(t, "UNKNOWNKEY", testSecretAccessKey, time.Minute)
		status, body := get(t, u)
		if status != http.StatusForbidden || !strings.Contains(body, "InvalidAccessKeyId") {
			t.Errorf("expect 403 InvalidAccessKeyId, got %d: %s", status, body)
		}
	})
	t.Run("wrong secret", func(t *testing.T) {
		_, u := newProxyAndURL(t, testAccessKeyID, "wrongsecret", time.Minute)
		status, body := get(t, u)
		if status != http.StatusForbidden || !strings.Contains(body, "SignatureDoesNotMatch") {
			t.Errorf("expect 403 SignatureDoesNotMatch, got %d: %s", status, body)
		}
	})

	// X-Amz-Expires is validated before the signature: it must be a
	// positive integer of at most 604800 seconds (one week).
	expiresCases := []struct{ name, value string }{
		{"non-numeric expires", "abc"},
		{"negative expires", "-1"},
		{"zero expires", "0"},
		{"over one week expires", "604801"},
	}
	for _, tc := range expiresCases {
		t.Run(tc.name, func(t *testing.T) {
			_, u := newProxyAndURL(t, testAccessKeyID, testSecretAccessKey, time.Minute)
			u = replaceQueryParam(u, "X-Amz-Expires", tc.value)
			status, body := get(t, u)
			if status != http.StatusBadRequest || !strings.Contains(body, "AuthorizationQueryParametersError") {
				t.Errorf("expect 400 AuthorizationQueryParametersError, got %d: %s", status, body)
			}
		})
	}
}

// replaceQueryParam rewrites a single query parameter's value in a URL.
func replaceQueryParam(rawURL, key, value string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic(err)
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}
