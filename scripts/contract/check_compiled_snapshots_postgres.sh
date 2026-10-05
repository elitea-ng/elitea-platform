#!/usr/bin/env bash
set -euo pipefail

# The owning job provides an empty disposable loopback database.
# Required mode cannot turn missing fixture material into a successful skip.
compiled_contract_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
cd -- "${compiled_contract_root}/services/elitea-main"
export ELITEA_COMPILED_SNAPSHOT_PG_REQUIRED=true
# Keep the fixture URL authoritative. Do not read inherited libpq files.
export PGHOST= PGPORT= PGDATABASE= PGUSER= PGPASSWORD= PGSERVICE=
export PGSERVICEFILE=/dev/null PGPASSFILE=/dev/null
export PGSSLCERT= PGSSLKEY= PGSSLROOTCERT=
export GOMAXPROCS=2
export GOWORK=off
export GOTOOLCHAIN=local
go test -p=2 -race -count=1 -timeout=90s -run '^TestCompiledSnapshotPostgres(FixtureGuard|Lifecycle)$' ./internal/infra/db/repos
