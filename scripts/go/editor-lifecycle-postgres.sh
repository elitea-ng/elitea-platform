#!/usr/bin/env bash
# Run only the exact disposable editor fixture. Never use product database settings.
set -euo pipefail

if [ -z "${ELITEA_EDITOR_TEST_DATABASE_URL:-}" ]; then
    echo "editor lifecycle: ELITEA_EDITOR_TEST_DATABASE_URL is required" >&2
    exit 1
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
# TestMain otherwise creates its package template from these service settings.
unset ELITEA_TEST_DATABASE_URL ELITEA_TEST_USE_SERVICE_DATABASE_URL DATABASE_URL
export ELITEA_REQUIRE_EDITOR_POSTGRES_TEST=true
export GOMAXPROCS=2

artifact_dir="${ELITEA_EDITOR_TEST_ARTIFACT_DIR:-}"
if [ -z "$artifact_dir" ]; then
    artifact_dir="$(mktemp -d)"
    trap 'rm -rf "$artifact_dir"' EXIT
fi
mkdir -p "$artifact_dir"
log="$artifact_dir/editor-postgres.jsonl"
cd "$repo_root/services/elitea-main"
if go test -p 2 -race -json -count=1 -timeout=5m ./internal/infra/db/repos \
    -run '^(TestEditorLifecycle.*|TestEditorEmptyStopPostgres)$' >"$log" 2>&1; then
    python3 "$script_dir/editor-lifecycle-gate.py" "$log" \
        >"$artifact_dir/summary.json"
    cat "$artifact_dir/summary.json"
else
    status=$?
    echo "editor lifecycle: Go tests failed; see ${log}" >&2
    exit "$status"
fi
