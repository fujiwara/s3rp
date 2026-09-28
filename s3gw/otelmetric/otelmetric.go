// Package otelmetric records the s3gw metrics convention (docs/metrics.md,
// generated from the registry in s3gw/semconv) with the OpenTelemetry
// metrics API. It depends on the API only: the SDK, the exporter and the
// meter provider's configuration are the service's to choose.
package otelmetric

import (
	"context"
	"net/http"

	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/s3gw/semconv"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// ScopeName is the instrumentation scope the instruments are created under.
const ScopeName = "github.com/fujiwara/s3rp/s3gw/otelmetric"

// DurationBuckets are the explicit bucket boundaries of
// s3gw.request.duration, in seconds: OpenTelemetry's HTTP server buckets
// extended past 10s, since an object transfer is bounded by its size, not by
// a request timeout.
var DurationBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75,
	1, 2.5, 5, 7.5, 10, 30, 60, 300,
}

// errorTypeOther is error.type for a failure the client was given no code
// for: one discovered after the response had started.
const errorTypeOther = "_OTHER"

// Recorder turns the RequestInfo the gateway reports into the per-request
// metrics. Its Observe method has the shape of s3gw.Observer.
type Recorder struct {
	duration metric.Float64Histogram
	io       metric.Int64Counter
	tenant   bool
	bucket   bool
}

// Option configures a Recorder.
type Option func(*Recorder)

// WithTenant adds the opt-in s3gw.tenant attribute: one series per tenant.
func WithTenant() Option { return func(r *Recorder) { r.tenant = true } }

// WithBucket adds the opt-in aws.s3.bucket attribute: one series per bucket.
func WithBucket() Option { return func(r *Recorder) { r.bucket = true } }

// New creates the per-request instruments from mp.
func New(mp metric.MeterProvider, opts ...Option) (*Recorder, error) {
	m := mp.Meter(ScopeName)
	r := &Recorder{}
	for _, o := range opts {
		o(r)
	}
	var err error
	r.duration, err = m.Float64Histogram(semconv.MetricS3GWRequestDuration,
		metric.WithUnit(semconv.MetricS3GWRequestDurationUnit),
		metric.WithDescription(semconv.MetricS3GWRequestDurationDescription),
		metric.WithExplicitBucketBoundaries(DurationBuckets...))
	if err != nil {
		return nil, err
	}
	r.io, err = m.Int64Counter(semconv.MetricS3GWIO,
		metric.WithUnit(semconv.MetricS3GWIOUnit),
		metric.WithDescription(semconv.MetricS3GWIODescription))
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Observe records one request. Install it with Gateway.SetObserver, or call
// it from the service's own observer next to its logging.
func (r *Recorder) Observe(ctx context.Context, info *s3gw.RequestInfo) {
	attrs := r.attributes(info)
	r.duration.Record(ctx, info.Duration.Seconds(), metric.WithAttributes(attrs...))
	// the direction is appended last so both adds share the rest
	if info.BytesIn > 0 {
		r.io.Add(ctx, info.BytesIn, metric.WithAttributes(append(attrs,
			attribute.String(semconv.AttrNetworkIODirection, semconv.NetworkIODirectionReceive))...))
	}
	if info.BytesOut > 0 {
		r.io.Add(ctx, info.BytesOut, metric.WithAttributes(append(attrs,
			attribute.String(semconv.AttrNetworkIODirection, semconv.NetworkIODirectionTransmit))...))
	}
}

func (r *Recorder) attributes(info *s3gw.RequestInfo) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 7)
	attrs = append(attrs,
		attribute.String(semconv.AttrHTTPRequestMethod, method(info.Method)),
		attribute.Int(semconv.AttrHTTPResponseStatusCode, info.Status))
	if info.Op != nil {
		attrs = append(attrs, attribute.String(semconv.AttrS3GWOperation, info.Op.Operation))
	}
	switch {
	case info.Code != "":
		attrs = append(attrs, attribute.String(semconv.AttrErrorType, info.Code))
	case info.Err != nil:
		attrs = append(attrs, attribute.String(semconv.AttrErrorType, errorTypeOther))
	}
	if r.tenant && info.Tenant != "" {
		attrs = append(attrs, attribute.String(semconv.AttrS3GWTenant, info.Tenant))
	}
	if r.bucket && info.Op != nil && info.Op.Bucket != "" {
		attrs = append(attrs, attribute.String(semconv.AttrAWSS3Bucket, info.Op.Bucket))
	}
	return attrs
}

