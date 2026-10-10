#!/usr/bin/env bash
# render-vector.sh — elitea-vector and its Qdrant (ADR-0031 V0).
#
# The render-inventory.sh shape: every assertion reads the RENDERED manifest,
# and every refusal case is the complete install minus exactly one thing.
#
# What is asserted, and why it would fail silently otherwise:
#   * vector off renders nothing of it, and elitea-main gets no
#     ELITEA_VECTOR_INTROSPECTION_CLIENTS;
#   * vector on admits elitea-vector's client identity on elitea-main's
#     control listener (env AND NetworkPolicy), and that identity is the client
#     certificate's one DNS SAN — a server certificate with several SANs has
#     no valid client identity;
#   * the internal Qdrant reads the same API-key Secret elitea-vector mounts,
#     and only elitea-vector reaches its gRPC port;
#   * external mode renders no StatefulSet and passes the URL through;
#   * elitea-vector's gRPC port admits the DeepWiki and Inventory engine pods
#     (callback-token callers) exactly when each component is enabled, and
#     never when it is not;
#   * vector.allowedSpaces reaches ELITEA_VECTOR_ALLOWED_SPACES (empty by
#     default: no allowlist).
#
# Usage: deploy/helm/tests/render-vector.sh
# Needs: helm, yq. No cluster, no network.
set -euo pipefail

CHART="deploy/helm/elitea"

GATEWAY_POSTURES="--set llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1"

# values-standalone.yaml turns the runtime plane on, which vector needs.
# vector.enabled adds the 8-connection token-introspection pool to every
# elitea-main replica (50 instead of 42), so the profile's 4 replicas no
# longer fit its 175 available connections: 3 x 50 = 150 do.
COMPLETE="\
-f $CHART/values-standalone.yaml \
--set llmGateway.egressPosture=allowlist \
--set llmGateway.env.GATEWAY_EGRESS_ALLOWLIST=elitea.invalid:8000 \
--set vector.enabled=true \
--set main.autoscaling.maxReplicas=3 \
--set vector.qdrant.apiKeySecret.name=elitea-qdrant-api-key"

failures=0
note() { printf '  %s\n' "$1"; }
fail() { printf 'FAIL: %s\n' "$1" >&2; failures=$((failures + 1)); }

render() { helm template test "$CHART" $GATEWAY_POSTURES "$@" 2>&1; }

select_one() {
  # manifest, kind, name, expression
  printf '%s' "$1" | yq eval-all "select(.kind == \"$2\" and .metadata.name == \"$3\") | $4" -
}

expect_refusal() {
  # description, expected message fragment, extra args...
  local description="$1" fragment="$2"
  shift 2
  local out
  if out="$(render $COMPLETE "$@")"; then
    fail "$description: rendered, expected a refusal"
  elif ! printf '%s' "$out" | grep -qF "$fragment"; then
    fail "$description: refused for the wrong reason: $(printf '%s' "$out" | tail -1)"
  else
    note "refused: $description"
  fi
}

echo "== vector off (the default)"
off="$(render)"
# Comment lines excluded: elitea-main's NetworkPolicy header names the peer.
if printf '%s' "$off" | grep -v '^[[:space:]]*#' | grep -q 'elitea-vector\|elitea-qdrant\|ELITEA_VECTOR_'; then
  fail "vector is off, but something of it rendered"
else
  note "nothing of elitea-vector or Qdrant renders"
fi

echo "== vector on, internal Qdrant"
on="$(render $COMPLETE)"

clients="$(select_one "$on" ConfigMap elitea-main-config '.data.ELITEA_VECTOR_INTROSPECTION_CLIENTS')"
[ "$clients" = "dns:elitea-vector" ] \
  && note "elitea-main admits dns:elitea-vector" \
  || fail "ELITEA_VECTOR_INTROSPECTION_CLIENTS is '$clients', want dns:elitea-vector"

sans="$(select_one "$on" Certificate elitea-vector-client '.spec.dnsNames | length')"
san="$(select_one "$on" Certificate elitea-vector-client '.spec.dnsNames[0]')"
usage="$(select_one "$on" Certificate elitea-vector-client '.spec.usages | join(",")')"
[ "$sans" = "1" ] && [ "$san" = "elitea-vector" ] && [ "$usage" = "client auth" ] \
  && note "the client certificate has one DNS SAN (elitea-vector), client auth" \
  || fail "client certificate SANs=$sans first=$san usages=$usage"

control_from="$(select_one "$on" NetworkPolicy elitea-main-netpol \
  '.spec.ingress[] | select(.ports[0].port == 9443 and (.ports | length) == 1) | .from[0].podSelector.matchLabels."app.kubernetes.io/name"')"
[ "$control_from" = "elitea-vector" ] \
  && note "elitea-main's NetworkPolicy admits elitea-vector on the control port only" \
  || fail "no control-port-only ingress from elitea-vector on elitea-main (got '$control_from')"

