package s3gw_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
