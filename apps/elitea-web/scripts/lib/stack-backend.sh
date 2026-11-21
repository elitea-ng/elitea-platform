# shellcheck shell=bash
# stack-backend.sh — every call a stack script makes on its running stack, in
# one place, for two backends:
#
#   E2E_STACK=compose (default)  docker/podman compose, exactly as before: the
#                                same commands, flags and container lookups.
#   E2E_STACK=native             the self-hosted pool, which has no container
#                                runtime: scripts/e2e/native-stack.mjs runs the
#                                compose services as processes from a table in
#                                deploy/native/ (E2E_NATIVE_TABLE), and the
#                                in-container commands become host commands
#                                (psql to `postgres`, the binaries from
#                                E2E_NATIVE_BIN, rc on the host).
#
# Sourced by apps/elitea-web/scripts/e2e-stack.sh. The caller sets REPO_ROOT,
# E2E_PROJECT and COMPOSE_F (compose's project and files) before calling
# stack_init, and sources lib/container-lookup.sh and lib/compose-detect.sh
# first. A second stack script (deploy/scripts/standalone-stack.sh, stage 2)
# sets E2E_NATIVE_TABLE to its own table and reuses every function.

E2E_STACK="${E2E_STACK:-compose}"
NATIVE_STACK="${REPO_ROOT}/scripts/e2e/native-stack.mjs"

# stack_init: pick the backend. Compose mode needs a compose binary; native
# mode needs node (the supervisor) and must not look for docker or podman.
stack_init() {
  case "$E2E_STACK" in
    compose)
      detect_compose_bin
      # The first word of COMPOSE_BIN: podman or docker, for exec/run/ps.
      EXEC_BIN="${COMPOSE_BIN%% *}"
      ;;
    native)
      command -v node >/dev/null || { echo "ERROR: E2E_STACK=native needs node on PATH (the supervisor)" >&2; exit 1; }
      export E2E_NATIVE_TABLE="${E2E_NATIVE_TABLE:-${REPO_ROOT}/deploy/native/e2e-standalone.mjs}"
      : "${E2E_NATIVE_BIN:?E2E_STACK=native needs E2E_NATIVE_BIN, the build directory of the native binaries}"
      export E2E_NATIVE_BIN
      COMPOSE_BIN="native-stack"
      EXEC_BIN="native-stack"
      ;;
    *)
      echo "ERROR: E2E_STACK must be compose or native (got ${E2E_STACK})" >&2
      exit 1
      ;;
  esac
}

native_stack() {
  node "$NATIVE_STACK" "$@"
}

# stack_up: start every service and wait until each is healthy. On failure,
# print the state and the tail of every service that is not running and
# healthy into the job log, then fail.
stack_up() {
  if [ "$E2E_STACK" = native ]; then
    native_stack up --wait
    return
  fi
  # --wait: wait for every healthcheck to report healthy before returning.
  # elitea-main runs migrations on startup; postgres is its dependency.
  # --remove-orphans drops the `traefik` container a pre-#1359 stack still
  # runs; it holds E2E_PORT, which elitea-main now publishes itself.
  # A service that dies before it is healthy fails `up --wait` with only
  # "container ... exited (1)"; its reason is in its own log, and the
  # `logs` step's file is an artifact nobody opens first (#1359: elitea-main
  # refused its platform key and the job log said nothing). So on this
  # failure print, into the job log, the tail of every service that is not
  # running or not healthy, then fail as before. Never fatal itself.
  # shellcheck disable=SC2086 # COMPOSE_BIN and COMPOSE_F are word lists.
  if ! $COMPOSE_BIN $COMPOSE_F up -d --wait --remove-orphans; then
    echo "✗ The E2E stack did not come up. Services that are not running and healthy:" >&2
    # shellcheck disable=SC2086
    $COMPOSE_BIN $COMPOSE_F ps -a >&2 || true
    # A one-shot that exited 0 (bucket init, NATS bootstrap) is fine.
    # shellcheck disable=SC2086
    for service in $($COMPOSE_BIN $COMPOSE_F ps -a --format '{{.Service}} {{.State}} {{.ExitCode}} {{.Health}}' 2>/dev/null \
        | awk '($2 != "running" && $3 != "0") || ($2 == "running" && $4 != "" && $4 != "healthy") { print $1 }' \
        | sort -u); do
      echo "=== ${service}: last 80 log lines ===" >&2
      # shellcheck disable=SC2086
      $COMPOSE_BIN $COMPOSE_F logs --no-color --tail 80 "$service" >&2 || true
    done
    return 1
  fi
}

