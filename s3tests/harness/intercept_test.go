package harness_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/fujiwara/s3rp/s3tests/harness"
)

// stubBackend records backend calls and returns configured errors.
type stubBackend struct {
	mu        sync.Mutex
	created   []*s3.CreateBucketInput
	deleted   []*s3.DeleteBucketInput
	createErr error
	deleteErr error
}

func (s *stubBackend) CreateBucket(ctx context.Context, in *s3.CreateBucketInput, opts ...func(*s3.Options)) (*s3.CreateBucketOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return nil, s.createErr
	}
	s.created = append(s.created, in)
	return &s3.CreateBucketOutput{}, nil
}

func (s *stubBackend) DeleteBucket(ctx context.Context, in *s3.DeleteBucketInput, opts ...func(*s3.Options)) (*s3.DeleteBucketOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteErr != nil {
		return nil, s.deleteErr
	}
	s.deleted = append(s.deleted, in)
	return &s3.DeleteBucketOutput{}, nil
}

func (s *stubBackend) createCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.created)
}

// recordedReq is what the fall-through stub handler saw.
type recordedReq struct {
	Method, Path, RawQuery, Body string
}

type recordingHandler struct {
	mu   sync.Mutex
	reqs []recordedReq
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	h.reqs = append(h.reqs, recordedReq{r.Method, r.URL.Path, r.URL.RawQuery, string(body)})
	h.mu.Unlock()
	w.WriteHeader(http.StatusTeapot) // distinct marker: reached the gateway stub
}

func (h *recordingHandler) last(t *testing.T) recordedReq {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.reqs) == 0 {
		t.Fatal("no request reached the next handler")
	}
	return h.reqs[len(h.reqs)-1]
}

func newHarness(t *testing.T) (*harness.MemStore, *stubBackend, *recordingHandler, string) {
	t.Helper()
	st := harness.NewMemStore("http://backend.invalid", "bk", "bs", harness.DefaultKeys())
	backend := &stubBackend{}
	next := &recordingHandler{}
	ts := httptest.NewServer(harness.NewInterceptor(st, backend, next))
	t.Cleanup(ts.Close)
	return st, backend, next, ts.URL
}

func sdkClient(t *testing.T, url, keyID, secret string) *s3.Client {
	t.Helper()
	cfg, err := awsconfig.LoadDefaultConfig(t.Context(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(keyID, secret, "")),
	)
	if err != nil {
		t.Fatal(err)
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(url)
		o.UsePathStyle = true
	})
}

func errorCode(t *testing.T, err error) string {
	t.Helper()
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("not an API error: %v", err)
	}
	return apiErr.ErrorCode()
}

func TestCreateBucket(t *testing.T) {
	st, backend, _, url := newHarness(t)
	client := sdkClient(t, url, harness.MainAccessKeyID, harness.MainSecretAccessKey)

	if _, err := client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String("bkt-new")}); err != nil {
		t.Fatal(err)
	}
	if owner, ok := st.Owner("bkt-new"); !ok || owner != "main" {
		t.Errorf("bucket not registered for main: %s %v", owner, ok)
	}
	if backend.createCount() != 1 || *backend.created[0].Bucket != "bkt-new" {
		t.Errorf("backend create not called as expected: %+v", backend.created)
	}
}

func TestCreateBucketObjectLock(t *testing.T) {
	_, backend, _, url := newHarness(t)
	client := sdkClient(t, url, harness.MainAccessKeyID, harness.MainSecretAccessKey)

	if _, err := client.CreateBucket(t.Context(), &s3.CreateBucketInput{
		Bucket:                     aws.String("bkt-lock"),
		ObjectLockEnabledForBucket: aws.Bool(true),
	}); err != nil {
		t.Fatal(err)
	}
	in := backend.created[0]
	if in.ObjectLockEnabledForBucket == nil || !*in.ObjectLockEnabledForBucket {
		t.Error("ObjectLockEnabledForBucket not forwarded to the backend")
	}
}

