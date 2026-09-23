#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <centry-repository> <runtime-directory>" >&2
  exit 2
fi

centry_dir="$(cd "$1" && pwd -P)"
runtime_root="$2"

: "${APPLICATION_AUTH_SECRET_KEY:?missing APPLICATION_AUTH_SECRET_KEY in the Centry environment}"
: "${DEFAULT_ADMIN_PASSWORD:?missing DEFAULT_ADMIN_PASSWORD in the Centry environment}"
: "${SECRETS_MASTER_KEY:?missing SECRETS_MASTER_KEY in the Centry environment}"
: "${POSTGRES_USER:?missing POSTGRES_USER in the Centry environment}"
: "${POSTGRES_PASSWORD:?missing POSTGRES_PASSWORD in the Centry environment}"
: "${POSTGRES_PORT:?missing POSTGRES_PORT in the Centry environment}"

for command in jq openssl; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "required command is not installed: $command" >&2
    exit 2
  fi
done

mkdir -p -m 700 "$runtime_root"
runtime_root="$(cd "$runtime_root" && pwd -P)"
chmod 700 "$runtime_root"

auth_targets=(
  "$runtime_root/form/form-users.json"
  "$runtime_root/redis-password"
  "$runtime_root/attempt-hmac-key"
  "$runtime_root/pat-hs512-key"
  "$runtime_root/redis-ca.crt"
  "$runtime_root/redis-server.crt"
  "$runtime_root/redis-server.key"
)

auth_present=0
for target in "${auth_targets[@]}"; do
  if [[ -e "$target" ]]; then
    auth_present=$((auth_present + 1))
  fi
done

if [[ "$auth_present" -eq 0 ]]; then
  "$centry_dir/hybrid_auth/bootstrap-auth-pov.sh" "$runtime_root"
elif [[ "$auth_present" -ne "${#auth_targets[@]}" ]]; then
  echo "Auth runtime material is incomplete; refusing to rotate or guess missing files in $runtime_root" >&2
  exit 1
fi

runtime_targets=(
  "$runtime_root/runtime/runtime-ca.crt"
  "$runtime_root/runtime/redis-server.crt"
  "$runtime_root/runtime/redis-server.key"
  "$runtime_root/runtime/gateway-server.crt"
  "$runtime_root/runtime/gateway-server.key"
  "$runtime_root/runtime/control-server.crt"
  "$runtime_root/runtime/control-server.key"
  "$runtime_root/runtime/output-server.crt"
  "$runtime_root/runtime/output-server.key"
  "$runtime_root/runtime/content-server.crt"
  "$runtime_root/runtime/content-server.key"
  "$runtime_root/runtime/workload-client-ca.crt"
  "$runtime_root/runtime/indexer-worker-client.crt"
  "$runtime_root/runtime/indexer-worker-client.key"
  "$runtime_root/runtime/command-signing-key.pem"
  "$runtime_root/runtime/command-signing-keyring.json"
  "$runtime_root/runtime/redis-health-password"
  "$runtime_root/runtime/redis-producer-password"
  "$runtime_root/runtime/redis-worker-password"
  "$runtime_root/runtime/redis-indexer-worker-password"
  "$runtime_root/runtime/redis-bootstrap-password"
  "$runtime_root/runtime/vault-master-key"
  "$runtime_root/runtime/litellm-master-key"
  "$runtime_root/runtime/indexer-output-spool-key"
)

runtime_present=0
for target in "${runtime_targets[@]}"; do
  if [[ -e "$target" ]]; then
    runtime_present=$((runtime_present + 1))
  fi
done

if [[ "$runtime_present" -eq 0 ]]; then
  ELITEA_POV_VAULT_MASTER_KEY="$SECRETS_MASTER_KEY" \
    ELITEA_POV_LITELLM_MASTER_KEY="sk-$DEFAULT_ADMIN_PASSWORD" \
    "$centry_dir/hybrid_auth/bootstrap-runtime.sh" "$runtime_root"
elif [[ "$runtime_present" -ne "${#runtime_targets[@]}" ]]; then
  echo "workload runtime material is incomplete; refusing to rotate or guess missing files in $runtime_root/runtime" >&2
  exit 1
fi

for certificate in \
  "$runtime_root/redis-ca.crt" \
  "$runtime_root/runtime/runtime-ca.crt" \
  "$runtime_root/runtime/gateway-server.crt" \
  "$runtime_root/runtime/control-server.crt" \
  "$runtime_root/runtime/output-server.crt" \
  "$runtime_root/runtime/content-server.crt"
