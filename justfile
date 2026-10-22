# Form2Stack tasks. Run inside `nix develop` (or direnv); it sets PGHOST/PGDATA/DATABASE_URL.

set shell := ["bash", "-euo", "pipefail", "-c"]

pg_opts := "-k $PGHOST -c listen_addresses="

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
    [ "$fresh" = 1 ] && createdb form2stack && createdb form2stack_test
    echo "postgres running (socket in $PGHOST)"

# stop local postgres
db-stop: env
    @pg_ctl status >/dev/null 2>&1 && pg_ctl -w stop >/dev/null && echo "postgres stopped" || true

# wipe local postgres data
db-reset: db-stop
    rm -rf "$PGDATA"

# open psql on the dev database
psql: db-start
    psql

# run the app on :8080 (starts postgres first)
run: db-start
    go run ./cmd/form2stack

# create an API token, printed once: scope registry (TF_TOKEN_<host>) or runner
token name scope="registry": db-start
    @go run ./cmd/form2stack token "{{name}}" "{{scope}}"

# run a runner on this machine against `just run` (local backend, state in .devdata/state)
runner: env
    #!/usr/bin/env bash
    set -euo pipefail
    export F2S_RUNNER_TOKEN=$(go run ./cmd/form2stack token "local-$(hostname)" runner)
    export F2S_SERVER_URL=${F2S_SERVER_URL:-http://127.0.0.1:8080} F2S_BACKEND=${F2S_BACKEND:-local} F2S_STATE_DIR=$PGHOST/state
    exec go run ./cmd/form2stack runner

# build a container image: target is app or runner; set DOCKER_HOST=ssh://user@host to build remotely
image target:
    docker build --target {{target}} -t form2stack-{{target}} .

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
    ADDR=127.0.0.1:18080 ./result/bin/form2stack > "$PGHOST/smoke.log" 2>&1 &
    pid=$!
    trap 'kill $pid 2>/dev/null' EXIT
    for _ in $(seq 50); do curl -sf 127.0.0.1:18080/readyz >/dev/null && break; sleep 0.1; done
    curl -sf 127.0.0.1:18080/readyz >/dev/null || { cat "$PGHOST/smoke.log"; exit 1; }
    code=$(curl -s -o /dev/null -w '%{http_code}' -d 'name=smoke01&location=westeurope&retention_days=1' 127.0.0.1:18080/templates/storage-account)
    [ "$code" = 303 ] || { echo "deploy POST returned $code" >&2; cat "$PGHOST/smoke.log"; exit 1; }
    curl -sf 127.0.0.1:18080/deployments | grep -q smoke01
    echo "smoke ok: ready, deployment created and listed"

# everything before a commit: tests then smoke
check: test smoke

# stop background services
down: db-stop
