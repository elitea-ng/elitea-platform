#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 ]]; then
  echo "usage: $0 <prepare|config|up|ps|logs|preflight> [centry-repository]" >&2
  exit 2
fi

action="$1"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
platform_dir="$(cd "$script_dir/../.." && pwd -P)"
centry_input="${2:-${CENTRY_DIR:-$platform_dir/../centry}}"

# ── Edge middleware references (#368, guarding the fix from #338) ─────────────
#
# Traefik does not fail a stack whose router names a middleware no loaded file
# defines. It logs the error, drops the router, and keeps serving. Here the
# dropped routers are the ones that select Go, and base.yml holds a
# PathPrefix("/") catch-all to pylon at priority 1. The caller then gets HTTP 200
# from pylon on a path the configuration says goes to elitea-main, and the
# header-stripping middleware the dropped router carried never runs either.
#
# This is the same command `task deploy:check-edge` runs and the same command
# the No Binaries workflow runs. Calling the test is the reuse: a second copy of
# the resolution logic written in bash or jq would drift from the Go one, and
# the day they disagree is the day nobody trusts either.
#
# -count=1 is required, not tidiness. The test reads YAML from deploy/, outside
# that Go module, so the test cache does not notice an edit to the edge files
# and can serve a stale pass.
#
# It runs HERE, before the private-repository inputs are checked, for two
# reasons. The edge file belongs to this repository, so its validity does not
# depend on centry. And a check placed after those inputs is unreachable for
# anybody who does not have them — a gate that never runs, which is the defect
# this guard exists to remove.
#
# It fails closed when go is absent. A skip would report success on an
# unvalidated edge.
validate_edge() {
  if ! command -v go >/dev/null 2>&1; then
    echo "go is not on PATH, so the Traefik edge cannot be validated." >&2
    echo "Install Go, or run 'task deploy:check-edge' from a machine that has it." >&2
    exit 2
  fi
  if ! go test -C "$platform_dir/services/elitea-main" -count=1 ./tests/deployedge/...; then
    echo "" >&2
    echo "The Traefik edge configuration is invalid. Deployment is refused." >&2
    echo "Traefik would NOT fail on this: it drops the router and keeps serving," >&2
    echo "so the traffic falls through to pylon and the header strip stops running." >&2
    exit 2
  fi
}
validate_edge

if [[ ! -d "$centry_input" ]]; then
  echo "Centry repository does not exist: $centry_input" >&2
  exit 2
fi
centry_dir="$(cd "$centry_input" && pwd -P)"

default_env="$centry_dir/envs/default.env"
override_env="$centry_dir/envs/override.env"
for required in \
  "$centry_dir/docker-compose.yml" \
  "$centry_dir/hybrid_auth/docker-compose.pov.yml" \
  "$centry_dir/hybrid_auth/docker-compose.indexing-checkpoint.yml" \
  "$centry_dir/hybrid_auth/bootstrap-auth-pov.sh" \
  "$centry_dir/hybrid_auth/bootstrap-runtime.sh" \
  "$default_env" \
  "$override_env"
do
  if [[ ! -f "$required" ]]; then
    echo "required mixed-deployment input is missing: $required" >&2
    exit 2
  fi
done

# Compose env files are also shell-compatible in the current Centry baseline.
# Loading them lets the bootstrap reuse the same secrets without copying or
# printing them. override.env remains local and untracked.
set -a
# shellcheck disable=SC1090
source "$default_env"
# shellcheck disable=SC1090
source "$override_env"
set +a

runtime_root="${ELITEA_HYBRID_RUNTIME_DIR:-$script_dir/.runtime}"
export ELITEA_PLATFORM_DIR="$platform_dir"
export ELITEA_CENTRY_DIR="$centry_dir"
export ELITEA_AUTH_POV_RUNTIME_DIR="$runtime_root"
export ELITEA_INDEX_ROUTE_FILE="$script_dir/traefik/index-routes.yml"
export ELITEA_MAIN_IMAGE="${ELITEA_MAIN_IMAGE:-elitea-ng/elitea-main:pov-integration}"
export ELITEA_WORKER_IMAGE="${ELITEA_WORKER_IMAGE:-elitea-ng/elitea-worker-python:pov-integration}"
export ELITEA_INDEXER_WORKER_IMAGE="${ELITEA_INDEXER_WORKER_IMAGE:-$ELITEA_WORKER_IMAGE}"

