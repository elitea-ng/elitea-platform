#!/usr/bin/env bash
# mint-secrets.sh — mint every generated secret value of a Helm install as ONE
# flat JSON object, the layout the chart reads with externalSecrets.remoteKey
# and externalSecrets.sharedProperties: true (deploy/helm/elitea/values.yaml,
# `externalSecrets`). Upload the file as one secret to your secret manager:
#
#   PUBLIC_URL=https://elitea.example.com NAMESPACE=elitea \
#     deploy/scripts/mint-secrets.sh elitea-platform.json
#   gcloud secrets create elitea-platform --data-file=elitea-platform.json
#   shred -u elitea-platform.json        # or rm; keep no copy on disk
#
# ══ RE-RUNNING THIS SCRIPT OVER A LIVE INSTALL ROTATES EVERY VALUE ══════════
# Every run mints NEW values. Uploading a new run over a running install
# replaces, among others:
#   * the vault master key (elitea-llm-gateway-secrets/secrets-master-key, a
#     Fernet key): every credential already stored in a project vault becomes
#     unreadable, for good;
#   * the PAT signing key and the attempt key: every personal access token
#     stops verifying, and every browser session ends;
#   * the runtime CA and the command-signing key: the worker certificate, the
#     runtime listeners and every queued command signature change together.
# The script refuses to write over an existing output file. To add ONE new
# value to a live install, add only that property to the remote secret.
# ════════════════════════════════════════════════════════════════════════════
#
# WHAT IT MINTS (the chart's default Secret names; rename here if you renamed
# them in values):
#   runtime-ca.crt, runtime-ca.key       the runtime CA (shared properties:
#                                        both material Secrets and, with
#                                        certManager.issuers.create, the CA
#                                        Issuer's Secret elitea-runtime-ca)
#   command-signing-keyring.json         shared: runtime and worker material
#   gateway-identity-secret              shared: the gateway and elitea-main
#   elitea-runtime-material/...          the control/output/content server
#                                        certificates (SANs: elitea-main in
#                                        NAMESPACE) and the Ed25519 signing key
#   elitea-worker-material/...           the worker client certificate, with
#                                        ONE URI SAN: the SPIFFE ID below
#   elitea-main-auth-material/...        attempt key, PAT key, Form users file
#   elitea-main-sso/application-secret-key
#   elitea-llm-gateway-secrets/secrets-master-key, hop-secret
#   elitea-deepwiki-secrets/identity-secret, elitea-inventory-secrets/...
#   elitea-qdrant-api/api-key
#
# THE WORKER IDENTITY. The worker certificate carries
#   spiffe://<host of PUBLIC_URL>/ns/<NAMESPACE>/sa/agent-worker
# which is the value the chart derives for worker.runtimeSession.expectedIdentity
# from global.publicUrl in the same namespace. Set WORKER_SPIFFE_ID to mint
# another one, and then set worker.runtimeSession.expectedIdentity to it.
#
# KEYS THAT ARE "RAW BYTES", AS STRINGS. A Secret Manager JSON property holds
# text only. The two binary keys accept a printable string:
#   * auth-attempt-key: 32 to 64 bytes. Minted as 43 base64 characters (32
#     random bytes encoded), so the file is the 43 ASCII bytes.
#   * worker-output-spool-key: EXACTLY 32 bytes. Minted as 32 random
#     alphanumeric characters (about 190 bits). Any 32-character ASCII string
#     is valid; a base64 encoding of 32 bytes (44 characters) is NOT.
#
# Needs: bash, openssl, python3 with `cryptography` (the Ed25519 key; macOS
# LibreSSL cannot mint it). No cluster, no network.
set -euo pipefail

out="${1:-}"
if [ -z "$out" ]; then
  echo "usage: PUBLIC_URL=https://elitea.example.com NAMESPACE=elitea $0 <output.json>" >&2
  exit 2
fi
: "${PUBLIC_URL:?set PUBLIC_URL, the origin browsers open (global.publicUrl), e.g. https://elitea.example.com}"
NAMESPACE="${NAMESPACE:-elitea}"
DAYS="${CERT_DAYS:-825}"
# Must equal main.runtime.signingKeyId (values-gke.yaml: elitea-runtime-v1).
SIGNING_KEY_ID="${RUNTIME_SIGNING_KEY_ID:-elitea-runtime-v1}"
MAIN_SERVICE="${MAIN_SERVICE:-elitea-main}"
ADMIN_LOGIN="${ADMIN_LOGIN:-admin}"
ADMIN_EMAIL="${ADMIN_EMAIL:-admin@example.com}"

