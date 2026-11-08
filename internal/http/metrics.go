package httpserver

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
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
	st, err := c.s.App.Store.Stats(ctx)
	if err != nil { // skip, don't fail the scrape: the runtime metrics still matter, and /readyz reports the database
		c.s.App.Log.Error("metrics: read database stats", "err", err)
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
	prometheus.MustRegister(poolStats{s.App.Store.Pool}, dbStats{s})
	return promhttp.Handler()
}

// poolStats reports the connection pool under the names database/sql's collector used (go_sql_*).
type poolStats struct{ p *pgxpool.Pool }

var poolDescs = func() map[string]*prometheus.Desc {
	m := map[string]*prometheus.Desc{}
	for name, help := range map[string]string{
		"max_open_connections":        "Maximum number of open connections to the database.",
		"open_connections":            "The number of established connections both in use and idle.",
		"in_use_connections":          "The number of connections currently in use.",
		"idle_connections":            "The number of idle connections.",
		"wait_count_total":            "The total number of connections waited for.",
		"wait_duration_seconds_total": "The total time blocked waiting for a new connection.",
		"max_idle_time_closed_total":  "The total number of connections closed due to idle time.",
		"max_lifetime_closed_total":   "The total number of connections closed due to max connection lifetime.",
	} {
		m[name] = prometheus.NewDesc("go_sql_"+name, help, nil, prometheus.Labels{"db_name": "opsolete"})
	}
	return m
}()

func (c poolStats) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range poolDescs {
		ch <- d
	}
}

func (c poolStats) Collect(ch chan<- prometheus.Metric) {
	st := c.p.Stat()
	for name, v := range map[string]float64{
		"max_open_connections": float64(st.MaxConns()), "open_connections": float64(st.TotalConns()),
		"in_use_connections": float64(st.AcquiredConns()), "idle_connections": float64(st.IdleConns()),
		"wait_count_total": float64(st.EmptyAcquireCount()), "wait_duration_seconds_total": st.EmptyAcquireWaitTime().Seconds(),
		"max_idle_time_closed_total": float64(st.MaxIdleDestroyCount()), "max_lifetime_closed_total": float64(st.MaxLifetimeDestroyCount()),
	} {
		vt := prometheus.GaugeValue
		if strings.HasSuffix(name, "_total") {
			vt = prometheus.CounterValue
		}
		ch <- prometheus.MustNewConstMetric(poolDescs[name], vt, v)
	}
}
