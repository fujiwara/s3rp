package otelmetric_test

import (
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/s3gw/otelmetric"
	"github.com/goccy/go-yaml"
	"github.com/google/go-cmp/cmp"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func collect(t *testing.T, r *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != otelmetric.ScopeName {
			t.Errorf("scope = %q", sm.Scope.Name)
		}
		for _, m := range sm.Metrics {
			got[m.Name] = m
		}
	}
	return got
}

// attrs renders an attribute set as a map for comparison.
func attrs(s attribute.Set) map[string]any {
	m := map[string]any{}
	for _, kv := range s.ToSlice() {
		m[string(kv.Key)] = kv.Value.AsInterface()
	}
	return m
}

func TestObserve(t *testing.T) {
	tests := []struct {
		name     string
		opts     []otelmetric.Option
		info     s3gw.RequestInfo
		want     map[string]any
		wantIn   int64
		wantOut  int64
		wantNoIO bool
	}{
		{
			name: "success",
			info: s3gw.RequestInfo{
				Method: "GET", Status: 200, Tenant: "acme", BytesOut: 5,
				Op: &s3gw.Op{Operation: "GetObject", Bucket: "photos", Key: "a.txt"},
			},
			want: map[string]any{
				"http.request.method":       "GET",
				"http.response.status_code": int64(200),
				"s3gw.operation":            "GetObject",
			},
			wantOut: 5,
		},
		{
			name: "opt-in tenant and bucket",
			opts: []otelmetric.Option{otelmetric.WithTenant(), otelmetric.WithBucket()},
			info: s3gw.RequestInfo{
				Method: "PUT", Status: 200, Tenant: "acme", BytesIn: 7,
				Op: &s3gw.Op{Operation: "PutObject", Bucket: "photos", Key: "a.txt"},
			},
			want: map[string]any{
				"http.request.method":       "PUT",
				"http.response.status_code": int64(200),
				"s3gw.operation":            "PutObject",
				"s3gw.tenant":               "acme",
				"aws.s3.bucket":             "photos",
			},
			wantIn: 7,
		},
		{
			// refused before routing: no Op, so no operation; the S3 code is
			// the error type
			name: "refused before an operation",
			opts: []otelmetric.Option{otelmetric.WithTenant(), otelmetric.WithBucket()},
			info: s3gw.RequestInfo{Method: "GET", Status: 403, Code: "AccessDenied", BytesOut: 200},
			want: map[string]any{
				"http.request.method":       "GET",
				"http.response.status_code": int64(403),
				"error.type":                "AccessDenied",
			},
			wantOut: 200,
		},
		{
			// the response had started: no code to report, only a cause
			name: "failure after the response started",
			info: s3gw.RequestInfo{
				Method: "GET", Status: 200, Err: errors.New("backend reset"), BytesOut: 3,
				Op: &s3gw.Op{Operation: "GetObject"},
			},
			want: map[string]any{
				"http.request.method":       "GET",
				"http.response.status_code": int64(200),
				"s3gw.operation":            "GetObject",
				"error.type":                "_OTHER",
			},
			wantOut: 3,
		},
		{
			name: "unknown method",
			info: s3gw.RequestInfo{Method: "PROPFIND", Status: 501, Code: "NotImplemented"},
			want: map[string]any{
				"http.request.method":       "_OTHER",
				"http.response.status_code": int64(501),
				"error.type":                "NotImplemented",
			},
			wantNoIO: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			rec, err := otelmetric.New(mp, tt.opts...)
			if err != nil {
				t.Fatal(err)
			}
			tt.info.Duration = 1500 * time.Millisecond
			rec.Observe(t.Context(), &tt.info)
			got := collect(t, reader)

			dur, ok := got["s3gw.request.duration"]
			if !ok {
				t.Fatal("s3gw.request.duration not recorded")
			}
			if dur.Unit != "s" {
				t.Errorf("duration unit = %q", dur.Unit)
			}
			h := dur.Data.(metricdata.Histogram[float64])
			if len(h.DataPoints) != 1 {
				t.Fatalf("duration points = %d", len(h.DataPoints))
			}
			if dp := h.DataPoints[0]; dp.Count != 1 || dp.Sum != 1.5 {
				t.Errorf("duration count/sum = %d/%v", dp.Count, dp.Sum)
			}
			if diff := cmp.Diff(tt.want, attrs(h.DataPoints[0].Attributes)); diff != "" {
				t.Errorf("duration attributes (-want +got):\n%s", diff)
			}

			io, ok := got["s3gw.io"]
			if tt.wantNoIO {
				if ok {
					t.Errorf("s3gw.io recorded for a request without bodies")
				}
				return
			}
			if !ok {
				t.Fatal("s3gw.io not recorded")
			}
			if io.Unit != "By" {
				t.Errorf("io unit = %q", io.Unit)
			}
			byDirection := map[string]int64{}
			for _, dp := range io.Data.(metricdata.Sum[int64]).DataPoints {
				a := attrs(dp.Attributes)
				dir := a["network.io.direction"].(string)
				delete(a, "network.io.direction")
				if diff := cmp.Diff(tt.want, a); diff != "" {
					t.Errorf("io %s attributes (-want +got):\n%s", dir, diff)
				}
				byDirection[dir] = dp.Value
			}
			want := map[string]int64{}
			if tt.wantIn > 0 {
				want["receive"] = tt.wantIn
			}
			if tt.wantOut > 0 {
				want["transmit"] = tt.wantOut
			}
			if diff := cmp.Diff(want, byDirection); diff != "" {
				t.Errorf("io by direction (-want +got):\n%s", diff)
			}
		})
	}
}

