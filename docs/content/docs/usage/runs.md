---
title: Runs
weight: 2
---

Every change to a workspace is a run:

`plan_queued` → `planning` → `needs_confirmation` → (confirm) `apply_queued` → `applying` → `applied`

A run can also end as `no_changes`, `discarded` (plan not confirmed), `errored` or `canceled`. Each status change lands in the run's history.

## Plan, then apply

The plan phase saves a plan file and reports what it will do (`+2 ~1 −0`, plus the resource addresses, never values). Confirming queues the apply phase, which may run on any runner and applies exactly the saved plan. Terraform refuses the apply if the state changed since the plan, so a stale plan can't be applied.

- **Auto-apply** (per workspace, off by default) applies plans without confirmation. **Destroy runs always ask.**
- **One active run per workspace.** A new run waits until the current one is applied, discarded or finished.
- **Cost estimation:** when the runners have an Infracost key, the run page shows the estimated monthly cost before anyone confirms. See [Cost estimation]({{< relref "/docs/operations/runners#cost-estimation" >}}).

## Kinds of runs

- **New run** re-plans with the workspace's inputs, for example to revert drift.
- **Refresh state only** runs `terraform plan -refresh-only` instead: it shows what changed outside Terraform, and once confirmed (always, even with auto-apply) updates the state to match without changing infrastructure.
- **Replace** resources: tick them in the workspace's resource list and choose **Plan replacing selected** (`-replace=<address>`; only addresses in the current state are accepted).
- **Destroy…** plans `terraform plan -destroy`; once confirmed and applied, the workspace is marked destroyed. A new run can recreate it.

## Drift detection

Per workspace, off by default: once a day the server queues a drift check, a refresh-only plan that is never applied, on workspaces whose state manages resources. **Check for drift** runs one now. Drift shows as a banner on the workspace and a *drifted* tag in the list until the next check or apply. Drift checks aren't listed as runs, but they hold the workspace's one active run while they plan.

## Cancel a run

- A queued run is canceled at once.
- A running one is asked to stop: the runner interrupts Terraform (SIGINT), which finishes in-flight operations and **still saves state**, and the run ends `canceled`.
- If a runner doesn't react, **Force cancel** cuts it off immediately; its lease, state password and state lock die with the run, but resources Terraform was creating at that moment may be missing from state.

## Logs

The run page follows the runner's output live. See [Runners]({{< relref "/docs/operations/runners#how-it-works" >}}) for how logs are shipped and capped.
