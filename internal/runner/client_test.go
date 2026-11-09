package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/opsolete/opsolete/internal/config"
	"github.com/opsolete/opsolete/internal/runnerapi"
)

// A claimed job can be far bigger than 64 KiB (one variable alone may be); over MaxJobSize is a clear error.
func TestClaimLargeJob(t *testing.T) {
	var body []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(body) }))
	defer ts.Close()
	u, _ := url.Parse(ts.URL)
	r := New(config.Runner{ServerURL: u, Token: "tok"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	vars := map[string]any{}
	for i := range 16 {
		vars[strings.Repeat("v", i+1)] = strings.Repeat("x", 64<<10)
	}
	body, _ = json.Marshal(runnerapi.Job{RunID: "r1", Vars: vars})
	j, ok, err := r.claim(context.Background())
	if err != nil || !ok || j.RunID != "r1" || len(j.Vars) != 16 {
		t.Fatalf("claim of %d bytes: ok=%v err=%v", len(body), ok, err)
	}
	body = []byte(`{"vars":{"v":"` + strings.Repeat("x", runnerapi.MaxJobSize) + `"}}`)
	if _, _, err := r.claim(context.Background()); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("oversized claim: err = %v", err)
	}
}
