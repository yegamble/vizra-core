package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/yegamble/vizra-core/internal/obs"
)

// TestMetricsScrapeByRouteTemplate drives requests through the real Echo stack,
// scrapes the metrics handler the api binary serves over real HTTP, parses it
// with the Prometheus text parser, and asserts the HTTP families are labelled by
// route template with no raw path, query string, id or token in any label.
func TestMetricsScrapeByRouteTemplate(t *testing.T) {
	reg := prometheus.NewRegistry()
	s := newProbeServer(t, func(d *Deps) { d.Metrics = reg })
	for _, p := range []string{"/healthz?token=tok-secret-1", "/i/user-12345/raw-path", "/healthz"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
	}
	ts := httptest.NewServer(obs.MetricsHandler(reg))
	defer ts.Close()
	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	p := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := p.TextToMetricFamilies(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"vizra_http_requests_total", "vizra_http_request_duration_seconds"} {
		f, ok := fams[name]
		if !ok {
			t.Fatalf("family %s missing; have %v", name, keys(fams))
		}
		routes := map[string]bool{}
		for _, m := range f.GetMetric() {
			for _, lp := range m.GetLabel() {
				v := lp.GetValue()
				for _, bad := range []string{"?", "tok-secret-1", "user-12345", "raw-path"} {
					if strings.Contains(v, bad) {
						t.Errorf("%s label %s=%q carries %q", name, lp.GetName(), v, bad)
					}
				}
				if lp.GetName() == "route" {
					routes[v] = true
				}
			}
		}
		if !routes["/healthz"] || !routes["unmatched"] {
			t.Errorf("%s routes = %v, want /healthz and unmatched", name, routes)
		}
	}
	c := fams["vizra_http_requests_total"]
	var n float64
	for _, m := range c.GetMetric() {
		n += m.GetCounter().GetValue()
	}
	if n != 3 {
		t.Errorf("requests_total sum = %v, want 3", n)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
