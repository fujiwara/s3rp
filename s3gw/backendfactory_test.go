package s3gw_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/store"
)

func TestBackendFactory(t *testing.T) {
	stub := &stubBackend{headOut: &s3.HeadObjectOutput{ETag: aws.String(`"e"`)}}
	gw := newTestGateway(t)
	var asked []string
	gw.SetBackendFactory(func(_ context.Context, b *store.Backend) (s3gw.BackendClient, error) {
		asked = append(asked, b.Endpoint)
		return stub, nil
	})
	client, _, _ := newSDKClientFor(t, gw)
	for range 2 {
		if _, err := client.HeadObject(t.Context(), &s3.HeadObjectInput{
			Bucket: aws.String("testbucket"), Key: aws.String("k"),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if stub.headIn == nil || aws.ToString(stub.headIn.Bucket) != "backend-testbucket" {
		t.Errorf("the factory's client was not the one called: %+v", stub.headIn)
	}
	// its client is cached by backend identity like any other
	if len(asked) != 1 || asked[0] != "http://backend.invalid" {
		t.Errorf("factory calls = %v, want one for the bucket's backend", asked)
	}
}

// A nil client leaves the backend to the gateway's own SDK client, so one
// service can serve some backends itself and reach the others over S3.
func TestBackendFactoryDeclines(t *testing.T) {
	gw := newTestGateway(t)
	stub := &stubBackend{}
	gw.SetBackendFactory(func(_ context.Context, b *store.Backend) (s3gw.BackendClient, error) {
		if strings.HasPrefix(b.Endpoint, "http://backend00") {
			return stub, nil
		}
		return nil, nil
	})
	mine, err := gw.BackendClientFor(t.Context(), lruTestBackend(0))
	if err != nil {
		t.Fatal(err)
	}
	if mine != s3gw.BackendClient(stub) {
		t.Errorf("backend 0 got %T, want the factory's client", mine)
	}
	other, err := gw.BackendClientFor(t.Context(), lruTestBackend(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := other.(*s3.Client); !ok {
		t.Errorf("a declined backend got %T, want the SDK client", other)
	}
}

func TestBackendFactoryError(t *testing.T) {
	gw := newTestGateway(t)
	boom := errors.New("no such backend")
	gw.SetBackendFactory(func(context.Context, *store.Backend) (s3gw.BackendClient, error) {
		return nil, boom
	})
	if _, err := gw.BackendClientFor(t.Context(), lruTestBackend(0)); !errors.Is(err, boom) {
		t.Errorf("expect the factory's error, got %v", err)
	}
	if n := gw.ClientCacheLen(); n != 0 {
		t.Errorf("a failed build was cached (%d entries)", n)
	}
}

// streamChecksumStub is a stub that answers for itself whether it can
// checksum a body it reads once.
type streamChecksumStub struct {
	*stubBackend
	can bool
}

func (s streamChecksumStub) ChecksumsStream() bool { return s.can }

// The algorithm of a trailing checksum is forwarded where the backend
// client can compute it over the stream. The SDK client can over https
// only, which is assumed from the endpoint; another client says so itself,
// whatever its definition's endpoint looks like.
func TestStreamChecksummerDecidesTrailerForwarding(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		client   func(*stubBackend) s3gw.BackendClient
		want     types.ChecksumAlgorithm
	}{
		{"sdk-like over http", "http://backend.invalid", func(s *stubBackend) s3gw.BackendClient { return s }, ""},
		{"sdk-like over https", "https://backend.invalid", func(s *stubBackend) s3gw.BackendClient { return s }, types.ChecksumAlgorithmCrc32},
		{"capable over http", "http://backend.invalid", func(s *stubBackend) s3gw.BackendClient { return streamChecksumStub{s, true} }, types.ChecksumAlgorithmCrc32},
		{"incapable over https", "https://backend.invalid", func(s *stubBackend) s3gw.BackendClient { return streamChecksumStub{s, false} }, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubBackend{putOut: &s3.PutObjectOutput{ETag: aws.String(`"e"`)}}
			m := buildStore(t, "t",
				[]userSpec{{name: "u", keyID: testAccessKeyID, secret: testSecretAccessKey}},
				[]bucketSpec{{name: "b", endpoint: tt.endpoint}})
			gw := s3gw.New(m)
			gw.SetBackendFactory(func(context.Context, *store.Backend) (s3gw.BackendClient, error) {
				return tt.client(stub), nil
			})
			// over TLS the SDK sends its checksum as a trailer
			ts := httptest.NewTLSServer(gw.Handler())
			t.Cleanup(ts.Close)
			cfg, err := awsconfig.LoadDefaultConfig(t.Context(),
				awsconfig.WithRegion("us-east-1"),
				awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(testAccessKeyID, testSecretAccessKey, "")),
				awsconfig.WithHTTPClient(ts.Client()),
			)
			if err != nil {
				t.Fatal(err)
			}
			client := s3.NewFromConfig(cfg, func(o *s3.Options) {
				o.BaseEndpoint = aws.String(ts.URL)
				o.UsePathStyle = true
			})
			if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
				Bucket: aws.String("b"), Key: aws.String("k"), Body: strings.NewReader("trailer"),
			}); err != nil {
				t.Fatal(err)
			}
			if got := stub.putIn.ChecksumAlgorithm; got != tt.want {
				t.Errorf("ChecksumAlgorithm forwarded = %q, want %q", got, tt.want)
			}
		})
	}
}
