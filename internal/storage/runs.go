package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/opsolete/opsolete/internal/runnerapi"
	"github.com/opsolete/opsolete/internal/storage/db"
	"github.com/opsolete/opsolete/internal/templates"
)

// Run statuses. A workspace has at most one active run (enforced by runs_one_active_idx).
const (
	PlanQueued        = "plan_queued"
	Planning          = "planning"
	NeedsConfirmation = "needs_confirmation"
	ApplyQueued       = "apply_queued"
	Applying          = "applying"
	Applied           = "applied"
	NoChanges         = "no_changes"
	Drifted           = "drifted" // final status of an assess run that found drift
	Planned           = "planned" // final status of a speculative plan that found changes
	Discarded         = "discarded"
	Errored           = "errored"
	Canceled          = "canceled"
)

// Event is what happens to a run. With the status the run is in, it allows the statuses it may move to
// (machine); setStatus refuses anything else. So a claim is undone only by EvUnclaim, and only EvConfirm and
// EvDiscard leave needs_confirmation.
type Event string

const (
	EvClaim   Event = "claim"   // a runner took the queued phase
	EvUnclaim Event = "unclaim" // its runner never acknowledged the claim: queued again (UnclaimJobs)
	EvPlanned Event = "planned" // the plan finished: it waits, auto-applies, or ends with its result (afterPlan)
	EvConfirm Event = "confirm" // a human confirmed the plan
	EvDiscard Event = "discard" // a human discarded it
	EvApplied Event = "applied" // the apply finished
	EvFail    Event = "fail"    // the runner reported a failure, the lease expired, or the server failed the job or plan
	EvCancel  Event = "cancel"  // a user canceled it, or the runner reported it canceled
)

// machine is the run state machine: per event, each status it moves a run from and the statuses it may move it
// to. A status no event moves from is terminal: the run has ended. setStatus is the only writer of runs.status.
var machine = map[Event]map[string][]string{
	EvClaim:   {PlanQueued: {Planning}, ApplyQueued: {Applying}},
	EvUnclaim: {Planning: {PlanQueued}, Applying: {ApplyQueued}},
	EvPlanned: {Planning: {NeedsConfirmation, ApplyQueued, NoChanges, Drifted, Planned}},
	EvConfirm: {NeedsConfirmation: {ApplyQueued}},
	EvDiscard: {NeedsConfirmation: {Discarded}},
	EvApplied: {Applying: {Applied}},
	EvFail:    {Planning: {Errored}, Applying: {Errored}},
	EvCancel:  {PlanQueued: {Canceled}, Planning: {Canceled}, ApplyQueued: {Canceled}, Applying: {Canceled}},
}

// ErrTransition: a status change the state machine doesn't allow. It means a bug, or a race the
// caller lost (the run moved on meanwhile); callers that race legitimately map it to their own error.
var ErrTransition = errors.New("run status transition not allowed")

func canTransition(ev Event, from, to string) bool { return slices.Contains(machine[ev][from], to) }

// Ended: the run reached a terminal status, so it will never change again.
func Ended(status string) bool {
	for _, from := range machine {
		if _, ok := from[status]; ok {
			return false
		}
	}
	return true
}

// Why a finished plan waits for a human (runs.confirm_reason, afterPlan).
const (
	ReasonPromotion = "promotion" // a promotion's runs always ask
	ReasonRollout   = "rollout"   // a template rollout's runs always ask
	ReasonImport    = "import"    // a plan that imports into a template's module
	ReasonDestroy   = "destroy"   // destroys always ask
	ReasonRefresh   = "refresh"   // a refresh-only plan always asks
	ReasonManual    = "manual"    // the workspace doesn't auto-apply
)

// PlanContext is what decides where a finished plan goes, besides its changes.
type PlanContext struct {
	Action             string // the run's
	AutoApply          bool   // the workspace's
	Rollout, Promotion bool   // the run is a rollout's, a promotion's
	Importing          bool   // the plan imports into a template's module
}

// afterPlan is the one rule for where a finished plan goes: without changes it ends (no_changes; a drift
// check that found some drifted, a speculative plan planned); with changes it auto-applies unless a reason
// says a human must confirm it (the first that applies, in the order of the Reason* constants).
func afterPlan(c PlanContext, changes bool) (status, reason string) {
	switch {
	case !changes:
		return NoChanges, ""
	case c.Action == "assess":
		return Drifted, ""
	case c.Action == "plan":
		return Planned, ""
	case c.Promotion:
		reason = ReasonPromotion
	case c.Rollout:
		reason = ReasonRollout
	case c.Importing:
		reason = ReasonImport
	case c.Action == "destroy":
		reason = ReasonDestroy
	case c.Action == "refresh":
		reason = ReasonRefresh
	case !c.AutoApply:
		reason = ReasonManual
	default:
		return ApplyQueued, ""
	}
	return NeedsConfirmation, reason
}

