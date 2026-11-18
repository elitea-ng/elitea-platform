#!/usr/bin/env bash
# Prepare everything docker-compose.sandbox.yml and
# docker-compose.sandbox-postgres-tls.yml need, for a single-host Compose
# deployment of the Docker code sandbox:
#
#   deploy/scripts/sandbox-compose-material.sh <output-directory>
#
# Run it after `standalone-stack.sh certs` (it reads the runtime CA and the
# signing keyring that script creates) and before `standalone-stack.sh up`.
#
# Inputs (environment):
#   ELITEA_SANDBOX_DENO_IMAGE   the runner image for Python, JavaScript and
#                               TypeScript (services/elitea-code-runner, target
#                               `default`), any reference the daemon resolves
#   ELITEA_SANDBOX_RUST_IMAGE   the Rust runner image (target `rust-runtime`)
#   CONTAINER_BIN               docker (default) or podman; only used to read
#                               each image's ID
#   RUNTIME_DIR                 the runtime CA directory (deploy/certs/runtime)
#
# Output, in <output-directory>:
#   deno/ rust/        one supervisor profile each: config.json plus its
#                      private material, owned by the supervisor's UID 10001
#   postgres-tls/      postgres.crt and postgres.key for the stack's database
#   worker-runtime.json  the Rust worker configuration plus its four
#                      `sandbox_runtimes` entries
#   tls/               the issued server certificates (kept for reruns)
#
# It prints the `export` lines the compose files read, so a caller can
# `eval "$(deploy/scripts/sandbox-compose-material.sh <dir>)"`. Everything else
# goes to stderr.
#
# Why the image IDs matter: the supervisor and the worker both name the runner
# image by ID and refuse a job whose image differs. Both profiles are rendered
# from the one value read here.
#
# Why chown: the supervisor runs as UID 10001 on a read-only root and refuses a
# private file that is group or world readable, so the material must be mode
# 0600 AND owned by 10001. That needs root (sudo) on a Linux host. Do not loosen
# the modes to avoid it.
set -euo pipefail

out="${1:?Usage: $0 <output-directory>}"
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
RUNTIME_DIR="${RUNTIME_DIR:-${REPO_ROOT}/deploy/certs/runtime}"
CONTAINER_BIN="${CONTAINER_BIN:-docker}"
SUPERVISOR_UID=10001
# Main's identity when it asks a supervisor to recover or authorize a Code node
# (docker-compose.sandbox-code-owner.yml). The overlay, the supervisor profiles
# and the certificate below must all carry this exact string.
CODE_OWNER_IDENTITY="spiffe://elitea.standalone/ns/default/sa/main-code-owner"

log() { printf '%s\n' "$*" >&2; }
# A rerun finds the profile directories owned by the supervisor's UID.
rm_rf() { rm -rf "$@" 2>/dev/null || sudo -n rm -rf "$@"; }

for file in runtime-ca.crt runtime-ca.key command-signing-keyring.json; do
  [ -f "$RUNTIME_DIR/$file" ] || { log "ERROR: $RUNTIME_DIR/$file is missing; run deploy/scripts/standalone-stack.sh certs first"; exit 1; }
done
: "${ELITEA_SANDBOX_DENO_IMAGE:?Set the Python/JavaScript/TypeScript runner image}"
: "${ELITEA_SANDBOX_RUST_IMAGE:?Set the Rust runner image}"

image_id() {
  local id
  id="$("$CONTAINER_BIN" image inspect --format '{{.Id}}' "$1")"
  case "$id" in
    sha256:*) [ "${#id}" -eq 71 ] || { log "ERROR: $1 has an unexpected image ID '$id'"; exit 1; } ;;
    *) log "ERROR: $1 has an unexpected image ID '$id'"; exit 1 ;;
  esac
  printf '%s' "$id"
}
deno_id="$(image_id "$ELITEA_SANDBOX_DENO_IMAGE")"
rust_id="$(image_id "$ELITEA_SANDBOX_RUST_IMAGE")"

mkdir -p "$out"
out="$(cd "$out" && pwd)"
umask 077

