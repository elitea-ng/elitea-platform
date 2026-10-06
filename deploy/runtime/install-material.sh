#!/bin/sh
# Install the generated runtime material into per-consumer volumes.
#
# Why this indirection instead of bind-mounting deploy/certs/runtime straight
# into each service: internal/security/securefile requires private material to
# carry EXACTLY owner-only bits (0600/0400) and to be readable by the reading
# process. Under rootless podman a bind-mounted host file arrives owned by
# container uid 0, so a 0600 file is unreadable by elitea-main (distroless
# `nonroot`, uid 65532) and by the worker (uid 10001) — and the fix cannot be "chmod
# 644 on the host", because securefile rejects any group/other bit on private
# material. Copying into a named volume is the only place we can set owner AND
# mode independently per consumer.
#
# Each consumer gets ONLY the material it needs. The worker never sees the
# server private keys or the signing key; elitea-main never sees the worker's
# client key.
set -eu

SRC=/src
COMPILED_ENABLED=${ELITEA_RUNTIME_RUST_COMPILED_SNAPSHOTS_ENABLED:-false}
case "$COMPILED_ENABLED" in
  false) [ -z "${ELITEA_RUNTIME_RUST_COMPILED_SNAPSHOTS_PROFILES_SHA256:-}" ] || exit 1 ;;
  true)
    pin=${ELITEA_RUNTIME_RUST_COMPILED_SNAPSHOTS_PROFILES_SHA256:-}
    [ "${#pin}" -eq 64 ] || exit 1
    case "$pin" in *[!a-f0-9]*) exit 1 ;; esac
    for name in rust-compiled-profiles.json agent-checkpoint-connection; do
      [ -f "$SRC/$name" ] && [ ! -L "$SRC/$name" ] || exit 1
    done
    size=$(wc -c < "$SRC/rust-compiled-profiles.json")
    [ "$size" -gt 0 ] && [ "$size" -le 1048576 ] || exit 1
    size=$(wc -c < "$SRC/agent-checkpoint-connection")
    [ "$size" -gt 0 ] && [ "$size" -le 16384 ] || exit 1
    measured=$(sha256sum "$SRC/rust-compiled-profiles.json")
    [ "${measured%% *}" = "$pin" ] || exit 1
    ;;
  *) exit 1 ;;
esac
CODE_OWNER_ENABLED=${ELITEA_RUNTIME_CODE_OWNER_RECOVERY_ENABLED:-false}
CODE_PLATFORM_ENABLED=${ELITEA_RUNTIME_CODE_PLATFORM_ENABLED:-false}
CODE_DEBUG_ENABLED=${ELITEA_RUNTIME_CODE_DEBUG_ARTIFACTS_ENABLED:-false}
for flag in "$CODE_OWNER_ENABLED" "$CODE_PLATFORM_ENABLED" "$CODE_DEBUG_ENABLED"; do
  case "$flag" in true|false) ;; *) exit 1 ;; esac
done
if [ "$CODE_PLATFORM_ENABLED" = true ] || [ "$CODE_DEBUG_ENABLED" = true ]; then
  [ "$CODE_OWNER_ENABLED" = true ] || exit 1
fi
# These files enter Main's volume only. No private broker material enters a runner.
if [ "$CODE_OWNER_ENABLED" = true ]; then
  for name in code-owner-client.crt code-owner-client.key; do
    [ -f "$SRC/$name" ] && [ ! -L "$SRC/$name" ] || exit 1
    size=$(wc -c < "$SRC/$name")
    [ "$size" -gt 0 ] && [ "$size" -le 1048576 ] || exit 1
  done
fi
if [ "$CODE_PLATFORM_ENABLED" = true ]; then
  [ -f "$SRC/code-platform-content-keys.json" ] && [ ! -L "$SRC/code-platform-content-keys.json" ] || exit 1
  size=$(wc -c < "$SRC/code-platform-content-keys.json")
  [ "$size" -gt 0 ] && [ "$size" -le 4096 ] || exit 1