dep_key="$(select_one "$on" Deployment elitea-vector '.spec.template.spec.volumes[] | select(.name == "qdrant-api-key") | .secret.secretName')"
sts_key="$(select_one "$on" StatefulSet elitea-qdrant '.spec.template.spec.containers[0].env[] | select(.name == "QDRANT__SERVICE__API_KEY") | .valueFrom.secretKeyRef.name')"
[ "$dep_key" = "elitea-qdrant-api-key" ] && [ "$sts_key" = "elitea-qdrant-api-key" ] \
  && note "elitea-vector and Qdrant read the same API-key Secret" \
  || fail "API-key Secret: vector '$dep_key', qdrant '$sts_key'"

url="$(select_one "$on" Deployment elitea-vector '.spec.template.spec.containers[0].env[] | select(.name == "ELITEA_VECTOR_QDRANT_URL") | .value')"
[ "$url" = "http://elitea-qdrant.default.svc:6334" ] \
  && note "elitea-vector dials the internal Qdrant over gRPC" \
  || fail "ELITEA_VECTOR_QDRANT_URL is '$url'"

qdrant_from="$(select_one "$on" NetworkPolicy elitea-qdrant-netpol \
  '.spec.ingress[] | select(.ports[0].port == 6334) | .from[0].podSelector.matchLabels."app.kubernetes.io/name"')"
[ "$qdrant_from" = "elitea-vector" ] \
  && note "only elitea-vector reaches Qdrant's gRPC port" \
  || fail "Qdrant gRPC ingress admits '$qdrant_from'"

min="$(select_one "$on" HorizontalPodAutoscaler elitea-vector '.spec.minReplicas')"
[ "$min" -ge 2 ] && note "the HPA keeps at least 2 replicas" || fail "HPA minReplicas is $min"

echo "== vector on, external Qdrant"
ext="$(render $COMPLETE --set vector.qdrant.mode=external \
  --set vector.qdrant.external.url=https://qdrant.example.invalid:6334 \
  --set vector.qdrant.external.caSecretName=qdrant-ca)"
if printf '%s' "$ext" | grep -q 'kind: StatefulSet' && \
   [ -n "$(select_one "$ext" StatefulSet elitea-qdrant '.metadata.name')" ]; then
  fail "external mode still renders the Qdrant StatefulSet"
else
  note "external mode renders no Qdrant"
fi
ext_url="$(select_one "$ext" Deployment elitea-vector '.spec.template.spec.containers[0].env[] | select(.name == "ELITEA_VECTOR_QDRANT_URL") | .value')"
ssl="$(select_one "$ext" Deployment elitea-vector '.spec.template.spec.containers[0].env[] | select(.name == "SSL_CERT_FILE") | .value')"
[ "$ext_url" = "https://qdrant.example.invalid:6334" ] && [ "$ssl" = "/etc/elitea-vector/qdrant-ca/ca.crt" ] \
  && note "the external URL and its CA reach elitea-vector" \
  || fail "external URL '$ext_url', SSL_CERT_FILE '$ssl'"

echo "== engine callers and the space allowlist"
grpc_names() {
  # manifest -> the app.kubernetes.io/name of every peer admitted on the gRPC port
  select_one "$1" NetworkPolicy elitea-vector-netpol \
    '.spec.ingress[] | select(.ports[0].port == 9470) | .from[].podSelector.matchLabels."app.kubernetes.io/name"' | sort | paste -sd, -
}
base_peers="$(grpc_names "$on")"
case ",$base_peers," in
  *,elitea-deepwiki,* | *,elitea-inventory,*) fail "engines are off, but the gRPC port admits $base_peers" ;;
  *) note "engines off: the gRPC port admits only $base_peers" ;;
esac

DEEPWIKI="\
--set deepwiki.enabled=true \
--set deepwiki.env.ELITEA_DEEPWIKI_GIT_ALLOWLIST=github.com \
--set main.fileConfig.deepwikiClientMaterial.enabled=true \
--set main.fileConfig.deepwikiClientMaterial.secretName=elitea-main-deepwiki-client-tls \
--set main.env.ELITEA_DEEPWIKI_ENABLED=true \
--set main.env.ELITEA_DEEPWIKI_BASE_URL=https://elitea-deepwiki-svc:8443 \
--set main.env.ELITEA_DEEPWIKI_CALLBACK_BASE_URL=http://elitea-main:8080 \
--set main.env.ELITEA_DEEPWIKI_GIT_ALLOWLIST=github.com \
--set main.env.ELITEA_DEEPWIKI_CLIENT_CERT_FILE=/run/elitea-deepwiki/tls.crt \
--set main.env.ELITEA_DEEPWIKI_CLIENT_KEY_FILE=/run/elitea-deepwiki/tls.key \
--set main.env.ELITEA_DEEPWIKI_CA_FILE=/run/elitea-deepwiki/ca.crt"
INVENTORY="\
--set inventory.enabled=true \
--set main.fileConfig.inventoryClientMaterial.enabled=true \
--set main.fileConfig.inventoryClientMaterial.secretName=elitea-main-inventory-client-tls \
--set main.env.ELITEA_INVENTORY_ENABLED=true \
--set main.env.ELITEA_INVENTORY_BASE_URL=https://elitea-inventory-svc:8443 \
--set main.env.ELITEA_INVENTORY_CALLBACK_BASE_URL=http://elitea-main:8080 \
--set main.env.ELITEA_INVENTORY_GIT_ALLOWLIST=github.com \
--set inventory.env.ELITEA_INVENTORY_GIT_ALLOWLIST=github.com \
--set inventory.secrets.ELITEA_INVENTORY_DATABASE_URL.secretName=elitea-inventory-db \
--set inventory.secrets.ELITEA_INVENTORY_DATABASE_URL.key=url \
--set main.env.ELITEA_INVENTORY_CLIENT_CERT_FILE=/run/elitea-inventory/tls.crt \
--set main.env.ELITEA_INVENTORY_CLIENT_KEY_FILE=/run/elitea-inventory/tls.key \
--set main.env.ELITEA_INVENTORY_CA_FILE=/run/elitea-inventory/ca.crt"