func TestCreateBucketConflicts(t *testing.T) {
	st, _, _, url := newHarness(t)
	main := sdkClient(t, url, harness.MainAccessKeyID, harness.MainSecretAccessKey)
	alt := sdkClient(t, url, harness.AltAccessKeyID, harness.AltSecretAccessKey)

	if _, err := main.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String("bkt-dup")}); err != nil {
		t.Fatal(err)
	}
	_, err := main.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String("bkt-dup")})
	if code := errorCode(t, err); code != "BucketAlreadyOwnedByYou" {
		t.Errorf("owner re-create: expected BucketAlreadyOwnedByYou, got %s", code)
	}
	_, err = alt.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String("bkt-dup")})
	if code := errorCode(t, err); code != "BucketAlreadyExists" {
		t.Errorf("cross-tenant create: expected BucketAlreadyExists, got %s", code)
	}
	if owner, _ := st.Owner("bkt-dup"); owner != "main" {
		t.Errorf("owner changed: %s", owner)
	}
}

func TestCreateBucketInvalidName(t *testing.T) {
	_, backend, _, url := newHarness(t)
	client := sdkClient(t, url, harness.MainAccessKeyID, harness.MainSecretAccessKey)

	_, err := client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String("Invalid_Name")})
	if code := errorCode(t, err); code != "InvalidBucketName" {
		t.Errorf("expected InvalidBucketName, got %s", code)
	}
	if backend.createCount() != 0 {
		t.Error("backend must not be called for an invalid name")
	}
}

func TestCreateBucketBackendFailureRollsBack(t *testing.T) {
	st, backend, _, url := newHarness(t)
	backend.createErr = errors.New("backend down")
	client := sdkClient(t, url, harness.MainAccessKeyID, harness.MainSecretAccessKey)

	if _, err := client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String("bkt-fail")}); err == nil {
		t.Fatal("expected error")
	}
	if _, ok := st.Owner("bkt-fail"); ok {
		t.Error("claim not rolled back after backend failure")
	}
}

func TestCreateBucketToleratesLeftoverBackendBucket(t *testing.T) {
	st, backend, _, url := newHarness(t)
	backend.createErr = &types.BucketAlreadyOwnedByYou{}
	client := sdkClient(t, url, harness.MainAccessKeyID, harness.MainSecretAccessKey)

	// a bucket left on the backend by a previous harness run is adopted
	if _, err := client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String("bkt-left")}); err != nil {
		t.Fatal(err)
	}
	if owner, ok := st.Owner("bkt-left"); !ok || owner != "main" {
		t.Errorf("bucket not registered: %s %v", owner, ok)
	}
}

func TestDeleteBucket(t *testing.T) {
	st, backend, _, url := newHarness(t)
	client := sdkClient(t, url, harness.MainAccessKeyID, harness.MainSecretAccessKey)

	if _, err := client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String("bkt-del")}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.DeleteBucket(t.Context(), &s3.DeleteBucketInput{Bucket: aws.String("bkt-del")}); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Owner("bkt-del"); ok {
		t.Error("bucket still registered after delete")
	}
	if len(backend.deleted) != 1 || *backend.deleted[0].Bucket != "bkt-del" {
		t.Errorf("backend delete not called as expected: %+v", backend.deleted)
	}

	// deleting again: the harness's own operations answer AWS-compatibly
	_, err := client.DeleteBucket(t.Context(), &s3.DeleteBucketInput{Bucket: aws.String("bkt-del")})
	if code := errorCode(t, err); code != "NoSuchBucket" {
		t.Errorf("expected NoSuchBucket, got %s", code)
	}
}

