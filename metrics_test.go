package s3rp_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/fujiwara/s3rp"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

// otlpReceiver collects what an OTLP/HTTP exporter sends.
type otlpReceiver struct {
	mu       sync.Mutex
	names    []string
	services []string
}

func (o *otlpReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil || r.URL.Path != "/v1/metrics" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var req colmetricpb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, rm := range req.ResourceMetrics {
		for _, kv := range rm.Resource.GetAttributes() {
			if kv.Key == "service.name" {
				o.services = append(o.services, kv.Value.GetStringValue())
			}
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				o.names = append(o.names, m.Name)
			}
		}
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	b, _ := proto.Marshal(&colmetricpb.ExportMetricsServiceResponse{})
	w.Write(b)
}

func TestMetricsExport(t *testing.T) {
	recv := &otlpReceiver{}
	collector := httptest.NewServer(recv)
	t.Cleanup(collector.Close)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")

	cfg := loadTestConfig(t)
	cfg.Metrics = &s3rp.MetricsConfig{Tenant: true}
	app, err := s3rp.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(ts.Close)

	// an unsigned request is refused, and still observed
	res, err := http.Get(ts.URL + "/somebucket/key")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	if err := app.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	recv.mu.Lock()
	defer recv.mu.Unlock()
	for _, name := range []string{"s3gw.request.duration", "s3gw.cache.lookups", "s3gw.cache.capacity"} {
		if !slices.Contains(recv.names, name) {
			t.Errorf("%s not exported; got %v", name, recv.names)
		}
	}
	if !slices.Contains(recv.services, "s3rp") {
		t.Errorf("service.name = %v, want s3rp", recv.services)
	}
}

func TestMetricsUnsupportedProtocol(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/json")
	cfg := loadTestConfig(t)
	cfg.Metrics = &s3rp.MetricsConfig{}
	if _, err := s3rp.New(t.Context(), cfg); err == nil {
		t.Error("expect an error for an unsupported protocol")
	}
}

func TestShutdownWithoutMetrics(t *testing.T) {
	app, err := s3rp.New(t.Context(), loadTestConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Shutdown(t.Context()); err != nil {
		t.Error(err)
	}
}
