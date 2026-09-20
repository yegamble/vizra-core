package jobs

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/yegamble/vizra-core/internal/store/sqlcgen"
)

// The three metrics ADR-004 names. vizra_jobs_oldest_queued_age_seconds is the
// one readiness and `vizra doctor` read against the Q-028 threshold, so it is
// not optional instrumentation: a probe depends on it.
type Metrics struct {
	Depth           *prometheus.GaugeVec
	OldestQueuedAge *prometheus.GaugeVec
	StaleLeases     prometheus.Gauge
}

// NewMetrics registers the collectors. Passing a per-process registry rather
// than the default one keeps two processes in one test from colliding.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Depth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vizra_jobs_depth",
			Help: "Number of jobs in a live state, by kind and state.",
		}, []string{"kind", "state"}),
		OldestQueuedAge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vizra_jobs_oldest_queued_age_seconds",
			Help: "Age of the oldest queued job, by kind. Readiness and doctor fail above the configured threshold (Q-028).",
		}, []string{"kind"}),
		StaleLeases: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "vizra_jobs_stale_leases",
			Help: "Number of leased jobs whose lease has elapsed and which the sweep will reclaim.",
		}),
	}
	reg.MustRegister(m.Depth, m.OldestQueuedAge, m.StaleLeases)
	return m
}

// Snapshot is the queue state readiness reads. It is computed by the same query
// that feeds the metrics, so a probe and a dashboard can never disagree.
type Snapshot struct {
	OldestQueuedAge time.Duration
	StaleLeases     int64
	Depth           map[string]int64 // "kind/state" -> depth
}

// Collect reads the queue state once and updates the gauges.
func Collect(ctx context.Context, pool *pgxpool.Pool, m *Metrics) (Snapshot, error) {
	q := sqlcgen.New(pool)
	snap := Snapshot{Depth: map[string]int64{}}

	depths, err := q.JobDepthByKindState(ctx)
	if err != nil {
		return snap, err
	}
	if m != nil {
		m.Depth.Reset()
	}
	for _, d := range depths {
		snap.Depth[d.Kind+"/"+d.State] = d.Depth
		if m != nil {
			m.Depth.WithLabelValues(d.Kind, d.State).Set(float64(d.Depth))
		}
	}

	ages, err := q.OldestQueuedAgeByKind(ctx)
	if err != nil {
		return snap, err
	}
	if m != nil {
		m.OldestQueuedAge.Reset()
	}
	for _, a := range ages {
		if m != nil {
			m.OldestQueuedAge.WithLabelValues(a.Kind).Set(a.OldestAgeSeconds)
		}
		if d := time.Duration(a.OldestAgeSeconds * float64(time.Second)); d > snap.OldestQueuedAge {
			snap.OldestQueuedAge = d
		}
	}

	stale, err := q.CountStaleLeases(ctx)
	if err != nil {
		return snap, err
	}
	snap.StaleLeases = stale
	if m != nil {
		m.StaleLeases.Set(float64(stale))
	}
	return snap, nil
}
