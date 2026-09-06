#!/usr/bin/env bash
# render-inventory.sh — the Inventory provider component (ADR-0023 H4c).
#
# The shape render-deepwiki.sh settled on, for the same reason: a chart that
# renders the feature is only half the claim, and the half that rots silently is
# the refusal. Every assertion reads the RENDERED manifest — a values file that
# sets a key proves nothing on its own, because the key still has to survive
# into an env list and the container still has to consume it.
#
# WHAT IS DIFFERENT FROM DEEPWIKI, and asserted here BECAUSE it is different:
#
#   * no migration Job and no database URL. Inventory has no storage package
#     and no migrations; a graph's home is the platform artifact bucket. A Job
#     that appeared here would migrate a schema nobody owns.
#   * no provider-side git allowlist. Inventory reads its source through the
#     SDK's toolkit, not from a request-supplied clone URL, so the only
#     allowlist is the facade's — one that always passed on the provider side
#     would look like an egress control while being none.
#   * the facade renders with NO callback origin. That disables source
#     expansion and mounts the facade anyway, which is a supported deployment:
#     the graph-read tools work and the three that name a source get the
#     provider's own refusal.
#
# Usage: deploy/helm/tests/render-inventory.sh
# Needs: helm, yq. No cluster, no network.
set -euo pipefail

CHART="deploy/helm/elitea"

# The chart's LLM gateway refuses to render until an operator states its two
# postures, so every render below supplies them. Render-only values (.invalid is
# reserved by RFC 2606).
GATEWAY_POSTURES="--set llmGateway.egressPosture=public-unrestricted --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1"

# A complete, correct Inventory install. Every refusal case below is this minus
# exactly one thing, so a case can never pass because of a second omission.
COMPLETE="\
--set inventory.enabled=true \
--set main.fileConfig.inventoryClientMaterial.enabled=true \
--set main.fileConfig.inventoryClientMaterial.secretName=elitea-main-inventory-client-tls \
--set main.env.ELITEA_INVENTORY_ENABLED=true \
--set main.env.ELITEA_INVENTORY_BASE_URL=https://elitea-inventory-svc:8443 \
--set main.env.ELITEA_INVENTORY_CALLBACK_BASE_URL=http://elitea-main:8080 \
--set main.env.ELITEA_INVENTORY_GIT_ALLOWLIST=github.com \
--set main.env.ELITEA_INVENTORY_CLIENT_CERT_FILE=/run/elitea-inventory/tls.crt \
--set main.env.ELITEA_INVENTORY_CLIENT_KEY_FILE=/run/elitea-inventory/tls.key \
--set main.env.ELITEA_INVENTORY_CA_FILE=/run/elitea-inventory/ca.crt"

failures=0
note() { printf '  %s\n' "$1"; }
fail() { printf 'FAIL: %s\n' "$1" >&2; failures=$((failures + 1)); }

render() { helm template test "$CHART" $GATEWAY_POSTURES "$@" 2>&1; }

select_one() {
  # kind, name, expression
  printf '%s' "$3" | yq eval-all "select(.kind == \"$1\" and .metadata.name == \"$2\") | $4" -
}

# ── 1. The component renders, and renders the things it needs ────────────────

echo "== the enabled component renders its objects =="
manifest="$(render $COMPLETE)" || fail "a complete install does not render: $manifest"

for kind_name in \
  "Deployment/elitea-inventory" \
  "Service/elitea-inventory-svc" \
  "ServiceAccount/elitea-inventory" \
  "Certificate/elitea-inventory-server" \
  "Certificate/elitea-inventory-facade-client"
do
  kind="${kind_name%%/*}"
  name="${kind_name##*/}"
  if ! printf '%s' "$manifest" \
      | yq eval-all "select(.kind == \"$kind\" and .metadata.name == \"$name\") | .metadata.name" - \
      | grep -qx "$name"; then
    fail "$kind/$name is not in the render"
  else
    note "$kind/$name"
  fi
done

echo "== and NO migration Job: Inventory owns no schema =="
if printf '%s' "$manifest" | grep -q "name: elitea-inventory-migrate"; then
  fail "a migration Job rendered; Inventory has no storage package and no migrations, so a Job here would run against a schema nobody owns"
else
  note "no Job/elitea-inventory-migrate"
fi

echo "== mTLS material reaches the provider container =="
env_names="$(select_one Deployment elitea-inventory "$manifest" \
  '.spec.template.spec.containers[0].env[].name')"
for required in ELITEA_INVENTORY_TLS_CERTFILE ELITEA_INVENTORY_TLS_KEYFILE ELITEA_INVENTORY_TLS_CA_FILE; do
  if ! printf '%s' "$env_names" | grep -qx "$required"; then
    fail "$required is not in the provider's environment; without it the service serves plain HTTP and its own refusal of non-mTLS traffic has nothing to enforce"
  else
    note "$required"
  fi
done

echo "== the identity secret reaches the provider from a Secret, not plaintext =="
secret_ref="$(select_one Deployment elitea-inventory "$manifest" \
  '.spec.template.spec.containers[0].env[] | select(.name == "ELITEA_INVENTORY_IDENTITY_SECRET") | .valueFrom.secretKeyRef.name')"
