#!/usr/bin/env bash
# The native-services action (action.yml beside this file): start the job's
# services as processes of the runner user, on 127.0.0.1 at the ports and
# credentials of the job's `services:` block, and wait until each is healthy.
#
# Inputs arrive as NS_* variables (action.yml). Everything a service writes
# lives under $RUNNER_TEMP/native-services, on the pool's work volume: the
# runner pod has 10 GiB of ephemeral storage outside it.
#
# The processes outlive this step (pg_ctl daemonises PostgreSQL; the others
# are started with nohup), and the runner ends them with the job.
set -euo pipefail

# `start.sh --http-answers <url>`: any HTTP answer at all, even a 4xx, means
# the server listens; the storage emulators answer 400 or 403 on their root
# (deploy/docker-compose.storage-test.yml). A mode of this script rather than
# a function, so wait_healthy's timeout(1) can run it.
if [ "${1:-}" = --http-answers ]; then
  code="$(curl -s --connect-timeout 2 --max-time 4 -o /dev/null -w '%{http_code}' "$2" || true)"
  [ -n "$code" ] && [ "$code" != 000 ]
  exit
fi

: "${RUNNER_TEMP:?}" "${GITHUB_OUTPUT:?}" "${NS_ACTION_DIR:?}"
root="${RUNNER_TEMP}/native-services"
logs="${root}/logs"
mkdir -p "$logs"

fail() {
  echo "::error::native-services: $*"
  exit 1
}

# alive <pid>: the process exists and is not a zombie. The daemons are
# started inside $(...), so their parent is gone and the pod's PID 1 may not
# reap them: kill -0 alone succeeds on a zombie.
alive() {
  local state
  state="$(ps -o stat= -p "$1" 2>/dev/null || true)"
  [ -n "$state" ] && [ "${state#Z}" = "$state" ]
}

# wait_healthy <name> <log> <pid|-> <tries> <command...>: one try a second,
# each bounded to 5 s (a server that accepts the connection and never answers
# must not hang the step). A process that exits before it is healthy fails at
# once, with its log.
wait_healthy() {
  local name="$1" log="$2" pid="$3" tries="$4"
  shift 4
  for ((i = 0; i < tries; i++)); do
    if timeout 5 "$@" >/dev/null 2>&1; then
      echo "native-services: ${name} is healthy after ${i}s"
      return 0
    fi
    if [ "$pid" != - ] && ! alive "$pid"; then
      tail -n 80 "$log" >&2 || true
      fail "${name} exited before it was healthy (log above; the job's artifact has all of it)"
    fi
    sleep 1
  done
  tail -n 80 "$log" >&2 || true
  fail "${name} was not healthy after ${tries}s"
}

# The image's binary must be the release the services block names, or the
# two pools would test against different servers.
require_version() {
  local name="$1" want="$2" got="$3"
  case "$got" in
    *"$want"*) echo "native-services: ${name} ${want} ($(printf '%s' "$got" | head -n 1))" ;;
    *) fail "${name}: the services block runs ${want}, this pod's image has '${got}'. Bump the image (deploy/ci-runners/image/Containerfile) and the pool's pin together with the services block" ;;
  esac
}

