#!/usr/bin/env bash
# `source`d (not exec'd) by the harbor verifier runner, so the Postgres server
# started here persists into the verifier process. Runs in the verifier phase
# (network up for the model gateway only); everything it needs is already baked.
#
# Brings up PostgreSQL 16 on localhost:5432 (user=postgres password=root-password
# database=postgres — the DSN the bytebase store suite uses) and exports the
# connection parameters the Go verifier reads to build its pgURL. The repo's own
# store tests reach Postgres through testcontainers-go, which pulls an image and
# needs a Docker daemon; neither is available offline, so the verifier connects
# to this locally-baked server instead.

set -euo pipefail

# Offline switches for the task's own tooling only (never baked into image ENV,
# which would break harbor's `uv tool install` at trial prep). The module cache
# and build cache are warm from the image; a miss should fail fast, not hang.
export GOFLAGS=-mod=mod
export GOPROXY=off
export GOTOOLCHAIN=local

PGPORT=5432

# 1. Start the baked cluster if it is not already accepting connections. A Modal
#    sandbox cold start may have lost the cluster's runtime state, so starting is
#    idempotent and the role password is re-applied below.
if ! pg_isready -h localhost -p "${PGPORT}" -q 2>/dev/null; then
    pg_ctlcluster 16 main start || service postgresql start
    for _ in $(seq 1 30); do
        if pg_isready -h localhost -p "${PGPORT}" -q 2>/dev/null; then break; fi
        sleep 1
    done
fi

if ! pg_isready -h localhost -p "${PGPORT}" -q 2>/dev/null; then
    echo "ERROR: postgres failed to start on localhost:${PGPORT}" >&2
    return 1 2>/dev/null || exit 1
fi

# 2. Idempotent password setup. The Dockerfile set this during build and it
#    persists in the baked cluster data directory, so a Modal cold start (which
#    only restarts the server process, not the on-disk catalog) keeps it. The
#    role authenticates over TCP with that same password, so re-apply it through
#    an authenticated connection; PGPASSWORD is ignored if a trust rule matches
#    first and required if the default scram rule does. The re-apply is defensive
#    and idempotent.
PGPASSWORD=root-password psql -h localhost -p "${PGPORT}" -U postgres -d postgres \
    -v ON_ERROR_STOP=1 \
    -tAc "ALTER USER postgres WITH PASSWORD 'root-password';" >/dev/null

# 3. Export connection parameters for the Go verifier. The verifier builds its
#    pgURL as the repo's store tests do:
#      host=$PGHOST port=$PGPORT user=$PGUSER password=$PGPASSWORD database=$PGDATABASE
#    Each test creates its own fresh database off this server (CREATE DATABASE),
#    so the verifier never collides on the shared `postgres` database.
export PGHOST=localhost
export PGPORT
export PGUSER=postgres
export PGPASSWORD=root-password
export PGDATABASE=postgres
export BYTEBASE_TEST_PG_HOST=localhost
export BYTEBASE_TEST_PG_PORT="${PGPORT}"
export BYTEBASE_TEST_PG_USER=postgres
export BYTEBASE_TEST_PG_PASSWORD=root-password
export BYTEBASE_TEST_PG_DATABASE=postgres

echo "postgres-ready: localhost:${PGPORT} (user=postgres database=postgres)"
