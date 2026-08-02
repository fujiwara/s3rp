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
)

// S3RP is the service assembled from a config: it decides where definitions
// come from and runs the HTTP server, while the S3 API itself is the Gateway.
type S3RP struct {
	*s3gw.Gateway
	cfg   *Config
	store store.Store
}

// New creates an S3RP from a config, serving the definitions it declares.
func New(ctx context.Context, cfg *Config) (*S3RP, error) {
	return NewWithStore(ctx, cfg, NewConfigStore(cfg))
}

// NewWithStore creates an S3RP using the given Store for tenant, key and
// bucket definitions.
func NewWithStore(_ context.Context, cfg *Config, st store.Store) (*S3RP, error) {
	gw := s3gw.New(st)
	gw.SetObserver(logRequest)
	return &S3RP{Gateway: gw, cfg: cfg, store: st}, nil
}

// logRequest writes the access log, and the reason whenever a request failed.
// The gateway reports rather than logs, so the format and the level are
// decided here: a server-side fault is an error, a refusal the client caused
// is routine but still worth being able to explain afterwards.
func logRequest(ctx context.Context, info *s3gw.RequestInfo) {
	if info.Code != "" {
		level := slog.LevelInfo
		if info.Status >= http.StatusInternalServerError {
			level = slog.LevelError
		}
		attrs := []any{"code", info.Code, "status", info.Status, "request_id", info.RequestID}
		if info.Err != nil {
			attrs = append(attrs, "error", info.Err)
		}
		slog.Log(ctx, level, "request failed", attrs...)
	}
	attrs := []any{
		"remote", info.RemoteAddr,
		"method", info.Method,
		"path", info.Path,
		"query", info.RawQuery,
		"status", info.Status,
		"code", info.Code,
		"bytes_in", info.BytesIn,
		"bytes_out", info.BytesOut,
		"duration", info.Duration.String(),
		"request_id", info.RequestID,
	}
	// who asked is known as soon as the signature verifies, so it is logged
	// even for a request that is then refused; what they asked for is only
	// known once the request reached an operation
	if info.Tenant != "" {
		attrs = append(attrs, "tenant", info.Tenant, "user", info.User)
	}
	if info.Op != nil {
		attrs = append(attrs, "action", info.Op.Action, "bucket", info.Op.Bucket)
	}
	slog.InfoContext(ctx, "request", attrs...)
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
		// a store may hold external handles (e.g. a database connection)
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