host="${PUBLIC_URL#http://}"
host="${host#https://}"
host="${host%%/*}"
host="${host%%:*}"
if [ -z "$host" ] || [ "$host" = "$PUBLIC_URL" ]; then
  echo "ERROR: PUBLIC_URL must be a URL such as https://elitea.example.com, got '$PUBLIC_URL'" >&2
  exit 2
fi
WORKER_SPIFFE_ID="${WORKER_SPIFFE_ID:-spiffe://$host/ns/$NAMESPACE/sa/agent-worker}"

for command in openssl python3; do
  command -v "$command" >/dev/null || { echo "ERROR: $command not found" >&2; exit 1; }
done
python3 -c 'import cryptography' 2>/dev/null || {
  echo "ERROR: python3 'cryptography' is required to mint the Ed25519 signing key" >&2
  echo "       python3 -m pip install cryptography" >&2
  exit 1
}

warn_rotation() {
  cat >&2 <<'WARN'
WARNING: every value in this file is NEW. Upload it only for a NEW install.
         Over a live install it rotates the vault master key (stored
         credentials become unreadable), the PAT and attempt keys (every token
         and session ends), the runtime CA and the command-signing key.
WARN
}

if [ -e "$out" ]; then
  if [ "${FORCE:-0}" != "1" ]; then
    echo "ERROR: $out exists. This script does not write over minted secrets." >&2
    warn_rotation
    echo "       Move the file away, or set FORCE=1 to replace it." >&2
    exit 1
  fi
  echo "FORCE=1: replacing $out." >&2
fi
warn_rotation

umask 077
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# ── the runtime CA and its certificates (gen-runtime-certs.sh, for a cluster) ─
openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout "$work/runtime-ca.key" -out "$work/runtime-ca.crt" \
  -days "$DAYS" -subj "/CN=elitea-runtime-ca" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null

issue() {
  # $1 file stem, $2 CN, $3 subjectAltName, $4 extendedKeyUsage, $5 keyUsage
  openssl req -newkey rsa:2048 -nodes \
    -keyout "$work/$1.key" -out "$work/$1.csr" -subj "/CN=$2" 2>/dev/null
  openssl x509 -req -in "$work/$1.csr" \
    -CA "$work/runtime-ca.crt" -CAkey "$work/runtime-ca.key" -CAcreateserial \
    -out "$work/$1.crt" -days "$DAYS" \
    -extfile <(printf 'subjectAltName=%s\nextendedKeyUsage=%s\nkeyUsage=critical,%s\n' "$3" "$4" "$5") 2>/dev/null
}
# Every name the worker may dial elitea-main by inside the namespace. The
# platform listener (:9446) serves with the content keypair.
sans="DNS:$MAIN_SERVICE,DNS:$MAIN_SERVICE.$NAMESPACE.svc,DNS:$MAIN_SERVICE.$NAMESPACE.svc.cluster.local"
for listener in control output content; do
  issue "$listener-server" "$MAIN_SERVICE" "$sans" serverAuth digitalSignature,keyEncipherment
done
# Exactly ONE URI SAN: internal/auth/workloadidentity admits one SPIFFE URI
# SAN or one DNS SAN, never both, and never reads the CommonName.
issue agent-worker-client elitea-agent-worker "URI:$WORKER_SPIFFE_ID" clientAuth digitalSignature

# ── the Ed25519 command-signing key, the generated values, the JSON ─────────
python3 - "$work" "$out" "$SIGNING_KEY_ID" "$ADMIN_LOGIN" "$ADMIN_EMAIL" <<'PY'
import base64, json, os, pathlib, secrets, string, sys
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import ed25519

work, out, key_id, login, email = pathlib.Path(sys.argv[1]), sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5]
read = lambda name: (work / name).read_text(encoding="ascii")

def token():
    # 32 random bytes as unpadded standard base64: 43 printable characters.
    return base64.b64encode(os.urandom(32)).decode("ascii").rstrip("=")

private_key = ed25519.Ed25519PrivateKey.generate()
signing_pem = private_key.private_bytes(
    serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8,
    serialization.NoEncryption()).decode("ascii")
