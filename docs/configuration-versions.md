# Design: configuration versions and the run split

Status: proposal (November 2026), for review before any code. Not published (outside `docs/content`).
Steps 2 and 3 of the redesign; step 5 (import as a session) builds on it.

## Why

A run changes the workspace's live configuration when it's queued and undoes the change when it ends without its
apply. Every feature since 1.0 hangs on that undo:

| Mechanism | Where | What it's for |
|---|---|---|
| `runs.config_changes` (`[{module, before, after}]`) | `storage/runs.go` `recordConfigChanges`, written 3 ways (`workspacetemplates.go`, `rollout.go`, `promote.go`) | undo log |
| `restoreConfig` + CAS against `template_config(...)` | `storage/runs.go`, `queries/runs.sql` | the undo; silently skipped when the row moved on |
| `keepEntered`, `Found.Saved` | `storage/runs.go` | keep what was entered, because the undo removes it |
| `runs.found` | `008_import_v2.sql` | import findings *and* Change settings drafts, one column |
| `endImport` (`jsonb_set(inputs,'{existing}',false)`) | `storage/jobs.go`, `queries/runs.sql` | configuration changed with no run; storage knows a form input's name |
| `runs.always_confirm`, `rollout_id`, `importing` | SQL, Go and the transition table | "must a human confirm?", decided in three places |
| `runs.promotion`, `remove_templates`, `continuePromotion` | `storage/runs.go` `transition` | a status change that creates another run |
| `RotateKey` refusing while a run is active | `app/rotate.go` | sealed inputs copied into `config_changes` escape re-sealing |

`transition()` (`storage/runs.go:122`) is the hub: one status change can write history, notify, release the state
lock, write `found`, restore templates, delete removed templates and start a promotion's next run. claude.md's 1.0
rule, "one place per rule, no hidden behaviour", has moved from a trigger into Go but not gone.

A correction to the review: the apply phase does **not** pick up changed module inputs or tfvars. A saved plan
carries its configuration snapshot and input-variable values, and Terraform refuses new variable values with one.
Only environment variables (provider credentials) are read again at apply, and that's wanted for credential
rotation. So snapshotting variables is not a correctness fix. It's for auditing what a run ran with, and it stays
optional below.

## Design

**A configuration is a value. A run plans one. The workspace points at the one whose apply started.**

### Schema

```sql
-- One immutable configuration of a workspace: the template modules it adds (a VCS workspace: the commit).
CREATE TABLE configuration_versions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces ON DELETE CASCADE,
    parent_id    uuid REFERENCES configuration_versions,   -- what it was made from (optimistic concurrency)
    source       text NOT NULL CHECK (source IN ('form', 'promote', 'rollout', 'import', 'remove', 'vcs', 'cli')),
    vcs_commit   text,                                     -- VCS workspaces
    created_by   text,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE configuration_modules (
    cv_id         uuid NOT NULL REFERENCES configuration_versions ON DELETE CASCADE,
    name          text NOT NULL,
    template_id   text NOT NULL,
    module_source text NOT NULL,
    module_ref    text NOT NULL,
    inputs        jsonb NOT NULL,     -- sealed inputs as today; rotate-key re-seals this table
    import        boolean NOT NULL DEFAULT false,  -- this module imports what exists (step 5)
    PRIMARY KEY (cv_id, name)
);
ALTER TABLE workspaces ADD COLUMN current_cv_id uuid REFERENCES configuration_versions;  -- apply started (or no changes)
ALTER TABLE runs ADD COLUMN cv_id uuid REFERENCES configuration_versions;                -- what it plans
```

`workspace_templates` becomes a view of `current_cv_id`'s modules, so the readers (Overview, rollouts' "update
available", promotion's source) don't change in phase A.

### The rules

1. **Changing the configuration creates a version and queues a run on it.** Add, configure, remove, promote,
   roll out, import: each builds a new version from the workspace's current one (its `parent_id`), and nothing else
   changes. A version whose parent is no longer current is refused (`ErrChanged`). This one check replaces the CAS
   restores, `sameTemplates` and the promotion digest.
2. **The workspace moves when an apply starts.** `current_cv_id := run.cv_id` in the transition to `applying` (or to
   `no_changes`). That keeps today's rule that once an apply started, the state may hold it.
3. **Ending without an apply changes nothing.** Discard, cancel, a failed plan: the version just isn't current.
   `config_changes`, `restoreConfig` and `keepEntered` go.
4. **Change settings opens the run's version** (plus what its plan found, step 5) as a draft form. Saving creates a
   new version whose parent is the current one. Nothing is lost, because nothing was removed.
5. **Removing a template** is a version without its module. When the module still manages resources, the run is a
   destroy with `-target module.<name>` on the *current* version (modules configure their own providers, so their
   block must be there), then the version without it. That's an explicit two-step plan (below), not
   `remove_templates` plus a delete inside `transition`.