do
  if ! openssl x509 -checkend 86400 -noout -in "$certificate" >/dev/null 2>&1; then
    echo "runtime certificate is expired or expires within 24 hours: $certificate" >&2
    echo "rotate the local runtime directory before starting the mixed deployment" >&2
    exit 1
  fi
done

runtime="$runtime_root/runtime"
chmod 700 "$runtime"

health_password="$(<"$runtime/redis-health-password")"
bootstrap_password="$(<"$runtime/redis-bootstrap-password")"
producer_password="$(<"$runtime/redis-producer-password")"
worker_password="$(<"$runtime/redis-worker-password")"
indexer_worker_password="$(<"$runtime/redis-indexer-worker-password")"

acl_tmp="$(mktemp "$runtime/.redis-users-v2.acl.XXXXXX")"
config_tmp="$(mktemp "$runtime/.indexer-runtime-v2.json.XXXXXX")"
checkpoint_tmp="$(mktemp "$runtime/.agent-checkpoint-connection.XXXXXX")"
main_database_config_tmp="$(mktemp "$runtime/.pylon-main-shared.XXXXXX")"
auth_database_config_tmp="$(mktemp "$runtime/.pylon-auth-core.XXXXXX")"
cleanup() {
  rm -f "$acl_tmp" "$config_tmp" "$checkpoint_tmp" \
    "$main_database_config_tmp" "$auth_database_config_tmp"
}
trap cleanup EXIT

# The current services keep their production defaults in tracked Centry
# configuration. The mixed deployment runs them beside six isolated Go pools
# on one local PostgreSQL server, so generate deployment-scoped copies instead
# of mutating those authoritative defaults.
sed \
  -e 's/^    pool_size: 100$/    pool_size: 16/' \
  -e 's/^    max_overflow: 200$/    max_overflow: 4/' \
  "$centry_dir/pylon_main/configs/shared.yml" > "$main_database_config_tmp"
sed \
  -e 's/^  pool_size: 25$/  pool_size: 8/' \
  -e 's/^  max_overflow: 25$/  max_overflow: 2/' \
  "$centry_dir/pylon_auth/configs/auth_core.yml" > "$auth_database_config_tmp"
if ! grep -q '^    pool_size: 16$' "$main_database_config_tmp" || \
   ! grep -q '^    max_overflow: 4$' "$main_database_config_tmp" || \
   ! grep -q '^  pool_size: 8$' "$auth_database_config_tmp" || \
   ! grep -q '^  max_overflow: 2$' "$auth_database_config_tmp"; then
  echo "failed to generate bounded hybrid PostgreSQL pool configuration" >&2
  exit 1
fi
chmod 600 "$main_database_config_tmp" "$auth_database_config_tmp"

if [[ ! "$POSTGRES_PORT" =~ ^[1-9][0-9]{0,4}$ ]] || (( POSTGRES_PORT > 65535 )); then
  echo "POSTGRES_PORT must be a valid TCP port" >&2
  exit 2
fi
postgres_user_uri="$(printf '%s' "$POSTGRES_USER" | jq -sRr @uri)"
postgres_password_uri="$(printf '%s' "$POSTGRES_PASSWORD" | jq -sRr @uri)"
printf 'postgresql://%s:%s@postgres:%s/agentstate?sslmode=disable' \
  "$postgres_user_uri" "$postgres_password_uri" "$POSTGRES_PORT" \
  > "$checkpoint_tmp"
chmod 600 "$checkpoint_tmp"

index_v1_stream='commands.v1.index.ingest.indexing.shared.1.0'
index_v2_stream='commands.v1.index.ingest.indexing.shared.2.0'
configuration_stream='commands.v1.configuration.validate.v1.validation-small.shared-credential-free.1.0'
replay_wake_channel='elitea:runtime:execution-replay:wake:v1'