# Every binary the enabled services need, named at once: a pod whose image
# lacks one fails here with the list, not with a bare "command not found".
need=()
[ "${NS_POSTGRES:-false}" = true ] && need+=(postgres initdb pg_ctl pg_isready createdb)
[ "${NS_NATS:-false}" = true ] && need+=(nats-server curl)
[ "${NS_QDRANT:-false}" = true ] && need+=(qdrant curl)
[ "${NS_OPENBAO:-false}" = true ] && need+=(bao)
[ "${NS_STORAGE:-false}" = true ] && need+=(rustfs fake-gcs-server node npm yq curl)
missing=()
for tool in ps timeout "${need[@]}"; do command -v "$tool" >/dev/null || missing+=("$tool"); done
if [ ${#missing[@]} -gt 0 ]; then
  fail "this pod's image lacks ${missing[*]} (deploy/ci-runners/image/Containerfile; storage's node comes from eng-runner-env with node: \"true\")"
fi

start_bg() {
  local name="$1"
  shift
  nohup "$@" > "${logs}/${name}.log" 2>&1 &
  echo $!
}

if [ "${NS_POSTGRES:-false}" = true ]; then
  require_version postgres "(PostgreSQL) ${NS_POSTGRES_VERSION}." "$(postgres --version)"
  pg="${root}/postgres"
  mkdir -p "$pg"
  pwfile="${pg}/pwfile"
  ( umask 077; printf '%s\n' "$NS_POSTGRES_PASSWORD" > "$pwfile" )
  # The postgres image's initdb: the superuser and its password, password
  # (scram) auth over TCP, trust on the socket, UTF-8, and its locale
  # (en_US.utf8, LANG in the image), so text sorts and compares the same on
  # both legs. The pool's image generates that locale; an image built before
  # it did gets ICU's en-US, the nearest collation, and a warning.
  locale_args=(--locale=en_US.UTF-8)
  if ! locale -a 2>/dev/null | grep -qix 'en_US.utf-\?8'; then
    echo "::warning::native-services: this pod's image has no en_US.UTF-8 locale; PostgreSQL collates with ICU en-US instead of glibc's (rebuild the image: deploy/ci-runners/image)"
    locale_args=(--locale=C.UTF-8 --locale-provider=icu --icu-locale=en-US)
  fi
  initdb -D "${pg}/data" -U "$NS_POSTGRES_USER" --pwfile="$pwfile" \
    --auth-local=trust --auth-host=scram-sha-256 --encoding=UTF8 "${locale_args[@]}" \
    > "${logs}/initdb.log" 2>&1 || { cat "${logs}/initdb.log" >&2; fail "initdb failed"; }
  rm -f "$pwfile"
  # A Unix socket path is limited to ~104 bytes; on the pool this directory
  # is short, elsewhere a long RUNNER_TEMP falls back to a fresh /tmp one.
  sock="$pg"
  if [ "${#sock}" -gt 80 ]; then sock="$(mktemp -d /tmp/ns-pg.XXXXXX)"; fi
  # `localhost`: the jobs connect as localhost and as 127.0.0.1. The CI-only
  # settings (fsync, synchronous_commit, full_page_writes off) trade crash
  # safety nobody needs here for the pod's disk; every setting the services
  # block passes comes after them, so it wins.
  # shellcheck disable=SC2086 # NS_POSTGRES_ARGS is a list of -c words.
  pg_ctl -D "${pg}/data" -l "${logs}/postgres.log" -w -t 60 \
    -o "-c listen_addresses=localhost -c port=${NS_POSTGRES_PORT} -c unix_socket_directories=${sock} \
        -c fsync=off -c synchronous_commit=off -c full_page_writes=off ${NS_POSTGRES_ARGS}" \
    start > /dev/null || { tail -n 80 "${logs}/postgres.log" >&2; fail "PostgreSQL did not start"; }
  wait_healthy postgres "${logs}/postgres.log" - 60 \
    pg_isready -h 127.0.0.1 -p "$NS_POSTGRES_PORT" -U "$NS_POSTGRES_USER"
  if [ "$NS_POSTGRES_DB" != postgres ]; then
    createdb -h "$sock" -p"$NS_POSTGRES_PORT" -U "$NS_POSTGRES_USER" "$NS_POSTGRES_DB"
  fi
  wait_healthy postgres "${logs}/postgres.log" - 30 \
    pg_isready -h 127.0.0.1 -p "$NS_POSTGRES_PORT" -U "$NS_POSTGRES_USER" -d "$NS_POSTGRES_DB"
fi

if [ "${NS_NATS:-false}" = true ]; then
  bin="$(command -v nats-server)" || fail "this pod's image has no nats-server"
  require_version nats-server "v${NS_NATS_VERSION}" "$("$bin" --version)"
  store=()
  case " ${NS_NATS_ARGS} " in *" -js "*|*" --jetstream "*) store=(-sd "${root}/nats") ;; esac
  # shellcheck disable=SC2086 # NS_NATS_ARGS is a list of flags.
  pid="$(start_bg nats "$bin" ${NS_NATS_ARGS} "${store[@]}" -a 127.0.0.1 -p "$NS_NATS_PORT" -m "$NS_NATS_MONITOR_PORT")"
  probe="http://127.0.0.1:${NS_NATS_MONITOR_PORT}/healthz"
  if [ ${#store[@]} -gt 0 ]; then probe="${probe}?js-enabled-only=true"; fi
  wait_healthy nats "${logs}/nats.log" "$pid" 60 curl -fsS --connect-timeout 2 --max-time 4 "$probe"
  echo "nats-server-bin=${bin}" >> "$GITHUB_OUTPUT"
fi

if [ "${NS_QDRANT:-false}" = true ]; then
  require_version qdrant "${NS_QDRANT_VERSION#v}" "$(qdrant --version)"
  q="${root}/qdrant"
  mkdir -p "$q"
  pid="$(cd "$q" && start_bg qdrant env QDRANT__SERVICE__HOST=127.0.0.1 \
    QDRANT__SERVICE__HTTP_PORT="$NS_QDRANT_HTTP_PORT" QDRANT__SERVICE__GRPC_PORT="$NS_QDRANT_GRPC_PORT" \
    QDRANT__STORAGE__STORAGE_PATH="${q}/storage" QDRANT__STORAGE__SNAPSHOTS_PATH="${q}/snapshots" \
    QDRANT__TELEMETRY_DISABLED=true qdrant)"
  wait_healthy qdrant "${logs}/qdrant.log" "$pid" 60 curl -fsS --connect-timeout 2 --max-time 4 "http://127.0.0.1:${NS_QDRANT_HTTP_PORT}/readyz"
fi

if [ "${NS_OPENBAO:-false}" = true ]; then
  require_version bao "v${NS_OPENBAO_VERSION}" "$(bao version)"
  [ -n "$NS_OPENBAO_ROOT_TOKEN" ] || fail "openbao-root-token is required"
  [ -n "$NS_OPENBAO_TLS_CERT_DIR" ] || fail "openbao-tls-cert-dir is required"
  mkdir -p "$NS_OPENBAO_TLS_CERT_DIR"
  pid="$(start_bg openbao bao server -dev -dev-tls -dev-tls-cert-dir="$NS_OPENBAO_TLS_CERT_DIR" \
    -dev-listen-address="127.0.0.1:${NS_OPENBAO_PORT}" -dev-root-token-id="$NS_OPENBAO_ROOT_TOKEN")"
  wait_healthy openbao "${logs}/openbao.log" "$pid" 60 \
    bao status -address="https://127.0.0.1:${NS_OPENBAO_PORT}" -ca-cert="${NS_OPENBAO_TLS_CERT_DIR}/vault-ca.pem"
fi

if [ "${NS_STORAGE:-false}" = true ]; then
  # The ports and keys of deploy/docker-compose.storage-test.yml, read from
  # it: the hosted leg starts that file, and the jobs' S3_*/AZURE_*/GCS_*
  # variables name what it maps.
  compose="${GITHUB_WORKSPACE:-.}/deploy/docker-compose.storage-test.yml"
  [ -f "$compose" ] || fail "storage needs ${compose} (a sparse checkout must include deploy/)"
  host_port() { yq -r ".services[\"$1\"].ports[] | select(test(\":$2\$\")) | split(\":\") | .[-2]" "$compose"; }
  s3_port="$(host_port rustfs 9000)"
  az_port="$(host_port azurite 10000)"
  gcs_port="$(host_port fake-gcs-server 4443)"
  s3_access="$(yq -r '.services.rustfs.environment.RUSTFS_ACCESS_KEY' "$compose")"
  s3_secret="$(yq -r '.services.rustfs.environment.RUSTFS_SECRET_KEY' "$compose")"
  for v in "$s3_port" "$az_port" "$gcs_port" "$s3_access" "$s3_secret"; do
    case "$v" in ''|null) fail "could not read a port or key from ${compose}" ;; esac
  done
  s="${root}/storage"
  mkdir -p "${s}/rustfs/data" "${s}/rustfs/logs" "${s}/azurite"

  # Azurite is a Node program and the image ships no Node: the locked
  # package beside this script, installed into the work volume, while the
  # other two come up.
  cp "${NS_ACTION_DIR}/azurite/package.json" "${NS_ACTION_DIR}/azurite/package-lock.json" "${s}/azurite/"
  npm ci --prefix "${s}/azurite" --ignore-scripts --no-audit --no-fund --silent > "${logs}/azurite-npm.log" 2>&1 &
  npm_pid=$!

  rustfs_pid="$(start_bg rustfs env RUSTFS_ACCESS_KEY="$s3_access" RUSTFS_SECRET_KEY="$s3_secret" \
    RUSTFS_ADDRESS="127.0.0.1:${s3_port}" RUSTFS_CONSOLE_ENABLE=false \
    RUSTFS_VOLUMES="${s}/rustfs/data" RUSTFS_OBS_LOG_DIRECTORY="${s}/rustfs/logs" rustfs)"
  gcs_pid="$(start_bg fake-gcs-server fake-gcs-server -scheme http -host 127.0.0.1 -port "$gcs_port" \
    -public-host "127.0.0.1:${gcs_port}" -backend memory)"

  wait_healthy rustfs "${logs}/rustfs.log" "$rustfs_pid" 60 bash "${NS_ACTION_DIR}/start.sh" --http-answers "http://127.0.0.1:${s3_port}/"
  wait_healthy fake-gcs-server "${logs}/fake-gcs-server.log" "$gcs_pid" 60 \
    curl -fsS --connect-timeout 2 --max-time 4 "http://127.0.0.1:${gcs_port}/storage/v1/b"
  wait "$npm_pid" || { cat "${logs}/azurite-npm.log" >&2; fail "npm ci of Azurite failed"; }
  pid="$(start_bg azurite node "${s}/azurite/node_modules/azurite/dist/src/blob/main.js" \
    --blobHost 127.0.0.1 --blobPort "$az_port" --skipApiVersionCheck --location "${s}/azurite/data")"
  wait_healthy azurite "${logs}/azurite.log" "$pid" 60 bash "${NS_ACTION_DIR}/start.sh" --http-answers "http://127.0.0.1:${az_port}/"
fi

echo "native-services: up"
