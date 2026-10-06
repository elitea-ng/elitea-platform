#!/usr/bin/env bash
# Generate the local runtime-plane material for the full standalone stack.
#
# This is the sibling of gen-gateway-certs.sh. That script mints ONE CA for the
# elitea-main → elitea-llm-gateway hop; this one mints a SEPARATE runtime CA and
# every secret `ELITEA_RUNTIME_ENABLED=true` demands. They are deliberately not
# merged: the gateway trust root is an application-egress root, the runtime root
# signs workload identities that authorize gRPC calls into elitea-main.
#
# Why so much material: runtimecomposition/config.go accepts the runtime env
# block all-or-nothing and refuses to boot on a partial one, and every file it
# names goes through internal/security/securefile, which enforces canonical
# absolute paths, no symlinks, and exact permission bits (private material must
# be owner-only 0600/0400; public trust material must not be group/other
# writable or executable). Enabling the runtime therefore is not "set a flag" —
# it is provisioning three mTLS listeners, a TLS Redis, an Ed25519 command
# signing keypair, and the production Form auth plane that
# cmd/elitea-main/main.go:686-688 hard-requires alongside it.
#
# Output (gitignored — deploy/certs/ is in .gitignore; keep it that way):
#
#   deploy/certs/runtime/
#     runtime-ca.{crt,key}            trust root for everything below
#     redis-server.{crt,key}          served by runtime-redis (SAN: runtime-redis,
#                                      elitea-runtime-redis — the compose and
#                                      Kubernetes Service names)
#     control-server.{crt,key}        elitea-main control gRPC   :9443
#     output-server.{crt,key}         elitea-main output gRPC    :9444
#     content-server.{crt,key}        elitea-main content HTTPS  :9445
#     platform-edge.{crt,key}         TLS front for the worker's platform_origin
#     agent-worker-client.{crt,key}   presented by the selected agent worker
#     agent-checkpoint-connection     DSN for native agent session checkpoints
#     command-signing-key.pem         Ed25519 PKCS#8, signs dispatch envelopes
#     command-signing-keyring.json    its public half, keyed by ELITEA_RUNTIME_SIGNING_KEY_ID
#     redis-{producer,worker,bootstrap}-password
#     redis-users.acl                 ACL file consumed by runtime-redis
#     auth-attempt-key                browser-attempt HMAC key (32 raw bytes)
#     auth-pat-signing-key            PAT HS512 key
#     auth-form-users.json            Form provider users (unused in practice — the
#                                     browser logs in through oidc-mock; the Form
#                                     provider exists because the auth schema has
#                                     no OIDC variant yet)
#     vault-master-key                Fernet key for ELITEA_CONFIGURATIONS_ENABLED
#     worker-output-spool-key         exactly 32 raw bytes, for the worker (#282)
#
# The container copy of this tree is NOT the bind mount. compose's
# runtime-material init service copies it into per-consumer named volumes and
# applies the uid/mode each reader needs — a bind mount cannot satisfy
# securefile's 0600 check and the container uid at the same time under rootless
# podman, where host ownership does not survive into the container's userns.
#
# Idempotent: regenerates only when material is missing or a cert expires within
# 24h. FORCE=1 rotates everything.
set -euo pipefail

RUNTIME_DIR="$(cd "$(dirname "$0")/.." && pwd)/certs/runtime"
DAYS="${RUNTIME_CERT_DAYS:-825}"

# The signing key ID is part of the boot contract: composition.go cross-checks
# the private key against the keyring entry under exactly this ID and refuses to
# start on a mismatch. It must equal ELITEA_RUNTIME_SIGNING_KEY_ID in compose.
SIGNING_KEY_ID="${RUNTIME_SIGNING_KEY_ID:-standalone-runtime-v1}"

# The worker identity. internal/auth/workloadidentity accepts exactly ONE SPIFFE
# URI SAN or exactly one DNS SAN and never falls back to CommonName, so this
# certificate carries a URI SAN and nothing else — no DNS, no IP, no email. The
# same string must appear in elitea_runtime.workload_sessions.workload_identity
# (seeded by standalone-stack.sh seed-runtime), or every worker call is rejected.
WORKER_SPIFFE_ID="${RUNTIME_WORKER_SPIFFE_ID:-spiffe://elitea.standalone/ns/default/sa/agent-worker}"

