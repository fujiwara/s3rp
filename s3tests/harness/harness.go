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
	"github.com/fujiwara/s3rp/s3gw/localbackend"
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
	// InProcess serves every bucket from an in-memory backend
	// (s3gw/localbackend) instead of an S3 backend: nothing to start, nothing
	// kept. The backend endpoint and credentials are then unused.
	InProcess bool
	// InProcessDir, with InProcess, keeps the in-process backend's state in
	// this directory (localbackend.Open) instead of in memory.
	InProcessDir string
	// BackendEndpoint is the S3 backend buckets are created on
	// (e.g. http://127.0.0.1:7480 — use 127.0.0.1, never localhost, for
	// Ceph RGW). Required unless InProcess.
	BackendEndpoint string
	// Backend credentials.
	BackendAccessKeyID     string
	BackendSecretAccessKey string
	// LogWriter receives one JSON line per request (gateway observer
	// records plus the harness's own two operations). Default os.Stderr.
	LogWriter io.Writer
}

// Handler is the harness: an http.Handler to serve, and a Close to call
// once no request is being served any more.
type Handler interface {
	http.Handler
	// Close writes what an in-process backend kept in a directory has not
	// written yet. It does nothing otherwise.
	Close() error
}

// New returns the harness handler: the s3rp gateway with the
// CreateBucket/DeleteBucket interceptor in front. Serve it directly —
// never behind an http.ServeMux, whose path canonicalization breaks S3
// keys and signatures.
func New(ctx context.Context, cfg Config) (Handler, error) {
	if cfg.LogWriter == nil {
		cfg.LogWriter = os.Stderr
	}
	var backend backendBuckets
	var inProcess *localbackend.Backend
	if cfg.InProcess {
		inProcess = localbackend.New()
		if cfg.InProcessDir != "" {
			var err error
			if inProcess, err = localbackend.Open(cfg.InProcessDir); err != nil {
				return nil, fmt.Errorf("failed to open %s: %w", cfg.InProcessDir, err)
			}
		}
		backend = inProcess
		// never dialed: the definitions only have to name one backend
		cfg.BackendEndpoint = "http://localbackend.invalid"
		cfg.BackendAccessKeyID, cfg.BackendSecretAccessKey = "localbackend", "localbackend"
	} else {
		if cfg.BackendEndpoint == "" {
			return nil, fmt.Errorf("BackendEndpoint is required")
		}
		if cfg.BackendAccessKeyID == "" || cfg.BackendSecretAccessKey == "" {
			return nil, fmt.Errorf("backend credentials are required")
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
		backend = s3.NewFromConfig(awscfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.BackendEndpoint)
			o.UsePathStyle = true
		})
	}

	st := newMemStore(backendConfig{
		endpoint:        cfg.BackendEndpoint,
		accessKeyID:     cfg.BackendAccessKeyID,
		secretAccessKey: cfg.BackendSecretAccessKey,
	}, defaultKeys())

	logger := &jsonLogger{enc: json.NewEncoder(cfg.LogWriter)}
	gw := s3gw.New(st)
	if inProcess != nil {
		gw.SetBackendFactory(func(context.Context, *store.Backend) (s3gw.BackendClient, error) {
			return inProcess, nil
		})
	}
	gw.SetObserver(func(ctx context.Context, info *s3gw.RequestInfo) {
		logger.log(info)
	})

	return &bucketInterceptor{
		verifier:  sigv4.NewVerifier(),
		store:     st,
		backend:   backend,
		next:      gw.Handler(),
		logger:    logger,
		inProcess: inProcess,
	}, nil
}

func (i *bucketInterceptor) Close() error {
	if i.inProcess == nil {
		return nil
	}
	return i.inProcess.Close()
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
