package s3rp

import (
	"context"
	"fmt"
	"os"

	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/s3gw/otelmetric"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// MetricsConfig enables the metrics of docs/metrics.md, exported over OTLP.
// Where to and how is the standard OpenTelemetry environment: the
// OTEL_EXPORTER_OTLP_* variables, OTEL_EXPORTER_OTLP_PROTOCOL (grpc or
// http/protobuf, the default), OTEL_METRIC_EXPORT_INTERVAL,
// OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES.
type MetricsConfig struct {
	// Tenant and Bucket add the opt-in s3gw.tenant and aws.s3.bucket
	// attributes: one series per tenant or bucket.
	Tenant bool `yaml:"tenant,omitempty" json:"tenant,omitempty"`
	Bucket bool `yaml:"bucket,omitempty" json:"bucket,omitempty"`
}

// setupMetrics installs the metrics on gw and returns the provider to shut
// down, and the observer that records a request.
func setupMetrics(ctx context.Context, cfg *MetricsConfig, gw *s3gw.Gateway) (*sdkmetric.MeterProvider, s3gw.Observer, error) {
	exp, err := newMetricExporter(ctx)
	if err != nil {
		return nil, nil, err
	}
	// the environment comes last so OTEL_SERVICE_NAME overrides the default
	res, err := resource.New(ctx,
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			attribute.String("service.name", "s3rp"),
			attribute.String("service.version", Version),
		),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, nil, err
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)),
		sdkmetric.WithResource(res),
	)
	var opts []otelmetric.Option
	if cfg.Tenant {
		opts = append(opts, otelmetric.WithTenant())
	}
	if cfg.Bucket {
		opts = append(opts, otelmetric.WithBucket())
	}
	rec, err := otelmetric.New(mp, opts...)
	if err != nil {
		return nil, nil, err
	}
	if _, err := otelmetric.RegisterCacheStats(mp, gw); err != nil {
		return nil, nil, err
	}
	return mp, rec.Observe, nil
}

func newMetricExporter(ctx context.Context) (sdkmetric.Exporter, error) {
	protocol := os.Getenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL")
	if protocol == "" {
		protocol = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	switch protocol {
	case "", "http/protobuf":
		return otlpmetrichttp.New(ctx)
	case "grpc":
		return otlpmetricgrpc.New(ctx)
	}
	return nil, fmt.Errorf("unsupported OTLP protocol %q (grpc or http/protobuf)", protocol)
}
