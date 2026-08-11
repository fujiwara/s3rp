// Package harness assembles an s3rp gateway suitable for running the
// ceph/s3-tests compatibility suite against it. s3rp deliberately has no
// CreateBucket/DeleteBucket — bucket provisioning is the control plane's
// job — but the suite creates a bucket for nearly every test, so the
// harness emulates that control plane: a small interceptor in front of the
// gateway handles exactly those two operations, creating real buckets on
// the backend and registering them in a mutable in-memory store the
// gateway reads. Test-only; nothing here changes the proxy's behavior.
package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/sigv4"
	"github.com/fujiwara/s3rp/store"
)

// Fixed credentials, mirrored in s3tests.conf.in. The [s3 main] and
// [s3 alt] sections of s3-tests represent two AWS accounts, mapped here to
// two tenants so cross-account expectations (denials, ownership) line up;
// [s3 tenant] gets a third. user_id/display_name in the conf must be the
// tenant name — that is what the gateway reports as Owner.
const (
	MainAccessKeyID       = "S3RPMAINKEY000000000"
	MainSecretAccessKey   = "s3rpmainsecret0000000000000000000000000k"
	AltAccessKeyID        = "S3RPALTKEY0000000000"
	AltSecretAccessKey    = "s3rpaltsecret00000000000000000000000000k"
	TenantAccessKeyID     = "S3RPTENANTKEY0000000"
	TenantSecretAccessKey = "s3rptenantsecret00000000000000000000000k"
)

func defaultKeys() []*store.Key {
	return []*store.Key{
		{AccessKeyID: MainAccessKeyID, SecretAccessKey: MainSecretAccessKey, Tenant: "main", User: "tester"},
		{AccessKeyID: AltAccessKeyID, SecretAccessKey: AltSecretAccessKey, Tenant: "alt", User: "tester"},
		{AccessKeyID: TenantAccessKeyID, SecretAccessKey: TenantSecretAccessKey, Tenant: "tenanted", User: "tester"},
	}
}

// Config configures the harness.
type Config struct {
	// BackendEndpoint is the S3 backend buckets are created on
	// (e.g. http://127.0.0.1:7480 — use 127.0.0.1, never localhost, for
	// Ceph RGW). Required.
	BackendEndpoint string
	// Backend credentials.
	BackendAccessKeyID     string
	BackendSecretAccessKey string
	// LogWriter receives one JSON line per request (gateway observer
	// records plus the harness's own two operations). Default os.Stderr.
	LogWriter io.Writer
}

// New returns the harness handler: the s3rp gateway with the
// CreateBucket/DeleteBucket interceptor in front. Serve it directly —
// never behind an http.ServeMux, whose path canonicalization breaks S3
// keys and signatures.
func New(ctx context.Context, cfg Config) (http.Handler, error) {
	if cfg.BackendEndpoint == "" {
		return nil, fmt.Errorf("BackendEndpoint is required")
	}
	if cfg.LogWriter == nil {
		cfg.LogWriter = os.Stderr
	}
	awscfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(store.DefaultRegion),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.BackendAccessKeyID, cfg.BackendSecretAccessKey, ""),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load backend client config: %w", err)
	}
	client := s3.NewFromConfig(awscfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.BackendEndpoint)
		o.UsePathStyle = true
	})

	st := newMemStore(backendConfig{
		endpoint:        cfg.BackendEndpoint,
		accessKeyID:     cfg.BackendAccessKeyID,
		secretAccessKey: cfg.BackendSecretAccessKey,
	}, defaultKeys())

	logger := &jsonLogger{enc: json.NewEncoder(cfg.LogWriter)}
	gw := s3gw.New(st)
	gw.SetObserver(func(ctx context.Context, info *s3gw.RequestInfo) {
		logger.log(info)
	})

	return &bucketInterceptor{
		verifier: sigv4.NewVerifier(),
		store:    st,
		backend:  client,
		next:     gw.Handler(),
		logger:   logger,
	}, nil
}

// jsonLogger serializes JSON-line writes from the observer and the
// interceptor onto one stream.
type jsonLogger struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (l *jsonLogger) log(v any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.enc.Encode(v)
}
