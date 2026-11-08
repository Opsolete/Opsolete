package storage

import "testing"

func TestTransitions(t *testing.T) {
	for _, c := range []struct {
		from, to string
		ok       bool
	}{
		{PlanQueued, Planning, true},
		{PlanQueued, Canceled, true},
		{PlanQueued, Errored, false},
		{Planning, NeedsConfirmation, true},
		{Planning, ApplyQueued, true},
		{Planning, Applied, false},
		{NeedsConfirmation, ApplyQueued, true},
		{NeedsConfirmation, Discarded, true},
		{NeedsConfirmation, Canceled, false},
		{ApplyQueued, Applying, true},
		{ApplyQueued, Discarded, false},
		{Applying, Applied, true},
		{Applying, NoChanges, false},
		{Applied, Planning, false},
		{Canceled, Canceled, false},
		{Errored, PlanQueued, false},
		{Planning, PlanQueued, true},  // unclaim only
		{Applying, ApplyQueued, true}, // unclaim only
		{Planning, ApplyQueued, true}, // auto-apply, not unclaim
		{Applying, PlanQueued, false},
	} {
		if got := canTransition(c.from, c.to); got != c.ok {
			t.Errorf("%s → %s: %v, want %v", c.from, c.to, got, c.ok)
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

// run_active and run_working (001_schema.sql) agree with the Go state machine for every status.
func TestRunActiveSQL(t *testing.T) {
	s := testStore(t)
	statuses := map[string]bool{}
	for from, tos := range transitions {
		statuses[from] = true
		for _, to := range tos {
			statuses[to] = true
		}
	}
	if len(statuses) != 12 {
		t.Fatalf("statuses: %v", statuses)
	}
	for st := range statuses {
		var active, working bool
		if err := s.Pool.QueryRow(t.Context(), `SELECT run_active($1), run_working($1)`, st).Scan(&active, &working); err != nil {
			t.Fatal(err)
		}
		if active != !Ended(st) || working != (st == Planning || st == Applying) {
			t.Errorf("%s: run_active %v, run_working %v", st, active, working)
		}
	}
}
