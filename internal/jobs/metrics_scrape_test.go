package jobs_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/yegamble/vizra-core/internal/jobs"
	"github.com/yegamble/vizra-core/internal/obs"
)

// TestWorkerMetricsScrape serves the worker's real metrics handler (the one
// cmd/worker mounts) on a test listener, scrapes it over HTTP and parses the
// exposition: every job family is present and carries only bounded labels.
func TestWorkerMetricsScrape(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := jobs.NewMetrics(reg)
	m.Depth.WithLabelValues("media.derive", "queued").Set(3)
	m.OldestQueuedAge.WithLabelValues("media.derive").Set(12)
	m.StaleLeases.Set(1)

	srv := httptest.NewServer(obs.MetricsHandler(reg))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	parser := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := parser.TextToMetricFamilies(strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, body)
	}
	allowed := map[string]bool{"kind": true, "state": true}
	for _, name := range []string{"vizra_jobs_depth", "vizra_jobs_oldest_queued_age_seconds", "vizra_jobs_stale_leases"} {
		f, ok := fams[name]
		if !ok {
			t.Fatalf("family %s missing from scrape", name)
		}
		for _, mt := range f.GetMetric() {
			for _, l := range mt.GetLabel() {
				if !allowed[l.GetName()] {
					t.Errorf("%s: unexpected label %q (high-cardinality or private)", name, l.GetName())
				}
			}
		}
	}
}
