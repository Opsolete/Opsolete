package storage

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	s, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DB.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Leftovers from earlier runs would be claimed first.
	if _, err := s.DB.Exec(`UPDATE deployments SET status = 'failed' WHERE status IN ('queued', 'running')`); err != nil {
		t.Fatal(err)
	}
	return s
}

func queued(t *testing.T, s *Store) Deployment {
	ctx := context.Background()
	d, err := s.CreateDeployment(ctx, "job-test", "git::x", "v1", map[string]any{"n": 1.0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DB.Exec(`DELETE FROM deployments WHERE id=$1`, d.ID) })
	return d
}

func TestJobLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	d := queued(t, s)

	// Many runners race for one job: exactly one wins.
	var mu sync.Mutex
	var won []Job
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, ok, err := s.ClaimJob(ctx, "r", time.Minute)
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				won = append(won, j)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(won) != 1 || won[0].DeploymentID != d.ID || won[0].ModuleSource != "git::x" || won[0].Inputs["n"] != 1.0 {
		t.Fatalf("claims: %+v", won)
	}
	j := won[0]

	if err := s.Heartbeat(ctx, d.ID, "not-the-lease", time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("foreign lease must be rejected, got %v", err)
	}
	if err := s.AppendLog(ctx, d.ID, j.LeaseID, "0123456789", time.Minute, 15); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendLog(ctx, d.ID, j.LeaseID, "abcdefghij", time.Minute, 15); err != nil { // crosses the limit
		t.Fatal(err)
	}
	if err := s.AppendLog(ctx, d.ID, j.LeaseID, "dropped", time.Minute, 15); err != nil {
		t.Fatal(err)
	}
	logs, err := s.Logs(ctx, d.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 2 || logs[0].Chunk != "0123456789" || !strings.Contains(logs[1].Chunk, "truncated") {
		t.Fatalf("logs: %+v", logs)
	}
	if more, _ := s.Logs(ctx, d.ID, logs[0].Seq, 100); len(more) != 1 {
		t.Fatalf("cursor: %+v", more)
	}

	if err := s.FinishJob(ctx, d.ID, j.LeaseID, "succeeded", "", map[string]any{"id": "abc"}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJob(ctx, d.ID, j.LeaseID, "failed", "", nil); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("finishing twice must fail, got %v", err)
	}
	got, hist, err := s.GetDeployment(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" || got.Outputs["id"] != "abc" || *got.Runner != "r" || hist[len(hist)-1].Status != "succeeded" {
		t.Fatalf("finished deployment: %+v %+v", got, hist)
	}
	if _, ok, _ := s.ClaimJob(ctx, "r", time.Minute); ok {
		t.Fatal("nothing should be left to claim")
	}
}

func TestExpiredLeaseIsFailed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	d := queued(t, s)
	j, ok, err := s.ClaimJob(ctx, "r", 10*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	time.Sleep(50 * time.Millisecond)
	ids, err := s.ReapExpiredLeases(ctx)
	if err != nil || len(ids) != 1 || ids[0] != d.ID {
		t.Fatalf("reap: %v %v", ids, err)
	}
	// The old runner is fenced off once its lease is gone.
	if err := s.AppendLog(ctx, d.ID, j.LeaseID, "late", time.Minute, 1<<20); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("late write must be rejected, got %v", err)
	}
	got, _, _ := s.GetDeployment(ctx, d.ID)
	if got.Status != "failed" {
		t.Fatalf("status %s", got.Status)
	}
}
