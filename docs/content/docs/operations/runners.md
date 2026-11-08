---
title: Runners
weight: 3
---

`opsolete runner` executes runs, one phase (plan or apply) at a time. It's the same binary as the server, packaged as the `runner` image (built with nix): git, ssh, coreutils and Terraform 1.17.0 pinned by checksum, running as a non-root user. See [Deployment]({{< relref "deployment" >}}) for how to run it.

## How it works

- **Pull, don't push.** Runners long-poll `POST /api/runner/v1/jobs/claim` with the server's `RUNNER_TOKEN`: a claim waits up to 20 seconds and gets a job as soon as one is queued (PostgreSQL `LISTEN`/`NOTIFY`, with a look every 5 seconds as a safety net). They need no database credentials and accept no inbound connections. Registry tokens can't claim jobs, and the runner token can't use the registry. All runners share that one token, so revoking one runner means rotating it for all of them.
- **Postgres queue.** Queued run phases are the jobs. `FOR UPDATE SKIP LOCKED` lets any number of runners claim without double-running a job.
- **Claim, then acknowledge.** A claimed job is the runner's only once it acknowledges it (`POST /api/runner/v1/jobs/<run>/ack` with the lease), before any other call for the job; the server refuses logs, heartbeats, artifacts, the result and the state credential until then. A claim not acknowledged within 30 seconds (the response was lost on the way, or didn't decode) goes back to the queue for any runner, and the run's history says "not acknowledged by runner X: queued again". Nothing ran yet, so that is safe; an acknowledged job is never queued again.
- **Leases and fencing.** A claim returns a lease id that every log, heartbeat and finish call must send. Log posts double as heartbeats, sent about every 30 s while idle. If a runner disappears for 2 minutes, the server fails the job ("runner stopped responding") and fences the old runner off. A 409 tells a runner it lost the job, so it interrupts Terraform. Jobs are never auto-retried, because a half-applied run needs a human.
- **Isolation.** One job at a time per runner, in a fresh temp directory with its own `HOME`, both removed afterwards, so a job can't leave a `.terraformrc`, `.gitconfig` or provider mirror behind for the next one. The runner runs `init -from-module`, then `plan -out` and `apply <plan>` itself, with Terraform's colors on (the run page shows them) and `-compact-warnings` (unless the workspace sets the env variable `OPS_DEBUG=1`), and `show`/`output` via HashiCorp's `terraform-exec`. Each run is capped by `OPS_JOB_TIMEOUT`.
- **Jobs can't attack the runner or each other** (Linux). Module code runs as the runner's user, so the runner makes itself non-dumpable: jobs can't read its `/proc/<pid>/environ` or memory, which still hold the runner token. As a container's PID 1 (the image's default), it kills every process a job left behind (e.g. a backgrounded `local-exec`) before claiming the next job, so nothing can watch the next job's credentials. Run it under an init such as `tini` or `docker run --init` and that cleanup is off; the runner warns at startup. The provider plugin cache is shared between jobs; Terraform only uses a cached provider that matches the lock file's checksums.
- **Plans between phases.** The plan phase uploads the plan file and `.terraform.lock.hcl` to the server, which keeps them in `STATE_STORE` (a plan holds variable values, so it's as sensitive as state). The apply phase downloads both and restores the lock file before `init`, so it uses exactly the providers the plan used. Both files are deleted when the run ends.
- **Logs.** Output is shipped about once a second, cut at line ends, and capped at 10 MB per run. The run page follows it live via Server-Sent Events, woken by each new chunk (or every 5 seconds), resuming with `Last-Event-ID` after reconnects. A user may follow at most 8 logs at once per server replica; more get `429 Too Many Requests`. `…/runs/<id>/logs` serves the raw log.
- **Secrets.** Sensitive outputs are shown as `(sensitive)` and never leave the runner. The `output -json` call isn't streamed, and signed module URLs are redacted from logs. Values of sensitive variables and sensitive template inputs are shown as `***` in the log, also line by line for multi-line values; values shorter than 4 characters, and inputs that aren't strings, aren't masked. Masking is best-effort: a module can still leak a value in another form (encoded, or split up). The runner reads its `OPS_*` settings and then removes them from its environment, so Terraform, providers and module code never see the runner token.
- **Shutdown.** The first SIGTERM stops claiming and lets the current job finish; a claim already waiting completes (up to 20 seconds), and a job it returns is acknowledged and run like the current one, so scaling down never strands a job. A second one interrupts Terraform with SIGINT, so it releases its state lock, and the job ends as `canceled`. Set the pod's `terminationGracePeriodSeconds` to cover a job: a phase that hits `OPS_JOB_TIMEOUT` gets up to as long again to save its state.

## Settings

| Env var | Default | Purpose |
|---|---|---|
| `OPS_SERVER_URL` | required | How the runner reaches Opsolete (in k8s: the Service URL). Registry modules are fetched from here too |
| `OPS_RUNNER_TOKEN` | required | The server's `RUNNER_TOKEN` |
| `OPS_JOB_TIMEOUT` | `1h` | Hard limit per run phase |
| `OPS_TERRAFORM` | `terraform` on `PATH` | Terraform binary |
| `OPS_RUNNER_NAME` | hostname | Shown on runs; the pod name in k8s |
| `OPS_INFRACOST_API_KEY` | off | Turns on cost estimation (get a key with `infracost auth login`) |
| `OPS_INFRACOST` | `infracost` on `PATH` | Infracost binary (the runner image ships 0.10.47) |

## Credentials

Provider credentials come from the workspace's [variables]({{< relref "/docs/usage/variables" >}}), set as environment variables for Terraform on top of the runner's own environment. Runners are generic: give them no cloud credentials of their own, or every workspace without variables runs with them. Runners need no state storage credentials.

**Every runner is trusted with every workspace's secrets.** There are no runner pools: any runner, or anyone holding `RUNNER_TOKEN`, can claim any job and receives that workspace's variables (sensitive ones in clear text) and a credential for its state. Treat `RUNNER_TOKEN` like the most privileged cloud credential in any workspace: keep it in a secret store, run runners only on infrastructure as trusted as the server, and if production and non-production credentials must not share hosts, run a separate Opsolete per trust level. Module code runs on the runner with the provider credentials, so scope them to what Opsolete may deploy.

## Cost estimation

With `OPS_INFRACOST_API_KEY` set, every plan with changes (except refresh-only and drift checks) is priced with `infracost breakdown`, and the run page shows the estimated monthly cost, its change and the number of resources priced before anyone confirms the apply.

- It's an estimate: usage-based charges are left out, and resources Infracost doesn't support count as 0.
- Infracost runs on the runner with the plan's JSON and only its own key in its environment, never the job's variables. It sends price-relevant resource attributes (type, region, SKU, size) to Infracost's Cloud Pricing API, not the plan.
- If it fails, the run goes on without an estimate and the log says why.
- Admins choose the currency (US dollar or euro) under **Settings → Organization**. A change applies to plans run after it.