public_raw = private_key.public_key().public_bytes(
    serialization.Encoding.Raw, serialization.PublicFormat.Raw)
keyring = json.dumps({
    "schema_version": "elitea.runtime-ed25519-keyring.v1",
    "keys": [{"key_id": key_id, "public_key_base64": base64.b64encode(public_raw).decode("ascii")}],
}, indent=2) + "\n"
admin_password = base64.b64encode(os.urandom(18)).decode("ascii")
users = json.dumps({"users": [{"login": login, "email": email, "password": admin_password}]}, indent=2) + "\n"
alphanumeric = string.ascii_letters + string.digits

doc = {
    # Shared properties (externalSecrets.sharedProperties): written once.
    "runtime-ca.crt": read("runtime-ca.crt"),
    "runtime-ca.key": read("runtime-ca.key"),
    "command-signing-keyring.json": keyring,
    "gateway-identity-secret": token(),
    # One Secret each.
    "elitea-runtime-material/command-signing-key.pem": signing_pem,
    "elitea-worker-material/agent-worker-client.crt": read("agent-worker-client.crt"),
    "elitea-worker-material/agent-worker-client.key": read("agent-worker-client.key"),
    # Exactly 32 bytes, as 32 printable characters.
    "elitea-worker-material/worker-output-spool-key": "".join(secrets.choice(alphanumeric) for _ in range(32)),
    # 32 to 64 bytes, as 43 printable characters.
    "elitea-main-auth-material/auth-attempt-key": token(),
    "elitea-main-auth-material/auth-pat-signing-key": token(),
    "elitea-main-auth-material/auth-form-users.json": users,
    "elitea-main-sso/application-secret-key": token(),
    # A Fernet key: exactly 44 base64url characters, no newline.
    "elitea-llm-gateway-secrets/secrets-master-key": base64.urlsafe_b64encode(os.urandom(32)).decode("ascii"),
    "elitea-llm-gateway-secrets/hop-secret": token(),
    "elitea-deepwiki-secrets/identity-secret": token(),
    "elitea-inventory-secrets/identity-secret": token(),
    "elitea-qdrant-api/api-key": secrets.token_hex(32),
}
for listener in ("control", "output", "content"):
    for ext in ("crt", "key"):
        doc[f"elitea-runtime-material/{listener}-server.{ext}"] = read(f"{listener}-server.{ext}")

# Values only the operator has. Added from the environment when set.
optional = {
    "elitea-main-sso/oidc-client-secret": "OIDC_CLIENT_SECRET",
    "elitea-main-db/database-url": "DATABASE_URL",
    "elitea-worker-material/agent-checkpoint-connection": "AGENT_CHECKPOINT_CONNECTION",
}
missing = []
for prop, env in optional.items():
    if os.environ.get(env):
        doc[prop] = os.environ[env]
    else:
        missing.append((prop, env))

text = json.dumps(dict(sorted(doc.items())), indent=2) + "\n"
size = len(text.encode("utf-8"))
if size > 64 * 1024:
    sys.exit(f"ERROR: the JSON is {size} bytes; GCP Secret Manager caps a version at 64 KiB")
fd = os.open(out, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
with os.fdopen(fd, "w", encoding="utf-8") as handle:
    handle.write(text)

print(f"wrote {out}: {len(doc)} properties, {size} bytes", file=sys.stderr)
print(f"  Form login (the authentication document needs one user): {login} / {admin_password}", file=sys.stderr)
if missing:
    print("\nADD THESE YOURSELF (the script cannot know them), as properties of the same secret:", file=sys.stderr)
    for prop, env in missing:
        print(f"  {prop:52s} (or set {env} and run again on a new file)", file=sys.stderr)
PY

cat >&2 <<EOF

SET IN YOUR VALUES (not secret, not in this file):
  global.publicUrl                      $PUBLIC_URL
  main.runtime.signingKeyId             $SIGNING_KEY_ID
  main.env.OIDC_ISSUER_URL, main.env.OIDC_CLIENT_ID   from your identity provider
  externalSecrets.remoteKey             the name you upload this file as
  externalSecrets.sharedProperties      true
  worker.runtimeSession.expectedIdentity  only if it is not the derived
                                        $WORKER_SPIFFE_ID
The agent-checkpoint-connection DSN names the agent-state database and the
database-url DSN the platform one, as the same role (dbInit reads both).
values-gke.yaml reads database-url from its own secret, elitea-database-url.
EOF