// Run is one plan → (confirm) → apply of a workspace, or of its destruction.
type Run struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	Action      string // apply | destroy | refresh (refresh-only) | assess (drift check, never applied) | plan (speculative, never applied)
	Status      string
	Replace     []string        // resource addresses an apply run plans to replace
	Plan        *runnerapi.Plan // nil until planned, or when the plan had no changes
	Outputs     map[string]any
	Runner      *string
	CreatedBy   string // emails
	ConfirmedBy string
	// CancelRequested: a user asked to cancel while a runner works on it.
	CancelRequested bool
	VCSCommit       string // commit a VCS workspace's run planned; "" until its plan fetched the configuration
	VCSPullRequest  int    // pull request a speculative plan was made for; 0 for other runs
	ConfirmReason   string // why its plan waits (waited) for a human (Reason*); "" if it didn't
	CreatedAt       time.Time
	UpdatedAt       time.Time  // when it ended, once it has
	LogRemovedAt    *time.Time // retention deleted its log (PruneRunLogs); nil while it's kept
}

// LogRemoved is what an ended run whose log was deleted shows instead; "" while the log is kept.
func (r Run) LogRemoved() string {
	if r.LogRemovedAt == nil {
		return ""
	}
	return fmt.Sprintf("The log was removed %d days after the run ended (RUN_LOG_RETENTION_DAYS).", int(r.LogRemovedAt.Sub(r.UpdatedAt)/(24*time.Hour)))
}

// Active: the run still holds its workspace (queued, working or waiting for confirmation).
func (r Run) Active() bool { return !Ended(r.Status) }

// Working: a runner is on it or about to be, so its log is still growing.
func (r Run) Working() bool {
	return r.Status == PlanQueued || r.Status == Planning || r.Status == ApplyQueued || r.Status == Applying
}

type StatusEntry struct {
	Status    string
	Message   *string
	CreatedAt time.Time
}

// setStatus moves run runID to status to on event ev, with message ("" none) in its history. It is the one
// place a run's status changes: it locks the run, refuses what the state machine doesn't allow
// (ErrTransition; ErrNotFound for no such run), and performs the effects of the change (effects).
// Status-specific columns (plan, outputs, confirmed_by, confirm_reason, runner and lease) stay with the
// callers. It returns the status the run came from.
func setStatus(ctx context.Context, q *db.Queries, runID uuid.UUID, ev Event, to, message string) (string, error) {
	from, _, err := transition(ctx, q, runID, ev, to, message)
	return from, err
}

// effect is one thing a status change implies, besides the status and its history (effects).
type effect string

const (
	effNotifyJobs        effect = "notify jobs"        // a phase was queued: wake the runners' claims
	effReleaseStateLock  effect = "release state lock" // left planning/applying: the lock its Terraform may have left (never a rollback's or CLI's); the lease ends with the status
	effTakeVersion       effect = "take version"       // its apply started, or no changes: the workspace takes the run's configuration version
	effRemoveTemplates   effect = "remove templates"   // a destroy removing templates succeeded: a version without them
	effContinuePromotion effect = "continue promotion" // a promotion's destroy succeeded: queue its apply run
	effDropPromotion     effect = "drop promotion"     // a promotion's destroy ended otherwise: noted, nothing else changes
)

// runFacts is what of a run decides its effects, besides the statuses.
type runFacts struct {
	Version  bool // it plans a configuration version that it may apply (not a speculative plan or drift check)
	Removes  bool // a destroy that removes templates (remove_templates)
	Promotes bool // a promotion's destroy run, its apply run not queued yet
}

// effects lists, in order, what moving a run from → to implies. Pure: the state machine's side effects are
// this list and nothing else (transition performs them).
func effects(f runFacts, from, to string) []effect {
	var out []effect
	if to == PlanQueued || to == ApplyQueued {
		out = append(out, effNotifyJobs)
	}
	if (from == Planning || from == Applying) && to != Planning && to != Applying {
		out = append(out, effReleaseStateLock)
	}
	if f.Version && (to == Applying || to == NoChanges) {
		out = append(out, effTakeVersion)
	}
	succeeded := to == Applied || to == NoChanges
	if f.Removes && succeeded {
		out = append(out, effRemoveTemplates)
	}
	switch {
	case f.Promotes && succeeded:
		out = append(out, effContinuePromotion)
	case f.Promotes && Ended(to):
		out = append(out, effDropPromotion)
	}
	return out
}

