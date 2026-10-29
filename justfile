# Opsolete tasks. Run inside `nix develop` (or direnv); it sets PGHOST/PGDATA/DATABASE_URL.

set shell := ["bash", "-euo", "pipefail", "-c"]

pg_opts := "-k $PGHOST -c listen_addresses="

# Same as the dev shell sets; the fallback covers shells opened before STATE_STORE existed.
export STATE_STORE := env("STATE_STORE", "file://" + env("PGHOST", justfile_directory() + "/.devdata") + "/state-store?create_dir=1&metadata=skip")

# list recipes
default:
    @just --list

[private]
env:
    @[ -n "${PGDATA:-}" ] || { echo "not in the dev shell: run 'nix develop' or 'direnv allow'" >&2; exit 1; }

# start local postgres (initialises it on first run)
db-start: env
    #!/usr/bin/env bash
    set -euo pipefail
    pg_ctl status >/dev/null 2>&1 && exit 0
    mkdir -p "$PGHOST"
    fresh=0
    [ -d "$PGDATA" ] || { initdb --auth=trust --no-locale --encoding=UTF8 >/dev/null; fresh=1; }
    pg_ctl -o "{{pg_opts}}" -l "$PGHOST/postgres.log" -w start >/dev/null
    [ "$fresh" = 1 ] && createdb opsolete && createdb opsolete_test
    echo "postgres running (socket in $PGHOST)"

# stop local postgres
db-stop: env
    @pg_ctl status >/dev/null 2>&1 && pg_ctl -w stop >/dev/null && echo "postgres stopped" || true

# wipe local postgres data and the state files that belong to it
db-reset: db-stop
    rm -rf "$PGDATA" "$PGHOST/state-store"

# load the examples (registry module, workspaces, variable set) into an empty dev database; a local runner applies them
seed: db-start
    #!/usr/bin/env bash
    set -euo pipefail
    base=http://127.0.0.1:18081 bin="$PGHOST/ops-seed" out="$PGHOST/seed.out"
    go build -o "$bin" ./cmd/opsolete
    ADDR=127.0.0.1:18081 "$bin" > "$PGHOST/seed.log" 2>&1 &
    pids=$!
    trap 'kill $pids 2>/dev/null || true' EXIT
    for _ in $(seq 100); do curl -sf $base/readyz >/dev/null && break; sleep 0.1; done
    curl -sf $base/readyz >/dev/null || { cat "$PGHOST/seed.log"; exit 1; }
    # req <path> <curl args>: fails on anything but 2xx/303, e.g. when the examples already exist (use `just reseed`)
    req() {
        local path=$1; shift
        code=$(curl -s -o "$out" -w '%{http_code}' "$@" "$base$path")
        [[ $code == 2* || $code == 303 ]] || { echo "seed: $path returned $code" >&2; sed 's/<[^>]*>//g' "$out" | grep -v '^\s*$' | tail -5 >&2; exit 1; }
    }

    echo "registry: examples/greeting 1.0.0 and 1.1.0"
    for v in 1.0.0 1.1.0; do
        tar czf "$PGHOST/greeting-$v.tar.gz" -C examples/modules/greeting/$v .
        req /registry -F namespace=examples -F name=greeting -F system=terraform -F version=$v -F archive=@"$PGHOST/greeting-$v.tar.gz"
    done

    echo "variable set: example-azure (fake credentials)"
    req /variable-sets -d name=example-azure -d description="Example credentials with fake values"
    req /variable-sets/example-azure/variables -d category=env -d key=ARM_TENANT_ID -d value=00000000-0000-0000-0000-000000000001
    req /variable-sets/example-azure/variables -d category=env -d key=ARM_CLIENT_SECRET -d value=not-a-real-secret -d sensitive=on

    echo "workspaces: greeting-dev (auto-apply), greeting-prod (waits for confirmation), empty, credentials-only"
    req /templates/greeting -d ops-workspace=greeting-dev -d ops-auto-apply=on -d greeting=hello -d name=world
    req /templates/greeting -d ops-workspace=greeting-prod -d greeting=hi -d "name=Opsolete team"
    req /variable-sets/example-azure/workspaces -d workspace=greeting-prod
    req /workspaces/new -d name=empty
    req /workspaces/new -d name=credentials-only
    req /workspaces/credentials-only/variables -d category=env -d key=ARM_SUBSCRIPTION_ID -d value=00000000-0000-0000-0000-000000000002
    req /workspaces/credentials-only/variables -d category=env -d key=ARM_CLIENT_SECRET -d value=not-a-real-secret -d sensitive=on

    echo "runner: applying greeting-dev, planning greeting-prod"
    OPS_RUNNER_TOKEN=$RUNNER_TOKEN OPS_SERVER_URL=$base "$bin" runner > "$PGHOST/seed-runner.log" 2>&1 &
    pids="$pids $!"
    for ws in greeting-dev:applied greeting-prod:needs_confirmation; do
        for _ in $(seq 180); do curl -sf "$base/workspaces/${ws%%:*}" | grep -q "status ${ws#*:}\"" && continue 2; sleep 1; done
        echo "seed: ${ws%%:*} never reached ${ws#*:}; see $PGHOST/seed-runner.log" >&2; exit 1
    done
    echo "seed ok: run 'just run' and open http://localhost:8080/workspaces"

