package s3gw_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
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
