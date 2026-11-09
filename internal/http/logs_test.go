package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage"
)

// A run queued while a claim waits is handed out at once (a notification), not at the next poll.
func TestClaimWakesOnQueue(t *testing.T) {
	ctx, store := context.Background(), testStore(t)
	srv := newApp(t, "../../catalog", store, devAuth(t, store, "admin@example.com", true), Features{Deployments: true})
	srv.ClaimWait = 10 * time.Second
	h := srv.Handler()
	ws, err := store.CreateEmptyWorkspace(ctx, storage.EmptyWorkspace{Name: "claim-wake"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	done := make(chan *httptest.ResponseRecorder)
	go func() {
		req := httptest.NewRequest("POST", runnerapi.Prefix+"/jobs/claim", strings.NewReader(`{"runner":"r1"}`))
		req.Header.Set("Authorization", "Bearer "+testRunnerToken)
		done <- do(h, req)
	}()
	time.Sleep(300 * time.Millisecond)
	// adding a template queues its plan
	if _, err := store.SetWorkspaceTemplate(ctx, ws.ID, storage.WorkspaceTemplate{Name: "m", TemplateID: "x",
		ModuleSource: "example.com/m", ModuleRef: "1.0.0", Inputs: map[string]any{}}, true, uuid.Nil); err != nil {
		t.Fatal(err)
	}
	rec := <-done
	if took := time.Since(start); rec.Code != http.StatusOK || took > 800*time.Millisecond {
		t.Fatalf("claim: %d after %v: %s", rec.Code, took, rec.Body.String())
	}
}

// Shutdown ends a waiting claim (204) and a log stream, not other requests (their context lives on).
func TestStoppingEndsLongPolls(t *testing.T) {
	ctx, store := context.Background(), testStore(t)
	srv := newApp(t, "../../catalog", store, devAuth(t, store, "admin@example.com", true), Features{Deployments: true})
	stopping, stop := context.WithCancel(ctx)
	srv.Stopping, srv.ClaimWait = stopping, 10*time.Second
	h := srv.Handler()
	ws, err := store.CreateEmptyWorkspace(ctx, storage.EmptyWorkspace{Name: "stopping"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(ctx, ws.ID, "apply", uuid.Nil, nil) // queued but not claimable: its stream stays open
	if err != nil {
		t.Fatal(err)
	}
	claim, stream := make(chan *httptest.ResponseRecorder), make(chan *httptest.ResponseRecorder)
	go func() {
		req := httptest.NewRequest("POST", runnerapi.Prefix+"/jobs/claim", strings.NewReader(`{"runner":"r1"}`))
		req.Header.Set("Authorization", "Bearer "+testRunnerToken)
		claim <- do(h, req)
	}()
	go func() {
		stream <- do(h, httptest.NewRequest("GET", "/workspaces/stopping/runs/"+run.ID.String()+"/logs/stream", nil))
	}()
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	stop()
	if rec := <-claim; rec.Code != http.StatusNoContent {
		t.Fatalf("claim on shutdown: %d %s", rec.Code, rec.Body.String())
	}
	if rec := <-stream; rec.Code != http.StatusOK {
		t.Fatalf("stream on shutdown: %d", rec.Code)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("long polls ended %v after shutdown", took)
	}
}

// One user gets maxLogStreams concurrent log streams per replica; another user has their own.
func TestLogStreamCap(t *testing.T) {
	ctx, store := context.Background(), testStore(t)
	srv := newApp(t, "../../catalog", store, devAuth(t, store, "admin@example.com", true), Features{Deployments: true})
	h := srv.Handler()
	ws, err := store.CreateEmptyWorkspace(ctx, storage.EmptyWorkspace{Name: "capped"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(ctx, ws.ID, "apply", uuid.Nil, nil) // queued: its stream stays open
	if err != nil {
		t.Fatal(err)
	}
	streamURL := "/workspaces/capped/runs/" + run.ID.String() + "/logs/stream"
	open := func() (stop func() *httptest.ResponseRecorder) {
		c, cancel := context.WithCancel(ctx)
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- do(h, httptest.NewRequest("GET", streamURL, nil).WithContext(c)) }()
		return func() *httptest.ResponseRecorder { cancel(); return <-done }
	}
	streams := func() (n int) {
		srv.streams.Lock()
		defer srv.streams.Unlock()
		for _, c := range srv.streams.n {
			n += c
		}
		return n
	}
	var stops []func() *httptest.ResponseRecorder
	t.Cleanup(func() {
		for _, s := range stops {
			s()
		}
	})
	for range maxLogStreams {
		stops = append(stops, open())
	}
	for deadline := time.Now().Add(5 * time.Second); streams() < maxLogStreams; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d streams open, want %d", streams(), maxLogStreams)
		}
	}

	if rec := get(h, streamURL); rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "at most 8") {
		t.Fatalf("stream %d: %d %q", maxLogStreams+1, rec.Code, rec.Body.String())
	}
	other := uuid.New() // a dev login is one user: the slots are per user id
	if !srv.openStream(other) {
		t.Fatal("another user's slots are taken")
	}
	srv.closeStream(other)

	if rec := stops[0](); rec.Code != http.StatusOK {
		t.Fatalf("an open stream: %d", rec.Code)
	}
	stops = stops[1:]
	c, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if rec := do(h, httptest.NewRequest("GET", streamURL, nil).WithContext(c)); rec.Code != http.StatusOK {
		t.Fatalf("after closing one: %d %q", rec.Code, rec.Body.String())
	}
}

// TestLogPostLimit (Q8): one log post carries at most 256 KB (the runner cuts its batches there).
func TestLogPostLimit(t *testing.T) {
	_, store, h, r := flowServer(t)
	id := greetingWorkspace(t, h, "lp-"+strings.ToLower(newID()[:8]), false)
	r.claim(t, id, "plan")
	if c := r.jobCall("POST", "logs", strings.Repeat("a", maxLogChunk+1)); c != http.StatusRequestEntityTooLarge {
		t.Fatalf("log post over the limit: %d", c)
	}
	if c := r.jobCall("POST", "logs", strings.Repeat("b", maxLogChunk)); c != http.StatusNoContent {
		t.Fatalf("log post at the limit: %d", c)
	}
	if chunks, err := store.Logs(context.Background(), uuid.MustParse(id), 0, 10); err != nil || len(chunks) != 1 || chunks[0].Chunk != strings.Repeat("b", maxLogChunk) {
		t.Fatalf("stored log: %d chunks %v, want only the accepted post", len(chunks), err)
	}
}
