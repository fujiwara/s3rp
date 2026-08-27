package s3gw_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/store"
)

// traceKey is what a service's wrapping handler puts in the context; the
// gateway's request id can then be derived from it.
type traceKey struct{}

func TestSetRequestID(t *testing.T) {
	gw := newTestGateway(t)
	gw.SetRequestID(func(r *http.Request) string {
		id, _ := r.Context().Value(traceKey{}).(string)
		return id
	})
	var observed []*s3gw.RequestInfo
	gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) {
		observed = append(observed, info)
	})
	if err := gw.SetBackend("testbucket", stubGet{body: "hello"}); err != nil {
		t.Fatal(err)
	}
	// the service's own handler sits outside the gateway's, as an otel or
	// request-id middleware would
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("X-Test-Trace"); id != "" {
			r = r.WithContext(context.WithValue(r.Context(), traceKey{}, id))
		}
		gw.Handler().ServeHTTP(w, r)
	})
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	client := newS3Client(t, ts.URL, testAccessKeyID, testSecretAccessKey)

	t.Run("derived id", func(t *testing.T) {
		out, err := client.GetObject(t.Context(), &s3.GetObjectInput{
			Bucket: aws.String("testbucket"), Key: aws.String("a.txt"),
		}, func(o *s3.Options) {
			o.APIOptions = append(o.APIOptions, smithyhttp.AddHeaderValue("X-Test-Trace", "trace-0001"))
		})
		if err != nil {
			t.Fatal(err)
		}
		out.Body.Close()
		if got := observed[len(observed)-1].RequestID; got != "trace-0001" {
			t.Errorf("observed request id %q, want the derived one", got)
		}
	})
	t.Run("derived id on a refusal", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/testbucket/a.txt", nil)
		req.Header.Set("X-Test-Trace", "trace-0002")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expect 403 unsigned, got %d", resp.StatusCode)
		}
		if got := resp.Header.Get("x-amz-request-id"); got != "trace-0002" {
			t.Errorf("header %q", got)
		}
		if !strings.Contains(string(body), "<RequestId>trace-0002</RequestId>") {
			t.Errorf("error body lacks the id: %s", body)
		}
		if got := observed[len(observed)-1].RequestID; got != "trace-0002" {
			t.Errorf("observed request id %q", got)
		}
	})
	t.Run("empty falls back to a random id", func(t *testing.T) {
		out, err := client.GetObject(t.Context(), &s3.GetObjectInput{
			Bucket: aws.String("testbucket"), Key: aws.String("a.txt"),
		})
		if err != nil {
			t.Fatal(err)
		}
		out.Body.Close()
		got := observed[len(observed)-1].RequestID
		if len(got) != 16 {
			t.Errorf("expect the gateway's own 16-hex id, got %q", got)
		}
	})
}

// backendFacts is what a service collects about the backend exchange of one
// inbound request, carried in the request context and filled in by a
// middleware on the backend client — the pattern documented in
// docs/building-a-service.md, checked here against the real SDK client.
type backendFacts struct {
	mu         sync.Mutex
	requestIDs []string
}

type backendFactsKey struct{}

func TestBackendResponseFactsThroughContext(t *testing.T) {
	var calls int
	backendTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("x-amz-request-id", "backend-"+strings.Repeat("x", calls))
		if calls == 1 {
			// the SDK retries a 503 inside one inbound request
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("ETag", `"e"`)
		io.WriteString(w, "hello")
	}))
	t.Cleanup(backendTS.Close)

	m := buildStore(t, "factstenant",
		[]userSpec{{name: "u", keyID: testAccessKeyID, secret: testSecretAccessKey}},
		[]bucketSpec{{name: "factsbucket", endpoint: backendTS.URL}})
	gw := s3gw.New(m)
	gw.SetClientOptions(func(*store.Backend) []func(*s3.Options) {
		return []func(*s3.Options){func(o *s3.Options) {
			o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
				return stack.Deserialize.Add(middleware.DeserializeMiddlewareFunc("RecordBackendResponse",
					func(ctx context.Context, in middleware.DeserializeInput, next middleware.DeserializeHandler) (
						middleware.DeserializeOutput, middleware.Metadata, error,
					) {
						out, md, err := next.HandleDeserialize(ctx, in)
						if resp, ok := out.RawResponse.(*smithyhttp.Response); ok {
							if f, ok := ctx.Value(backendFactsKey{}).(*backendFacts); ok {
								f.mu.Lock()
								f.requestIDs = append(f.requestIDs, resp.Header.Get("x-amz-request-id"))
								f.mu.Unlock()
							}
						}
						return out, md, err
					}), middleware.After)
			})
		}}
	})
	var observed *backendFacts
	gw.SetObserver(func(ctx context.Context, info *s3gw.RequestInfo) {
		observed, _ = ctx.Value(backendFactsKey{}).(*backendFacts)
	})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(context.WithValue(r.Context(), backendFactsKey{}, &backendFacts{}))
		gw.Handler().ServeHTTP(w, r)
	})
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	cfg, err := awsconfig.LoadDefaultConfig(t.Context(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(testAccessKeyID, testSecretAccessKey, "")),
	)
	if err != nil {
		t.Fatal(err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(ts.URL)
		o.UsePathStyle = true
	})
	out, err := client.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("factsbucket"), Key: aws.String("a.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	out.Body.Close()
	if observed == nil {
		t.Fatal("observer did not find the record in the context")
	}
	// both attempts, in order: the retried 503 and the success
	want := []string{"backend-x", "backend-xx"}
	if strings.Join(observed.requestIDs, ",") != strings.Join(want, ",") {
		t.Errorf("backend request ids %v, want %v", observed.requestIDs, want)
	}
}
