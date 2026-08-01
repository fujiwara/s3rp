package s3gw_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/fujiwara/s3rp/s3gw"
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
