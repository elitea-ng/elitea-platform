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
#     default: no allowlist);
#   * elitea-main's administrator client (ADR-0031 decision 4): the chart
#     issues it ONE client certificate (one DNS SAN, client auth, the runtime
#     CA issuer), mounts it, points ELITEA_VECTOR_GRPC_* at it, and
#     elitea-vector admits exactly that identity; mainClient off renders none
#     of it, an external elitea-vector is named explicitly with
#     fileConfig.vectorClientMaterial, and the half-configured shapes (a target
#     without material, material without a target, two volume sources, rust
#     indexing without the client) are refused.
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

# Rollout order (worker unit M9): the whole vector stack is in Argo CD
# sync-wave -5, so elitea-vector is healthy before elitea-main (wave 0) starts
# minting memory sources it introspects for. Every vector manifest shares the
# wave (a Deployment ahead of its own certificates would never become ready).
off_wave="$(printf '%s' "$on" | yq eval 'select((.metadata.labels."app.kubernetes.io/component" == "vector" or .metadata.name == "elitea-qdrant" or .metadata.name == "elitea-qdrant-headless" or .metadata.name == "elitea-qdrant-netpol") and .metadata.annotations."argocd.argoproj.io/sync-wave" != "-5") | .kind + "/" + .metadata.name' - | grep -v '^---$' || true)"
main_wave="$(select_one "$on" Deployment elitea-main '.metadata.annotations."argocd.argoproj.io/sync-wave" // "0"')"
vector_wave="$(select_one "$on" Deployment elitea-vector '.metadata.annotations."argocd.argoproj.io/sync-wave"')"
if [ -z "$off_wave" ] && [ "$vector_wave" = "-5" ] && [ "$main_wave" -gt "$vector_wave" ]; then
  note "the vector stack is in sync-wave -5, before elitea-main (wave $main_wave)"
else
  fail "rollout order: vector wave '$vector_wave', main wave '$main_wave', vector manifests outside wave -5: $off_wave"
fi

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

echo "== elitea-main's administrator client (ADR-0031 decision 4)"
# The chart issues elitea-main ONE client certificate from the runtime-CA
# issuer (one DNS SAN, client auth), mounts it, points the four
# ELITEA_VECTOR_GRPC_* entries at it, and elitea-vector admits that identity.
main_sans="$(select_one "$on" Certificate elitea-main-vector-client '.spec.dnsNames | length')"
main_san="$(select_one "$on" Certificate elitea-main-vector-client '.spec.dnsNames[0]')"
main_usage="$(select_one "$on" Certificate elitea-main-vector-client '.spec.usages | join(",")')"
main_issuer="$(select_one "$on" Certificate elitea-main-vector-client '.spec.issuerRef.name')"
[ "$main_sans" = "1" ] && [ "$main_san" = "elitea-main" ] && [ "$main_usage" = "client auth" ] && [ "$main_issuer" = "elitea-runtime-ca" ] \
  && note "elitea-main's vector client certificate has one DNS SAN (elitea-main), client auth, the runtime-CA issuer" \
  || fail "elitea-main vector client certificate SANs=$main_sans first=$main_san usages=$main_usage issuer=$main_issuer"

admins="$(select_one "$on" Deployment elitea-vector '.spec.template.spec.containers[0].env[] | select(.name == "ELITEA_VECTOR_ADMIN_IDENTITIES") | .value')"
[ "$admins" = "dns:elitea-main" ] \
  && note "elitea-vector admits dns:elitea-main as administrator" \
  || fail "ELITEA_VECTOR_ADMIN_IDENTITIES is '$admins', want dns:elitea-main"
admins_extra="$(select_one "$(render $COMPLETE --set 'vector.adminIdentities[0]=dns:ops-tool')" Deployment elitea-vector '.spec.template.spec.containers[0].env[] | select(.name == "ELITEA_VECTOR_ADMIN_IDENTITIES") | .value')"
[ "$admins_extra" = "dns:elitea-main,dns:ops-tool" ] \
  && note "vector.adminIdentities are admitted beside elitea-main" \
  || fail "ELITEA_VECTOR_ADMIN_IDENTITIES with an extra identity is '$admins_extra'"

main_target="$(select_one "$on" ConfigMap elitea-main-config '.data.ELITEA_VECTOR_GRPC_TARGET')"
main_cert="$(select_one "$on" ConfigMap elitea-main-config '.data.ELITEA_VECTOR_GRPC_CLIENT_CERT_FILE')"
main_key="$(select_one "$on" ConfigMap elitea-main-config '.data.ELITEA_VECTOR_GRPC_CLIENT_KEY_FILE')"
main_ca="$(select_one "$on" ConfigMap elitea-main-config '.data.ELITEA_VECTOR_GRPC_CA_FILE')"
[ "$main_target" = "elitea-vector:9470" ] && [ "$main_cert" = "/run/elitea-vector-client/tls.crt" ] \
  && [ "$main_key" = "/run/elitea-vector-client/tls.key" ] && [ "$main_ca" = "/run/elitea-vector-client/ca.crt" ] \
  && note "elitea-main dials elitea-vector:9470 with the mounted certificate" \
  || fail "elitea-main vector client env: target='$main_target' cert='$main_cert' key='$main_key' ca='$main_ca'"