fi
if [ "$CODE_DEBUG_ENABLED" = true ]; then
  [ -f "$SRC/agent-checkpoint-connection" ] && [ ! -L "$SRC/agent-checkpoint-connection" ] || exit 1
  size=$(wc -c < "$SRC/agent-checkpoint-connection")
  [ "$size" -gt 0 ] && [ "$size" -le 16384 ] || exit 1
fi
[ -r "$SRC/runtime-ca.crt" ] || {
  echo "ERROR: $SRC/runtime-ca.crt missing — run deploy/scripts/gen-runtime-certs.sh" >&2
  exit 1
}

# install <dest-dir> <uid:gid> <mode> <name>...
install_files() {
  dest="$1"; owner="$2"; mode="$3"; shift 3
  for name in "$@"; do
    cp "$SRC/$name" "$dest/$name"
    chown "$owner" "$dest/$name"
    chmod "$mode" "$dest/$name"
  done
}

# ── elitea-main (distroless nonroot, uid 65532) ──────────────────────────────
MAIN=/dst/main
mkdir -p "$MAIN"; chown 65532:65532 "$MAIN"; chmod 755 "$MAIN"
# PublicMaterial: readable by anyone, writable by nobody but the owner.
install_files "$MAIN" 65532:65532 0644 \
  runtime-ca.crt \
  control-server.crt output-server.crt content-server.crt \
  command-signing-keyring.json
# PrivateMaterial: owner-only, no exceptions.
install_files "$MAIN" 65532:65532 0600 \
  control-server.key output-server.key content-server.key \
  command-signing-key.pem \
  auth-attempt-key auth-pat-signing-key auth-form-users.json \
  vault-master-key

# ── elitea-worker-python (uid 10001) ─────────────────────────────────────────
# The worker never receives a server private key or the command-signing key: it
# verifies signatures with the public keyring, and reaches
# the command bus over NATS (plaintext in compose; the secured overlay mounts
# its elitea-worker certificate).
WORKER=/dst/worker
mkdir -p "$WORKER"; chown 10001:10001 "$WORKER"; chmod 700 "$WORKER"
install_files "$WORKER" 10001:10001 0644 runtime-ca.crt command-signing-keyring.json
install_files "$WORKER" 10001:10001 0600 \
  agent-worker-client.crt agent-worker-client.key \
  worker-output-spool-key \
  agent-checkpoint-connection

# ── platform-edge (traefik, root) ────────────────────────────────────────────
# Only the edge certificate and its key. The edge terminates TLS for the
# worker's platform_origin and has no part in the runtime's own trust decisions,
# so it must not hold the runtime CA key, the signing key or any password.
EDGE=/dst/edge
mkdir -p "$EDGE"; chown 0:0 "$EDGE"; chmod 755 "$EDGE"
install_files "$EDGE" 0:0 0644 platform-edge.crt
install_files "$EDGE" 0:0 0600 platform-edge.key

if [ "$COMPILED_ENABLED" = true ]; then
  install_files "$MAIN" 65532:65532 0644 rust-compiled-profiles.json
  install_files "$MAIN" 65532:65532 0600 agent-checkpoint-connection
  install_files "$WORKER" 10001:10001 0600 rust-compiled-profiles.json
fi

if [ "$CODE_OWNER_ENABLED" = true ]; then
  install_files "$MAIN" 65532:65532 0644 code-owner-client.crt
  install_files "$MAIN" 65532:65532 0600 code-owner-client.key
fi
if [ "$CODE_PLATFORM_ENABLED" = true ]; then
  install_files "$MAIN" 65532:65532 0600 code-platform-content-keys.json
fi
if [ "$CODE_DEBUG_ENABLED" = true ]; then
  install_files "$MAIN" 65532:65532 0600 agent-checkpoint-connection
fi

echo "runtime material installed"