for command in openssl python3; do
  command -v "$command" >/dev/null || { echo "ERROR: $command not found" >&2; exit 1; }
done

mkdir -p -m 700 "$RUNTIME_DIR"

material=(
  runtime-ca.crt runtime-ca.key
  redis-server.crt redis-server.key
  control-server.crt control-server.key
  output-server.crt output-server.key
  content-server.crt content-server.key
  platform-edge.crt platform-edge.key
  agent-worker-client.crt agent-worker-client.key
  agent-checkpoint-connection
  command-signing-key.pem command-signing-keyring.json
  redis-producer-password redis-worker-password
  redis-bootstrap-password
  redis-users.acl
  auth-attempt-key auth-pat-signing-key auth-form-users.json
  vault-master-key worker-output-spool-key
)

needs_regen=0
for name in "${material[@]}"; do
  [ -f "$RUNTIME_DIR/$name" ] || needs_regen=1
done
# A tree minted before the Form sign-in state moved to PostgreSQL still grants
# the retired `auth` Redis user. Drop just that line (and its password file) in
# place, so no unused credential stays live. Nothing else is touched: a full
# rotation would also rotate vault-master-key, auth-pat-signing-key and the
# Form users, and every vault secret already stored would become unreadable.
# Idempotent: a tree without the line is left alone.
if [ -f "$RUNTIME_DIR/redis-users.acl" ] && grep -q '^user auth ' "$RUNTIME_DIR/redis-users.acl"; then
  acl_tmp="$RUNTIME_DIR/.redis-users.acl.tmp"
  cp -p "$RUNTIME_DIR/redis-users.acl" "$acl_tmp"   # keeps the file's mode
  grep -v '^user auth ' "$RUNTIME_DIR/redis-users.acl" > "$acl_tmp" || true
  mv -f "$acl_tmp" "$RUNTIME_DIR/redis-users.acl"
  rm -f "$RUNTIME_DIR/redis-auth-password"
  echo "→ Removed the retired 'auth' user from $RUNTIME_DIR/redis-users.acl (restart runtime-redis to apply)."
fi
if [ "$needs_regen" -eq 0 ]; then
  for name in runtime-ca.crt redis-server.crt control-server.crt output-server.crt \
              content-server.crt platform-edge.crt agent-worker-client.crt; do
    openssl x509 -checkend 86400 -noout -in "$RUNTIME_DIR/$name" >/dev/null 2>&1 || needs_regen=1
  done
fi
if [ "$needs_regen" -eq 0 ] && [ "${FORCE:-0}" != "1" ]; then
  echo "→ Runtime material in $RUNTIME_DIR is present and valid; nothing to do (FORCE=1 to rotate)."
  exit 0
fi