main_mount="$(select_one "$on" Deployment elitea-main '.spec.template.spec.containers[] | select(.name == "elitea-main") | .volumeMounts[] | select(.name == "vector-client-material") | .mountPath')"
main_secret="$(select_one "$on" Deployment elitea-main '.spec.template.spec.volumes[] | select(.name == "vector-client-material") | .secret.secretName')"
[ "$main_mount" = "/run/elitea-vector-client" ] && [ "$main_secret" = "elitea-main-vector-client-tls" ] \
  && note "the issued Secret is mounted where the env paths point" \
  || fail "elitea-main vector client mount='$main_mount' secret='$main_secret'"

# The client is on elitea-vector's gRPC ingress: the main pods are admitted.
case ",$(select_one "$on" NetworkPolicy elitea-vector-netpol \
    '.spec.ingress[] | select(.ports[0].port == 9470) | .from[].podSelector.matchLabels."app.kubernetes.io/name"' | sort | paste -sd, -)," in
  *,elitea-main,*) note "elitea-vector's gRPC port admits the elitea-main pods" ;;
  *) fail "elitea-vector's gRPC port does not admit elitea-main" ;;
esac

# mainClient off: no certificate, no mount, no env, no admin identity.
noclient="$(render $COMPLETE --set vector.mainClient.enabled=false)"
if [ -n "$(select_one "$noclient" Certificate elitea-main-vector-client '.metadata.name')" ] \
  || [ -n "$(select_one "$noclient" ConfigMap elitea-main-config '.data.ELITEA_VECTOR_GRPC_TARGET // ""')" ] \
  || [ -n "$(select_one "$noclient" Deployment elitea-main '.spec.template.spec.volumes[] | select(.name == "vector-client-material") | .name')" ] \
  || [ "$(select_one "$noclient" Deployment elitea-vector '.spec.template.spec.containers[0].env[] | select(.name == "ELITEA_VECTOR_ADMIN_IDENTITIES") | .value')" != "" ]; then
  fail "vector.mainClient.enabled=false still renders part of elitea-main's client"
else
  note "mainClient off renders no certificate, env, mount or administrator identity"
fi

# An elitea-vector elsewhere: explicit env plus fileConfig.vectorClientMaterial.
EXTERNAL_CLIENT="\
--set vector.mainClient.enabled=false \
--set main.env.ELITEA_VECTOR_GRPC_TARGET=vector.platform.example:9470 \
--set main.env.ELITEA_VECTOR_GRPC_CLIENT_CERT_FILE=/run/elitea-vector-client/tls.crt \
--set main.env.ELITEA_VECTOR_GRPC_CLIENT_KEY_FILE=/run/elitea-vector-client/tls.key \
--set main.env.ELITEA_VECTOR_GRPC_CA_FILE=/run/elitea-vector-client/ca.crt \
--set main.fileConfig.vectorClientMaterial.enabled=true \
--set main.fileConfig.vectorClientMaterial.secretName=my-main-vector-client"
external="$(render $COMPLETE $EXTERNAL_CLIENT)"
ext_target="$(select_one "$external" ConfigMap elitea-main-config '.data.ELITEA_VECTOR_GRPC_TARGET')"
ext_secret="$(select_one "$external" Deployment elitea-main '.spec.template.spec.volumes[] | select(.name == "vector-client-material") | .secret.secretName')"
[ "$ext_target" = "vector.platform.example:9470" ] && [ "$ext_secret" = "my-main-vector-client" ] \
  && note "an external elitea-vector is named explicitly and its material mounted from the operator's Secret" \
  || fail "external vector client: target='$ext_target' secret='$ext_secret'"

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
expect_refusal "elitea-main's identity equal to elitea-vector's own" "must differ" \
  --set vector.mainClient.dnsName=elitea-vector
expect_refusal "a vector target without its material" "env.ELITEA_VECTOR_GRPC_CA_FILE must be set too" \
  --set vector.mainClient.enabled=false --set main.env.ELITEA_VECTOR_GRPC_TARGET=vector.platform.example:9470 \
  --set main.env.ELITEA_VECTOR_GRPC_CLIENT_CERT_FILE=/run/x/tls.crt --set main.env.ELITEA_VECTOR_GRPC_CLIENT_KEY_FILE=/run/x/tls.key