"$script_dir/prepare-runtime.sh" "$centry_dir" "$runtime_root"

deploy_ui() {
  local ui_input="${ELITEA_HYBRID_UI_DIR:-}"
  if [[ -z "$ui_input" ]]; then
    return
  fi
  if [[ ! -f "$ui_input/package.json" ]]; then
    echo "EliteaUI checkout does not contain package.json: $ui_input" >&2
    exit 2
  fi

  local ui_dir
  ui_dir="$(cd "$ui_input" && pwd -P)"
  local ui_target="$centry_dir/pylon_main/plugins/elitea_core/static/ui/dist"
  local ui_stage
  ui_stage="$(mktemp -d "$runtime_root/.elitea-ui-dist.XXXXXX")"
  trap 'rm -rf "$ui_stage"' RETURN

  (
    cd "$ui_dir"
    VITE_SERVER_URL="${ELITEA_HYBRID_UI_SERVER_URL:-/api/v2}" npm run build
  )
  cp -R "$ui_dir/dist/." "$ui_stage/"
  rm -rf "$ui_target"
  mkdir -p "$(dirname "$ui_target")"
  mv "$ui_stage" "$ui_target"
  trap - RETURN

  local ui_revision="unknown"
  if git -C "$ui_dir" rev-parse --verify HEAD >/dev/null 2>&1; then
    ui_revision="$(git -C "$ui_dir" rev-parse --short=12 HEAD)"
  fi
  echo "deployed EliteaUI revision $ui_revision from $ui_dir"
}

compose=(
  docker compose
  --project-directory "$centry_dir"
  --env-file "$default_env"
  --env-file "$override_env"
  -f "$centry_dir/docker-compose.yml"
  -f "$centry_dir/hybrid_auth/docker-compose.pov.yml"
  -f "$centry_dir/hybrid_auth/docker-compose.indexing-checkpoint.yml"
  -f "$script_dir/pov-compose.yml"
)