if [ "$secret_ref" != "elitea-inventory-secrets" ]; then
  fail "the provider reads its identity secret from '$secret_ref'; it must come from a Secret, and it must hold the same value the facade signs with"
else
  note "identity secret from: $secret_ref"
fi

echo "== the facade's client material is mounted where its paths point =="
mount_path="$(printf '%s' "$manifest" \
  | yq eval-all 'select(.kind == "Deployment" and .metadata.name == "elitea-main")
      | .spec.template.spec.containers[]
      | select(.volumeMounts[]?.name == "inventory-client-material")
      | .volumeMounts[] | select(.name == "inventory-client-material") | .mountPath' -)"
if [ "$mount_path" != "/run/elitea-inventory" ]; then
  fail "the material is mounted at '$mount_path' but the three env paths point at /run/elitea-inventory; a path outside the mount is a file that does not exist in the container"
else
  note "mountPath: $mount_path"
fi

echo "== the mount and its volume are a PAIR =="
volume="$(printf '%s' "$manifest" \
  | yq eval-all 'select(.kind == "Deployment" and .metadata.name == "elitea-main")
      | .spec.template.spec.volumes[] | select(.name == "inventory-client-material") | .secret.secretName' -)"
if [ "$volume" != "elitea-main-inventory-client-tls" ]; then
  fail "the inventory-client-material volume names '$volume'; a mount with no volume is a pod that will not schedule"
else
  note "volume secret: $volume"
fi

echo "== the provider pod is the Go host plus the engine sidecar over one socket =="
containers="$(select_one Deployment elitea-inventory "$manifest" \
  '.spec.template.spec.containers[].name' | tr '\n' ' ')"
if [ "$containers" != "elitea-inventory engine " ]; then
  fail "the provider pod's containers are '$containers'; expected the host and the engine sidecar"
else
  note "containers: $containers"
fi
host_image="$(select_one Deployment elitea-inventory "$manifest" \
  '.spec.template.spec.containers[0].image')"
case "$host_image" in
  ghcr.io/elitea-ng/elitea-subapp-host:*) note "host image: $host_image" ;;
  *) fail "the host container runs '$host_image', not the sub-application host" ;;
esac
engine_image="$(select_one Deployment elitea-inventory "$manifest" \
  '.spec.template.spec.containers[1].image')"
case "$engine_image" in
  ghcr.io/elitea-ng/elitea-inventory:*-engine) note "engine image: $engine_image" ;;
  *) fail "the engine sidecar runs '$engine_image'; without the -engine closure every tool fails at invocation time" ;;
esac
engine_command="$(select_one Deployment elitea-inventory "$manifest" \
  '.spec.template.spec.containers[1].command | join(" ")')"
if [ "$engine_command" != "python -m elitea_inventory" ]; then
  fail "the engine sidecar runs '$engine_command', which is not the sidecar module"
else
  note "engine command: $engine_command"
fi
for container in 0 1; do
  socket_mount="$(select_one Deployment elitea-inventory "$manifest" \
    ".spec.template.spec.containers[$container].volumeMounts[] | select(.name == \"engine-socket\") | .mountPath")"
  if [ "$socket_mount" != "/run/inventory" ]; then
    fail "container $container does not mount the engine socket at /run/inventory (got '$socket_mount')"
  else
    note "container $container mounts the socket at $socket_mount"
  fi
done
host_socket="$(select_one Deployment elitea-inventory "$manifest" \
  '.spec.template.spec.containers[0].env[] | select(.name == "ELITEA_INVENTORY_ENGINE_SOCKET") | .value')"
if [ "$host_socket" != "/run/inventory/engine.sock" ]; then
  fail "the host's ELITEA_INVENTORY_ENGINE_SOCKET is '$host_socket', which is not inside the shared mount"
else
  note "host socket: $host_socket"
fi

echo "== the Service selects the Deployment's pods =="
selector="$(select_one Service elitea-inventory-svc "$manifest" \
  '.spec.selector["app.kubernetes.io/name"]')"
pod_label="$(select_one Deployment elitea-inventory "$manifest" \
  '.spec.template.metadata.labels["app.kubernetes.io/name"]')"
if [ "$selector" != "$pod_label" ] || [ -z "$selector" ]; then
  fail "the Service selects '$selector' and the pods are labelled '$pod_label'; a Service with no endpoints fails every call at connect"
else
  note "selector: $selector"
fi

echo "== the certificate SANs name the Service elitea-main verifies =="
san="$(select_one Certificate elitea-inventory-server "$manifest" '.spec.dnsNames[0]')"
service_name="$(select_one Service elitea-inventory-svc "$manifest" '.metadata.name')"
if [ "$san" != "$service_name" ]; then
  fail "the server certificate's first SAN is '$san' and the Service is '$service_name'; a mismatch fails every handshake with an error that reads like a trust problem"
else
  note "SAN: $san"
fi