# Main's Code-owner client certificate, off the runtime CA, in the runtime
# directory the runtime-material service installs into Main's volume. One
# SPIFFE URI SAN and clientAuth only. Existing files are kept.
# Re-issued when missing, expiring within a day, or not signed by the current
# runtime CA (`certs` rotated it).
if ! { [ -f "$RUNTIME_DIR/code-owner-client.crt" ] && [ -f "$RUNTIME_DIR/code-owner-client.key" ] \
    && openssl verify -CAfile "$RUNTIME_DIR/runtime-ca.crt" "$RUNTIME_DIR/code-owner-client.crt" >/dev/null 2>&1 \
    && openssl x509 -in "$RUNTIME_DIR/code-owner-client.crt" -checkend 86400 -noout >/dev/null 2>&1; }; then
  owner_scratch="$(mktemp -d "$out/.owner-XXXXXX")"
  openssl req -new -newkey rsa:2048 -nodes -keyout "$owner_scratch/key" \
    -out "$owner_scratch/csr" -subj "/CN=main-code-owner" >/dev/null 2>&1
  printf 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=clientAuth\nsubjectAltName=URI:%s\n' "$CODE_OWNER_IDENTITY" > "$owner_scratch/ext"
  openssl x509 -req -in "$owner_scratch/csr" -CA "$RUNTIME_DIR/runtime-ca.crt" \
    -CAkey "$RUNTIME_DIR/runtime-ca.key" -set_serial "0x$(openssl rand -hex 16)" \
    -days 30 -sha256 -extfile "$owner_scratch/ext" -out "$owner_scratch/crt" >/dev/null 2>&1
  mv "$owner_scratch/key" "$RUNTIME_DIR/code-owner-client.key"
  mv "$owner_scratch/crt" "$RUNTIME_DIR/code-owner-client.crt"
  rm -rf "$owner_scratch"
fi

# Server certificates for both supervisor listeners and the database, off the
# runtime CA. Existing ones are kept and re-verified.
bash "${REPO_ROOT}/deploy/scripts/gen-sandbox-certs.sh" "$RUNTIME_DIR" "$out/tls" >&2

render_profile() {
  local profile="$1" port="$2" languages="$3" policy="$4" memory="$5" timeout="$6"
  local dir="$out/$profile" base="/run/elitea-sandbox/$profile" image="$7"
  rm_rf "$dir"
  mkdir -p "$dir"
  cp "$RUNTIME_DIR/runtime-ca.crt" "$dir/client-ca.pem"
  cp "$RUNTIME_DIR/runtime-ca.crt" "$dir/database-ca.pem"
  cp "$RUNTIME_DIR/command-signing-keyring.json" "$dir/command-signing-keyring.json"
  cp "$out/tls/elitea-sandbox-$profile.crt" "$dir/server.pem"
  cp "$out/tls/elitea-sandbox-$profile.key" "$dir/server.key"
  # The ledger lives in the `agentstate` database; the supervisor forces
  # verify-full, so the host name must be the certificate's: `postgres`.
  printf 'postgresql://elitea:elitea@postgres:5432/agentstate' > "$dir/database-url"
  python3 - "$dir/config.json" "$base" "$profile" "$port" "$languages" "$policy" "$memory" "$timeout" "$image" "$CODE_OWNER_IDENTITY" <<'PY'
import json, sys
path, base, profile, port, languages, policy, memory, timeout, image, owner = sys.argv[1:]
config = {
    "revision": 1,
    "listen_address": f"0.0.0.0:{port}",
    # A distinct, stable owner per profile: pending stops are partitioned by it.
    "owner": f"compose-sandbox-{profile}-1",
    "audience": f"dns:elitea-sandbox-{profile}",
    # Main, asking to recover or authorize a Code node.
    "code_owner_requester": owner,
    "ca_path": f"{base}/client-ca.pem",
    "certificate_path": f"{base}/server.pem",
    "private_key_path": f"{base}/server.key",
    "verification_keyring_path": f"{base}/command-signing-keyring.json",
    "database_url_path": f"{base}/database-url",
    "database_ca_path": f"{base}/database-ca.pem",
    "database_connections": 4,
    "image_digest": image,
    "policy_revision": policy,
    "languages": languages.split(","),
    "concurrency": 4,
    "memory_bytes": int(memory),
    "cpu_limit": 1.0,
    "timeout_seconds": int(timeout),
}
with open(path, "w") as handle:
    json.dump(config, handle, indent=2)
    handle.write("\n")
PY
  chmod 0644 "$dir/client-ca.pem" "$dir/database-ca.pem" "$dir/command-signing-keyring.json" "$dir/server.pem" "$dir/config.json"
  chmod 0600 "$dir/server.key" "$dir/database-url"
  chmod 0755 "$dir"
}
render_profile deno 9446 python,javascript,typescript python-js-v1 536870912 60 "$deno_id"
render_profile rust 9447 rust rust-v1 1073741824 120 "$rust_id"