// method maps a method to http.request.method: an unknown one is _OTHER,
// since the method is client-chosen and would otherwise be unbounded.
func method(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPost,
		http.MethodDelete, http.MethodOptions, http.MethodPatch,
		http.MethodConnect, http.MethodTrace:
		return m
	}
	return "_OTHER"
}

// CacheStatsSource is what RegisterCacheStats reads; *s3gw.Gateway
// implements it.
type CacheStatsSource interface {
	SignerCacheStats() s3gw.CacheStats
	ClientCacheStats() s3gw.CacheStats
}

// RegisterCacheStats registers the s3gw.cache.* instruments, observed from
// src on every collection. Unregister the returned registration when src
// goes away.
func RegisterCacheStats(mp metric.MeterProvider, src CacheStatsSource) (metric.Registration, error) {
	m := mp.Meter(ScopeName)
	lookups, err := m.Int64ObservableCounter(semconv.MetricS3GWCacheLookups,
		metric.WithUnit(semconv.MetricS3GWCacheLookupsUnit),
		metric.WithDescription(semconv.MetricS3GWCacheLookupsDescription))
	if err != nil {
		return nil, err
	}
	evictions, err := m.Int64ObservableCounter(semconv.MetricS3GWCacheEvictions,
		metric.WithUnit(semconv.MetricS3GWCacheEvictionsUnit),
		metric.WithDescription(semconv.MetricS3GWCacheEvictionsDescription))
	if err != nil {
		return nil, err
	}
	entries, err := m.Int64ObservableUpDownCounter(semconv.MetricS3GWCacheEntries,
		metric.WithUnit(semconv.MetricS3GWCacheEntriesUnit),
		metric.WithDescription(semconv.MetricS3GWCacheEntriesDescription))
	if err != nil {
		return nil, err
	}
	capacity, err := m.Int64ObservableUpDownCounter(semconv.MetricS3GWCacheCapacity,
		metric.WithUnit(semconv.MetricS3GWCacheCapacityUnit),
		metric.WithDescription(semconv.MetricS3GWCacheCapacityDescription))
	if err != nil {
		return nil, err
	}
	hit := attribute.String(semconv.AttrS3GWCacheResult, semconv.S3GWCacheResultHit)
	miss := attribute.String(semconv.AttrS3GWCacheResult, semconv.S3GWCacheResultMiss)
	return m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		for _, c := range []struct {
			name  string
			stats s3gw.CacheStats
		}{
			{semconv.S3GWCacheNameSigner, src.SignerCacheStats()},
			{semconv.S3GWCacheNameClient, src.ClientCacheStats()},
		} {
			name := attribute.String(semconv.AttrS3GWCacheName, c.name)
			o.ObserveInt64(lookups, int64(c.stats.Hits), metric.WithAttributes(name, hit))
			o.ObserveInt64(lookups, int64(c.stats.Misses), metric.WithAttributes(name, miss))
			o.ObserveInt64(evictions, int64(c.stats.Evictions), metric.WithAttributes(name))
			o.ObserveInt64(entries, int64(c.stats.Len), metric.WithAttributes(name))
			o.ObserveInt64(capacity, int64(c.stats.Capacity), metric.WithAttributes(name))
		}
		return nil
	}, lookups, evictions, entries, capacity)
}