### Who must confirm: one function

```go
// NeedsConfirmation is why a finished plan waits for a human; "" when it may auto-apply.
func NeedsConfirmation(r Run, cv Version, plan Plan, ws Workspace) Reason
// Reasons: destroy, refresh, rollout, promotion, import, no auto-apply.
```

It's stored on the run (`runs.confirm_reason`) when the plan finishes and shown on the run page ("waits for
confirmation: a promotion"). `always_confirm`, the `rollout_id OR always_confirm` SQL alias and the `importing`
special case go.

### Run split and state machine

| Table | Holds |
|---|---|
| `runs` | id, workspace, `cv_id`, action, status, `confirm_reason`, created/confirmed by, VCS pull request |
| `run_phases` | run, phase (plan/apply), runner, lease id/until/acked, started/ended, log bytes |
| `run_results` | run, plan summary, outputs, cost, found values (step 5) |

Status changes become `Apply(run, event) (status, []Effect, error)`. Its typed events are `Claimed`, `Acked`,
`Unclaimed`, `PlanFinished{Plan}`, `Confirmed`, `Discarded`, `CancelRequested`, `LeaseExpired` and
`ApplyFinished`. `Unclaimed` is the only way back from `planning`/`applying` to queued, enforced by its type
instead of a comment. Effects are values (`ReleaseStateLock`, `SetCurrentVersion`, `Notify`, `StartStep`), and the
caller runs them in the same transaction, so a reader of `FinishJob` sees every side effect it causes.

### Promotions and rollouts as plans of steps

```sql
CREATE TABLE workflows (id uuid PK, kind text CHECK (kind IN ('promotion','rollout')), created_by text, created_at, status text);
CREATE TABLE workflow_steps (workflow_id uuid, seq int, workspace_id uuid, cv_id uuid, action text, run_id uuid NULL,
                             PRIMARY KEY (workflow_id, seq));
```

A promotion that removes a deployed template is two steps: a destroy on the current version (targets), then the
promoted version. When a step's run applies, the `StartStep` effect queues the next. A rollout is one step per
workspace, in batches. `runs.promotion`, `remove_templates`, `continuePromotion` and `template_rollouts`'
per-run columns go.

## Phases (one PR each, all tests green)

| Phase | Change | Deletes |
|---|---|---|
| A | Tables; every configuration change writes a version; `workspace_templates` becomes a view; a migration turns each workspace's templates into its first version | nothing yet (both written, read the old) |
| B | Runs plan `cv_id`; the workspace moves at apply start; discard changes nothing; Change settings opens the run's version | `config_changes`, `restoreConfig`, `keepEntered`, `Found.Saved`, CAS restores, `ErrChanged` digests; RotateKey's active-run refusal |
| C | `NeedsConfirmation` + `confirm_reason` | `always_confirm`, the SQL alias, `importing` in FinishJob |
| D | `run_phases`, `run_results`, event state machine with effects | lease columns on `runs`, `transition`'s side effects |
| E | `workflows` for promotions and rollouts | `runs.promotion`, `remove_templates`, `continuePromotion` |
| F (step 5) | Import as a session on versions: the `import` flag adds the switch at assembly, plans are speculative on a draft, a reconcile page | `endImport`, `ImportInput` in storage, `?import/?found/?from`, the forced `existing` |

Phase A is additive and can ship alone. B is the one that changes behaviour users see (discard no longer touches
the workspace), so it carries the docs and flows.md updates.

## Tests that pin it

- Discard, cancel, failed plan, lost lease: the workspace's version is unchanged, and Change settings opens the run's.
- Two forms saved from the same current version: the second is `ErrChanged` (422 on the form, with a reload hint).
- Apply errored after it started: the workspace is on the run's version.
- Promotion with a deployed removal: two steps, a failed first step queues nothing more, the target is unchanged.
- `RotateKey` with a run active re-seals versions too.
- `NeedsConfirmation` table test: every reason × auto-apply on/off.
- A transition table test per event, and a test that `FinishJob`'s effects are exactly the returned list.

## Open questions

1. **What the workspace shows during a run.** Today it shows the pending configuration, because the rows change at
   queue time. Proposal: show the current (applied) one, with a "pending in run X" banner and a diff. Agree?
2. **Keep every version?** Proposal: yes, they're small. Prune versions no run references after 30 days, like
   speculative runs.
3. **Variables snapshot at plan time.** Not needed for correctness (see Why). Do we want it for audit ("what did
   this run run with")? It would be sealed and pruned with the plan artifact.
4. **Upgrades.** Releases 0.6 and 0.7 are out, so phase A needs a real migration (`workspace_templates` →
   first versions), not a schema reset. Confirm that's the target.
