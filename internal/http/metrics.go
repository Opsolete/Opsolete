package httpserver

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/opsolete/opsolete/internal/storage"
)

var httpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name: "opsolete_http_request_duration_seconds",
	Help: "HTTP requests by route pattern. Log streams and runner long-polls land in the top bucket.",
}, []string{"code", "method", "route"})

type routeKey struct{}

// instrument records every request under its mux pattern, not its path: IDs in paths would explode label cardinality.
func instrument(mux *http.ServeMux, next http.Handler) http.Handler {
	h := promhttp.InstrumentHandlerDuration(httpDuration, next,
		promhttp.WithLabelFromCtx("route", func(ctx context.Context) string { return ctx.Value(routeKey{}).(string) }))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, route := mux.Handler(r)
		if route == "" {
			route = "unmatched"
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), routeKey{}, route)))
	})
}

var (
	runsDesc    = prometheus.NewDesc("opsolete_runs", "Runs by status, all time (final statuses only grow).", []string{"status"}, nil)
	queueDesc   = prometheus.NewDesc("opsolete_run_queue_oldest_seconds", "Age of the oldest queued plan or apply job; 0 when the queue is empty.", nil, nil)
	wsDesc      = prometheus.NewDesc("opsolete_workspaces", "Workspaces.", nil, nil)
	runStatuses = []string{storage.PlanQueued, storage.Planning, storage.NeedsConfirmation, storage.ApplyQueued, storage.Applying,
		storage.Applied, storage.NoChanges, storage.Drifted, storage.Planned, storage.Discarded, storage.Errored, storage.Canceled}
)

// dbStats is read from PostgreSQL at scrape time, so every server replica reports the same values: aggregate with max, not sum.
type dbStats struct{ s *Server }

func (c dbStats) Describe(ch chan<- *prometheus.Desc) { ch <- runsDesc; ch <- queueDesc; ch <- wsDesc }

func (c dbStats) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := c.s.Store.Stats(ctx)
	if err != nil { // skip, don't fail the scrape: the runtime metrics still matter, and /readyz reports the database
		c.s.Log.Error("metrics: read database stats", "err", err)
		return
	}
	for _, status := range runStatuses { // zeros too, so queue alerts don't depend on absent()
		ch <- prometheus.MustNewConstMetric(runsDesc, prometheus.GaugeValue, float64(st.Runs[status]), status)
	}
	ch <- prometheus.MustNewConstMetric(queueDesc, prometheus.GaugeValue, st.OldestQueued.Seconds())
	ch <- prometheus.MustNewConstMetric(wsDesc, prometheus.GaugeValue, float64(st.Workspaces))
}

// MetricsHandler serves Prometheus metrics: Go runtime, process, HTTP requests, the database pool and runs.
// Call it once: it registers on the default registry.
func (s *Server) MetricsHandler() http.Handler {
	prometheus.MustRegister(collectors.NewDBStatsCollector(s.Store.DB, "opsolete"), dbStats{s})
	return promhttp.Handler()
}
