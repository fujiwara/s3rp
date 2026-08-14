// Command bwlimit is s3rp with the bandwidth-limit hook installed, for
// benchmarking it. It takes the same flags run.sh passes to the stock
// binary; S3RP_BWLIMIT is the shared per-tenant rate in bytes/sec, 0 or
// unset arms the hook at an infinite rate (overhead only, no throttling).
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"

	app "github.com/fujiwara/s3rp"
	"github.com/fujiwara/s3rp/s3gw"
	"golang.org/x/time/rate"
)

const mib = 1 << 20

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.ErrorContext(ctx, err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	config := flag.String("config", "config.yml", "config file path")
	logLevel := flag.String("log-level", "warn", "log level")
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		level = slog.LevelWarn
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	cfg, err := app.LoadConfig(*config)
	if err != nil {
		return err
	}
	a, err := app.New(ctx, cfg)
	if err != nil {
		return err
	}

	limit := rate.Inf
	if bps, _ := strconv.ParseFloat(os.Getenv("S3RP_BWLIMIT"), 64); bps > 0 {
		limit = rate.Limit(bps)
	}
	var limiters sync.Map // tenant -> *rate.Limiter
	a.SetBandwidthLimit(func(op *s3gw.Op) (in, out s3gw.BandwidthLimiter) {
		l, _ := limiters.LoadOrStore(op.Tenant, rate.NewLimiter(limit, mib)) // burst
		lim := l.(*rate.Limiter)
		return lim, lim // one shared budget per tenant, both directions
	})
	slog.Info("bandwidth limit hook armed", "limit", limit)
	return a.Serve(ctx)
}
