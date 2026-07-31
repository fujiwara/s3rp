package s3rp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// S3RP is an S3 API reverse proxy that verifies SigV4 signatures with
// front-side access keys and forwards operations to per-bucket backends.
type S3RP struct {
	cfg     *Config
	keys    map[string]Password             // front access key id -> secret
	byKey   map[string]map[string]*bucketRT // front access key id -> bucket name -> runtime
	buckets map[string]*bucketRT            // bucket name -> runtime
	signer  *v4.Signer
	now     func() time.Time
}

type bucketRT struct {
	cfg    *BucketConfig
	client BackendClient
}

// New creates an S3RP from a config.
func New(ctx context.Context, cfg *Config) (*S3RP, error) {
	app := &S3RP{
		cfg:     cfg,
		keys:    make(map[string]Password),
		byKey:   make(map[string]map[string]*bucketRT),
		buckets: make(map[string]*bucketRT),
		signer: v4.NewSigner(func(o *v4.SignerOptions) {
			o.DisableURIPathEscaping = true // S3 mode
		}),
		now: time.Now,
	}
	for _, b := range cfg.Buckets {
		client, err := newBackendClient(ctx, b.Backend)
		if err != nil {
			return nil, fmt.Errorf("bucket %s: %w", b.Name, err)
		}
		rt := &bucketRT{cfg: b, client: client}
		app.buckets[b.Name] = rt
		for _, k := range b.Keys {
			app.keys[k.AccessKeyID] = k.SecretAccessKey
			if app.byKey[k.AccessKeyID] == nil {
				app.byKey[k.AccessKeyID] = make(map[string]*bucketRT)
			}
			app.byKey[k.AccessKeyID][b.Name] = rt
		}
	}
	return app, nil
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
