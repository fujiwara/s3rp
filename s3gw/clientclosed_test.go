package s3gw_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/fujiwara/s3rp/s3gw"
)

// stubSlow is a backend whose calls end the way they do when the inbound
// request is abandoned: GetObject with the context's cancellation, PutObject
// with the error of the body it was reading.
type stubSlow struct {
	s3gw.BackendClient
	called chan struct{}
}

func (s stubSlow) GetObject(ctx context.Context, _ *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	close(s.called)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s stubSlow) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	close(s.called)
	_, err := io.Copy(io.Discard, in.Body)
	if err == nil {
		err = errors.New("expect the body read to fail")
	}
	return nil, err
}

// A client that goes away is not a failure of the service: recorded as a
// 5xx it would count against the error rate and read as a backend problem.
func TestClientClosedRequest(t *testing.T) {
	tests := []struct {
		name   string
		method string
		body   bool
	}{
		{name: "waiting for the backend", method: "GET"},
		{name: "upload abandoned midway", method: "PUT", body: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw := newTestGateway(t)
			observed := make(chan *s3gw.RequestInfo, 1)
			gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) { observed <- info })
			stub := stubSlow{called: make(chan struct{})}
			if err := gw.SetBackend("testbucket", stub); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(gw.Handler())
			t.Cleanup(srv.Close)

			signed := signedRequest(t, tt.method, srv.URL+"/testbucket/a.txt", nil, "UNSIGNED-PAYLOAD",
				time.Now(), testCreds(), nil)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, tt.method, srv.URL+"/testbucket/a.txt", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header = signed.Header
			var pw *io.PipeWriter
			if tt.body {
				var pr *io.PipeReader
				pr, pw = io.Pipe()
				req.Body = pr
				req.ContentLength = 1024
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				res, err := http.DefaultTransport.RoundTrip(req)
				if err == nil {
					res.Body.Close()
				}
			}()
			if pw != nil {
				go pw.Write([]byte("only the beginning"))
			}
			<-stub.called
			cancel()
			if pw != nil {
				// the transport waits for its body writer, which is
				// blocked on the pipe
				pw.CloseWithError(context.Canceled)
			}
			<-done

			var info *s3gw.RequestInfo
			select {
			case info = <-observed:
			case <-time.After(10 * time.Second):
				t.Fatal("expect an observation")
			}
			if info.Status != s3gw.StatusClientClosedRequest || info.Code != "ClientClosedRequest" {
				t.Errorf("expect 499 ClientClosedRequest, got %d %s (%v)", info.Status, info.Code, info.Err)
			}
			if !errors.Is(info.Err, s3gw.ErrClientClosed) {
				t.Errorf("expect ErrClientClosed in the cause, got %v", info.Err)
			}
		})
	}
}

// A refusal the client was already owed keeps its own status even when the
// client is gone by the time it is written.
func TestClientClosedKeepsRefusal(t *testing.T) {
	gw := newTestGateway(t)
	var got *s3gw.RequestInfo
	gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) { got = info })
	if err := gw.SetBackend("testbucket", stubGet{body: "hello"}); err != nil {
		t.Fatal(err)
	}
	req := signedRequest(t, "GET", "http://s3.example.com/testbucket/a.txt", nil, emptyPayloadHash,
		time.Now(), testCreds(), func(r *http.Request) { r.Header.Set("x-amz-request-payer", "requester") })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	gw.Handler().ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))
	if got == nil || got.Status != http.StatusNotImplemented || got.Code != "NotImplemented" {
		t.Errorf("expect the 501 to stand, got %+v", got)
	}
}

// brokenBody yields the beginning of an object and then fails, as a backend
// connection reset midway does.
type brokenBody struct{ sent bool }

func (b *brokenBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, errors.New("connection reset by peer")
	}
	b.sent = true
	return copy(p, "hel"), nil
}

func (b *brokenBody) Close() error { return nil }

// stubGetBody answers GetObject with the given body.
type stubGetBody struct {
	s3gw.BackendClient
	body io.Reader
}

func (s stubGetBody) GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	return &s3.GetObjectOutput{Body: io.NopCloser(s.body)}, nil
}

type stubBrokenGet struct{ s3gw.BackendClient }

func (stubBrokenGet) GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	return &s3.GetObjectOutput{Body: &brokenBody{}, ContentLength: aws.Int64(5)}, nil
}

// failingWriter is a client connection that breaks on the first body
// write; net/http cancels the request's context when that happens.
type failingWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w failingWriter) Write([]byte) (int, error) {
	w.cancel()
	return 0, errors.New("write: broken pipe")
}

// A download that stops midway has its 200 already: the observer is the
// only place that can say it was not complete, and which side broke it.
func TestTruncatedDownloadIsObserved(t *testing.T) {
	t.Run("backend read fails", func(t *testing.T) {
		gw := newTestGateway(t)
		var got *s3gw.RequestInfo
		gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) { got = info })
		res := serve(t, gw, stubBrokenGet{})
		if res.Code != http.StatusOK || res.Body.String() != "hel" {
			t.Fatalf("expect a started, truncated response, got %d %q", res.Code, res.Body.String())
		}
		if got == nil || got.Status != http.StatusOK || got.Code != "" {
			t.Fatalf("expect the status the client was sent and no code, got %+v", got)
		}
		if got.Err == nil || !strings.Contains(got.Err.Error(), "from the backend") {
			t.Errorf("expect a backend read failure, got %v", got.Err)
		}
		if errors.Is(got.Err, s3gw.ErrClientClosed) {
			t.Errorf("a backend failure must not read as the client leaving: %v", got.Err)
		}
	})
	t.Run("write fails while the read fails too", func(t *testing.T) {
		// the read hands over bytes with its error and writing them fails:
		// io.Copy reports the write, and so must the cause
		gw := newTestGateway(t)
		var got *s3gw.RequestInfo
		gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) { got = info })
		if err := gw.SetBackend("testbucket", stubGetBody{body: iotest.DataErrReader(iotest.TimeoutReader(strings.NewReader("hello")))}); err != nil {
			t.Fatal(err)
		}
		req := signedRequest(t, "GET", "http://s3.example.com/testbucket/a.txt",
			nil, emptyPayloadHash, time.Now(), testCreds(), nil)
		gw.Handler().ServeHTTP(failingWriter{httptest.NewRecorder(), func() {}}, req)
		if got == nil || got.Err == nil {
			t.Fatalf("expect a cause, got %+v", got)
		}
		if msg := got.Err.Error(); strings.Contains(msg, "from the backend") || !strings.Contains(msg, "broken pipe") {
			t.Errorf("expect the write failure, got %v", got.Err)
		}
	})
	t.Run("client leaves", func(t *testing.T) {
		gw := newTestGateway(t)
		var got *s3gw.RequestInfo
		gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) { got = info })
		if err := gw.SetBackend("testbucket", stubGet{body: "hello"}); err != nil {
			t.Fatal(err)
		}
		req := signedRequest(t, "GET", "http://s3.example.com/testbucket/a.txt",
			nil, emptyPayloadHash, time.Now(), testCreds(), nil)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		gw.Handler().ServeHTTP(failingWriter{httptest.NewRecorder(), cancel}, req.WithContext(ctx))
		if got == nil || got.Status != http.StatusOK || got.Code != "" {
			t.Fatalf("expect the status the client was sent and no code, got %+v", got)
		}
		if !errors.Is(got.Err, s3gw.ErrClientClosed) {
			t.Errorf("expect ErrClientClosed, got %v", got.Err)
		}
	})
}
