package runner

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opsolete/opsolete/internal/config"
	"github.com/opsolete/opsolete/internal/runnerapi"
)

// The shipper masks a signed URL even when it arrives split over two writes, and its final flush
// ships a tail larger than one post.
func TestShipperRedactsAndDrains(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	r := New(config.Runner{ServerURL: u, Token: "tok"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	signed := "/v1/archives/m.tar.gz?expires=1&sig=secret-signature"
	pem := "-----BEGIN KEY-----\nAAAAkeyline\n-----END KEY-----"
	s := newShipper(r, runnerapi.Job{RunID: "run1", LeaseID: "lease", Secrets: []string{"hunter22", "hunter22-long", pem, "eu"}}, func() {}, []string{signed}, time.Now().Add(time.Hour))
	s.Write([]byte("init " + signed[:30]))
	s.Write([]byte(signed[30:] + " failed\n"))
	s.Write([]byte("local-exec: hunter22-long hunter22 eu AAAAkeyline\n"))
	big := strings.Repeat("x", 300<<10) + "\nthe error at the end"
	s.Write([]byte(big))
	s.Close()
	got := f.logs.String()
	if strings.Contains(got, "secret-signature") || !strings.Contains(got, "?<signed>") {
		t.Fatalf("signed URL not masked: %.200q", got)
	}
	if !strings.Contains(got, "local-exec: *** *** eu ***\n") {
		t.Fatalf("sensitive values not masked (whole, per line, not too short): %.400q", got)
	}
	if !strings.HasSuffix(got, big) {
		t.Fatalf("tail lost: got %d bytes, want suffix of %d", len(got), len(big))
	}
}

// After the phase's hard end the shipper neither posts logs nor heartbeats, so the lease runs out
// unless the runner finishes; Close still ships the tail once.
func TestShipperStopsAtHardEnd(t *testing.T) {
	f := &fakeServer{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	r := New(config.Runner{ServerURL: u, Token: "tok"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// a heartbeat every 2s otherwise
	s := newShipper(r, runnerapi.Job{RunID: "run1", LeaseID: "lease", LeaseSeconds: 8}, func() {}, nil, time.Now())
	s.Write([]byte("after the end\n"))
	time.Sleep(3500 * time.Millisecond)
	f.mu.Lock()
	calls := slices.Clone(f.calls)
	f.mu.Unlock()
	s.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(calls) != 0 || !slices.Equal(f.calls, []string{"logs"}) || f.logs.String() != "after the end\n" {
		t.Fatalf("calls before Close %v, after %v, logs %q", calls, f.calls, f.logs.String())
	}
}
