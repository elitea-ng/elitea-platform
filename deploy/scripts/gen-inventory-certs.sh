#!/usr/bin/env bash
# Generate the local mTLS material for the elitea-main → elitea-inventory hop.
#
# The SAME shape as gen-deepwiki-certs.sh, for the same reason: there is no
# plaintext mode on either side of a provider hop. The facade
# (internal/providerhost/proxy) refuses a base URL that is not https and always
# builds an mTLS transport, so an enabled facade with no client certificate is a
# fatal boot error; the Go sub-application host refuses any hop that is not
# mutually authenticated once a CA file is configured. Issuing throwaway
# certificates is cheaper than a dev-only relaxation in the production path,
# which is how a deployment ends up serving without mTLS while believing it has
# it.
#
# WHY A SECOND SCRIPT AND NOT A FLAG ON THE FIRST. A deployment may run one
# provider and not the other — that is the whole reason the two facades have
# separate enable flags and separate allowlists — so each provider's material
# has to be obtainable on its own. What is shared is the CA and the elitea-main
# CLIENT certificate: both hops are elitea-main presenting `CN=elitea-main` to a
# service that verifies against one trust root, and a second CA would be a
# second thing to keep in step for no gain. This script REUSES
# deploy/certs/ca.{crt,key} and client.{crt,key} when they exist and creates
# them when they do not, so it composes with gen-gateway-certs.sh and
# gen-deepwiki-certs.sh in any order.
#
# Output (gitignored, see .gitignore):
#   deploy/certs/ca.crt                     — shared trust root
#   deploy/certs/inventory-server.{crt,key} — served by elitea-inventory
#   deploy/certs/client.{crt,key}           — presented by elitea-main
#
# Idempotent: regenerates only what is missing or expires within 24h.
set -euo pipefail

CERT_DIR="$(cd "$(dirname "$0")/.." && pwd)/certs"
DAYS="${INVENTORY_CERT_DAYS:-825}"

# The SANs must cover every name elitea-main may use as the TLS ServerName. The
# facade defaults it to the base URL's hostname, which is the compose service
# name; localhost and 127.0.0.1 are here so the service can also be reached from
# the host for debugging.
SERVER_SANS="${INVENTORY_CERT_SANS:-DNS:elitea-inventory,DNS:localhost,IP:127.0.0.1}"

command -v openssl >/dev/null || { echo "ERROR: openssl not found" >&2; exit 1; }

mkdir -p "$CERT_DIR"

fresh() {
  # A file that exists and does not expire within a day.
  [ -f "$CERT_DIR/$1" ] || return 1
  case "$1" in
    *.crt) openssl x509 -checkend 86400 -noout -in "$CERT_DIR/$1" >/dev/null 2>&1 ;;
    *)     return 0 ;;
  esac
}

# ── CA, created only when this script is the first to need one ───────────────
if ! fresh ca.crt || [ ! -f "$CERT_DIR/ca.key" ]; then
  echo "→ issuing a shared local CA"
  openssl req -x509 -newkey rsa:2048 -nodes \
    -keyout "$CERT_DIR/ca.key" -out "$CERT_DIR/ca.crt" \
    -days "$DAYS" -subj "/CN=elitea-standalone-local-ca" \
    -addext "basicConstraints=critical,CA:TRUE" \
    -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null
  # A new CA invalidates everything it did not sign. Removing the peers'
  # certificates is what makes that visible now rather than as a handshake
  # failure later — and it must remove the OTHER provider's server certificate
  # too, or that hop fails with an x509 error nowhere near this script.
  rm -f "$CERT_DIR"/inventory-server.crt "$CERT_DIR"/deepwiki-server.crt "$CERT_DIR"/client.crt
fi

# ── provider server cert ─────────────────────────────────────────────────────
if ! fresh inventory-server.crt; then
  echo "→ issuing inventory-server.crt (SANs: $SERVER_SANS)"
  openssl req -newkey rsa:2048 -nodes \
    -keyout "$CERT_DIR/inventory-server.key" -out "$CERT_DIR/inventory-server.csr" \
    -subj "/CN=elitea-inventory" 2>/dev/null
  openssl x509 -req -in "$CERT_DIR/inventory-server.csr" \
    -CA "$CERT_DIR/ca.crt" -CAkey "$CERT_DIR/ca.key" -CAcreateserial \
    -out "$CERT_DIR/inventory-server.crt" -days "$DAYS" \
    -extfile <(printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth\n' "$SERVER_SANS") 2>/dev/null
fi

# ── elitea-main client cert, shared with the gateway and DeepWiki hops ───────
if ! fresh client.crt; then
  echo "→ issuing client.crt"
  openssl req -newkey rsa:2048 -nodes \
    -keyout "$CERT_DIR/client.key" -out "$CERT_DIR/client.csr" \
    -subj "/CN=elitea-main" 2>/dev/null
  openssl x509 -req -in "$CERT_DIR/client.csr" \
    -CA "$CERT_DIR/ca.crt" -CAkey "$CERT_DIR/ca.key" -CAcreateserial \
    -out "$CERT_DIR/client.crt" -days "$DAYS" \
    -extfile <(printf 'extendedKeyUsage=clientAuth\n') 2>/dev/null
fi

rm -f "$CERT_DIR"/*.csr
# World-readable: throwaway local material, and the containers run as different
# uids (the provider images 10001, elitea-main distroless nonroot). Per-runtime
# uid juggling buys nothing for certificates in a gitignored directory.
chmod 644 "$CERT_DIR"/*.key "$CERT_DIR"/*.crt

echo "→ Done. CA: $CERT_DIR/ca.crt (valid ${DAYS}d)"
