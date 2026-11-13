---
title: Runners
weight: 3
aliases:
  - /docs/operations/runners/
---

`opsolete runner` executes runs, one phase (plan or apply) at a time. It's the same binary as the server, packaged as the `runner` image (built with nix): git, ssh, coreutils and Terraform 1.17.1 pinned by checksum, running as a non-root user. See [Install]({{< relref "/docs/install" >}}) for how to run it and [option reference]({{< relref "/docs/config/reference#runner" >}}) for its settings.

## How it works

- **Pull, don't push.** Runners long-poll `POST /api/runner/v1/jobs/claim` with their own token: a claim waits up to 20 seconds and gets a job as soon as one is queued (PostgreSQL `LISTEN`/`NOTIFY`, with a look every 5 seconds as a safety net). They need no database credentials and accept no inbound connections. Registry tokens can't claim jobs, and runner tokens can't use the registry.
- **Postgres queue.** Queued run phases are the jobs. `FOR UPDATE SKIP LOCKED` lets any number of runners claim without double-running a job.
- **Claim, then acknowledge.** A claimed job is the runner's only once it acknowledges it (`POST /api/runner/v1/jobs/<run>/ack` with the lease), before any other call for the job; the server refuses logs, heartbeats, artifacts, the result and the state credential until then. A claim not acknowledged within 30 seconds (the response was lost on the way, or didn't decode) goes back to the queue for any runner, and the run's history says "not acknowledged by runner X: queued again". Nothing ran yet, so that is safe; an acknowledged job is never queued again.
- **Leases and fencing.** A claim returns a lease id that every log, heartbeat and finish call must send. Log posts double as heartbeats, sent about every 30 s while idle. If a runner disappears for 2 minutes, the server fails the job ("runner stopped responding") and fences the old runner off. An outage of the server or its database doesn't count against the runners: a server that takes over background work waits 1 minute before failing any job, so runners that kept working through the outage report back in time and their applies save their state. A 409 tells a runner it lost the job, so it interrupts Terraform. Jobs are never auto-retried, because a half-applied run needs a human.
- **Isolation.** One job at a time per runner, in a fresh temp directory with its own `HOME`, both removed afterwards, so a job can't leave a `.terraformrc`, `.gitconfig` or provider mirror behind for the next one. The runner runs `init -from-module`, then `plan -out` and `apply <plan>` itself, with Terraform's colors on (the run page shows them) and `-compact-warnings` (unless the workspace sets the env variable `OPS_DEBUG=1`), and reads `show -json`/`output -json` itself (never logged). Each run phase is capped by `OPS_JOB_TIMEOUT`.
- **Jobs can't attack the runner or each other** (Linux). Module code runs as the runner's user, so the runner makes itself non-dumpable: jobs can't read its `/proc/<pid>/environ` or memory, which still hold the runner token. Every process a job starts stays the runner's child, even one that daemonizes (the runner is a child subreaper), and after each job the runner kills them all (e.g. a backgrounded `local-exec`) before it claims the next one, so nothing can watch the next job's credentials. This works the same as a container's PID 1, under an init such as `tini` or `docker run --init`, or as a service; a runner that can't become a subreaper refuses to start. On other systems than Linux (development) there is no such cleanup and the runner warns at startup. The provider plugin cache is shared between jobs; Terraform only uses a cached provider that matches the lock file's checksums. Where it can, the runner also puts every Terraform command in [the job sandbox](#the-job-sandbox).
- **Plans between phases.** The plan phase uploads the plan file and `.terraform.lock.hcl` to the server, which keeps them in `STATE_STORE` (a plan holds variable values, so it's as sensitive as state). The apply phase downloads both and restores the lock file before `init`, so it uses exactly the providers the plan used. Both files are deleted when the run ends.
- **Logs.** Output is shipped about once a second, cut at line ends, at most 256 KB per post, and capped at 10 MB per run. The run page follows it live via Server-Sent Events, woken by each new chunk (or every 5 seconds), resuming with `Last-Event-ID` after reconnects. A user may follow at most 8 logs at once per server replica; more get `429 Too Many Requests`. `…/runs/<id>/logs` serves the raw log.
- **Secrets.** Sensitive outputs are shown as `(sensitive)` and never leave the runner. The `output -json` call isn't streamed, and signed module URLs are redacted from logs. Values of sensitive variables and sensitive template inputs, and the run's state password (`TF_HTTP_PASSWORD`), are shown as `***` in the log, and an output containing one is reported as `(sensitive)` ([masking rules and limits]({{< relref "/docs/features/variables" >}})). The runner reads its `OPS_*` settings and then removes them from its environment, so Terraform, providers and module code never see the runner token.
- **Interrupts.** A phase that hits `OPS_JOB_TIMEOUT`, a cancel by a user, a lost lease and a runner shutdown all stop Terraform the same way: SIGINT, so it starts nothing new, finishes the operations in flight, saves its state and releases its lock; if it hasn't exited `OPS_JOB_GRACE` later, its process group is killed. A phase therefore never runs longer than `OPS_JOB_TIMEOUT` + `OPS_JOB_GRACE`; after that the runner stops sending heartbeats and log posts, so if it can't report the result the lease runs out and the server fails the job. The result says what happened: `timed out after …`, `canceled by a user`, or `runner was shut down during the run`, each followed by `Terraform was interrupted and saved its state`, or, when it was killed, that the state may be missing resources it was changing (check them before the next apply), or, when the interrupt came between Terraform commands (e.g. while downloading the plan), `no Terraform command was running at the time`.
- **State an apply couldn't save.** If an apply's state write fails, the runner pushes it once more. If that fails too (lease lost, server unreachable), it keeps the state in its temp dir, sealed with its runner token so later jobs, which run as the same user, can't read it, and the run's result names the file. Push it from a machine with the runner token: `OPS_RUNNER_TOKEN=… opsolete unseal-state <file> | terraform state push -`. A container loses the file when it restarts.
- **Shutdown.** The first SIGTERM stops claiming and lets the current job finish; a claim already waiting completes (up to 20 seconds), and a job it returns is acknowledged and run like the current one, so scaling down never strands a job. A second one interrupts the current job as above, and it ends as `canceled`. Set the pod's `terminationGracePeriodSeconds` to at least `OPS_JOB_TIMEOUT` + `OPS_JOB_GRACE` + 5 minutes (for the claim in flight and reporting the result), or a long phase is killed without the grace.

## The job sandbox

On Linux, every Terraform command of a job runs in its own user, mount, PID and IPC namespaces (`OPS_SANDBOX`, default `auto`):

- **A private `/tmp`.** Terraform, providers and module code see an empty `/tmp` with only the job's own directories in it (its work directory and `HOME`, and `TF_PLUGIN_CACHE_DIR` if it's there). They can't read what the runner or an earlier job left in `/tmp` (such as a [sealed errored state](#how-it-works)), and what they write there is gone when the command ends. `TMPDIR`, `TMP` and `TEMP` point into the job's `HOME`, sandbox or not. The rest of the file system looks as before: the runner's container or service already makes it read-only.
- **Its own processes.** Terraform is process 1 of its namespace: it sees no other process (where the container lets the sandbox mount its own `/proc`), and when it exits the kernel kills everything it left running at once.
- **No new privileges.** The runner's user is root inside the namespace and nobody else; outside it's still the runner's user, and files it writes in the job's directories belong to the runner.

Network is not separated: a job reaches what the runner reaches. Block the cloud's metadata endpoint (`169.254.169.254`) for runners, so module code can't take the node's identity; the Helm chart does this with a NetworkPolicy.

The sandbox needs unprivileged user namespaces, which not every host and container allows:

- **NixOS:** on (the module sets `OPS_SANDBOX=namespaces` and lets the service create these namespaces), unless the kernel forbids them (`security.unprivilegedUsernsClone = false`, as in the hardened profile).
- **Docker and Kubernetes:** the default seccomp and AppArmor profiles forbid them, and Ubuntu 23.10 and later restrict them for the whole host (`kernel.apparmor_restrict_unprivileged_userns`). Lifting that is a trade, not a free gain: unprivileged user namespaces make more of the kernel reachable to every process (many privilege escalations went through them), to win isolation between jobs. On Kubernetes, run [a pod per run phase](#a-pod-per-run-phase) instead: each job gets a fresh container (own file system, `/tmp`, processes and network namespace) and the node's defaults stay; add a sandboxed runtime (gVisor, Kata: `runtimeClassName`) to keep module code off the node's kernel too. With Docker, `--security-opt seccomp=unconfined` turns the sandbox on where the host allows user namespaces.

`auto` uses the sandbox where it works and logs a warning where it doesn't; `namespaces` makes the runner refuse to start without it; `off` never uses it. The runner logs `job sandbox: namespaces` when it's on. Infracost runs outside the sandbox.

## A pod per run phase

On Kubernetes a runner can be a pod per run phase instead of a long-lived Deployment: with `OPS_RUNNER_ONCE=true` it claims one job, runs it and exits (or exits when one claim, 20 seconds, finds none: another pod took it). An autoscaler starts the pods: [KEDA](https://keda.sh)'s `ScaledJob` with the `metrics-api` scaler polls `GET /api/runner/v1/queue` with the runner's token (`{"queued": n}`, the phases its pool could claim now; not those waiting for a workspace's lock) and starts a Job per queued phase. The Helm chart does this with `runner.mode: scaledJob`.

Every phase then starts clean: plan and apply run in different pods (the plan file goes through the server anyway), and nothing a job leaves, files or processes, outlives it. The costs: a pod start per phase (seconds, with the image cached), and providers downloaded per phase unless the pods share a cache volume (`TF_PLUGIN_CACHE_DIR` on a ReadWriteMany volume) or use a provider mirror. The pods share the runner's token, so they're one runner to the server.

## Runners and pools

Each runner has its own token, so the server knows which runner it is (by its token, not by the name it sends) and an admin can revoke one without touching the others. Create one:

- under **Settings → Runners**: name and pool; the token is shown once, with the command to start the runner;
- with `opsolete runner-token [-pool p] <name>` and the server's settings (NixOS: `sudo opsolete-admin runner-token …`), which prints it;
- or register a token you generated yourself, for deployments that keep it in their own secret store: `opsolete runner-token -from-stdin [-pool p] <name> < token-file` (at least 32 characters; again replaces that runner's token and pool), or from an environment variable in a container without a shell, `opsolete runner-token -from-env NAME [-pool p] <name>`. The NixOS module does this for `runnerTokenFile` at every server start, the Helm chart for its `RUNNER_TOKEN` secret.

Deleting a runner revokes its token at once: its next request gets `401` and it stops; a run it was working on fails when its lease runs out (2 minutes). Settings → Runners shows when each runner was last seen.

**Pools.** Every runner is in a pool (`default` unless given), and every project uses one (`default` unless changed in the project's settings, by an admin). A run goes only to runners in its workspace's project's pool, and that's where its variables go. Put production projects in a `prd` pool served by production runners, and production credentials never reach the other runners. A workspace moved to another project takes that project's pool from its next queued phase. A project whose pool has no runner keeps its runs queued.

## Credentials

Provider credentials come from the workspace's [variables]({{< relref "/docs/features/variables" >}}), set as environment variables for Terraform on top of the runner's own environment. Runners are generic: give them no cloud credentials of their own, or every workspace without variables runs with them. Runners need no state storage credentials.

**Every runner is trusted with the secrets of every workspace in its pool.** A runner, or anyone holding its token, can claim any job of a project in its pool and receives that workspace's variables (sensitive ones in clear text) and a credential for its state. Treat a runner's token like the most privileged cloud credential in its pool: keep it in a secret store, run runners only on infrastructure as trusted as the workspaces they serve, and use pools to keep production credentials on production runners. Module code runs on the runner with the provider credentials, so scope them to what Opsolete may deploy.

## Cost estimation

With `OPS_INFRACOST_API_KEY` set, every plan with changes (except refresh-only and drift checks) is priced with `infracost breakdown`, and the run page shows the estimated monthly cost, its change and the number of resources priced before anyone confirms the apply.

- It's an estimate: usage-based charges are left out, and resources Infracost doesn't support count as 0.
- Infracost runs on the runner with the plan's JSON and only its own key in its environment, never the job's variables. It sends price-relevant resource attributes (type, region, SKU, size) to Infracost's Cloud Pricing API, not the plan.
- If it fails, the run goes on without an estimate and the log says why.
- Admins choose the currency (US dollar or euro) under **Settings → General**. A change applies to plans run after it.