# The stack's database key pair (root-readable; the entrypoint re-owns a copy).
rm -rf "$out/postgres-tls"
mkdir -p "$out/postgres-tls"
cp "$out/tls/postgres.crt" "$out/tls/postgres.key" "$out/postgres-tls/"
chmod 0755 "$out/postgres-tls"
chmod 0644 "$out/postgres-tls/postgres.crt"
chmod 0600 "$out/postgres-tls/postgres.key"

# The worker configuration: the shipped Rust one plus the four runtimes. The
# target host names are the compose network aliases docker-compose.sandbox.yml
# gives the supervisor; they are also the certificates' DNS names.
python3 - "${REPO_ROOT}/deploy/runtime/worker-runtime.rust.json" "$out/worker-runtime.json" "$deno_id" "$rust_id" <<'PY'
import json, sys
source, target, deno, rust = sys.argv[1:]
config = json.load(open(source))
def entry(language, host, port, image, policy, seconds):
    return {
        "language": language,
        "target": f"{host}:{port}",
        "audience": f"dns:{host}",
        "image_digest": image,
        "policy_revision": policy,
        "timeout_seconds": seconds,
        # The supervisor's `concurrency`: profiles sharing a target must agree.
        "code_slots": 4,
    }
config["sandbox_runtimes"] = [
    entry("python", "elitea-sandbox-deno", 9446, deno, "python-js-v1", 60),
    entry("javascript", "elitea-sandbox-deno", 9446, deno, "python-js-v1", 60),
    entry("typescript", "elitea-sandbox-deno", 9446, deno, "python-js-v1", 60),
    entry("rust", "elitea-sandbox-rust", 9447, rust, "rust-v1", 120),
]
with open(target, "w") as handle:
    json.dump(config, handle, indent=2)
    handle.write("\n")
PY
chmod 0644 "$out/worker-runtime.json"

# Ownership last, so every copy above is done as the invoking user.
own() {
  if [ "$(id -u)" -eq 0 ]; then
    chown -R "$SUPERVISOR_UID:$SUPERVISOR_UID" "$@"
  else
    sudo -n chown -R "$SUPERVISOR_UID:$SUPERVISOR_UID" "$@"
  fi
}
own "$out/deno" "$out/rust" || { log "ERROR: could not give $out/deno and $out/rust to UID $SUPERVISOR_UID (needs root or passwordless sudo)"; exit 1; }

docker_socket="${ELITEA_SANDBOX_DOCKER_SOCKET:-/var/run/docker.sock}"
docker_gid="${ELITEA_SANDBOX_DOCKER_GID:-$(stat -c %g "$docker_socket" 2>/dev/null || stat -f %g "$docker_socket")}"

cat <<EOF
export ELITEA_CODE_SANDBOX_LANGUAGES='python,javascript,typescript,rust'
export ELITEA_SANDBOX_DENO_MATERIAL='$out/deno'
export ELITEA_SANDBOX_RUST_MATERIAL='$out/rust'
export ELITEA_SANDBOX_WORKER_CONFIG='$out/worker-runtime.json'
export ELITEA_SANDBOX_POSTGRES_TLS='$out/postgres-tls'
export ELITEA_SANDBOX_DOCKER_SOCKET='$docker_socket'
export ELITEA_SANDBOX_DOCKER_GID='$docker_gid'
EOF
log "Sandbox material ready in $out (deno $deno_id, rust $rust_id)."
