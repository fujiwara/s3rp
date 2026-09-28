package s3gw_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/store"
)

// cannedHTTPClient stands in for an instrumented HTTP client a service would
// install (an otelhttp transport, custom timeouts): it records the requests
// the backend SDK client sends and answers them itself.
type cannedHTTPClient struct {
	mu       sync.Mutex
	requests []*http.Request
	body     string
}

func (c *cannedHTTPClient) Do(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.requests = append(c.requests, r)
	c.mu.Unlock()
	return &http.Response{
		StatusCode:    http.StatusOK,
		ContentLength: int64(len(c.body)),
		Header:        http.Header{"Content-Type": []string{"application/octet-stream"}},
		Body:          io.NopCloser(strings.NewReader(c.body)),
	}, nil
}

// TestSetClientOptions proves the hook's options reach the client the
// gateway builds: with an injected HTTP client, the request is answered by
// it instead of the (nonexistent) backend.
func TestSetClientOptions(t *testing.T) {
	gw := newTestGateway(t)
	canned := &cannedHTTPClient{body: "hello"}
	var backends []string
	gw.SetClientOptions(func(b *store.Backend) []func(*s3.Options) {
		backends = append(backends, b.Endpoint)
		return []func(*s3.Options){func(o *s3.Options) {
			o.HTTPClient = canned
		}}
	})

	// no SetBackend: the gateway must build the real SDK client, through
	// the hook
	req := signedRequest(t, "GET", "http://s3.example.com/testbucket/a.txt",
		nil, emptyPayloadHash, time.Now(), testCreds(), nil)
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "hello" {
		t.Errorf("expect the canned body, got %q", w.Body.String())
	}
	if len(backends) != 1 || backends[0] != "http://backend.invalid" {
		t.Errorf("expect the hook to see the backend definition once, got %v", backends)
	}
	if len(canned.requests) != 1 {
		t.Fatalf("expect one backend request through the injected client, got %d", len(canned.requests))
	}
	if host := canned.requests[0].URL.Host; host != "backend.invalid" {
		t.Errorf("unexpected backend host %q", host)
	}

	// a second request reuses the cached client: the hook is not consulted
	// again, but the injected HTTP client still serves
	w = httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, signedRequest(t, "GET", "http://s3.example.com/testbucket/b.txt",
		nil, emptyPayloadHash, time.Now(), testCreds(), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("second request: unexpected status %d: %s", w.Code, w.Body.String())
	}
	if len(backends) != 1 {
		t.Errorf("expect the cached client to be reused without consulting the hook, got %v", backends)
	}
	if len(canned.requests) != 2 {
		t.Errorf("expect the cached client to keep its injected HTTP client, got %d requests", len(canned.requests))
	}
}

// refuseCopyChecksum is the documented pattern for refusing what a backend
// cannot do: an Initialize middleware returning an *s3err.Error, which runs
// once per operation, before the SDK's retry loop and the network.
func refuseCopyChecksum(*store.Backend) []func(*s3.Options) {
	return []func(*s3.Options){func(o *s3.Options) {
		o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
			return stack.Initialize.Add(middleware.InitializeMiddlewareFunc("RefuseCopyChecksum",
				func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (
					middleware.InitializeOutput, middleware.Metadata, error,
				) {
					if cin, ok := in.Parameters.(*s3.CopyObjectInput); ok && cin.ChecksumAlgorithm != "" {
						return middleware.InitializeOutput{}, middleware.Metadata{},
							s3err.New(http.StatusNotImplemented, "NotImplemented",
								"A checksum algorithm on CopyObject is not supported.")
					}
					return next.HandleInitialize(ctx, in)
				}), middleware.Before)
		})
	}}
}

func TestClientOptionsRefusal(t *testing.T) {
	var calls atomic.Int32
	backendTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`<CopyObjectResult><ETag>"e"</ETag></CopyObjectResult>`))
	}))
	t.Cleanup(backendTS.Close)

	m := buildStore(t, "refusetenant",
		[]userSpec{{name: "u", keyID: testAccessKeyID, secret: testSecretAccessKey}},
		[]bucketSpec{{name: "refusebucket", endpoint: backendTS.URL}})
	gw := s3gw.New(m)
	gw.SetClientOptions(refuseCopyChecksum)
	var info *s3gw.RequestInfo
	gw.SetObserver(func(_ context.Context, i *s3gw.RequestInfo) { info = i })
	ts := newTestServer(t, gw)
	client := newS3Client(t, ts, testAccessKeyID, testSecretAccessKey)

	_, err := client.CopyObject(t.Context(), &s3.CopyObjectInput{
		Bucket:            aws.String("refusebucket"),
		Key:               aws.String("dst"),
		CopySource:        aws.String("refusebucket/src"),
		ChecksumAlgorithm: types.ChecksumAlgorithmSha256,
	})
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("expect an API error, got %v", err)
	}
	// the middleware's error reaches the client as it stands
	if ae.ErrorCode() != "NotImplemented" || ae.ErrorMessage() != "A checksum algorithm on CopyObject is not supported." {
		t.Errorf("unexpected error %s: %s", ae.ErrorCode(), ae.ErrorMessage())
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("a refused operation reached the backend %d times", n)
	}
	if info == nil || info.Status != http.StatusNotImplemented || info.Code != "NotImplemented" {
		t.Errorf("unexpected observation %+v", info)
	}

	if _, err := client.CopyObject(t.Context(), &s3.CopyObjectInput{
		Bucket:     aws.String("refusebucket"),
		Key:        aws.String("dst"),
		CopySource: aws.String("refusebucket/src"),
	}); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("expect the plain copy to reach the backend once, got %d", n)
	}
}
