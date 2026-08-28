package s3gw_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/store"
)

func TestConsecutiveFailures(t *testing.T) {
	now := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)
	b := s3gw.NewConsecutiveFailures(2, time.Minute)
	s3gw.SetBreakerClock(b, func() time.Time { return now })
	expect := func(t *testing.T, allow bool, state string) {
		t.Helper()
		if got := b.Allow(); got != allow {
			t.Errorf("Allow = %v, want %v", got, allow)
		}
		if got := b.State(); got != state {
			t.Errorf("state %q, want %q", got, state)
		}
	}

	expect(t, true, "closed")
	b.Report(false)
	b.Report(true) // a success resets the run
	b.Report(false)
	expect(t, true, "closed")
	b.Report(false) // second consecutive failure opens it
	expect(t, false, "open")
	now = now.Add(30 * time.Second)
	expect(t, false, "open")
	now = now.Add(31 * time.Second)
	expect(t, true, "half-open")  // the probe
	expect(t, false, "half-open") // one probe per cooldown
	b.Report(false)               // failed probe re-opens
	expect(t, false, "open")
	now = now.Add(time.Minute)
	expect(t, true, "half-open")
	// this probe is never reported (client went away); the next one is
	// admitted a cooldown later
	now = now.Add(time.Minute)
	expect(t, true, "half-open")
	b.Report(true)
	expect(t, true, "closed")
}

func TestBackendName(t *testing.T) {
	for _, tc := range []struct{ endpoint, region, want string }{
		{"http://rgw:7480", "us-east-1", "http://rgw:7480"},
		{"https://user:secret@rgw.example.com/prefix?x=1", "us-east-1", "https://rgw.example.com"},
		{"", "ap-northeast-1", "aws:ap-northeast-1"},
	} {
		b := &store.Backend{Endpoint: tc.endpoint, Region: tc.region}
		if got := s3gw.BackendName(b); got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.endpoint, got, tc.want)
		}
	}
}

func TestClassifyAttempt(t *testing.T) {
	resp := func(status int) *smithyhttpResponse {
		return &smithyhttpResponse{Response: &http.Response{StatusCode: status}}
	}
	tests := []struct {
		name       string
		raw        any
		err        error
		ok, report bool
	}{
		{"200", resp(200), nil, true, true},
		{"404 proves the backend is up", resp(404), errors.New("NoSuchKey"), true, true},
		{"503", resp(503), errors.New("SlowDown"), false, true},
		{"transport failure", nil, errors.New("dial tcp: connection refused"), false, true},
		// the SDK hands a transport failure over with an empty response
		{"transport failure with the SDK's empty response", resp(0), errors.New("dial tcp: connection refused"), false, true},
		{"timeout", nil, context.DeadlineExceeded, false, true},
		{"client canceled", nil, context.Canceled, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var raw any
			if tc.raw != nil {
				raw = tc.raw.(*smithyhttpResponse).toSmithy()
			}
			ok, report := s3gw.ClassifyAttempt(raw, tc.err)
			if ok != tc.ok || report != tc.report {
				t.Errorf("got ok=%v report=%v, want ok=%v report=%v", ok, report, tc.ok, tc.report)
			}
		})
	}
}

// breakerBackend is an httptest backend whose answer is switchable and
// which counts what reached it.
type breakerBackend struct {
	status atomic.Int32
	hits   atomic.Int32
	block  chan struct{} // when non-nil, requests block until it is closed
}

func (b *breakerBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.hits.Add(1)
	if b.block != nil {
		select {
		case <-b.block:
		case <-r.Context().Done():
			return
		}
	}
	status := int(b.status.Load())
	if status == http.StatusOK {
		w.Header().Set("ETag", `"e"`)
		io.WriteString(w, "hello")
		return
	}
	w.WriteHeader(status)
}

// newBreakerProxy serves "cbbucket" on the recording backend through the
// real SDK client, guarded by a breaker of 2 failures / cooldown, with the
// backend client's retries pinned to attempts.
func newBreakerProxy(t *testing.T, backend *breakerBackend, cooldown time.Duration, attempts int) (*s3.Client, *s3gw.ConsecutiveFailures, func() *s3gw.RequestInfo) {
	t.Helper()
	backendTS := httptest.NewServer(backend)
	t.Cleanup(backendTS.Close)
	m := buildStore(t, "cbtenant",
		[]userSpec{{name: "u", keyID: testAccessKeyID, secret: testSecretAccessKey}},
		[]bucketSpec{{name: "cbbucket", endpoint: backendTS.URL}})
	gw := s3gw.New(m)
	br := s3gw.NewConsecutiveFailures(2, cooldown)
	gw.SetBreaker(func(*store.Backend) s3gw.Breaker { return br })
	gw.SetClientOptions(func(*store.Backend) []func(*s3.Options) {
		return []func(*s3.Options){func(o *s3.Options) {
			o.Retryer = retry.AddWithMaxAttempts(retry.NewStandard(), attempts)
		}}
	})
	var last atomic.Pointer[s3gw.RequestInfo]
	gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) { last.Store(info) })
	ts := newTestServer(t, gw)
	cfg, err := awsconfig.LoadDefaultConfig(t.Context(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(testAccessKeyID, testSecretAccessKey, "")),
		awsconfig.WithRetryMaxAttempts(1), // the front client must not retry either
	)
	if err != nil {
		t.Fatal(err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(ts)
		o.UsePathStyle = true
	})
	return client, br, last.Load
}

