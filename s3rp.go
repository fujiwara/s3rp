package s3rp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/store"
	"github.com/fujiwara/s3rp/store/rdb"
)

// S3RP is the service assembled from a config: it decides where definitions
// come from and runs the HTTP server, while the S3 API itself is the Gateway.
type S3RP struct {
	*s3gw.Gateway
	cfg   *Config
	store store.Store
}

// New creates an S3RP from a config, selecting the definition store by the
// store.driver setting.
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
func NewWithStore(_ context.Context, cfg *Config, st store.Store) (*S3RP, error) {
	return &S3RP{Gateway: s3gw.New(st), cfg: cfg, store: st}, nil
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