// transition is setStatus, and returns the promotion step it queued (nil: none) for the caller's
// audit entry: only FinishJob ends a run applied or with no changes.
func transition(ctx context.Context, q *db.Queries, runID uuid.UUID, ev Event, to, message string) (string, *PromotionStep, error) {
	run, err := q.LockRun(ctx, runID)
	if err != nil {
		return "", nil, notFound(err)
	}
	from := run.Status
	if !canTransition(ev, from, to) {
		return from, nil, fmt.Errorf("%w: run %s on %s from %s to %s", ErrTransition, runID, ev, from, to)
	}
	if err := q.UpdateRunStatus(ctx, db.UpdateRunStatusParams{ID: runID, Status: to, Leased: to == Planning || to == Applying}); err != nil {
		return from, nil, err
	}
	if err := addHistory(ctx, q, runID, to, message); err != nil {
		return from, nil, err
	}
	if err := notifyRun(ctx, q, runID); err != nil {
		return from, nil, err
	}
	var step *PromotionStep
	f := runFacts{Version: run.CvID.Valid && run.Action != "plan" && run.Action != "assess", Removes: len(run.RemoveTemplates) > 0, Promotes: run.Promotes}
	for _, e := range effects(f, from, to) {
		switch e {
		case effNotifyJobs:
			err = notifyJobs(ctx, q)
		case effReleaseStateLock:
			n, rerr := q.ReleaseRunStateLock(ctx, db.ReleaseRunStateLockParams{WorkspaceID: run.WorkspaceID, RunID: runID})
			err = locksReleased(ctx, q, n, rerr)
		case effTakeVersion:
			err = setCurrent(ctx, q, run.WorkspaceID, run.CvID.UUID)
		case effRemoveTemplates: // the removed templates' resources are gone now
			err = removeTemplates(ctx, q, run.WorkspaceID, run.RemoveTemplates)
		case effContinuePromotion:
			step, err = continuePromotion(ctx, q, runID, run.WorkspaceID, to)
		case effDropPromotion:
			err = addHistory(ctx, q, runID, to, "promotion not continued: "+to)
		}
		if err != nil {
			return from, nil, err
		}
	}
	return from, step, nil
}

// removeTemplates makes the workspace's current version one without the templates names.
func removeTemplates(ctx context.Context, q *db.Queries, workspaceID uuid.UUID, names []string) error {
	cur, mods, err := currentConfig(ctx, q, workspaceID)
	if err != nil {
		return err
	}
	cv, err := newVersion(ctx, q, workspaceID, cur, without(mods, names), SourceRemove, uuid.Nil)
	if err != nil {
		return err
	}
	return setCurrent(ctx, q, workspaceID, cv)
}

// Notification channels, listened to by app.Notifier. They are sent inside the transaction of the
// change, so Postgres delivers them on commit (never for a rollback); payloads are ids only.
const (
	JobsChannel = "opsolete_jobs" // a job may have become claimable (no payload)
	RunsChannel = "opsolete_runs" // a run's log or status changed (payload: the run id)
)

// notifyJobs: a run was queued (createRun, QueueVCSRun, QueueAssessments, setStatus) or a state
// lock released (locksReleased).
func notifyJobs(ctx context.Context, q *db.Queries) error {
	return q.Notify(ctx, db.NotifyParams{Channel: JobsChannel})
}

// notifyRun: run id's status changed (setStatus) or its log grew (AppendLog).
func notifyRun(ctx context.Context, q *db.Queries, id uuid.UUID) error {
	return q.Notify(ctx, db.NotifyParams{Channel: RunsChannel, Payload: id.String()})
}

// locksReleased takes the result of a state-lock release query: n released locks may make runs of
// their workspaces claimable (ClaimableRun skips locked workspaces).
func locksReleased(ctx context.Context, q *db.Queries, n int64, err error) error {
	if err != nil || n == 0 {
		return err
	}
	return notifyJobs(ctx, q)
}

// addHistory records an entry in a run's status history: a transition (setStatus), or a note on
// the current status (a cancel request).
func addHistory(ctx context.Context, q *db.Queries, runID uuid.UUID, status, message string) error {
	return q.AddRunHistory(ctx, db.AddRunHistoryParams{RunID: runID, Status: status, Message: message})
}

