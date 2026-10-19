#!/usr/bin/env bash
# Generate the egress proxy's audit client certificate for the compose stack
# (ADR-0034 decision 9, "Host allowlist: the egress proxy", Logging).
#
# The proxy reports its per-run summaries to elitea-main's
# elitea.runtime.v1.EgressAuditService on the control listener (:9443) over
# mTLS. That listener verifies clients against the RUNTIME CA that
# gen-runtime-certs.sh mints (ELITEA_RUNTIME_CONTROL_TLS_CLIENT_CA_FILE), and
# elitea-main admits one identity for the service
# (ELITEA_EGRESS_AUDIT_CLIENTS=dns:elitea-egress-proxy). So the certificate is:
# DNS SAN `elitea-egress-proxy` and nothing else (internal/auth/workloadidentity
# accepts exactly one DNS or one SPIFFE SAN), clientAuth only, signed by the
# runtime CA. The proxy never serves TLS, so no serverAuth.
#
# Like gen-vector-certs.sh this is its OWN script and directory, not an
# addition to gen-runtime-certs.sh: that script rotates its whole tree (the
# vault master key and the PAT signing key included) when any listed file is
# missing, so adding a file to its list would rotate a working developer tree.
# This one re-issues its certificate when it no longer verifies against the
# CURRENT runtime CA, so it follows a runtime rotation without forcing one.
#
# Output (gitignored, deploy/certs/ is in .gitignore):
#   deploy/certs/egress-proxy/audit-client.{crt,key}  the proxy's client cert
#   deploy/certs/egress-proxy/runtime-ca.crt          copy of the runtime trust root
#
# Idempotent: regenerates only what is missing, expiring within 24h, or not
# signed by the current runtime CA. FORCE=1 re-issues it.
set -euo pipefail

DEPLOY_DIR="$(cd "$(dirname "$0")/.." && pwd)"
RUNTIME_DIR="$DEPLOY_DIR/certs/runtime"
PROXY_DIR="$DEPLOY_DIR/certs/egress-proxy"
DAYS="${EGRESS_PROXY_CERT_DAYS:-825}"
# Must equal the dns: identity in ELITEA_EGRESS_AUDIT_CLIENTS
# (docker-compose.egress-proxy.yml).
CLIENT_DNS_NAME="${EGRESS_PROXY_AUDIT_DNS_NAME:-elitea-egress-proxy}"

command -v openssl >/dev/null || { echo "ERROR: openssl not found" >&2; exit 1; }
for name in runtime-ca.crt runtime-ca.key; do
  [ -f "$RUNTIME_DIR/$name" ] || {
    echo "ERROR: $RUNTIME_DIR/$name missing — run deploy/scripts/gen-runtime-certs.sh first" >&2
    exit 1
  }
done

mkdir -p "$PROXY_DIR"

needs_cert=0
if [ "${FORCE:-0}" = "1" ] || [ ! -f "$PROXY_DIR/audit-client.crt" ] || [ ! -f "$PROXY_DIR/audit-client.key" ]; then
  needs_cert=1
elif ! openssl x509 -checkend 86400 -noout -in "$PROXY_DIR/audit-client.crt" >/dev/null 2>&1; then
  needs_cert=1
elif ! openssl verify -CAfile "$RUNTIME_DIR/runtime-ca.crt" "$PROXY_DIR/audit-client.crt" >/dev/null 2>&1; then
  needs_cert=1
elif ! openssl x509 -noout -text -in "$PROXY_DIR/audit-client.crt" 2>/dev/null | grep -qE "DNS:$CLIENT_DNS_NAME(,|\$)"; then
  # (-text, not -ext: macOS ships LibreSSL, which has no -ext.)
  needs_cert=1
fi

if [ "$needs_cert" -eq 1 ]; then
  echo "→ issuing audit-client.crt (DNS:$CLIENT_DNS_NAME, client; the egress proxy's audit identity)"
  openssl req -newkey rsa:2048 -nodes \
    -keyout "$PROXY_DIR/audit-client.key" -out "$PROXY_DIR/audit-client.csr" \
    -subj "/CN=$CLIENT_DNS_NAME" 2>/dev/null
  openssl x509 -req -in "$PROXY_DIR/audit-client.csr" \
    -CA "$RUNTIME_DIR/runtime-ca.crt" -CAkey "$RUNTIME_DIR/runtime-ca.key" -CAcreateserial \
    -CAserial "$PROXY_DIR/runtime-ca.srl" \
    -out "$PROXY_DIR/audit-client.crt" -days "$DAYS" \
    -extfile <(printf 'subjectAltName=DNS:%s\nextendedKeyUsage=clientAuth\nkeyUsage=critical,digitalSignature\n' "$CLIENT_DNS_NAME") 2>/dev/null
  rm -f "$PROXY_DIR/audit-client.csr" "$PROXY_DIR/runtime-ca.srl"
fi

cp "$RUNTIME_DIR/runtime-ca.crt" "$PROXY_DIR/runtime-ca.crt"

# World-readable, like the vector and provider material: throwaway local
# material in a gitignored directory, read by the proxy (distroless nonroot,
# uid 65532) through a read-only bind mount, where a 0600 file owned by the
# host user is unreadable under rootless podman.
chmod 644 "$PROXY_DIR"/*

echo "→ Done. Egress proxy audit material in $PROXY_DIR"
echo "  identity: dns:$CLIENT_DNS_NAME (set ELITEA_EGRESS_AUDIT_CLIENTS to this on elitea-main)"
