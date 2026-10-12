#!/usr/bin/env bash
# Generate the local material for elitea-vector and its Qdrant (ADR-0031 V0).
#
# elitea-vector has two mTLS hops, and both use the RUNTIME CA that
# gen-runtime-certs.sh mints:
#
#   * its own gRPC listener (:9470). The callers are the workers (their
#     agent-worker-client certificate, runtime CA) and elitea-main (its
#     administrator identity). So the listener trusts the runtime CA.
#   * its client hop to elitea-main's control listener (:9443) for token
#     introspection. That listener verifies clients against the runtime CA
#     (ELITEA_RUNTIME_CONTROL_TLS_CLIENT_CA_FILE), so this certificate must
#     chain to it.
#
# One certificate serves both hops: DNS SAN `elitea-vector` and nothing else,
# with serverAuth AND clientAuth. internal/auth/workloadidentity accepts
# exactly one DNS SAN or one SPIFFE URI SAN, so elitea-main reads the client
# identity as `dns:elitea-vector` (ELITEA_VECTOR_INTROSPECTION_CLIENTS).
#
# WHY A SEPARATE DIRECTORY AND NOT deploy/certs/runtime. gen-runtime-certs.sh
# rotates its whole tree when any listed file is missing, which also rotates
# vault-master-key and the PAT key. Adding files to that list would rotate a
# working developer tree. This script instead re-issues its certificate when
# it no longer verifies against the CURRENT runtime CA, so it follows a
# runtime rotation without forcing one.
#
# Output (gitignored, deploy/certs/ is in .gitignore):
#   deploy/certs/vector/vector.{crt,key}   elitea-vector server + client
#   deploy/certs/vector/runtime-ca.crt     copy of the runtime trust root
#   deploy/certs/vector/qdrant-api-key     Qdrant API key (random, 32 bytes)
#
# Idempotent: regenerates only what is missing, expiring within 24h, or not
# signed by the current runtime CA. FORCE=1 re-issues everything.
set -euo pipefail

DEPLOY_DIR="$(cd "$(dirname "$0")/.." && pwd)"
RUNTIME_DIR="$DEPLOY_DIR/certs/runtime"
VECTOR_DIR="$DEPLOY_DIR/certs/vector"
DAYS="${VECTOR_CERT_DAYS:-825}"

command -v openssl >/dev/null || { echo "ERROR: openssl not found" >&2; exit 1; }
for name in runtime-ca.crt runtime-ca.key; do
  [ -f "$RUNTIME_DIR/$name" ] || {
    echo "ERROR: $RUNTIME_DIR/$name missing — run deploy/scripts/gen-runtime-certs.sh first" >&2
    exit 1
  }
done

mkdir -p "$VECTOR_DIR"

needs_cert=0
if [ "${FORCE:-0}" = "1" ] || [ ! -f "$VECTOR_DIR/vector.crt" ] || [ ! -f "$VECTOR_DIR/vector.key" ]; then
  needs_cert=1
elif ! openssl x509 -checkend 86400 -noout -in "$VECTOR_DIR/vector.crt" >/dev/null 2>&1; then
  needs_cert=1
elif ! openssl verify -CAfile "$RUNTIME_DIR/runtime-ca.crt" "$VECTOR_DIR/vector.crt" >/dev/null 2>&1; then
  needs_cert=1
fi

if [ "$needs_cert" -eq 1 ]; then
  echo "→ issuing vector.crt (DNS:elitea-vector, server + client)"
  openssl req -newkey rsa:2048 -nodes \
    -keyout "$VECTOR_DIR/vector.key" -out "$VECTOR_DIR/vector.csr" \
    -subj "/CN=elitea-vector" 2>/dev/null
  openssl x509 -req -in "$VECTOR_DIR/vector.csr" \
    -CA "$RUNTIME_DIR/runtime-ca.crt" -CAkey "$RUNTIME_DIR/runtime-ca.key" -CAcreateserial \
    -CAserial "$VECTOR_DIR/runtime-ca.srl" \
    -out "$VECTOR_DIR/vector.crt" -days "$DAYS" \
    -extfile <(printf 'subjectAltName=DNS:elitea-vector\nextendedKeyUsage=serverAuth,clientAuth\nkeyUsage=critical,digitalSignature,keyEncipherment\n') 2>/dev/null
  rm -f "$VECTOR_DIR/vector.csr" "$VECTOR_DIR/runtime-ca.srl"
fi

cp "$RUNTIME_DIR/runtime-ca.crt" "$VECTOR_DIR/runtime-ca.crt"

if [ "${FORCE:-0}" = "1" ] || [ ! -s "$VECTOR_DIR/qdrant-api-key" ]; then
  echo "→ generating qdrant-api-key"
  # No trailing newline: the value is read as-is by both Qdrant and
  # elitea-vector.
  printf '%s' "$(openssl rand -hex 32)" > "$VECTOR_DIR/qdrant-api-key"
fi

# World-readable, like the provider certificates: throwaway local material in
# a gitignored directory, read by containers that run as different uids
# (elitea-vector 10001, Qdrant 1000 or root) through a bind mount.
chmod 644 "$VECTOR_DIR"/*

echo "→ Done. elitea-vector material in $VECTOR_DIR"