validate_model() {
  local rendered
  rendered="$(mktemp "$runtime_root/.compose-model.json.XXXXXX")"
  chmod 600 "$rendered"
  trap 'rm -f "$rendered"' EXIT

  "${compose[@]}" --profile runtime config --format json > "$rendered"

  jq -e \
    --arg route "$ELITEA_INDEX_ROUTE_FILE" \
    --arg runtime "$runtime_root/runtime/indexer-runtime-v2.json" \
    --arg checkpoint "$runtime_root/runtime/agent-checkpoint-connection" \
    --arg main_database_config "$runtime_root/runtime/pylon-main-shared.yml" \
    --arg auth_database_config "$runtime_root/runtime/pylon-auth-core.yml" \
    --arg interface "$script_dir/runtime-interface-litellm.yml" \
    --arg engine "$script_dir/runtime-engine-litellm.yml" \
    '
      .services["elitea-main"].environment.ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM
      == "commands.v1.index.ingest.indexing.shared.2.0"
      and .services["elitea-main"].environment.ELITEA_ARTIFACTS_ENABLED
        == "true"
      and .services["elitea-main"].environment.STORAGE_BACKEND == "s3"
      and .services["elitea-main"].environment.STORAGE_CONTAINER == "elitea-artifacts"
      and .services["elitea-main"].environment.S3_ENDPOINT_URL
        == "http://runtime-artifacts:9000"
      and .services["runtime-artifacts"] != null
      and .services["runtime-artifacts-bucket-init"] != null
      and .services["elitea-main"].environment.ELITEA_RUNTIME_INDEX_SCHEDULING_ENABLED
      == "true"
      and .services["elitea-main"].environment.ELITEA_RUNTIME_SCHEDULER_INSTANCE_ID
        == "elitea-main-pov-1"
      and .services["elitea-main"].environment.ELITEA_RUNTIME_INDEX_INGEST_CONSUMER_GROUP
        == "elitea-indexer-worker-v2"
      and .services["elitea-main"].environment.ELITEA_RUNTIME_AGENT_EXECUTION_DISPATCH_ENABLED
        == "true"
      and .services["elitea-main"].environment.ELITEA_RUNTIME_AGENT_EXECUTION_COMMAND_STREAM
        == .services["elitea-main"].environment.ELITEA_RUNTIME_INDEX_INGEST_COMMAND_STREAM
      and .services["elitea-main"].environment.ELITEA_RUNTIME_AGENT_EXECUTION_CONSUMER_GROUP
        == .services["elitea-main"].environment.ELITEA_RUNTIME_INDEX_INGEST_CONSUMER_GROUP
      and .services["elitea-main"].environment.ELITEA_RUNTIME_CURRENT_MAIN_BASE_URL
        == "https://elitea-gateway"
      and .services["elitea-main"].environment.ELITEA_RUNTIME_DB_ADMISSION_MAX_CONNS == "3"
      and .services["elitea-main"].environment.ELITEA_RUNTIME_DB_CONTROL_MAX_CONNS == "2"
      and .services["elitea-main"].environment.ELITEA_RUNTIME_DB_OUTPUT_MAX_CONNS == "2"
      and .services["elitea-main"].environment.ELITEA_RUNTIME_DB_REPLAY_MAX_CONNS == "1"
      and .services["elitea-main"].environment.ELITEA_RUNTIME_DB_TERMINAL_MAX_CONNS == "1"
      and .services["elitea-main"].environment.ELITEA_RUNTIME_DB_CONTENT_MAX_CONNS == "1"
      and .services["elitea-main"].environment.ELITEA_DATABASE_MAX_CONNS == "4"
      and (.services["elitea-main"].networks | has("runtime_gateway"))
      and any(.services.auth_gateway.volumes[];
        .source == $route and .target == "/etc/traefik/dynamic/index.yml")
      and any(.services["elitea-indexer-worker"].volumes[];
        .source == $runtime and .target == "/run/elitea-runtime/indexer-runtime.json")
      and any(.services["elitea-indexer-worker"].volumes[];
        .source == $checkpoint and .target == "/run/elitea-runtime/agent-checkpoint-connection")
      and .services["elitea-indexer-worker"].environment.ELITEA_SENSITIVE_TOOLS
        == "{\"*\":[\"delete_file\"]}"
      and any(.services.pylon_main.volumes[];
        .source == $interface and .target == "/data/configs/runtime_interface_litellm.yml")
      and any(.services.pylon_main.volumes[];
        .source == $main_database_config and .target == "/data/configs/shared.yml")
      and any(.services.pylon_auth.volumes[];
        .source == $auth_database_config and .target == "/data/configs/auth_core.yml")
      and any(.services.pylon_indexer.volumes[];
        .source == $engine and .target == "/data/configs/runtime_engine_litellm.yml")
      and (.services["elitea-litellm"].build.context | endswith("/hybrid_auth"))
      and .services["runtime-index-v2-bootstrap"] != null
      and .services["index-v1-cutover-preflight"] != null
    ' "$rendered" >/dev/null

  grep -q 'go-current-notification-events:' "$ELITEA_INDEX_ROUTE_FILE"
  grep -q '/api/v2/notifications/events/prompt_lib/' "$ELITEA_INDEX_ROUTE_FILE"
  # #337. The artifact plane resolves to elitea-main. Asserting the router
  # NAME alone would pass on a router that still names current-main, so the
  # service is asserted too — by the Go edge gate above, which resolves every
  # router's service, and here by refusing the retired router name outright.
  grep -q 'go-artifacts:' "$ELITEA_INDEX_ROUTE_FILE"
  grep -q '/artifacts/s3/' "$ELITEA_INDEX_ROUTE_FILE"
  grep -q '/api/v2/artifacts/objects/' "$ELITEA_INDEX_ROUTE_FILE"
  if grep -q 'runtime-worker-current-artifacts:' "$ELITEA_INDEX_ROUTE_FILE"; then
    echo "the retired pylon artifact router is back in $ELITEA_INDEX_ROUTE_FILE" >&2
    exit 2
  fi
  grep -q 'go-current-notifications:' "$ELITEA_INDEX_ROUTE_FILE"
  grep -q '/api/v2/notifications/notifications/prompt_lib/' "$ELITEA_INDEX_ROUTE_FILE"
  grep -q '/api/v2/notifications/notification/prompt_lib/' "$ELITEA_INDEX_ROUTE_FILE"
  grep -q '/api/v2/elitea_core/messages/prompt_lib/' "$ELITEA_INDEX_ROUTE_FILE"
  grep -q 'agent.execute.application.v1' "$ELITEA_INDEX_ROUTE_FILE"
  grep -q 'agent.execute.adhoc.v1' "$ELITEA_INDEX_ROUTE_FILE"
  grep -q '/api/v2/elitea_core/regenerate/prompt_lib/' "$ELITEA_INDEX_ROUTE_FILE"
  grep -q 'agent.regenerate.v1' "$ELITEA_INDEX_ROUTE_FILE"
  grep -q '/api/v2/elitea_core/continue_predict/prompt_lib/' "$ELITEA_INDEX_ROUTE_FILE"
  grep -q 'agent.continue.hitl.v1' "$ELITEA_INDEX_ROUTE_FILE"
  grep -q 'agent.continue.authorization.v1' "$ELITEA_INDEX_ROUTE_FILE"
  grep -q '/api/v2/elitea_core/task/prompt_lib/' "$ELITEA_INDEX_ROUTE_FILE"
  grep -q 'runtime-go-current-next-input-suggestion-policy:' "$ELITEA_INDEX_ROUTE_FILE"
  grep -q '/api/v2/elitea_core/next_input_suggestion_config/prompt_lib/' "$ELITEA_INDEX_ROUTE_FILE"
  grep -q 'runtime-worker-go-current-models:' "$ELITEA_INDEX_ROUTE_FILE"
  grep -q '/api/v2/configurations/models/' "$ELITEA_INDEX_ROUTE_FILE"

  rm -f "$rendered"
  trap - EXIT
}

