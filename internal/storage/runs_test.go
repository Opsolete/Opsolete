package storage

import (
	"slices"
	"testing"
)

func TestTransitions(t *testing.T) {
	for _, c := range []struct {
		ev       Event
		from, to string
		ok       bool
	}{
		{EvClaim, PlanQueued, Planning, true},
		{EvClaim, PlanQueued, Applying, false},
		{EvCancel, PlanQueued, Canceled, true},
		{EvFail, PlanQueued, Errored, false},
		{EvPlanned, Planning, NeedsConfirmation, true},
		{EvPlanned, Planning, ApplyQueued, true}, // auto-apply
		{EvPlanned, Planning, Applied, false},
		{EvConfirm, NeedsConfirmation, ApplyQueued, true},
		{EvConfirm, Planning, ApplyQueued, false}, // only a plan that waits is confirmed
		{EvDiscard, NeedsConfirmation, Discarded, true},
		{EvCancel, NeedsConfirmation, Canceled, false},
		{EvClaim, ApplyQueued, Applying, true},
		{EvDiscard, ApplyQueued, Discarded, false},
		{EvApplied, Applying, Applied, true},
		{EvApplied, Applying, NoChanges, false},
		{EvClaim, Applied, Planning, false},
		{EvCancel, Canceled, Canceled, false},
		{EvUnclaim, Errored, PlanQueued, false},
		{EvUnclaim, Planning, PlanQueued, true},
		{EvUnclaim, Applying, ApplyQueued, true},
		{EvPlanned, Planning, PlanQueued, false}, // back to the queue: unclaim only
		{EvUnclaim, Applying, PlanQueued, false},
		{EvFail, Applying, Errored, true},
	} {
		if got := canTransition(c.ev, c.from, c.to); got != c.ok {
			t.Errorf("%s: %s → %s: %v, want %v", c.ev, c.from, c.to, got, c.ok)
		}
	}
	for _, s := range []string{Applied, NoChanges, Drifted, Planned, Discarded, Errored, Canceled} {
		if !Ended(s) || (Run{Status: s}).Active() {
			t.Errorf("%s must be terminal", s)
		}
	}
	for _, s := range []string{PlanQueued, Planning, NeedsConfirmation, ApplyQueued, Applying} {
		if Ended(s) {
			t.Errorf("%s must not be terminal", s)
		}
	}
}

// TestEffects: a status change's side effects are effects' list, in order.
func TestEffects(t *testing.T) {
	all := runFacts{Version: true, Removes: true, Promotes: true}
	for _, c := range []struct {
		f        runFacts
		from, to string
		want     []effect
	}{
		{runFacts{}, PlanQueued, Planning, nil},
		{all, Planning, PlanQueued, []effect{effNotifyJobs, effReleaseStateLock}},
		{all, Planning, NeedsConfirmation, []effect{effReleaseStateLock}},
		{all, NeedsConfirmation, ApplyQueued, []effect{effNotifyJobs}},
		{all, Planning, ApplyQueued, []effect{effNotifyJobs, effReleaseStateLock}},
		{all, ApplyQueued, Applying, []effect{effTakeVersion}},
		{runFacts{}, ApplyQueued, Applying, nil}, // no version (a VCS-only workspace)
		{all, Applying, Applied, []effect{effReleaseStateLock, effRemoveTemplates, effContinuePromotion, effPromotePending}},
		{all, Planning, NoChanges, []effect{effReleaseStateLock, effTakeVersion, effRemoveTemplates, effContinuePromotion, effPromotePending}},
		{all, NeedsConfirmation, Discarded, []effect{effDropPromotion, effPromotePending}},
		{all, Applying, Errored, []effect{effReleaseStateLock, effDropPromotion, effPromotePending}},
		{runFacts{Version: true}, NeedsConfirmation, Discarded, []effect{effPromotePending}}, // nothing to undo; the next one may go
		{runFacts{}, Pending, PlanQueued, []effect{effNotifyJobs}},
		{runFacts{}, Pending, Canceled, nil}, // it never held the workspace: the active run still does
	} {
		if got := effects(c.f, c.from, c.to); !slices.Equal(got, c.want) {
			t.Errorf("%+v %s → %s: %v, want %v", c.f, c.from, c.to, got, c.want)
		}
	}
}

// TestAfterPlan: the one rule for where a finished plan goes, and why it waits.
func TestAfterPlan(t *testing.T) {
	for _, c := range []struct {
		c              PlanContext
		changes        bool
		status, reason string
	}{
		{PlanContext{Action: "apply", AutoApply: true}, false, NoChanges, ""},
		{PlanContext{Action: "assess"}, true, Drifted, ""},
		{PlanContext{Action: "plan"}, true, Planned, ""},
		{PlanContext{Action: "apply", AutoApply: true}, true, ApplyQueued, ""},
		{PlanContext{Action: "apply"}, true, NeedsConfirmation, ReasonManual},
		{PlanContext{Action: "destroy", AutoApply: true}, true, NeedsConfirmation, ReasonDestroy},
		{PlanContext{Action: "refresh", AutoApply: true}, true, NeedsConfirmation, ReasonRefresh},
		{PlanContext{Action: "apply", AutoApply: true, Importing: true}, true, NeedsConfirmation, ReasonImport},
		{PlanContext{Action: "apply", AutoApply: true, Rollout: true, Importing: true}, true, NeedsConfirmation, ReasonRollout},
		{PlanContext{Action: "destroy", AutoApply: true, Promotion: true, Rollout: true}, true, NeedsConfirmation, ReasonPromotion},
		{PlanContext{Action: "apply", Promotion: true}, false, NoChanges, ""},
	} {
		if s, r := afterPlan(c.c, c.changes); s != c.status || r != c.reason {
			t.Errorf("%+v changes %v: %s %q, want %s %q", c.c, c.changes, s, r, c.status, c.reason)
		}
	}
}

// run_active and run_working (001_schema.sql) agree with the Go state machine for every status.
func TestRunActiveSQL(t *testing.T) {
	s := testStore(t)
	statuses := map[string]bool{}
	for _, edges := range machine {
		for from, tos := range edges {
			statuses[from] = true
			for _, to := range tos {
				statuses[to] = true
			}
		}
	}
	if len(statuses) != 13 {
		t.Fatalf("statuses: %v", statuses)
	}
	for st := range statuses {
		var active, working bool
		if err := s.Pool.QueryRow(t.Context(), `SELECT run_active($1), run_working($1)`, st).Scan(&active, &working); err != nil {
			t.Fatal(err)
		}
		if active != (!Ended(st) && st != Pending) || working != (st == Planning || st == Applying) { // pending waits, it doesn't hold the workspace
			t.Errorf("%s: run_active %v, run_working %v", st, active, working)
		}
	}
}
