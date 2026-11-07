package storage

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/opsolete/opsolete/internal/runnerapi"
)

// A rollout moves root and added uses in one run, which waits for confirmation despite auto-apply;
// a use that changed since it was read isn't overwritten; a discarded run counts as failed until
// the rollout is resumed; a closed rollout queues nothing.
func TestUpgradeTemplates(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	w, run := newWorkspace(t, s, true) // root: template job-test, git::x v1
	j := claim(t, s, "plan")
	if _, err := s.FinishJob(ctx, run.ID, j.LeaseID, "succeeded", "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`INSERT INTO workspace_templates (workspace_id, name, template_id, module_source, module_ref, inputs)
		VALUES ($1, 'extra', 'job-test', 'git::x', 'v1', '{}')`, w.ID); err != nil {
		t.Fatal(err)
	}
	s.DB.Exec(`UPDATE template_rollouts SET status = 'stopped' WHERE template_id = 'job-test'`) // from earlier runs
	ro, err := s.CreateRollout(ctx, Rollout{TemplateID: "job-test", ModuleSource: "git::x", ModuleRef: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DB.Exec(`DELETE FROM template_rollouts WHERE id = $1`, ro.ID) })
	if _, err := s.CreateRollout(ctx, Rollout{TemplateID: "job-test", ModuleSource: "git::x", ModuleRef: "v2"}); !errors.Is(err, ErrExists) {
		t.Fatalf("second open rollout: %v", err)
	}
	uses, err := s.TemplateUses(ctx, "job-test", w.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	var ups []TemplateUpgrade
	for _, u := range uses {
		if u.WorkspaceID == w.ID {
			ups = append(ups, TemplateUpgrade{TemplateID: "job-test", Module: u.Module, ModuleSource: "git::x", ModuleRef: "v2",
				Inputs: map[string]any{"n": 2.0}, From: u})
		}
	}
	if len(ups) != 2 || ups[0].Module != "" || ups[1].Module != "extra" {
		t.Fatalf("uses: %+v", ups)
	}

	stale := ups[0]
	stale.From.Inputs = map[string]any{"n": 9.0} // read before someone changed it
	if _, err := s.UpgradeTemplates(ctx, w.ID, []TemplateUpgrade{ups[1], stale}, "", ro.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("stale use: %v", err)
	}
	r, err := s.UpgradeTemplates(ctx, w.ID, ups, "", ro.ID)
	if err != nil {
		t.Fatal(err)
	}
	if uses, _ := s.TemplateUses(ctx, "job-test", ""); !upgraded(uses, w.ID) {
		t.Fatalf("not upgraded: %+v", uses)
	}
	j = claim(t, s, "plan")
	if st, _ := s.FinishJob(ctx, r.ID, j.LeaseID, "succeeded", "", nil, &runnerapi.Plan{Change: 1}, nil); st != NeedsConfirmation {
		t.Fatalf("rollout plan with auto-apply: %q", st)
	}
	if _, err := s.UpgradeTemplates(ctx, w.ID, nil, "", ro.ID); !errors.Is(err, ErrRunActive) {
		t.Fatalf("busy workspace: %v", err)
	}
	if open, failed, err := s.RolloutRuns(ctx, ro.ID, ro.ResumedAt); open != 1 || failed != "" || err != nil {
		t.Fatalf("rollout runs: %d %q %v", open, failed, err)
	}
	if err := s.DiscardRun(ctx, r.ID, ""); err != nil {
		t.Fatal(err)
	}
	if open, failed, _ := s.RolloutRuns(ctx, ro.ID, ro.ResumedAt); open != 0 || failed != "the run in "+w.Name+": discarded" {
		t.Fatalf("after discard: %d %q", open, failed)
	}
	if ok, err := s.SetRolloutStatus(ctx, ro.ID, "paused", ""); !ok || err != nil {
		t.Fatalf("pause: %v %v", ok, err)
	}
	if _, err := s.UpgradeTemplates(ctx, w.ID, nil, "", ro.ID); !errors.Is(err, ErrRolloutClosed) {
		t.Fatalf("paused rollout: %v", err)
	}
	s.SetRolloutStatus(ctx, ro.ID, "active", "")
	// the resuming request ticks it itself: no other turn within the interval, so the batch limit holds
	if due, err := s.DueRollouts(ctx, time.Minute); err != nil || slices.ContainsFunc(due, func(o Rollout) bool { return o.ID == ro.ID }) {
		t.Fatalf("resumed rollout handed out next to the resuming request's turn: %v", err)
	}
	if due, err := s.DueRollouts(ctx, 0); err != nil || !slices.ContainsFunc(due, func(o Rollout) bool { return o.ID == ro.ID }) {
		t.Fatalf("resumed rollout not due after the interval: %v", err)
	}
	ro, _ = s.LatestRollout(ctx, "job-test")
	if _, failed, _ := s.RolloutRuns(ctx, ro.ID, ro.ResumedAt); failed != "" {
		t.Fatalf("resume must forget earlier failures: %q", failed)
	}
}

func upgraded(uses []TemplateUse, wsID string) bool {
	n := 0
	for _, u := range uses {
		if u.WorkspaceID == wsID {
			if u.ModuleRef != "v2" || u.Inputs["n"] != 2.0 || u.RunID == "" {
				return false
			}
			n++
		}
	}
	return n == 2
}
