package s3gw_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/google/go-cmp/cmp"
)

// stubGet is a backend that answers GetObject, or fails the way a backend
// that is down does — with no API error code of its own.
type stubGet struct {
	s3gw.BackendClient
	body string
	err  error
}

func (s stubGet) GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(s.body))}, nil
}

// serve runs one signed GET through the gateway's handler.
func serve(t *testing.T, gw *s3gw.Gateway, backend s3gw.BackendClient) *httptest.ResponseRecorder {
	t.Helper()
	if err := gw.SetBackend("testbucket", backend); err != nil {
		t.Fatal(err)
	}
	req := signedRequest(t, "GET", "http://s3.example.com/testbucket/a.txt",
		nil, emptyPayloadHash, time.Now(), testCreds(), nil)
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)
	return w
}

// TestObserverSeesEveryRequest checks what a service needs in order to write
// its own log: the identity of the request, what the client was told, and —
// only here — why.
func TestObserverSeesEveryRequest(t *testing.T) {
	gw := newTestGateway(t)
	var got []*s3gw.RequestInfo
	gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) {
		got = append(got, info)
	})

	res := serve(t, gw, stubGet{body: "hello"})
	if res.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", res.Code, res.Body.String())
	}

	if len(got) != 1 {
		t.Fatalf("expect one observation, got %d", len(got))
	}
	info := got[0]
	if info.Method != "GET" || info.Path != "/testbucket/a.txt" {
		t.Errorf("unexpected request %+v", info)
	}
	if info.Status != http.StatusOK || info.Code != "" || info.Err != nil {
		t.Errorf("expect a clean success, got %+v", info)
	}
	if info.BytesOut != int64(len("hello")) {
		t.Errorf("expect the served bytes, got %d", info.BytesOut)
	}
	// the id the client was handed, so a reported request can be found
	if info.RequestID == "" || info.RequestID != res.Header().Get("x-amz-request-id") {
		t.Errorf("request id %q does not match the response header %q",
			info.RequestID, res.Header().Get("x-amz-request-id"))
	}
	// the record must stand on its own: when it happened, not only how long
	if info.Duration <= 0 {
		t.Error("expect a duration")
	}
	if info.Start.IsZero() {
		t.Error("expect a start time")
	}
	if before := info.Start.Add(info.Duration); before.After(time.Now()) {
		t.Errorf("start %v plus duration %v is in the future", info.Start, info.Duration)
	}
}

func TestObserverSeesTheCauseOfAFailure(t *testing.T) {
	gw := newTestGateway(t)
	var got *s3gw.RequestInfo
	gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) { got = info })

	down := errors.New("dial tcp 10.0.0.1:9000: connection refused")
	res := serve(t, gw, stubGet{err: down})

	if strings.Contains(res.Body.String(), "connection refused") {
		t.Error("the cause must not reach the client")
	}
	if got == nil {
		t.Fatal("expect an observation")
	}
	if got.Code == "" || got.Status < 500 {
		t.Errorf("expect a server-side failure, got %+v", got)
	}
	if !errors.Is(got.Err, down) {
		t.Errorf("expect the cause, got %v", got.Err)
	}
}

// Without an observer the gateway says nothing at all: a service that
// installs none gets no log rather than one it cannot control.
func TestNoObserverIsSilent(t *testing.T) {
	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	serve(t, newTestGateway(t), stubGet{body: "hello"})
	if buf.Len() != 0 {
		t.Errorf("expect no logging without an observer, got %s", buf.String())
	}
}