# stack_down: stop the stack and drop its data (compose's volumes).
stack_down() {
  if [ "$E2E_STACK" = native ]; then
    native_stack down -v
    return
  fi
  # shellcheck disable=SC2086
  $COMPOSE_BIN $COMPOSE_F down --remove-orphans -v
}

# stack_restart <service>: restart one service; does not wait for it.
stack_restart() {
  if [ "$E2E_STACK" = native ]; then
    native_stack restart "$@"
    return
  fi
  # shellcheck disable=SC2086
  $COMPOSE_BIN $COMPOSE_F restart "$@"
}

# stack_ps [--all]: the services and their state, for a log.
stack_ps() {
  if [ "$E2E_STACK" = native ]; then
    native_stack ps
    return
  fi
  # shellcheck disable=SC2086
  $COMPOSE_BIN $COMPOSE_F ps "$@"
}

# stack_logs: every service's log, with timestamps, uncoloured.
stack_logs() {
  if [ "$E2E_STACK" = native ]; then
    native_stack logs --timestamps
    return
  fi
  # shellcheck disable=SC2086
  $COMPOSE_BIN $COMPOSE_F logs --no-color --timestamps
}

# stack_container <service> [pattern]: a handle for stack_psql/stack_exec_main,
# empty when the service is not running. Compose: the container name, found
# among `ps` names by pattern (default the service), preferring this project's
# (container-lookup.sh). Native: the service name, when the supervisor has it
# running.
stack_container() {
  local service="$1" pattern="${2:-$1}"
  if [ "$E2E_STACK" = native ]; then
    native_stack ps --running "$service" 2>/dev/null || true
    return
  fi
  resolve_container_name "$E2E_PROJECT" "$pattern" "$($EXEC_BIN ps --format '{{.Names}}' 2>/dev/null || true)"
}

# stack_psql <container> <psql args…>: psql in the stack's database, stdin
# passed through. Compose: inside the postgres container (its socket, trust).
# Native: to `postgres` over TCP as the compose service's user.
stack_psql() {
  local container="$1"
  shift
  if [ "$E2E_STACK" = native ]; then
    PGPASSWORD="${E2E_NATIVE_PGPASSWORD:-elitea}" psql -h postgres -p 5432 "$@"
    return
  fi
  $EXEC_BIN exec -i "$container" psql "$@"
}

# stack_exec_main <container> /<binary> [args…]: one of elitea-main's image
# binaries (/elitea-migrate, /elitea-main -healthcheck) with elitea-main's
# environment. Native: the same binary from E2E_NATIVE_BIN, under the
# supervisor's `exec`, which gives it elitea-main's table environment.
stack_exec_main() {
  local container="$1" binary="$2"
  shift 2
  if [ "$E2E_STACK" = native ]; then
    native_stack exec elitea-main -- "${E2E_NATIVE_BIN}/${binary#/}" "$@"
    return
  fi
  $EXEC_BIN exec "$container" "$binary" "$@"
}

# stack_probe <image> <host dir|""> <script>: a one-off tool on the stack's
# network, the host directory (if any) at $PROBE_SRC. Compose: `run --rm` of
# the image on the project network, the directory mounted read-only at /src.
# Native: the same script with sh on the host, whose names resolve through
# the pod's hostAliases; the image's tool must be on PATH (the runner image
# carries rc, psql, openssl).
stack_probe() {
  local image="$1" dir="$2" script="$3"
  if [ "$E2E_STACK" = native ]; then
    PROBE_SRC="$dir" sh -c "$script"
    return
  fi
  if [ -n "$dir" ]; then
    $EXEC_BIN run --rm --network "${E2E_PROJECT}_default" -v "${dir}:/src:ro" -e PROBE_SRC=/src \
      --entrypoint sh "$image" -c "$script"
  else
    $EXEC_BIN run --rm --network "${E2E_PROJECT}_default" --entrypoint sh "$image" -c "$script"
  fi
}