func TestDeleteBucketNonOwner(t *testing.T) {
	st, _, _, url := newHarness(t)
	main := sdkClient(t, url, harness.MainAccessKeyID, harness.MainSecretAccessKey)
	alt := sdkClient(t, url, harness.AltAccessKeyID, harness.AltSecretAccessKey)

	if _, err := main.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String("bkt-owned")}); err != nil {
		t.Fatal(err)
	}
	_, err := alt.DeleteBucket(t.Context(), &s3.DeleteBucketInput{Bucket: aws.String("bkt-owned")})
	if code := errorCode(t, err); code != "AccessDenied" {
		t.Errorf("expected AccessDenied, got %s", code)
	}
	if _, ok := st.Owner("bkt-owned"); !ok {
		t.Error("bucket must remain registered")
	}
}

func TestDeleteBucketNotEmpty(t *testing.T) {
	st, backend, _, url := newHarness(t)
	client := sdkClient(t, url, harness.MainAccessKeyID, harness.MainSecretAccessKey)

	if _, err := client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String("bkt-full")}); err != nil {
		t.Fatal(err)
	}
	backend.deleteErr = &awshttp.ResponseError{
		ResponseError: &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusConflict, Header: http.Header{}}},
			Err:      &smithy.GenericAPIError{Code: "BucketNotEmpty", Message: "the bucket is not empty"},
		},
	}
	_, err := client.DeleteBucket(t.Context(), &s3.DeleteBucketInput{Bucket: aws.String("bkt-full")})
	if code := errorCode(t, err); code != "BucketNotEmpty" {
		t.Errorf("expected BucketNotEmpty, got %s", code)
	}
	if _, ok := st.Owner("bkt-full"); !ok {
		t.Error("bucket must remain registered when the backend refuses the delete")
	}
}

func TestAuthFailures(t *testing.T) {
	_, backend, next, url := newHarness(t)

	badSecret := sdkClient(t, url, harness.MainAccessKeyID, "wrongsecret")
	_, err := badSecret.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String("bkt-x")})
	if code := errorCode(t, err); code != "SignatureDoesNotMatch" {
		t.Errorf("expected SignatureDoesNotMatch, got %s", code)
	}

	unknown := sdkClient(t, url, "UNKNOWNKEY0000000000", "whatever")
	_, err = unknown.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String("bkt-x")})
	if code := errorCode(t, err); code != "InvalidAccessKeyId" {
		t.Errorf("expected InvalidAccessKeyId, got %s", code)
	}

	// unsigned request on the intercepted shape: refused, not forwarded
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPut, url+"/bkt-x", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("unsigned create: expected 403, got %d", resp.StatusCode)
	}

	if backend.createCount() != 0 {
		t.Error("backend must not be reached on auth failure")
	}
	next.mu.Lock()
	defer next.mu.Unlock()
	if len(next.reqs) != 0 {
		t.Errorf("auth failures must not fall through: %+v", next.reqs)
	}
}

func TestFallThrough(t *testing.T) {
	_, backend, next, url := newHarness(t)

	// requests the interceptor must NOT claim: subresource queries go to
	// the gateway (whose genuine 501/ACL answers the suite must see), as
	// do object paths and non-PUT/DELETE methods. No signature needed —
	// the predicate runs before verification.
	cases := []struct {
		method, path, body string
	}{
		{http.MethodPut, "/bkt?acl", ""},
		{http.MethodPut, "/bkt?policy", ""},
		{http.MethodPut, "/bkt?versioning", ""},
		{http.MethodDelete, "/bkt?cors", ""},
		{http.MethodPut, "/bkt/key", "hello"},
		{http.MethodGet, "/bkt", ""},
		{http.MethodPut, "/", ""},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), c.method, url+c.path, strings.NewReader(c.body))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusTeapot {
				t.Fatalf("expected fall-through marker 418, got %d", resp.StatusCode)
			}
			got := next.last(t)
			if got.Method != c.method || got.Body != c.body {
				t.Errorf("request mangled in fall-through: %+v", got)
			}
		})
	}
	if backend.createCount() != 0 {
		t.Error("backend must not be touched by fall-through requests")
	}
}