// TestRequestInfoJSON pins the shape a service gets when it emits the record
// as it stands: snake_case keys, and the reason for a failure as a message
// rather than the empty object an error marshals to on its own.
func TestRequestInfoJSON(t *testing.T) {
	info := &s3gw.RequestInfo{
		Method: "GET", Path: "/photos/a.jpg", RemoteAddr: "10.0.0.5:1234",
		RawQuery: "X-Amz-Signature=REDACTED", RequestID: "abc123",
		Tenant: "ta", User: "app1", AccessKeyID: "S3RPKEY001",
		Status: 502, Code: "InternalError",
		Err:      errors.New("connection refused"),
		BytesOut: 217,
		Start:    time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		Duration: 1500 * time.Microsecond,
	}
	var got map[string]any
	b, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"method": "GET", "path": "/photos/a.jpg", "remote_addr": "10.0.0.5:1234",
		"raw_query": "X-Amz-Signature=REDACTED", "request_id": "abc123",
		"tenant": "ta", "user": "app1", "access_key_id": "S3RPKEY001",
		"status": float64(502), "code": "InternalError",
		"error":    "connection refused", // not {}
		"bytes_in": float64(0), "bytes_out": float64(217),
		"start": "2026-08-01T12:00:00Z", "duration": float64(1500000), // nanoseconds
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("unexpected JSON (-want +got):\n%s", diff)
	}

	// a success carries no code and no error
	b, err = json.Marshal(s3gw.RequestInfo{Method: "GET", Status: 200})
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{`"code"`, `"error"`} {
		if strings.Contains(string(b), absent) {
			t.Errorf("expect %s to be omitted on success: %s", absent, b)
		}
	}
}

// TestObserverIdentityAndOp covers the three shapes a record takes, which is
// the reason the identity is kept apart from the operation: a service has to
// be able to say who was refused, not only who succeeded.
func TestObserverIdentityAndOp(t *testing.T) {
	t.Run("an operation that ran", func(t *testing.T) {
		gw := newTestGateway(t)
		var got *s3gw.RequestInfo
		gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) { got = info })
		serve(t, gw, stubGet{body: "hello"})

		if got.Tenant != "testtenant" || got.User != "testuser" || got.AccessKeyID != testAccessKeyID {
			t.Errorf("expect the identity, got %+v", got)
		}
		if got.Op == nil {
			t.Fatal("expect the operation")
		}
		if got.Op.Action != "s3:GetObject" || got.Op.Bucket != "testbucket" || got.Op.Key != "a.txt" {
			t.Errorf("unexpected operation %+v", got.Op)
		}
	})

	// authenticated, then refused: the identity is exactly what an operator
	// needs here, and there is no operation to report
	t.Run("refused after the signature verified", func(t *testing.T) {
		gw := newTestGateway(t)
		var got *s3gw.RequestInfo
		gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) { got = info })
		req := signedRequest(t, "GET", "http://s3.example.com/nosuchbucket/a.txt",
			nil, emptyPayloadHash, time.Now(), testCreds(), nil)
		w := httptest.NewRecorder()
		gw.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Fatalf("expect the bucket to be refused, got %d", w.Code)
		}
		if got.Tenant != "testtenant" || got.User != "testuser" || got.AccessKeyID != testAccessKeyID {
			t.Errorf("expect the identity of the refused request, got %+v", got)
		}
		if got.Op != nil {
			t.Errorf("expect no operation, got %+v", got.Op)
		}
	})

	t.Run("signature that did not verify", func(t *testing.T) {
		gw := newTestGateway(t)
		var got *s3gw.RequestInfo
		gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) { got = info })
		req := signedRequest(t, "GET", "http://s3.example.com/testbucket/a.txt",
			nil, emptyPayloadHash, time.Now(),
			aws.Credentials{AccessKeyID: testAccessKeyID, SecretAccessKey: "wrong"}, nil)
		w := httptest.NewRecorder()
		gw.Handler().ServeHTTP(w, req)

		if got.Tenant != "" || got.User != "" || got.Op != nil {
			t.Errorf("nothing is proven by a bad signature, got %+v", got)
		}
		// the key the request claimed is recorded unverified, so a
		// leaked-key hunt can see which key was tried
		if got.AccessKeyID != testAccessKeyID {
			t.Errorf("expect the presented key id, got %q", got.AccessKeyID)
		}
		if got.Code != "SignatureDoesNotMatch" {
			t.Errorf("unexpected code %q", got.Code)
		}
	})
}