func expectCode(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != code {
		t.Fatalf("expect %s, got %v", code, err)
	}
}

func TestBreakerFailsFast(t *testing.T) {
	backend := &breakerBackend{}
	backend.status.Store(http.StatusServiceUnavailable)
	client, br, last := newBreakerProxy(t, backend, 200*time.Millisecond, 1)
	ctx := t.Context()
	head := func() error {
		_, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("cbbucket"), Key: aws.String("k")})
		return err
	}

	// two failures reach the backend and open the breaker
	for i := range 2 {
		if err := head(); err == nil {
			t.Fatalf("attempt %d: expect failure", i)
		}
	}
	if got := backend.hits.Load(); got != 2 {
		t.Fatalf("expect 2 backend hits, got %d", got)
	}
	if br.State() != "open" {
		t.Fatalf("expect open, got %s", br.State())
	}
	// the third is refused without touching the backend
	err := head()
	expectCode(t, err, "ServiceUnavailable")
	if got := backend.hits.Load(); got != 2 {
		t.Errorf("a refused request must not reach the backend, hits %d", got)
	}
	info := last()
	var open *s3gw.BreakerOpen
	if info == nil || info.Code != "ServiceUnavailable" || !errors.As(info.Err, &open) {
		t.Fatalf("expect the observer to see the open breaker, got %+v", info)
	}
	if open.Backend == "" {
		t.Error("expect the backend named in the cause")
	}

	// after the cooldown a probe goes through; the backend has recovered
	backend.status.Store(http.StatusOK)
	time.Sleep(250 * time.Millisecond)
	if err := head(); err != nil {
		t.Fatalf("probe must succeed: %v", err)
	}
	if br.State() != "closed" {
		t.Errorf("expect closed after a successful probe, got %s", br.State())
	}
}

// A backend nobody listens on: the real transport failure, with the SDK's
// default retries, must open the breaker.
func TestBreakerOpensOnConnectionRefused(t *testing.T) {
	m := buildStore(t, "cbtenant",
		[]userSpec{{name: "u", keyID: testAccessKeyID, secret: testSecretAccessKey}},
		[]bucketSpec{{name: "cbbucket", endpoint: "http://127.0.0.1:1"}})
	gw := s3gw.New(m)
	br := s3gw.NewConsecutiveFailures(2, time.Minute)
	gw.SetBreaker(func(*store.Backend) s3gw.Breaker { return br })
	var last atomic.Pointer[s3gw.RequestInfo]
	gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) { last.Store(info) })
	client := newS3Client(t, newTestServer(t, gw), testAccessKeyID, testSecretAccessKey)
	_, err := client.HeadBucket(t.Context(), &s3.HeadBucketInput{Bucket: aws.String("cbbucket")})
	// attempts 1 and 2 are refused connections, attempt 3 is refused by
	// the breaker — and that is the error the request ends with
	expectCode(t, err, "ServiceUnavailable")
	if br.State() != "open" {
		t.Errorf("expect open after two refused connections, got %s", br.State())
	}
	var open *s3gw.BreakerOpen
	if info := last.Load(); info == nil || !errors.As(info.Err, &open) {
		t.Errorf("expect the observer to see the open breaker, got %+v", info)
	}
}

func TestBreakerCountsRetriesAndIgnoresClientErrors(t *testing.T) {
	t.Run("4xx keeps it closed", func(t *testing.T) {
		backend := &breakerBackend{}
		backend.status.Store(http.StatusNotFound)
		client, br, _ := newBreakerProxy(t, backend, time.Minute, 1)
		for range 5 {
			_, err := client.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String("cbbucket"), Key: aws.String("k")})
			expectCode(t, err, "NotFound")
		}
		if br.State() != "closed" {
			t.Errorf("a 404 proves the backend is up; got %s", br.State())
		}
	})
	t.Run("retries count per attempt", func(t *testing.T) {
		backend := &breakerBackend{}
		backend.status.Store(http.StatusServiceUnavailable)
		client, br, _ := newBreakerProxy(t, backend, time.Minute, 3)
		// one request, three attempts: the breaker opens on the second and
		// the third is refused
		_, err := client.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String("cbbucket"), Key: aws.String("k")})
		expectCode(t, err, "ServiceUnavailable")
		if got := backend.hits.Load(); got != 2 {
			t.Errorf("expect 2 attempts to reach the backend, got %d", got)
		}
		if br.State() != "open" {
			t.Errorf("expect open, got %s", br.State())
		}
	})
	t.Run("a canceled request is not reported", func(t *testing.T) {
		backend := &breakerBackend{block: make(chan struct{})}
		backend.status.Store(http.StatusOK)
		client, br, _ := newBreakerProxy(t, backend, time.Minute, 1)
		for range 3 {
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			_, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("cbbucket"), Key: aws.String("k")})
			cancel()
			if err == nil {
				t.Fatal("expect the client to give up")
			}
		}
		close(backend.block)
		if br.State() != "closed" {
			t.Errorf("clients giving up say nothing about the backend; got %s", br.State())
		}
	})
}

// smithyhttpResponse builds the raw response shape the middleware sees.
type smithyhttpResponse struct{ Response *http.Response }

func (r *smithyhttpResponse) toSmithy() any { return &smithyhttp.Response{Response: r.Response} }