case "$action" in
  prepare)
    ;;
  config)
    validate_model
    echo "mixed Centry/Go/worker Compose model is valid"
    ;;
  up)
    validate_model
    deploy_ui
    # prepare-runtime.sh replaces the ACL file atomically. A running Redis
    # container remains bound to the previous inode and does not reload ACLs,
    # so recreate only this local PoV service before bringing up consumers.
    # Its AOF/data volume is retained; this makes one-command deploys apply
    # channel permissions without granting an application user +acl.
    "${compose[@]}" --profile runtime up \
      -d \
      --force-recreate \
      --no-deps \
      --wait \
      --wait-timeout "${ELITEA_HYBRID_WAIT_TIMEOUT:-600}" \
      runtime_redis
    "${compose[@]}" --profile runtime up \
      -d \
      --build \
      --wait \
      --wait-timeout "${ELITEA_HYBRID_WAIT_TIMEOUT:-600}"
    # auth_gateway bind-mounts the route file itself. Editors commonly replace
    # that file atomically, leaving an already-running container attached to
    # the previous inode even though the file provider has watch enabled.
    # Recreate only the gateway so this one-command deploy always activates
    # the routes from the selected platform checkout.
    "${compose[@]}" --profile runtime up \
      -d \
      --force-recreate \
      --no-deps \
      --wait \
      --wait-timeout "${ELITEA_HYBRID_WAIT_TIMEOUT:-600}" \
      auth_gateway
    "${compose[@]}" --profile runtime ps
    ;;
  ps)
    validate_model
    "${compose[@]}" --profile runtime ps
    ;;
  logs)
    validate_model
    "${compose[@]}" --profile runtime logs -f --tail=100
    ;;
  preflight)
    validate_model
    "${compose[@]}" --profile runtime run --rm index-v1-cutover-preflight
    ;;
  *)
    echo "unsupported action: $action" >&2
    exit 2
    ;;
esac
