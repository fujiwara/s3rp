package s3rp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/fujiwara/s3rp/sigv4"
	"github.com/fujiwara/s3rp/store"
	"github.com/fujiwara/s3rp/store/rdb"
)

// S3RP is an S3 API reverse proxy that verifies SigV4 signatures with
// front-side access keys and forwards operations to per-bucket backends.
type S3RP struct {
	cfg      *Config
	store    store.Store
	verifier *sigv4.Verifier

	authorizer   Authorizer
	interceptors []Interceptor

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

// New creates an S3RP from a config, selecting the definition store by
// the store.driver setting.
func New(ctx context.Context, cfg *Config) (*S3RP, error) {
	switch cfg.StoreDriver() {
	case "sqlite":
		st, err := rdb.Open(cfg.Store.DSN)
		if err != nil {
			return nil, err
		}
		return NewWithStore(ctx, cfg, st)
	default:
		return NewWithStore(ctx, cfg, NewConfigStore(cfg))
	}
}

// NewWithStore creates an S3RP using the given Store for tenant, key and
// bucket definitions.
func NewWithStore(_ context.Context, cfg *Config, store store.Store) (*S3RP, error) {
	return &S3RP{
		cfg:       cfg,
		store:     store,
		verifier:  sigv4.NewVerifier(),
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
	defer func() {
		// e.g. the sqlite-backed store holds a database handle
		if c, ok := app.store.(io.Closer); ok {
			c.Close()
		}
	}()
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
		// Bound slow-client attacks (slowloris) without capping transfer time:
		// ReadTimeout/WriteTimeout are intentionally left unset so large object
		// uploads and downloads are not cut off mid-stream. ReadHeaderTimeout
		// bounds how long a client may take to send request headers, and
		// IdleTimeout reaps idle keep-alive connections.
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
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