// pruneBatch is how many runs one retention statement handles: short locks on a big first pass.
const pruneBatch = 1000

// PruneRunLogs deletes the logs of runs that ended more than olderThan ago, marking each run's
// log_removed_at in the same statement. It returns how many runs lost their log.
func (s *Store) PruneRunLogs(ctx context.Context, olderThan time.Duration) (int64, error) {
	var logs int64 // runs that had a log; the batch counts every run it marked
	_, err := prune(func(lim int32) (int64, error) {
		had, err := s.q.PruneRunLogs(ctx, db.PruneRunLogsParams{OlderSecs: olderThan.Seconds(), Lim: lim})
		for _, h := range had {
			if h {
				logs++
			}
		}
		return int64(len(had)), err
	}, pruneBatch)
	return logs, err
}

// PruneSpeculativeRuns deletes drift checks and speculative plans that ended more than olderThan
// ago, except the latest ones their readers need (see the query). It returns how many it deleted.
func (s *Store) PruneSpeculativeRuns(ctx context.Context, olderThan time.Duration) (int64, error) {
	return prune(func(lim int32) (int64, error) {
		return s.q.PruneSpeculativeRuns(ctx, db.PruneSpeculativeRunsParams{OlderSecs: olderThan.Seconds(), Lim: lim})
	}, pruneBatch)
}

// prune runs batch until one handles fewer than lim rows, and returns the total.
func prune(batch func(lim int32) (int64, error), lim int32) (int64, error) {
	var total int64
	for {
		n, err := batch(lim)
		total += n
		if err != nil || n < int64(lim) {
			return total, err
		}
	}
}

// Found is, for one template module of a run that changed it and ended without applying (or whose plan imports),
// what it was set to and what its plan found importing (runnerapi.FinishRequest.Found): Change settings starts the
// form from it.
type Found struct {
	TemplateID string         `json:"template_id"`
	Inputs     map[string]any `json:"inputs,omitempty"` // found by the plan
	Saved      map[string]any `json:"saved,omitempty"`  // the module's inputs in the run's version, sensitive (sealed) ones left out
	Import     bool           `json:"-"`                // the module imports in the run's version
}

// unsealed is inputs without the sealed values: the form never shows them.
func unsealed(inputs map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range inputs {
		if !templates.IsSealed(v) {
			out[k] = v
		}
	}
	return out
}

// RunFound is, by template module, what run runID's plan found importing and, once the run ended without its
// apply, the modules it changed (it made the change: the first run of its version); each with its inputs in the
// run's version (Saved). nil none. ErrNotFound: no such run.
func (s *Store) RunFound(ctx context.Context, runID uuid.UUID) (map[string]Found, error) {
	raw, err := s.q.RunFound(ctx, runID)
	if err != nil {
		return nil, notFound(err)
	}
	var found map[string]Found
	if raw != nil {
		if err := json.Unmarshal(raw, &found); err != nil {
			return nil, err
		}
	}
	rc, err := s.q.RunConfig(ctx, runID)
	if err != nil {
		return nil, err
	}
	mods, err := configModules(ctx, s.q, rc.CvID)
	if err != nil {
		return nil, err
	}
	entered := rc.Changed && !rc.Active && !rc.Applied // ended without its apply
	var before []WorkspaceTemplate
	if entered {
		if before, err = configModules(ctx, s.q, rc.ParentID); err != nil {
			return nil, err
		}
	}
	for _, m := range mods {
		f, read := found[m.Name]
		changed := entered && !slices.ContainsFunc(before, func(b WorkspaceTemplate) bool {
			return sameTemplates([]WorkspaceTemplate{b}, []WorkspaceTemplate{m})
		})
		if !read && !changed {
			continue
		}
		if found == nil {
			found = map[string]Found{}
		}
		f.TemplateID, f.Saved, f.Import = m.TemplateID, unsealed(m.Inputs), m.Import
		found[m.Name] = f
	}
	return found, nil
}

// RunDiff is run runID's plan's attribute changes per resource; nil none (no changes, or planned before
// diffs were kept). ErrNotFound: no such run.
func (s *Store) RunDiff(ctx context.Context, runID uuid.UUID) ([]runnerapi.ResourceDiff, error) {
	raw, err := s.q.RunDiff(ctx, runID)
	if err != nil {
		return nil, notFound(err)
	}
	var out []runnerapi.ResourceDiff
	if raw != nil {
		err = json.Unmarshal(raw, &out)
	}
	return out, err
}
