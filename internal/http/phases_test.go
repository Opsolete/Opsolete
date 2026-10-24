package httpserver

import (
	"testing"
	"time"

	"github.com/opsolete/opsolete/internal/storage"
)

func TestRunPhases(t *testing.T) {
	t0 := time.Unix(0, 0)
	hist := func(statuses ...string) (h []storage.StatusEntry) {
		for i, s := range statuses {
			h = append(h, storage.StatusEntry{Status: s, CreatedAt: t0.Add(time.Duration(i) * time.Minute)})
		}
		return
	}
	for _, c := range []struct {
		status      string
		hist        []storage.StatusEntry
		plan, apply string
	}{
		{storage.PlanQueued, hist("plan_queued"), "queued", ""},
		{storage.Canceled, hist("plan_queued", "canceled"), "canceled", ""},
		{storage.Planning, hist("plan_queued", "planning"), "running", ""},
		{storage.Errored, hist("plan_queued", "planning", "errored"), "errored", ""},
		{storage.NoChanges, hist("plan_queued", "planning", "no_changes"), "finished", ""},
		{storage.NeedsConfirmation, hist("plan_queued", "planning", "needs_confirmation"), "finished", "pending"},
		{storage.Discarded, hist("plan_queued", "planning", "needs_confirmation", "discarded"), "finished", "discarded"},
		{storage.Applying, hist("plan_queued", "planning", "apply_queued", "applying"), "finished", "running"},
		{storage.Errored, hist("plan_queued", "planning", "apply_queued", "applying", "errored"), "finished", "errored"},
		{storage.Applied, hist("plan_queued", "planning", "plan_queued", "planning", "apply_queued", "applying", "applied"), "finished", "finished"},
	} {
		plan, apply := runPhases(storage.Run{Status: c.status}, c.hist)
		if plan.State != c.plan || apply.State != c.apply {
			t.Errorf("%v: plan %q apply %q, want %q %q", c.hist, plan.State, apply.State, c.plan, c.apply)
		}
	}
	// the last planning attempt counts: started at minute 3, done at 4
	if plan, _ := runPhases(storage.Run{Status: storage.Applied}, hist("plan_queued", "planning", "plan_queued", "planning", "apply_queued")); plan.Start != t0.Add(3*time.Minute) || plan.Duration() != "1m0s" {
		t.Errorf("plan %+v %q", plan, plan.Duration())
	}
}
