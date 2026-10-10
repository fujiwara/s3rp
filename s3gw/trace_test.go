package s3gw_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/tracing/smithyoteltracing"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/store"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// TestClientTraceThroughGateway runs the tracing setup documented in
// docs/building-a-service.md: a client whose instrumented transport adds a
// traceparent after signing, an otelhttp handler around the gateway, the
// request id derived from the server span, and an instrumented backend client.
func TestClientTraceThroughGateway(t *testing.T) {
	for _, tc := range []struct {
		name string
		// publicEndpoint is the setting for a client-facing endpoint; without
		// it the client's trace becomes the parent, which is right only behind
		// a hop that overwrites traceparent.
		publicEndpoint bool
	}{
		{name: "public endpoint links", publicEndpoint: true},
		{name: "trusted parent", publicEndpoint: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prop := propagation.TraceContext{}

			var mu sync.Mutex
			var backendTraceparent string
			backendTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				backendTraceparent = r.Header.Get("traceparent")
				mu.Unlock()
				w.Header().Set("ETag", `"e"`)
				io.WriteString(w, "hello")
			}))
			t.Cleanup(backendTS.Close)

			serverSpans := tracetest.NewSpanRecorder()
			serverTP := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(serverSpans))
			t.Cleanup(func() { serverTP.Shutdown(context.Background()) })

			m := buildStore(t, "tracetenant",
				[]userSpec{{name: "u", keyID: testAccessKeyID, secret: testSecretAccessKey}},
				[]bucketSpec{{name: "tracebucket", endpoint: backendTS.URL}})
			gw := s3gw.New(m)
			gw.SetClientOptions(func(*store.Backend) []func(*s3.Options) {
				return []func(*s3.Options){func(o *s3.Options) {
					o.TracerProvider = smithyoteltracing.Adapt(serverTP)
					o.HTTPClient = &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport,
						otelhttp.WithTracerProvider(serverTP), otelhttp.WithPropagators(prop))}
				}}
			})
			gw.SetRequestID(func(r *http.Request) string {
				if sc := trace.SpanContextFromContext(r.Context()); sc.HasTraceID() {
					return sc.TraceID().String()
				}
				return ""
			})
			var requestID string
			gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) {
				requestID = info.RequestID
			})
			opts := []otelhttp.Option{otelhttp.WithTracerProvider(serverTP), otelhttp.WithPropagators(prop)}
			if tc.publicEndpoint {
				opts = append(opts, otelhttp.WithPublicEndpointFn(func(*http.Request) bool { return true }))
			}
			ts := httptest.NewServer(otelhttp.NewHandler(gw.Handler(), "s3", opts...))

			clientSpans := tracetest.NewSpanRecorder()
			clientTP := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(clientSpans))
			t.Cleanup(func() { clientTP.Shutdown(context.Background()) })
			cfg, err := awsconfig.LoadDefaultConfig(t.Context(),
				awsconfig.WithRegion("us-east-1"),
				awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(testAccessKeyID, testSecretAccessKey, "")),
				awsconfig.WithHTTPClient(&http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport,
					otelhttp.WithTracerProvider(clientTP), otelhttp.WithPropagators(prop))}),
			)
			if err != nil {
				t.Fatal(err)
			}
			client := s3.NewFromConfig(cfg, func(o *s3.Options) {
				o.BaseEndpoint = aws.String(ts.URL)
				o.UsePathStyle = true
			})

			ctx, clientSpan := clientTP.Tracer("client").Start(t.Context(), "client-op")
			out, err := client.GetObject(ctx, &s3.GetObjectInput{
				Bucket: aws.String("tracebucket"), Key: aws.String("a.txt"),
			})
			clientSpan.End()
			if err != nil {
				t.Fatalf("the unsigned traceparent must not break verification: %v", err)
			}
			out.Body.Close()
			ts.Close() // waits for the handler, so the server span has ended
			clientTrace := clientSpan.SpanContext().TraceID()

			var server sdktrace.ReadOnlySpan
			for _, s := range serverSpans.Ended() {
				if s.SpanKind() == trace.SpanKindServer {
					server = s
				}
			}
			if server == nil {
				t.Fatal("no server span recorded")
			}
			serverTrace := server.SpanContext().TraceID()

			if tc.publicEndpoint {
				if serverTrace == clientTrace {
					t.Error("a public endpoint must not adopt the client's trace")
				}
				var linked bool
				for _, l := range server.Links() {
					linked = linked || l.SpanContext.TraceID() == clientTrace
				}
				if !linked {
					t.Errorf("server span lacks a link to the client trace: %v", server.Links())
				}
			} else if serverTrace != clientTrace {
				t.Errorf("server trace %s, want the client's %s", serverTrace, clientTrace)
			}
			if requestID != serverTrace.String() {
				t.Errorf("request id %q, want the server trace id %s", requestID, serverTrace)
			}

			// the backend call joins the server's trace: the SDK operation
			// span and the traceparent sent to the backend
			var sdkSpan bool
			for _, s := range serverSpans.Ended() {
				if s.SpanKind() != trace.SpanKindServer && s.Name() == "S3.GetObject" {
					sdkSpan = s.SpanContext().TraceID() == serverTrace
				}
			}
			if !sdkSpan {
				t.Error("no S3.GetObject backend span in the server trace")
			}
			mu.Lock()
			got := backendTraceparent
			mu.Unlock()
			h := propagation.HeaderCarrier{"Traceparent": {got}}
			if sc := trace.SpanContextFromContext(prop.Extract(context.Background(), h)); sc.TraceID() != serverTrace {
				t.Errorf("backend traceparent %q, want trace %s", got, serverTrace)
			}
		})
	}
}
