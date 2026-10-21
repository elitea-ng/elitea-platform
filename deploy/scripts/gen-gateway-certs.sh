#!/usr/bin/env bash
# Generate the local mTLS material for the elitea-main → elitea-llm-gateway hop.
#
# Why this script has to exist: llmproxy.New always builds an mTLS transport
# when Config.Transport is nil (services/elitea-main/internal/llmproxy/proxy.go:63),
# and the only injection seam for a plaintext transport is a struct field with no
# env binding — i.e. test-only. So LLM_GATEWAY_URL cannot be set without real
# client certs; an empty cert path makes tls.LoadX509KeyPair fail and
# cmd/elitea-main/main.go:845 turns that into a fatal boot error.
#
# The gateway's own TLS is symmetric: it serves plain HTTP when the three
# GATEWAY_TLS_* vars are empty, and requires client certs precisely when
# GATEWAY_TLS_CA_FILE is also set (internal/server/server.go:220-233). This
# script produces one CA that signs both sides, so the pair is consistent.
#
# Output (gitignored, see .gitignore):
#   deploy/certs/ca.crt              — shared trust root
#   deploy/certs/gateway-server.{crt,key} — served by elitea-llm-gateway
#   deploy/certs/client.{crt,key}         — presented by elitea-main
#
# Idempotent: regenerates only when a cert is missing or expires within 24h.
set -euo pipefail

CERT_DIR="$(cd "$(dirname "$0")/.." && pwd)/certs"
DAYS="${GATEWAY_CERT_DAYS:-825}"

# The server certificate's SANs must cover every name elitea-main may use as the
# TLS ServerName. llmproxy defaults ServerName to the target URL's hostname
# (proxy.go:65-69), which is the compose service name; localhost/127.0.0.1 are
# here so the gateway can also be reached directly from the host for debugging.
SERVER_SANS="${GATEWAY_CERT_SANS:-DNS:elitea-llm-gateway,DNS:localhost,IP:127.0.0.1}"

command -v openssl >/dev/null || { echo "ERROR: openssl not found" >&2; exit 1; }

mkdir -p "$CERT_DIR"

# Regenerate only if something is missing or close to expiry. -checkend takes
# seconds; 86400 = "expires within a day".
needs_regen=0
for f in ca.crt ca.key gateway-server.crt gateway-server.key client.crt client.key; do
  [ -f "$CERT_DIR/$f" ] || needs_regen=1
done
if [ "$needs_regen" -eq 0 ]; then
  for c in ca.crt gateway-server.crt client.crt; do
    openssl x509 -checkend 86400 -noout -in "$CERT_DIR/$c" >/dev/null 2>&1 || needs_regen=1
  done
  # The gateway gRPC listener (#1359) admits elitea-main by the client cert
  # SAN DNS:elitea-main, never by the CN. Material from before #1359 has no
  # SAN and every /llm call is refused, so replace it.
  openssl x509 -noout -text -in "$CERT_DIR/client.crt" 2>/dev/null | grep -q 'DNS:elitea-main' || needs_regen=1
fi
if [ "$needs_regen" -eq 0 ] && [ "${FORCE:-0}" != "1" ]; then
  echo "→ Certificates in $CERT_DIR are present and valid; nothing to do (FORCE=1 to regenerate)."
  exit 0
fi

echo "→ Generating local mTLS material in $CERT_DIR …"
rm -f "$CERT_DIR"/*.crt "$CERT_DIR"/*.key "$CERT_DIR"/*.srl

# ── CA ───────────────────────────────────────────────────────────────────────
openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout "$CERT_DIR/ca.key" -out "$CERT_DIR/ca.crt" \
  -days "$DAYS" -subj "/CN=elitea-standalone-local-ca" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null

# ── gateway server cert ──────────────────────────────────────────────────────
openssl req -newkey rsa:2048 -nodes \
  -keyout "$CERT_DIR/gateway-server.key" -out "$CERT_DIR/gateway-server.csr" \
  -subj "/CN=elitea-llm-gateway" 2>/dev/null
openssl x509 -req -in "$CERT_DIR/gateway-server.csr" \
  -CA "$CERT_DIR/ca.crt" -CAkey "$CERT_DIR/ca.key" -CAcreateserial \
  -out "$CERT_DIR/gateway-server.crt" -days "$DAYS" \
  -extfile <(printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth\n' "$SERVER_SANS") 2>/dev/null

# ── elitea-main client cert ──────────────────────────────────────────────────
openssl req -newkey rsa:2048 -nodes \
  -keyout "$CERT_DIR/client.key" -out "$CERT_DIR/client.csr" \
  -subj "/CN=elitea-main" 2>/dev/null
openssl x509 -req -in "$CERT_DIR/client.csr" \
  -CA "$CERT_DIR/ca.crt" -CAkey "$CERT_DIR/ca.key" -CAcreateserial \
  -out "$CERT_DIR/client.crt" -days "$DAYS" \
  -extfile <(printf 'subjectAltName=DNS:elitea-main\nextendedKeyUsage=clientAuth\n') 2>/dev/null

rm -f "$CERT_DIR"/*.csr
# The gateway container runs as distroless `nonroot` (uid 65532) and must be able
# to read its key; the client key is read by elitea-main. World-readable is
# acceptable for throwaway local material and avoids per-runtime uid juggling.
chmod 644 "$CERT_DIR"/*.key "$CERT_DIR"/*.crt

echo "→ Done. CA: $CERT_DIR/ca.crt (valid ${DAYS}d)"
