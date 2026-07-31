package s3rp

import (
	"context"
	"errors"
	"fmt"
	"github.com/fujiwara/s3rp/store"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// S3RP is an S3 API reverse proxy that verifies SigV4 signatures with
// front-side access keys and forwards operations to per-bucket backends.
type S3RP struct {
	cfg    *Config
	store  store.Store
	signer *v4.Signer
	now    func() time.Time

	newClient func(ctx context.Context, b *BackendConfig) (BackendClient, error)
	clients   map[clientCacheKey]BackendClient
	clientsMu sync.RWMutex
}

// bucketRT is a bucket resolved for a request: its definition and the
// backend client to use.
type bucketRT struct {
	cfg    *store.Bucket
	client BackendClient
}

// clientCacheKey identifies a backend client. Clients are bucket-agnostic:
// buckets sharing the same backend endpoint and credentials share a client.
type clientCacheKey struct {
	endpoint     string
	region       string
	accessKeyID  string
	secret       Password
	usePathStyle bool
}

func newClientCacheKey(b *BackendConfig) clientCacheKey {
	return clientCacheKey{
		endpoint:     b.Endpoint,
		region:       b.Region,
		accessKeyID:  b.AccessKeyID,
		secret:       b.SecretAccessKey,
		usePathStyle: b.UsePathStyle != nil && *b.UsePathStyle,
	}
}

// New creates an S3RP from a config.
func New(ctx context.Context, cfg *Config) (*S3RP, error) {
	return NewWithStore(ctx, cfg, NewConfigStore(cfg))
}

// NewWithStore creates an S3RP using the given Store for tenant, key and
// bucket definitions.
func NewWithStore(_ context.Context, cfg *Config, store store.Store) (*S3RP, error) {
	return &S3RP{
		cfg:   cfg,
		store: store,
		signer: v4.NewSigner(func(o *v4.SignerOptions) {
			o.DisableURIPathEscaping = true // S3 mode
		}),
		now:       time.Now,
		newClient: newBackendClient,
		clients:   make(map[clientCacheKey]BackendClient),
	}, nil
}

// backendClient returns the backend client for a backend definition,
// constructing and caching it on first use.
func (app *S3RP) backendClient(ctx context.Context, b *BackendConfig) (BackendClient, error) {
	key := newClientCacheKey(b)
	app.clientsMu.RLock()
	client, ok := app.clients[key]
	app.clientsMu.RUnlock()
	if ok {
		return client, nil
	}
	client, err := app.newClient(ctx, b)
	if err != nil {
		return nil, fmt.Errorf("failed to build backend client: %w", err)
	}
	app.clientsMu.Lock()
	defer app.clientsMu.Unlock()
	if cached, ok := app.clients[key]; ok {
		return cached, nil
	}
	app.clients[key] = client
	return client, nil
}

// Run parses the command line, loads the config and serves until ctx is done.
func Run(ctx context.Context) error {
	cli, err := parseCLI()
	if err != nil {
		return err
	}
	setupLogger(cli.LogLevel)
	cfg, err := LoadConfig(cli.Config)
	if err != nil {
		return err
	}
	if cli.Listen != "" {
		cfg.Listen = cli.Listen
	}
	app, err := New(ctx, cfg)
	if err != nil {
		return err
	}
	return app.Serve(ctx)
}

func setupLogger(level string) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l})
	slog.SetDefault(slog.New(h))
}

// Serve runs the HTTP server until ctx is done, then shuts down gracefully.
func (app *S3RP) Serve(ctx context.Context) error {
	srv := &http.Server{
		Addr:    app.cfg.Listen,
		Handler: app.Handler(),
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	slog.InfoContext(ctx, "s3rp starting", "version", Version, "listen", app.cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	slog.InfoContext(ctx, "s3rp shutdown")
	return nil
}