# wipe the dev database and state files, then load the examples
reseed: db-reset seed

# open psql on the dev database
psql: db-start
    psql

# run the app on :8080 (starts postgres first)
run: db-start
    go run ./cmd/opsolete

# create a registry API token (TF_TOKEN_<host>), printed once
token name: db-start
    @go run ./cmd/opsolete token "{{name}}"

# run a runner on this machine against `just run`
runner: env
    #!/usr/bin/env bash
    set -euo pipefail
    export OPS_RUNNER_TOKEN=$RUNNER_TOKEN
    unset RUNNER_TOKEN # OPS_* is hidden from jobs, this isn't
    export OPS_SERVER_URL=${OPS_SERVER_URL:-http://127.0.0.1:8080}
    exec go run ./cmd/opsolete runner

# build a container image with nix and load it into docker: target is app or runner (needs a linux builder on macOS)
image target:
    nix build -L .#packages.x86_64-linux.image-{{target}} -o result-{{target}}
    docker load < result-{{target}}

# fmt, vet and all tests including the DB tests
test: db-start
    gofmt -l . | (! grep .) || { echo "run: gofmt -w ." >&2; exit 1; }
    go vet ./...
    go test -count=1 -p 1 ./...  # -p 1: DB tests share one database and its job queue

# build the nix package into ./result
build:
    nix build

# build the package, boot it against postgres and submit a deployment end-to-end
smoke: build db-start
    #!/usr/bin/env bash
    set -euo pipefail
    ADDR=127.0.0.1:18080 ./result/bin/opsolete > "$PGHOST/smoke.log" 2>&1 &
    pid=$!
    trap 'kill $pid 2>/dev/null' EXIT
    for _ in $(seq 50); do curl -sf 127.0.0.1:18080/readyz >/dev/null && break; sleep 0.1; done
    curl -sf 127.0.0.1:18080/readyz >/dev/null || { cat "$PGHOST/smoke.log"; exit 1; }
    ws="smoke-$(date +%s)"
    code=$(curl -s -o /dev/null -w '%{http_code}' -d "ops-workspace=$ws&name=smoke01&location=westeurope&retention_days=1" 127.0.0.1:18080/templates/storage-account)
    [ "$code" = 303 ] || { echo "create workspace POST returned $code" >&2; cat "$PGHOST/smoke.log"; exit 1; }
    curl -sf 127.0.0.1:18080/workspaces | grep -q "$ws"
    echo "smoke ok: ready, workspace created and listed"

# everything before a commit: tests then smoke
check: test smoke

# stop background services
down: db-stop

# preview the documentation site (docs/) on http://localhost:1313/Opsolete/
docs:
    hugo server -s docs