printf '%s\n' \
  'user default off' \
  "user health on >$health_password -@all +@connection +ping +xlen +xpending +hlen +xinfo ~$index_v1_stream ~$index_v1_stream:delivery-index.v1 ~$index_v2_stream ~$index_v2_stream:delivery-index.v1" \
  "user bootstrap on >$bootstrap_password -@all +@connection +ping +xgroup +xinfo ~$configuration_stream ~$index_v1_stream ~$index_v2_stream" \
  "user producer on >$producer_password -@all +@connection +ping +eval +evalsha +xlen +xadd +hget +xrange +hdel +hlen +hset +publish +subscribe +unsubscribe ~$configuration_stream ~$configuration_stream:delivery-index.v1 ~$index_v1_stream ~$index_v1_stream:delivery-index.v1 ~$index_v2_stream ~$index_v2_stream:delivery-index.v1 &$replay_wake_channel" \
  "user worker on >$worker_password -@all +@connection +ping +eval +evalsha +xreadgroup +xclaim +xautoclaim +hget +xrange +xpending +xack +xdel +hdel ~$configuration_stream ~$configuration_stream:delivery-index.v1" \
  "user indexer-worker on >$indexer_worker_password -@all +@connection +ping +eval +evalsha +xreadgroup +xclaim +xautoclaim +hget +xrange +xpending +xack +xdel +hdel ~$index_v1_stream ~$index_v1_stream:delivery-index.v1 ~$index_v2_stream ~$index_v2_stream:delivery-index.v1" \
  > "$acl_tmp"
chmod 600 "$acl_tmp"

printf '%s\n' \
  '{' \
  '  "schema_version": "elitea.runtime-deploy.v1",' \
  '  "limits_revision": "elitea.runtime.limits.conformance.v2",' \
  '  "workload_session_id": "indexer-worker-pov-1",' \
  '  "producer_id": "indexer-worker-pov-1",' \
  '  "consumer_id": "indexer-worker-pov-v2-consumer",' \
  '  "redis_url": "rediss://indexer-worker@runtime-redis:6380/0",' \
  '  "redis_password_path": "/run/elitea-runtime/redis-indexer-worker-password",' \
  "  \"redis_stream\": \"$index_v2_stream\"," \
  '  "redis_group": "elitea-indexer-worker-v2",' \
  '  "control_target": "elitea-main-runtime:9443",' \
  '  "output_target": "elitea-main-runtime:9444",' \
  '  "content_origin": "https://elitea-main-runtime:9445",' \
  '  "platform_origin": "https://elitea-gateway",' \
  '  "ca_path": "/run/elitea-runtime/runtime-ca.crt",' \
  '  "certificate_path": "/run/elitea-runtime/indexer-worker-client.crt",' \
  '  "private_key_path": "/run/elitea-runtime/indexer-worker-client.key",' \
  '  "ed25519_keyring_path": "/run/elitea-runtime/command-signing-keyring.json",' \
  '  "spool_root": "/var/lib/elitea-indexer-worker/output-spool",' \
  '  "spool_key_path": "/run/elitea-runtime/indexer-output-spool-key",' \
  '  "agent_checkpoint_connection_path": "/run/elitea-runtime/agent-checkpoint-connection",' \
  '  "limits": {' \
  '    "redis_read_batch": 4,' \
  '    "redis_block_millis": 1000,' \
  '    "redis_reclaim_idle_millis": 60000,' \
  '    "redis_reclaim_interval_millis": 5000,' \
  '    "dependency_retry_millis": 250,' \
  '    "delivery_max_concurrency": 2,' \
  '    "delivery_queue_capacity": 4,' \
  '    "sync_max_workers": 2,' \
  '    "sync_max_in_flight": 2,' \
  '    "admission_timeout_millis": 5000,' \
  '    "grpc_deadline_millis": 5000,' \
  '    "content_timeout_millis": 15000,' \
  '    "http_max_connections": 8,' \
  '    "http_max_keepalive_connections": 4,' \
  '    "output_max_queued_frames": 2,' \
  '    "output_max_queued_bytes": 131072,' \
  '    "output_max_sessions": 2,' \
  '    "output_ack_timeout_millis": 15000,' \
  '    "output_stream_deadline_millis": 300000,' \
  '    "lease_poll_interval_millis": 10000,' \
  '    "shutdown_timeout_millis": 30000' \
  '  }' \
  '}' > "$config_tmp"
chmod 600 "$config_tmp"

jq -e \
  --arg stream "$index_v2_stream" \
  '.schema_version == "elitea.runtime-deploy.v1"
   and .redis_stream == $stream
   and .redis_group == "elitea-indexer-worker-v2"
   and .workload_session_id == "indexer-worker-pov-1"' \
  "$config_tmp" >/dev/null

mv "$acl_tmp" "$runtime/redis-users-v2.acl"
mv "$config_tmp" "$runtime/indexer-runtime-v2.json"
mv "$checkpoint_tmp" "$runtime/agent-checkpoint-connection"
mv "$main_database_config_tmp" "$runtime/pylon-main-shared.yml"
mv "$auth_database_config_tmp" "$runtime/pylon-auth-core.yml"
trap - EXIT

echo "mixed-deployment runtime material is ready at $runtime_root"
