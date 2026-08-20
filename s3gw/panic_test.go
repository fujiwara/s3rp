package s3gw_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/fujiwara/s3rp/s3gw"
)

// A panic in a hook is a bug in the service, but left to net/http it costs
// the client its response and the service its record of the request: the
// connection is dropped, the panic goes to net/http's own ErrorLog, and the
// client's retry runs the whole operation again. The gateway recovers at the
// boundary so a panic is an ordinary failure — answered, and observed once.

type panicHook struct{ value any }

func (p panicHook) Authorize(context.Context, *s3gw.Op) error { panic(p.value) }

// panicGateway serves one signed GET through a gateway with an observer,
// and reports what the recovered panic (if any) reached the caller as.
func panicGateway(t *testing.T, setup func(*s3gw.Gateway)) (*httptest.ResponseRecorder, []*s3gw.RequestInfo, any) {
	t.Helper()
	gw := newTestGateway(t)
	stub := &stubBackend{getOut: &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("hello"))}}
	if err := gw.SetBackend("testbucket", stub); err != nil {
		t.Fatal(err)
	}
	var seen []*s3gw.RequestInfo
	gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) { seen = append(seen, info) })
	setup(gw)

	req := signedRequest(t, "GET", "http://s3.example.com/testbucket/a.txt",
		nil, emptyPayloadHash, time.Now(), testCreds(), nil)
	w := httptest.NewRecorder()
	var escaped any
	func() {
		defer func() { escaped = recover() }()
		gw.Handler().ServeHTTP(w, req)
	}()
	return w, seen, escaped
}

func TestPanicBeforeResponseIsAnswered(t *testing.T) {
	w, seen, escaped := panicGateway(t, func(gw *s3gw.Gateway) {
		gw.SetAuthorizer(panicHook{value: "hook blew up"})
	})
	if escaped != nil {
		t.Fatalf("expect the panic to be handled, got %v", escaped)
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expect an InternalError status, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "<Code>InternalError</Code>") {
		t.Errorf("expect an error document, got %s", w.Body.String())
	}
	if w.Header().Get("x-amz-request-id") == "" {
		t.Error("expect the request id on the response")
	}

	if len(seen) != 1 {
		t.Fatalf("expect one observation, got %d", len(seen))
	}
	info := seen[0]
	if info.Status != http.StatusInternalServerError || info.Code != "InternalError" {
		t.Errorf("unexpected record %+v", info)
	}
	if info.RequestID != w.Header().Get("x-amz-request-id") {
		t.Error("expect the record to carry the id the client was given")
	}
	// the cause carries what panicked, and the stack the log line does not
	var pe *s3gw.PanicError
	if !errors.As(info.Err, &pe) {
		t.Fatalf("expect a PanicError cause, got %v", info.Err)
	}
	if pe.Value != "hook blew up" {
		t.Errorf("unexpected panic value %v", pe.Value)
	}
	if !strings.Contains(string(pe.Stack), "s3gw_test.panicHook") {
		t.Errorf("expect the panicking frame in the stack:\n%s", pe.Stack)
	}
	if !strings.Contains(info.Err.Error(), "hook blew up") {
		t.Errorf("expect the message to name the panic, got %q", info.Err.Error())
	}
}

func TestPanicAfterResponseStartedAborts(t *testing.T) {
	w, seen, escaped := panicGateway(t, func(gw *s3gw.Gateway) {
		gw.Use(func(_ context.Context, _ *s3gw.Op, next func() error) error {
			if err := next(); err != nil {
				return err
			}
			panic("metering blew up")
		})
	})
	// the response is already on its way, so it cannot be replaced by an
	// error document; the connection is aborted instead, which is what stops
	// the client from reading a truncated body as a complete one
	if escaped != http.ErrAbortHandler {
		t.Fatalf("expect the request to be aborted, got %v", escaped)
	}
	if body := w.Body.String(); body != "hello" {
		t.Errorf("expect the body left as it was, got %q", body)
	}
	if len(seen) != 1 {
		t.Fatalf("expect one observation, got %d", len(seen))
	}
	// the record says what the client was actually told: the status already
	// sent, and no code, since the error document could not be written
	if seen[0].Status != http.StatusOK {
		t.Errorf("expect the status already sent, got %d", seen[0].Status)
	}
	if seen[0].Code != "" {
		t.Errorf("expect no code for an error the client was never told, got %q", seen[0].Code)
	}
	if _, ok := errors.AsType[*s3gw.PanicError](seen[0].Err); !ok {
		t.Fatalf("expect the panic recorded as the cause, got %v", seen[0].Err)
	}
}

func TestPanicInObserverKeepsTheResponse(t *testing.T) {
	w, _, escaped := panicGateway(t, func(gw *s3gw.Gateway) {
		gw.SetObserver(func(context.Context, *s3gw.RequestInfo) { panic("access log blew up") })
	})
	if escaped != nil {
		t.Fatalf("expect the observer's panic to be contained, got %v", escaped)
	}
	if w.Code != http.StatusOK || w.Body.String() != "hello" {
		t.Errorf("expect the response to survive, got %d %q", w.Code, w.Body.String())
	}
}

func TestAbortHandlerPanicPassesThrough(t *testing.T) {
	_, seen, escaped := panicGateway(t, func(gw *s3gw.Gateway) {
		gw.SetAuthorizer(panicHook{value: http.ErrAbortHandler})
	})
	if escaped != http.ErrAbortHandler {
		t.Fatalf("expect ErrAbortHandler to reach net/http unchanged, got %v", escaped)
	}
	// net/http's convention is a silent abort, so it stays silent here too
	if len(seen) != 0 {
		t.Errorf("expect no observation for a deliberate abort, got %d", len(seen))
	}
}
