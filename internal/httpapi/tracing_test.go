package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/yegamble/vizra-core/internal/config"
	"github.com/yegamble/vizra-core/internal/obs"
)

// otlpReceiver is a fake collector accepting OTLP/HTTP protobuf on /v1/traces.
type otlpReceiver struct {
	mu       sync.Mutex
	requests int
	spans    []*tracepb.Span
}

func (r *otlpReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests++
	if req.Method != http.MethodPost || req.URL.Path != "/v1/traces" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var msg collectortrace.ExportTraceServiceRequest
	if err := proto.Unmarshal(body, &msg); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	for _, rs := range msg.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			r.spans = append(r.spans, ss.Spans...)
		}
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	out, _ := proto.Marshal(&collectortrace.ExportTraceServiceResponse{})
	_, _ = w.Write(out)
}

func tracingConfig(t *testing.T, endpoint string) *config.Config {
	t.Helper()
	env := map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": endpoint, "OTEL_SERVICE_NAME": "vizra-api-test"}
	cfg, err := config.LoadFrom(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

func anyValueString(v *commonpb.AnyValue) string {
	if v == nil {
		return ""
	}
	return v.String()
}

// VZ-OPS-005 "traces exported when enabled": the real config path, the real
// tracer provider and the real server handler; spans reach an OTLP receiver,
// named by route template, with no query string, header, client address or
// user agent in any attribute.
func TestTracesExportedWhenEnabled(t *testing.T) {
	recv := &otlpReceiver{}
	ts := httptest.NewServer(recv)
	defer ts.Close()

	cfg := tracingConfig(t, ts.URL)
	shutdown, err := obs.SetupTracing(context.Background(), cfg.OTLPEndpoint, cfg.OTelServiceName)
	if err != nil {
		t.Fatalf("SetupTracing: %v", err)
	}
	defer otel.SetTracerProvider(otel.GetTracerProvider())

	s := newProbeServer(t, nil)
	h := s.Handler()
	for _, target := range []string{"/healthz?secret=x", "/no-such-route/abc123?secret=x"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("User-Agent", "secret-agent/1.0")
		req.Header.Set("Authorization", "Bearer secretbearertoken")
		req.RemoteAddr = "203.0.113.7:4444"
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown flush: %v", err)
	}

	recv.mu.Lock()
	defer recv.mu.Unlock()
	if len(recv.spans) != 2 {
		t.Fatalf("want exactly 2 spans (one per request), got %d", len(recv.spans))
	}
	names := map[string]bool{}
	for _, sp := range recv.spans {
		names[sp.Name] = true
		for _, kv := range sp.Attributes {
			val := anyValueString(kv.Value)
			for _, bad := range []string{"secret", "?", "203.0.113.7", "agent", "Bearer", "abc123"} {
				if strings.Contains(val, bad) {
					t.Errorf("span %q attribute %s=%s leaks %q", sp.Name, kv.Key, val, bad)
				}
			}
			for _, banned := range []string{"url.full", "url.query", "http.target", "client.address", "user_agent.original", "http.request.header."} {
				if strings.HasPrefix(kv.Key, banned) {
					t.Errorf("span %q carries banned attribute %s", sp.Name, kv.Key)
				}
			}
			if obs.DeniedAttribute(kv.Key, val) {
				t.Errorf("span %q attribute %s fails the obs denylist", sp.Name, kv.Key)
			}
		}
	}
	if !names["GET /healthz"] || !names["HTTP GET"] {
		t.Fatalf("span names = %v; want route template GET /healthz and unrouted HTTP GET", names)
	}
}

// With the endpoint empty no exporter exists: the receiver sees nothing and the
// global provider is not an SDK provider (no batch goroutine).
func TestTracesNotExportedWhenDisabled(t *testing.T) {
	recv := &otlpReceiver{}
	ts := httptest.NewServer(recv)
	defer ts.Close()
	before := otel.GetTracerProvider()
	cfg := tracingConfig(t, "")
	shutdown, err := obs.SetupTracing(context.Background(), cfg.OTLPEndpoint, cfg.OTelServiceName)
	if err != nil {
		t.Fatalf("SetupTracing: %v", err)
	}
	if otel.GetTracerProvider() != before {
		t.Fatal("disabled tracing replaced the global tracer provider")
	}
	newProbeServer(t, nil).Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	recv.mu.Lock()
	defer recv.mu.Unlock()
	if recv.requests != 0 {
		t.Fatalf("receiver got %d requests with tracing disabled", recv.requests)
	}
}