with_deepwiki="$(render $COMPLETE $DEEPWIKI)"
peers="$(grpc_names "$with_deepwiki")"
case ",$peers," in
  *,elitea-deepwiki,*) note "DeepWiki enabled: its pods are admitted on the gRPC port" ;;
  *) fail "DeepWiki is enabled, but the gRPC port admits only $peers" ;;
esac
case ",$peers," in
  *,elitea-inventory,*) fail "Inventory is off, but its pods are admitted ($peers)" ;;
  *) ;;
esac

with_inventory="$(render $COMPLETE $INVENTORY)"
peers="$(grpc_names "$with_inventory")"
case ",$peers," in
  *,elitea-inventory,*) note "Inventory enabled: its pods are admitted on the gRPC port" ;;
  *) fail "Inventory is enabled, but the gRPC port admits only $peers" ;;
esac
case ",$peers," in
  *,elitea-deepwiki,*) fail "DeepWiki is off, but its pods are admitted ($peers)" ;;
  *) ;;
esac

# Each engine peer is pinned to the release namespace, like every other peer.
ns="$(select_one "$with_deepwiki" NetworkPolicy elitea-vector-netpol \
  '.spec.ingress[] | select(.ports[0].port == 9470) | .from[] | select(.podSelector.matchLabels."app.kubernetes.io/name" == "elitea-deepwiki") | .namespaceSelector.matchLabels."kubernetes.io/metadata.name"')"
[ "$ns" = "default" ] \
  && note "the engine peer is scoped to the release namespace" \
  || fail "the DeepWiki peer's namespaceSelector is '$ns'"

spaces_default="$(select_one "$on" Deployment elitea-vector '.spec.template.spec.containers[0].env[] | select(.name == "ELITEA_VECTOR_ALLOWED_SPACES") | .value')"
[ "$spaces_default" = "" ] \
  && note "ELITEA_VECTOR_ALLOWED_SPACES is empty by default (no allowlist)" \
  || fail "ELITEA_VECTOR_ALLOWED_SPACES defaults to '$spaces_default'"
spaces="$(select_one "$(render $COMPLETE --set 'vector.allowedSpaces[0]=bge-m3:1024' --set 'vector.allowedSpaces[1]=text-embedding-3-small:1536')" \
  Deployment elitea-vector '.spec.template.spec.containers[0].env[] | select(.name == "ELITEA_VECTOR_ALLOWED_SPACES") | .value')"
[ "$spaces" = "bge-m3:1024,text-embedding-3-small:1536" ] \
  && note "vector.allowedSpaces reaches ELITEA_VECTOR_ALLOWED_SPACES" \
  || fail "ELITEA_VECTOR_ALLOWED_SPACES is '$spaces'"

echo "== refusals"
expect_refusal "runtime plane off" "vector.enabled needs main.runtime.enabled" \
  --set main.runtime.enabled=false --set worker.enabled=false
expect_refusal "no Qdrant API key" "vector.qdrant.apiKeySecret.name is empty" \
  --set vector.qdrant.apiKeySecret.name=
expect_refusal "external without a URL" "vector.qdrant.external.url" \
  --set vector.qdrant.mode=external
expect_refusal "unknown Qdrant mode" "vector.qdrant.mode must be internal or external" \
  --set vector.qdrant.mode=embedded
expect_refusal "replication above node count" "cannot place more replicas than nodes" \
  --set vector.qdrant.internal.replicas=1
expect_refusal "one replica" "minReplicas must be at least 2" \
  --set vector.autoscaling.minReplicas=1
expect_refusal "the introspection pool is in the connection budget" "50 per elitea-main replica" \
  --set main.autoscaling.maxReplicas=4
expect_refusal "malformed admin identity" "is not a canonical identity" \
  --set 'vector.adminIdentities[0]=elitea-main'

if [ "$failures" -ne 0 ]; then
  echo "render-vector: $failures failure(s)" >&2
  exit 1
fi
echo "render-vector: OK"
