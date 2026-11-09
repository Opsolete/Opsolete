package app

import (
	"context"
	"time"
)

// speculativeRetention: drift checks and speculative plans only matter while they're recent; the
// latest ones, which the drift flag, the Health page and pull request dedupe read, are always kept.
const speculativeRetention = 30 * 24 * time.Hour

// Pruned is what one Retention pass removed.
type Pruned struct {
	Logs     int64 // runs whose log was deleted
	Runs     int64 // drift checks and speculative plans deleted
	Sessions int64 // expired browser sessions deleted
}

// Retention deletes the logs of runs that ended more than RunLogRetention ago (0: never) and the
// drift checks and speculative plans that ended more than speculativeRetention ago. Apply, destroy
// and refresh runs, state versions and the audit log are kept. Active runs are never touched.
// Expired browser sessions are deleted too.
func (a *App) Retention(ctx context.Context) (Pruned, error) {
	var p Pruned
	var err error
	if a.RunLogRetention > 0 {
		if p.Logs, err = a.Store.PruneRunLogs(ctx, a.RunLogRetention); err != nil {
			return p, err
		}
	}
	if p.Runs, err = a.Store.PruneSpeculativeRuns(ctx, speculativeRetention); err != nil {
		return p, err
	}
	if p.Sessions, err = a.Store.PruneSessions(ctx); err != nil {
		return p, err
	}
	if p.Logs > 0 || p.Runs > 0 || p.Sessions > 0 {
		a.Log.Info("retention", "logs_removed", p.Logs, "runs_deleted", p.Runs, "sessions_deleted", p.Sessions)
	}
	return p, nil
}
