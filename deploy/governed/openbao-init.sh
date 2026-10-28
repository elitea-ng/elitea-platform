#!/bin/sh
# Configures the governed conformance stack's OpenBao (issue #1124) the way an
# operator configures a corporate Vault for Elitea: a KV v2 mount with
# cas_required, a JWT auth method that trusts the workload token's key, one
# policy and one role per workload scope (<prefix>-<scope>).
#
# OpenBao runs in dev mode (in memory), so this one-shot runs on every `up`
# and is idempotent. The root token is a dev-mode constant on a private
# compose network; it never leaves this stack.
#
# Environment (set by deploy/docker-compose.governed.yml):
#   BAO_ADDR, BAO_CACERT, BAO_TOKEN   where and how to reach OpenBao
#   GOVERNED_VAULT_KV_MOUNT           the KV v2 mount (kv)
#   GOVERNED_VAULT_AUTH_MOUNT         the JWT auth mount (jwt)
#   GOVERNED_VAULT_ROLE_PREFIX        role prefix (elitea)
#   GOVERNED_VAULT_SCOPES             space-separated workload scopes
#   GOVERNED_VAULT_ISSUER, _SUBJECT, _AUDIENCE   the workload token's claims
#   GOVERNED_VAULT_PUBLIC_KEY         the PEM file of the key the token is signed with
set -eu

attempt=0
until bao status >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  if [ "$attempt" -gt 60 ]; then
    echo "openbao-init: OpenBao at ${BAO_ADDR} never answered" >&2
    exit 1
  fi
  sleep 1
done

kv="${GOVERNED_VAULT_KV_MOUNT}"
auth="${GOVERNED_VAULT_AUTH_MOUNT}"
prefix="${GOVERNED_VAULT_ROLE_PREFIX}"

if ! bao secrets list -format=json | grep -q "\"${kv}/\""; then
  bao secrets enable -path="${kv}" -version=2 kv
fi
# cas_required: every write names the version it replaces. Main's readiness
# check refuses a mount without it, and the write path refuses to write there.
bao write "${kv}/config" cas_required=true >/dev/null

bao policy write "${prefix}-governed" - >/dev/null <<EOF
path "${kv}/data/*"     { capabilities = ["create", "read", "update"] }
path "${kv}/metadata/*" { capabilities = ["read"] }
path "${kv}/destroy/*"  { capabilities = ["update"] }
path "${kv}/config"     { capabilities = ["read"] }
EOF

if ! bao auth list -format=json | grep -q "\"${auth}/\""; then
  bao auth enable -path="${auth}" jwt
fi
bao write "auth/${auth}/config" \
  jwt_validation_pubkeys=@"${GOVERNED_VAULT_PUBLIC_KEY}" \
  bound_issuer="${GOVERNED_VAULT_ISSUER}" >/dev/null

for scope in ${GOVERNED_VAULT_SCOPES}; do
  bao write "auth/${auth}/role/${prefix}-${scope}" \
    role_type=jwt \
    bound_audiences="${GOVERNED_VAULT_AUDIENCE}" \
    user_claim=sub \
    bound_subject="${GOVERNED_VAULT_SUBJECT}" \
    token_policies="${prefix}-governed" \
    token_ttl=10m >/dev/null
  echo "openbao-init: role ${prefix}-${scope}"
done
echo "openbao-init: ${kv} (cas_required) and ${auth} ready"
