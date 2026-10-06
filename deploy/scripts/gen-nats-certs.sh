#!/usr/bin/env bash
# Generate local material for the OPT-IN secured NATS compose overlay
# (deploy/docker-compose.nats-secure.yml, #1076).
#
# Compose runs NATS plaintext with no client identity by default: the server
# is not published to the host, and only compose services reach it. This
# script and the overlay run the CLUSTER posture instead, so a developer can
# see the same refusals a cluster gives:
#
#   * a dedicated NATS CA (never the gateway or runtime CA),
#   * a server certificate for the compose service name `nats`,
#   * one client certificate per identity, with the URI SAN
#     spiffe://elitea.internal/nats/<identity> the permission table maps,
#   * nats.conf rendered from the NATS CHART (scripts/nats/render-secure-conf.sh),
#     so the permission table is the chart's own and not a copy. Its paths are
#     the chart's container paths; the overlay mounts the files there.
#
# Needs openssl, helm (and network access once, for the upstream subchart),
# python3 + PyYAML.
#
# Output (gitignored with the rest of deploy/certs):
#   deploy/certs/nats/ca/ca.crt                   the trust root (ca.key beside the dir)
#   deploy/certs/nats/server/tls.{crt,key}        served by `nats`
#   deploy/certs/nats/clients/<identity>/{ca.crt,tls.crt,tls.key}
#   deploy/certs/nats/nats.conf
#
# Idempotent: regenerates only when something is missing or expires within
# 24h (FORCE=1 to regenerate). nats.conf is always re-rendered, so a change to
# the chart's permission table reaches the overlay on the next run.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
OUT="${ROOT}/deploy/certs/nats"
DAYS="${NATS_CERT_DAYS:-825}"
TRUST_DOMAIN="elitea.internal"
# One identity per client, and one bootstrap identity per NATS account
# (MAIN, GATEWAY, RUNTIME: deploy/helm/nats/values.yaml).
IDENTITIES="elitea-main elitea-llm-gateway elitea-scheduler elitea-nats-bootstrap-main elitea-nats-bootstrap-gateway elitea-nats-bootstrap-runtime"

command -v openssl >/dev/null || { echo "ERROR: openssl not found" >&2; exit 1; }
mkdir -p "$OUT/ca" "$OUT/server" "$OUT/clients"

needs_regen=0
for f in ca/ca.crt ca.key server/tls.crt server/tls.key; do
  [ -f "$OUT/$f" ] || needs_regen=1
done
for id in $IDENTITIES; do
  [ -f "$OUT/clients/$id/tls.crt" ] || needs_regen=1
done
if [ "$needs_regen" -eq 0 ]; then
  for c in ca/ca.crt server/tls.crt; do
    openssl x509 -checkend 86400 -noout -in "$OUT/$c" >/dev/null 2>&1 || needs_regen=1
  done
fi

if [ "$needs_regen" -eq 1 ] || [ "${FORCE:-0}" = "1" ]; then
  echo "→ Generating the dev NATS CA and certificates in $OUT …"
  rm -rf "$OUT/ca" "$OUT/server" "$OUT/clients" "$OUT/ca.key" "$OUT/ca.srl"
  mkdir -p "$OUT/ca" "$OUT/server" "$OUT/clients"

  openssl req -x509 -newkey rsa:2048 -nodes \
    -keyout "$OUT/ca.key" -out "$OUT/ca/ca.crt" \
    -days "$DAYS" -subj "/CN=elitea-nats-ca (compose dev)" \
    -addext "basicConstraints=critical,CA:TRUE" \
    -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null

  # issue <dir> <CN> <extfile body>
  issue() {
    local dir="$1" cn="$2" ext="$3"
    mkdir -p "$dir"
    openssl req -newkey rsa:2048 -nodes \
      -keyout "$dir/tls.key" -out "$dir/tls.csr" -subj "/CN=${cn}" 2>/dev/null
    openssl x509 -req -in "$dir/tls.csr" \
      -CA "$OUT/ca/ca.crt" -CAkey "$OUT/ca.key" -CAcreateserial -CAserial "$OUT/ca.srl" \
      -out "$dir/tls.crt" -days "$DAYS" \
      -extfile <(printf '%s\n' "$ext") 2>/dev/null
    rm -f "$dir/tls.csr"
  }

  issue "$OUT/server" "nats" \
    "subjectAltName=DNS:nats,DNS:localhost,IP:127.0.0.1
extendedKeyUsage=serverAuth,clientAuth"
  for id in $IDENTITIES; do
    issue "$OUT/clients/$id" "$id" \
      "subjectAltName=URI:spiffe://${TRUST_DOMAIN}/nats/${id}
extendedKeyUsage=clientAuth"
    cp "$OUT/ca/ca.crt" "$OUT/clients/$id/ca.crt"
  done
  # The services run as distroless nonroot; world-readable is acceptable for
  # throwaway local material and avoids per-runtime uid juggling (the same
  # call deploy/scripts/gen-gateway-certs.sh makes). Never ship this.
  find "$OUT/ca" "$OUT/server" "$OUT/clients" -type f -exec chmod 644 {} +
else
  echo "→ Certificates in $OUT are present and valid (FORCE=1 to regenerate)."
fi

bash "${ROOT}/scripts/nats/render-secure-conf.sh" "$OUT/nats.conf"
chmod 644 "$OUT/nats.conf"
echo "→ Done. Start the secured overlay with:"
echo "    podman compose -f deploy/docker-compose.yml -f deploy/docker-compose.nats-secure.yml up -d"