expect_refusal "vector material beside no target" "env.ELITEA_VECTOR_GRPC_TARGET is empty" \
  --set vector.mainClient.enabled=false --set main.env.ELITEA_VECTOR_GRPC_CA_FILE=/run/x/ca.crt
expect_refusal "the abandon switch beside the chart's client" "a vector store that is dialled is not abandoned" \
  --set main.env.ELITEA_VECTOR_CLEANUP_ABANDON=true
expect_refusal "the abandon switch beside an explicit target" "a vector store that is dialled is not abandoned" \
  $EXTERNAL_CLIENT --set main.env.ELITEA_VECTOR_CLEANUP_ABANDON=True
# Without any client the switch renders into elitea-main's ConfigMap; empty it is omitted.
abandoned="$(render $COMPLETE --set vector.mainClient.enabled=false --set main.env.ELITEA_VECTOR_CLEANUP_ABANDON=true)" || fail "abandon without a client refused: $abandoned"
[ "$(select_one "$abandoned" ConfigMap elitea-main-config '.data.ELITEA_VECTOR_CLEANUP_ABANDON')" = "true" ] \
  && note "ELITEA_VECTOR_CLEANUP_ABANDON=true renders without a client" \
  || fail "ELITEA_VECTOR_CLEANUP_ABANDON=true did not reach elitea-main's ConfigMap"
[ -z "$(select_one "$noclient" ConfigMap elitea-main-config '.data.ELITEA_VECTOR_CLEANUP_ABANDON // ""')" ] \
  && note "an empty ELITEA_VECTOR_CLEANUP_ABANDON is omitted" \
  || fail "an empty ELITEA_VECTOR_CLEANUP_ABANDON rendered into the ConfigMap"
expect_refusal "both the chart's client and fileConfig.vectorClientMaterial" "One Deployment volume cannot have two sources" \
  --set main.fileConfig.vectorClientMaterial.enabled=true --set main.fileConfig.vectorClientMaterial.secretName=x
expect_refusal "explicit vector material path outside the chart's mount while mainClient is on" "nothing else is mounted for it" \
  --set main.env.ELITEA_VECTOR_GRPC_TARGET=elitea-vector:9470 \
  --set main.env.ELITEA_VECTOR_GRPC_CLIENT_CERT_FILE=/etc/other/tls.crt \
  --set main.env.ELITEA_VECTOR_GRPC_CLIENT_KEY_FILE=/run/elitea-vector-client/tls.key \
  --set main.env.ELITEA_VECTOR_GRPC_CA_FILE=/run/elitea-vector-client/ca.crt
# A sibling directory that merely starts with the mount path's text is outside it.
expect_refusal "a sibling directory of the chart's mount" "nothing else is mounted for it" \
  --set main.env.ELITEA_VECTOR_GRPC_TARGET=elitea-vector:9470 \
  --set main.env.ELITEA_VECTOR_GRPC_CLIENT_CERT_FILE=/run/elitea-vector-client-other/tls.crt \
  --set main.env.ELITEA_VECTOR_GRPC_CLIENT_KEY_FILE=/run/elitea-vector-client/tls.key \
  --set main.env.ELITEA_VECTOR_GRPC_CA_FILE=/run/elitea-vector-client/ca.crt
expect_refusal "a sibling directory of fileConfig.vectorClientMaterial's mount" "does not serve" \
  $EXTERNAL_CLIENT --set main.env.ELITEA_VECTOR_GRPC_CA_FILE=/run/elitea-vector-client-other/ca.crt
# Explicit paths INSIDE the chart's mount are accepted beside mainClient.
inside="$(render $COMPLETE \
  --set main.env.ELITEA_VECTOR_GRPC_TARGET=elitea-vector:9470 \
  --set main.env.ELITEA_VECTOR_GRPC_CLIENT_CERT_FILE=/run/elitea-vector-client/tls.crt \
  --set main.env.ELITEA_VECTOR_GRPC_CLIENT_KEY_FILE=/run/elitea-vector-client/tls.key \
  --set main.env.ELITEA_VECTOR_GRPC_CA_FILE=/run/elitea-vector-client/ca.crt)"
[ "$(select_one "$inside" ConfigMap elitea-main-config '.data.ELITEA_VECTOR_GRPC_CLIENT_CERT_FILE')" = "/run/elitea-vector-client/tls.crt" ] \
  && note "explicit material paths inside the chart's mount render beside mainClient" \
  || fail "explicit in-mount material paths were not rendered"
expect_refusal "rust indexing without elitea-main's vector client" "elitea-main's elitea-vector client" \
  --set worker.enabled=true --set vector.mainClient.enabled=false

if [ "$failures" -ne 0 ]; then
  echo "render-vector: $failures failure(s)" >&2
  exit 1
fi
echo "render-vector: OK"
