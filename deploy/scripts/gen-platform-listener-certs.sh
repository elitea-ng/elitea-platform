#!/usr/bin/env bash
# Generate the local mTLS material for elitea-main's platform gRPC listener
# (:9446, #1359) on a stack WITHOUT the runtime plane: the E2E stack, and the
# dev stack's `deepwiki` profile (deploy/docker-compose.yml).
#
# The sub-application hosts store their artifacts through
# elitea.runtime.v1.ArtifactObjectService on that listener (#1359 U5). On the
# runtime stacks the runtime CA (gen-runtime-certs.sh) signs both ends; these
# stacks have no runtime CA, so this issues the two certificates off the shared
# provider CA (deploy/certs/ca.{crt,key}, created by gen-deepwiki-certs.sh or
# gen-inventory-certs.sh — run one of them first):
#
#   deploy/certs/platform-server.{crt,key}  — served by elitea-main (DNS:elitea-main)
#   deploy/certs/subapp-callback.{crt,key}  — presented by the hosts
#                                              (DNS:elitea-subapp-callback, the
#                                              identity elitea-main admits)
#
# Idempotent: regenerates only what is missing or expires within 24h.
set -euo pipefail

CERT_DIR="$(cd "$(dirname "$0")/.." && pwd)/certs"
DAYS="${PLATFORM_CERT_DAYS:-825}"

command -v openssl >/dev/null || { echo "ERROR: openssl not found" >&2; exit 1; }
[ -f "$CERT_DIR/ca.crt" ] && [ -f "$CERT_DIR/ca.key" ] || {
  echo "ERROR: $CERT_DIR/ca.{crt,key} missing — run gen-deepwiki-certs.sh first" >&2
  exit 1
}

fresh() {
  [ -f "$CERT_DIR/$1" ] && openssl x509 -checkend 86400 -noout -in "$CERT_DIR/$1" >/dev/null 2>&1
}

# issue NAME CN SANS USAGE
issue() {
  if fresh "$1.crt" && [ -f "$CERT_DIR/$1.key" ]; then
    return 0
  fi
  echo "→ issuing $1.crt"
  openssl req -newkey rsa:2048 -nodes \
    -keyout "$CERT_DIR/$1.key" -out "$CERT_DIR/$1.csr" \
    -subj "/CN=$2" 2>/dev/null
  openssl x509 -req -in "$CERT_DIR/$1.csr" \
    -CA "$CERT_DIR/ca.crt" -CAkey "$CERT_DIR/ca.key" -CAcreateserial \
    -out "$CERT_DIR/$1.crt" -days "$DAYS" \
    -extfile <(printf 'subjectAltName=%s\nextendedKeyUsage=%s\n' "$3" "$4") 2>/dev/null
  rm -f "$CERT_DIR/$1.csr"
}

issue platform-server elitea-main "${PLATFORM_CERT_SANS:-DNS:elitea-main,DNS:localhost,IP:127.0.0.1}" serverAuth
issue subapp-callback elitea-subapp-callback "DNS:elitea-subapp-callback" clientAuth

# World-readable: throwaway local material read by containers of several uids
# (elitea-main nonroot, the hosts 65532), in a gitignored directory.
chmod 644 "$CERT_DIR"/platform-server.* "$CERT_DIR"/subapp-callback.*
echo "→ Done. Platform listener material in $CERT_DIR"
