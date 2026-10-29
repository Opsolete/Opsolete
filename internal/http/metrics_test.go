package httpserver

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func scrape(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	promhttp.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func TestRequestMetricsUseRoutePattern(t *testing.T) {
	h := newTestServer(t)
	get(h, "/healthz")
	get(h, "/no/such/page/123")
	body := scrape(t)
	for _, want := range []string{`code="200",method="get",route="GET /healthz"`, `route="unmatched"`} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
	if strings.Contains(body, "/no/such/page/123") {
		t.Error("raw paths must not become labels")
	}
}

func TestDatabaseMetrics(t *testing.T) {
	store := testStore(t)
	newApp(t, "../../catalog", store, devAuth(t, store, "admin@example.com", true), Features{Deployments: true}).MetricsHandler()
	body := scrape(t)
	for _, want := range []string{`opsolete_runs{status="plan_queued"}`, "opsolete_run_queue_oldest_seconds ", "opsolete_workspaces ",
		`go_sql_max_open_connections{db_name="opsolete"}`, "go_goroutines ", "process_"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
}
