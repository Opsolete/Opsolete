---
title: Runs
weight: 3
aliases:
  - /docs/usage/runs/
---

Every change to a workspace is a run:

`plan_queued` → `planning` → `needs_confirmation` → (confirm) `apply_queued` → `applying` → `applied`

A run can also end as `no_changes`, `discarded` (plan not confirmed), `errored` or `canceled`; a plan-only run that found changes ends as `planned`, a drift check that found drift as `drifted`. Each status change lands in the run's history.

## Plan, then apply

The plan phase saves a plan file and reports what it will do (`+2 ~1 −0`, plus the resource addresses, never values). Confirming queues the apply phase, which may run on any runner and applies exactly the saved plan. Terraform refuses the apply if the state changed since the plan, so a stale plan can't be applied.

- **Auto-apply** (per workspace, off by default) applies plans without confirmation. **Destroy, refresh-only, [template rollout]({{< relref "/docs/features/workspaces#update-to-a-new-module-version" >}}), [promotion]({{< relref "/docs/features/environments#one-run-or-two" >}}) runs and plans that [import]({{< relref "/docs/features/workspaces#import-existing-resources" >}}) into a template always ask.** The run page says why a plan waits.
- **One active run per workspace, the others wait in line**. A run started while another is active (queued, planning, waiting for confirmation or applying) is *pending*: when that run ends, the oldest pending run is queued, with the workspace's configuration of then (a change applied meanwhile isn't undone by it). A push to a linked repository waits the same way; only the newest pending push stays (older ones are canceled as superseded). A pending run can be canceled. Changing templates (add, configure, remove, import, promote) still needs an idle workspace: those runs are built from the current configuration. A plan waiting for confirmation holds the workspace until then.
- **Remote execution only.** A workspace set to local execution (*Settings › General*) is run by the [Terraform CLI]({{< relref "/docs/features/terraform-cli" >}}) on your machine: its runs, drift checks, VCS triggers and rollouts don't start in Opsolete.
- **A held state lock** (a CLI, or *Settings › Locking*) keeps queued runs waiting until it's released; see [Locking]({{< relref "/docs/features/state#locking" >}}).
- **What changes:** the plan lists every resource it creates, updates, replaces, deletes or imports; open one to see its changed attributes, before → after, the ones that force a replacement marked and values known only after apply said so. Sensitive values, and any value holding a workspace secret, are hidden (by the runner and again by the server). Whoever may read the run sees this, as they see Terraform's plan in the log; the [JSON API]({{< relref "/docs/endpoints#json-api" >}}) returns it as `diff`.
- **Cost estimation:** when the runners have an Infracost key, the run page shows the estimated monthly cost before anyone confirms. See [Cost estimation]({{< relref "/docs/config/runners#cost-estimation" >}}).

## Kinds of runs

**New run** on a workspace's Overview offers:

- **Plan and apply** re-plans with the workspace's configuration, for example to revert drift.
- **Refresh state** runs `terraform plan -refresh-only` instead: it shows what changed outside Terraform, and once confirmed (always, even with auto-apply) updates the state to match without changing infrastructure.
- **Plan only** is a speculative plan that can't be applied (`planned` or `no_changes`). It runs even while the workspace is locked.
- **Check for drift** queues a [drift check](#drift-detection) now.

Elsewhere:

- **Replace** resources: tick them in the workspace's resource list and choose **Plan replacing selected** (`-replace=<address>`; only addresses in the current state are accepted).
- **Promotion:** a [promotion]({{< relref "/docs/features/environments#one-run-or-two" >}}) that removes or replaces a deployed template first runs a destroy targeting only those modules; once it is applied, the promotion's apply run is queued automatically. Both wait for confirmation.
- **Queue destroy plan** (*Settings › Destruction and deletion*) plans `terraform plan -destroy`; once confirmed and applied, the workspace is marked destroyed. A new run can recreate it.

## Drift detection

Per workspace, off by default: once a day the server queues a drift check, a refresh-only plan that is never applied, on workspaces whose state manages resources. **Check for drift** runs one now. Drift shows as a banner on the workspace and a *drifted* tag in the list until the next check or apply. Drift checks aren't listed as runs, but they hold the workspace's one active run while they plan.

## Cancel a run

- A queued run is canceled at once.
- A running one is asked to stop: the runner interrupts Terraform (SIGINT), which finishes in-flight operations and **still saves state**, and the run ends `canceled`.
- If a runner doesn't react, **Force cancel** cuts it off immediately; its lease, state password and state lock die with the run, but resources Terraform was creating at that moment may be missing from state.

## Logs

The run page follows the runner's output live. See [Runners]({{< relref "/docs/config/runners#how-it-works" >}}) for how logs are shipped and capped. Values of sensitive variables and inputs are masked in the log (`***`) and in outputs (`(sensitive)`): see [masking]({{< relref "/docs/features/variables" >}}).

## Retention

Once an hour the server prunes what has expired. Active runs are never touched; a run's end is its last status change.

- **Logs** of every run are deleted `RUN_LOG_RETENTION_DAYS` days (default 90, `0` = forever) after it ended. The run, its status history and plan summary stay; its page and raw log say "The log was removed N days after the run ended (RUN_LOG_RETENTION_DAYS)."
- **Drift checks and speculative plans** are deleted entirely 30 days after they ended, except a workspace's latest drift check, its latest drift check that found drift or none (the drift flag), and the latest speculative plan of each pull request.
- **Expired browser sessions** (12 hours after sign-in) are deleted too.
- **Apply, destroy and refresh runs** are kept forever (only their logs expire), and so are state versions, the audit log and module archives.
