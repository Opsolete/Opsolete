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