echo "→ Generating runtime-plane material in $RUNTIME_DIR …"
# Rotation is all-or-nothing on purpose: a keyring that no longer matches its
# signing key, or a worker cert signed by a retired CA, fails at boot with an
# error that points at the wrong file. prepare-runtime.sh refuses to guess at a
# partial tree for the same reason; here the tree is disposable, so rotate it.
rm -f "$RUNTIME_DIR"/*

# ── CA ───────────────────────────────────────────────────────────────────────
openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout "$RUNTIME_DIR/runtime-ca.key" -out "$RUNTIME_DIR/runtime-ca.crt" \
  -days "$DAYS" -subj "/CN=elitea-standalone-runtime-ca" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null

issue_server() {
  local name="$1" cn="$2" sans="$3"
  openssl req -newkey rsa:2048 -nodes \
    -keyout "$RUNTIME_DIR/$name.key" -out "$RUNTIME_DIR/$name.csr" \
    -subj "/CN=$cn" 2>/dev/null
  openssl x509 -req -in "$RUNTIME_DIR/$name.csr" \
    -CA "$RUNTIME_DIR/runtime-ca.crt" -CAkey "$RUNTIME_DIR/runtime-ca.key" -CAcreateserial \
    -out "$RUNTIME_DIR/$name.crt" -days "$DAYS" \
    -extfile <(printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth\nkeyUsage=critical,digitalSignature,keyEncipherment\n' "$sans") 2>/dev/null
}

# go-redis derives ServerName from the rediss:// hostname, so the Redis cert
# must cover every hostname a consumer dials it by. `runtime-redis` is the
# compose service name; `elitea-runtime-redis` is the Kubernetes Service name
# (deploy/helm/elitea/templates/runtimeRedis/_helpers.tpl, values.yaml
# runtimeRedis.service.name) that every rediss:// URL in values-standalone.yaml
# dials — both must be present so the same cert material serves both stacks.
# `localhost` is here so the ACL/stream bootstrap can also talk to it from
# inside the container over 127.0.0.1.
issue_server redis-server   runtime-redis "DNS:runtime-redis,DNS:elitea-runtime-redis,DNS:localhost,IP:127.0.0.1"
# The three private listeners all live in the elitea-main container; the worker
# dials them as elitea-main:9443/9444/9445.
issue_server control-server elitea-main   "DNS:elitea-main"
issue_server output-server  elitea-main   "DNS:elitea-main"
issue_server content-server elitea-main   "DNS:elitea-main"
# The worker's platform_origin. elitea-main has no TLS listener of its own for
# its ordinary /api/v2 + /llm surface, and the worker refuses a plaintext origin
# outright (config.py:187), so a TLS front terminates for it. Its own hostname,
# not elitea-main's: the edge is a distinct hop and its certificate must not be
# interchangeable with the three private listener certificates.
issue_server platform-edge  elitea-platform-edge "DNS:elitea-platform-edge"

# ── worker client cert: exactly one SPIFFE URI SAN, nothing else ─────────────
openssl req -newkey rsa:2048 -nodes \
  -keyout "$RUNTIME_DIR/agent-worker-client.key" -out "$RUNTIME_DIR/agent-worker-client.csr" \
  -subj "/CN=elitea-agent-worker" 2>/dev/null
openssl x509 -req -in "$RUNTIME_DIR/agent-worker-client.csr" \
  -CA "$RUNTIME_DIR/runtime-ca.crt" -CAkey "$RUNTIME_DIR/runtime-ca.key" -CAcreateserial \
  -out "$RUNTIME_DIR/agent-worker-client.crt" -days "$DAYS" \
  -extfile <(printf 'subjectAltName=URI:%s\nextendedKeyUsage=clientAuth\nkeyUsage=critical,digitalSignature\n' "$WORKER_SPIFFE_ID") 2>/dev/null

rm -f "$RUNTIME_DIR"/*.csr "$RUNTIME_DIR"/*.srl

# ── Ed25519 command signing key + verification keyring ───────────────────────
# runtimecomposition/files.go wants ONE PKCS#8 "PRIVATE KEY" PEM block and
# nothing else in the file; verification_keyring.go wants strict JSON with no
# unknown fields and a raw 32-byte public key in standard base64.
#
# The key is minted in Python rather than with `openssl genpkey -algorithm
# ed25519`, because macOS ships LibreSSL as `openssl` and LibreSSL 3.3 answers
# that with "Algorithm ed25519 not found" — under `2>/dev/null` inside a
# `set -e` script that surfaces as a silent abort halfway through generation.
# The RSA/X.509 work above is fine on LibreSSL, so only this step moves.
python3 - "$RUNTIME_DIR" "$SIGNING_KEY_ID" <<'PY'
import base64, json, pathlib, sys

try:
    from cryptography.hazmat.primitives import serialization
    from cryptography.hazmat.primitives.asymmetric import ed25519
except ImportError:
    raise SystemExit(
        "python3 'cryptography' is required to mint the Ed25519 command-signing key\n"
        "  install it with: python3 -m pip install cryptography")

runtime_dir, key_id = pathlib.Path(sys.argv[1]), sys.argv[2]
private_key = ed25519.Ed25519PrivateKey.generate()
(runtime_dir / "command-signing-key.pem").write_bytes(
    private_key.private_bytes(
        encoding=serialization.Encoding.PEM,
        format=serialization.PrivateFormat.PKCS8,
        encryption_algorithm=serialization.NoEncryption()))
public_key = private_key.public_key().public_bytes(
    encoding=serialization.Encoding.Raw,
    format=serialization.PublicFormat.Raw)
document = {
    "schema_version": "elitea.runtime-ed25519-keyring.v1",
    "keys": [{"key_id": key_id,
              "public_key_base64": base64.b64encode(public_key).decode("ascii")}],
}
(runtime_dir / "command-signing-keyring.json").write_text(
    json.dumps(document, indent=2) + "\n", encoding="ascii")
PY

# ── secrets ──────────────────────────────────────────────────────────────────
# Redis passwords are read by loadPassword()/normalizeRedisPassword(), which
# strip exactly one trailing newline and reject \r, \n and NUL anywhere else —
# so base64 without padding-free tricks is fine, and a trailing newline is not.
random_token() { openssl rand -base64 32 | tr -d '\n=' ; }

for name in redis-producer-password redis-worker-password \
            redis-bootstrap-password; do
  printf '%s' "$(random_token)" > "$RUNTIME_DIR/$name"
done

# materialize() rejects two auth material files that share a byte value, so these
# must be independently random — not derived from one seed.
# The browser-attempt key must be 32..64 raw bytes.
openssl rand 32 > "$RUNTIME_DIR/auth-attempt-key"
# The PAT key is UTF-8 text with no NUL and no trailing-newline normalisation:
# existing Python-issued HS512 PATs depend on the exact bytes, so write no newline.
printf '%s' "$(random_token)" > "$RUNTIME_DIR/auth-pat-signing-key"
# A Fernet key is exactly 44 base64url characters (32 raw bytes). The reader
# bounds the file at 44 bytes, so a trailing newline makes it invalid.
python3 -c 'import base64,os,sys;sys.stdout.write(base64.urlsafe_b64encode(os.urandom(32)).decode())' \
  > "$RUNTIME_DIR/vault-master-key"
# The worker spool key is exactly 32 RAW bytes (#282 config.py).
openssl rand 32 > "$RUNTIME_DIR/worker-output-spool-key"

# The native worker session and checkpoint connection uses a separate database.
# db-init creates it. elitea-agentstate-migrate applies its versioned history.
# Agent capability pools require this file. Other pools must not receive it.
printf 'postgresql://elitea:elitea@postgres:5432/agentstate?sslmode=disable' \
  > "$RUNTIME_DIR/agent-checkpoint-connection"

# The Form provider is required by the auth schema (it has no OIDC variant), but
# nothing signs in through it here: the browser authenticates via oidc-mock and
# the runtime authorizes workloads by certificate. The single user exists so
# NewFormProvider parses; its password is random and is never used.
python3 - "$RUNTIME_DIR" <<'PY'
import base64, json, os, pathlib, sys

runtime_dir = pathlib.Path(sys.argv[1])
password = base64.b64encode(os.urandom(24)).decode("ascii")
document = {"users": [{"login": "standalone-runtime-unused",
                       "email": "standalone-runtime-unused@example.invalid",
                       "password": password}]}
(runtime_dir / "auth-form-users.json").write_text(
    json.dumps(document, indent=2) + "\n", encoding="ascii")
PY

# ── Redis ACL ────────────────────────────────────────────────────────────────
# Four users, one per role, each scoped to the keys it owns:
#
#   producer   elitea-main: XADD onto both command streams and their delivery
#              index hashes, plus the replay wake channel.
#   worker     elitea-worker-python (#282): consumer-group reads on the agent
#              stream. Deliberately WITHOUT xgroup — the group is created by
#              bootstrap, so a worker cannot silently recreate a group it has
#              lost and skip undelivered commands. Mirrors prepare-runtime.sh.
#   bootstrap  seed-only: creates the consumer group from 0-0.
#
# There is no `auth` user. The production Form auth plane keeps its sign-in
# state in PostgreSQL (elitea-main shared migration 0153).
#
# `user default off` is what makes the ACL meaningful: without it every client
# that skips AUTH lands on an unrestricted default user.
CONFIGURATION_STREAM='commands.v1.configuration.validate.v1.validation-small.shared-credential-free.1.0'
AGENT_STREAM='commands.v1.agent.execute.agent.shared.1.0'
REPLAY_WAKE_CHANNEL='elitea:runtime:execution-replay:wake:v1'
# The consumer group's shared record of command entries that must not be run
# again (see services/elitea-worker-python/src/elitea_worker/execution/
# quarantine.py). One hash per (stream, group).
#
# The GROUP is a wildcard and the STREAM is pinned. Pinning both would be
# tighter by one segment and would add a sixth place that has to agree with
# `deploy/runtime/worker-runtime.json`'s `redis_group`; when those drift the
# grant stops matching, the shared store answers NOPERM, and the worker silently
# falls back to its per-filesystem record — a downgrade with no failing gate.
# The namespace holds nothing but these records, so the wildcard grants no
# access the pinned form would have withheld.
QUARANTINE_KEY_PREFIX="elitea:runtime:v1:quarantine:${AGENT_STREAM}"

producer_password="$(<"$RUNTIME_DIR/redis-producer-password")"
worker_password="$(<"$RUNTIME_DIR/redis-worker-password")"
bootstrap_password="$(<"$RUNTIME_DIR/redis-bootstrap-password")"

{
  printf 'user default off\n'
  printf 'user producer on >%s -@all +@connection +ping +eval +evalsha +xlen +xadd +xrange +xpending +hget +hset +hdel +hlen +publish +subscribe +unsubscribe ~%s ~%s:delivery-index.v1 ~%s ~%s:delivery-index.v1 &%s\n' \
    "$producer_password" "$CONFIGURATION_STREAM" "$CONFIGURATION_STREAM" \
    "$AGENT_STREAM" "$AGENT_STREAM" "$REPLAY_WAKE_CHANNEL"
  # `+hset +hkeys +hlen +expire` and the quarantine key are the ONLY widening of
  # this user, and they are confined to that one key pattern: nothing here adds
  # any capability against the command streams themselves. The worker previously
  # held no write primitive of its own at all (`+xack +xdel +hget +hdel` mutate
  # only the stream it consumes and that stream's delivery index), which is why
  # the quarantine record could not be shared between replicas before.
  #
  # `+hlen` and `+hexists` are required because the cap is enforced INSIDE the
  # Lua script rather than by the client, and `eval` runs under this user's own
  # permissions — a command missing here fails the script at runtime with
  # "ACL failure in script", which no fake client can reproduce.
  printf 'user worker on >%s -@all +@connection +ping +eval +evalsha +xreadgroup +xclaim +xautoclaim +xrange +xpending +xack +xdel +hget +hdel +hset +hkeys +hlen +hexists +expire ~%s ~%s:delivery-index.v1 ~%s:*\n' \
    "$worker_password" "$AGENT_STREAM" "$AGENT_STREAM" "$QUARANTINE_KEY_PREFIX"
  printf 'user bootstrap on >%s -@all +@connection +ping +xgroup +xinfo +xlen ~%s ~%s\n' \
    "$bootstrap_password" "$AGENT_STREAM" "$CONFIGURATION_STREAM"
} > "$RUNTIME_DIR/redis-users.acl"

# Host-side modes. The compose init service re-applies per-consumer ownership and
# mode inside the volume; these bits only keep the working copy from being
# world-readable on the developer's machine.
chmod 600 "$RUNTIME_DIR"/*
chmod 644 "$RUNTIME_DIR"/*.crt "$RUNTIME_DIR"/command-signing-keyring.json

echo "→ Done. Runtime CA: $RUNTIME_DIR/runtime-ca.crt (valid ${DAYS}d)"
echo "  signing key id : $SIGNING_KEY_ID"
echo "  worker identity: $WORKER_SPIFFE_ID"