echo "== the two hops use SEPARATE certificates =="
inventory_secret="$(select_one Certificate elitea-inventory-facade-client "$manifest" '.spec.secretName')"
if [ "$inventory_secret" = "elitea-main-deepwiki-client-tls" ]; then
  fail "the Inventory facade client certificate reuses DeepWiki's Secret; one provider's certificate must be revocable without touching the other's"
else
  note "facade client secret: $inventory_secret"
fi

# ── 2. The refusals still fire ───────────────────────────────────────────────

echo "== the guards refuse a half-configured install =="
refuses() {
  local description="$1"; shift
  local output
  if output="$(render $COMPLETE "$@" 2>&1)"; then
    fail "accepted: $description"
  else
    case "$output" in
      *Error*) note "refused: $description" ;;
      *) fail "failed for the wrong reason ($description): $output" ;;
    esac
  fi
}

refuses "facade with a plain-http base URL"     --set main.env.ELITEA_INVENTORY_BASE_URL=http://elitea-inventory-svc:8443
refuses "facade with no client certificate"     --set main.env.ELITEA_INVENTORY_CLIENT_CERT_FILE=
refuses "facade with no key"                    --set main.env.ELITEA_INVENTORY_CLIENT_KEY_FILE=
refuses "facade with no CA"                     --set main.env.ELITEA_INVENTORY_CA_FILE=
refuses "facade with a path outside the mount"  --set main.env.ELITEA_INVENTORY_CA_FILE=/elsewhere/ca.crt
refuses "facade with certificate TEXT"          --set main.env.ELITEA_INVENTORY_CA_FILE=-----BEGIN
refuses "facade with no material mounted"       --set main.fileConfig.inventoryClientMaterial.enabled=false
refuses "an unrecognised ENABLED spelling"      --set main.env.ELITEA_INVENTORY_ENABLED=ture
refuses "legacy runner on a non-engine image"   --set inventory.engine.image.tag=1.2.3
refuses "legacy runner with no engine socket"   --set inventory.env.ELITEA_INVENTORY_ENGINE_SOCKET=
refuses "a socket with no sidecar to answer"    --set inventory.env.ELITEA_INVENTORY_RUNNER=fixture

# The reverse direction: material configured with the facade off is a mounted
# Secret nothing reads, which looks configured and does nothing.
if output="$(render \
      --set main.fileConfig.inventoryClientMaterial.enabled=true \
      --set main.fileConfig.inventoryClientMaterial.secretName=s 2>&1)"; then
  fail "accepted: material mounted with the facade off"
else
  note "refused: material mounted with the facade off"
fi
if output="$(render --set main.env.ELITEA_INVENTORY_CA_FILE=/run/elitea-inventory/ca.crt 2>&1)"; then
  fail "accepted: a certificate path with the facade off"
else
  note "refused: a certificate path with the facade off"
fi

# ── 3. Off by default ────────────────────────────────────────────────────────

echo "== the default install ships the component off =="
default_manifest="$(render)" || fail "the default install does not render"
if printf '%s' "$default_manifest" | grep -q "name: elitea-inventory$"; then
  fail "the default install renders Inventory objects; it is off by default because the published image refuses every engine tool"
else
  note "no Inventory objects in the default render"
fi

# ── 4. The permitted combinations ARE permitted ──────────────────────────────
#
# A guard that refuses everything is indistinguishable from a broken template,
# so each refusal above is paired with the render it must allow.

echo "== the legacy runner renders on an engine image =="
if render $COMPLETE --set inventory.engine.image.tag=1.2.3-engine >/dev/null 2>&1; then
  note "runner=legacy with an -engine tag renders"
else
  fail "the guard refuses the CORRECT combination too, so it is not a guard"
fi

echo "== the host's own fixture runner renders ONE container and no sidecar =="
solo="$(render $COMPLETE \
  --set inventory.env.ELITEA_INVENTORY_RUNNER=fixture \
  --set inventory.env.ELITEA_INVENTORY_ENGINE_SOCKET=)" \
  || fail "runner=fixture does not render"
solo_containers="$(select_one Deployment elitea-inventory "$solo" \
  '.spec.template.spec.containers[].name' | tr '\n' ' ')"
if [ "$solo_containers" != "elitea-inventory " ]; then
  fail "runner=fixture still renders '$solo_containers'; the sidecar belongs to runner=legacy alone"
else
  note "containers: $solo_containers"
fi

echo "== a facade with NO callback origin still mounts =="
# Inventory's difference from DeepWiki: an empty callback origin disables
# source expansion rather than failing the boot. The eight graph-read tools
# still work, so refusing this install would refuse one the code supports.
if render $COMPLETE --set main.env.ELITEA_INVENTORY_CALLBACK_BASE_URL= >/dev/null 2>&1; then
  note "no callback origin renders (expansion off, reads still served)"
else
  fail "the chart refuses an install cmd/elitea-main supports: an empty callback origin mounts the facade WITHOUT source expansion and logs which of the two it built"
fi

if [ "$failures" -ne 0 ]; then
  echo "render-inventory: $failures assertion(s) failed" >&2
  exit 1
fi
echo "render-inventory: every assertion passed"