type fakeStats struct{ signer, client s3gw.CacheStats }

func (f fakeStats) SignerCacheStats() s3gw.CacheStats { return f.signer }
func (f fakeStats) ClientCacheStats() s3gw.CacheStats { return f.client }

func TestRegisterCacheStats(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	src := fakeStats{
		signer: s3gw.CacheStats{Hits: 10, Misses: 2, Evictions: 1, Len: 3, Capacity: 512},
		client: s3gw.CacheStats{Hits: 20, Misses: 1, Len: 1, Capacity: 128},
	}
	reg, err := otelmetric.RegisterCacheStats(mp, src)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Unregister() })

	got := map[string]map[string]int64{}
	for name, m := range collect(t, reader) {
		var points []metricdata.DataPoint[int64]
		switch d := m.Data.(type) {
		case metricdata.Sum[int64]:
			points = d.DataPoints
		default:
			t.Fatalf("%s: unexpected data %T", name, m.Data)
		}
		got[name] = map[string]int64{}
		for _, dp := range points {
			a := attrs(dp.Attributes)
			label := a["s3gw.cache.name"].(string)
			if r, ok := a["s3gw.cache.result"]; ok {
				label += "/" + r.(string)
			}
			got[name][label] = dp.Value
		}
	}
	want := map[string]map[string]int64{
		"s3gw.cache.lookups": {
			"signer/hit": 10, "signer/miss": 2,
			"client/hit": 20, "client/miss": 1,
		},
		"s3gw.cache.evictions": {"signer": 1, "client": 0},
		"s3gw.cache.entries":   {"signer": 3, "client": 1},
		"s3gw.cache.capacity":  {"signer": 512, "client": 128},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("cache metrics (-want +got):\n%s", diff)
	}
}

// TestRecorderIsAnObserver keeps Observe installable as it stands.
func TestRecorderIsAnObserver(t *testing.T) {
	rec, err := otelmetric.New(sdkmetric.NewMeterProvider())
	if err != nil {
		t.Fatal(err)
	}
	var _ s3gw.Observer = rec.Observe
	var _ otelmetric.CacheStatsSource = (*s3gw.Gateway)(nil)
}

// TestDurationBucketsMatchRegistry keeps the boundaries in step with the
// convention, which states them in the metric's note.
func TestDurationBucketsMatchRegistry(t *testing.T) {
	b, err := os.ReadFile("../semconv/model/metrics.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		Groups []struct {
			ID   string `yaml:"id"`
			Note string `yaml:"note"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(b, &reg); err != nil {
		t.Fatal(err)
	}
	for _, g := range reg.Groups {
		if g.ID != "metric.s3gw.request.duration" {
			continue
		}
		m := regexp.MustCompile("`\\[([^]]*)\\]`").FindStringSubmatch(g.Note)
		if m == nil {
			t.Fatal("no bucket list in the duration note")
		}
		var want []float64
		for f := range strings.FieldsSeq(strings.ReplaceAll(m[1], ",", " ")) {
			v, err := strconv.ParseFloat(f, 64)
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, v)
		}
		if diff := cmp.Diff(want, otelmetric.DurationBuckets); diff != "" {
			t.Errorf("DurationBuckets differ from the registry (-registry +code):\n%s", diff)
		}
		return
	}
	t.Fatal("metric.s3gw.request.duration not in the registry")
}
