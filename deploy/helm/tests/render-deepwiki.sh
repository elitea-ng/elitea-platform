#!/usr/bin/env bash
# render-deepwiki.sh — the DeepWiki provider component (ADR-0022 P3).
#
# Asserts BOTH directions, which is the shape render-llm-path.sh settled on
# and for the same reason: a chart that renders the feature is only half the
# claim, and the half that rots silently is the refusal. Delete either guard
# and every other gate in helm-lint.yml stays green.
#
# Every assertion reads the RENDERED manifest. A values file that sets a key
# proves nothing on its own — the key still has to survive into an env list,
# and the container still has to consume it.
#
# Usage: deploy/helm/tests/render-deepwiki.sh
# Needs: helm, yq. No cluster, no network.
set -euo pipefail

CHART="deploy/helm/elitea"

# The chart's LLM gateway refuses to render until an operator states its two
# postures, so every render below supplies them. They are render-only values
# (.invalid is reserved by RFC 2606).
GATEWAY_POSTURES="--set llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1"

# The runtime plane (the standalone profile): the host's artifact calls and
# the engine sidecar's model calls go over gRPC on elitea-main's platform
# listener (#1359 U2/U5), so DeepWiki renders only beside it.
RUNTIME="-f $CHART/values-standalone.yaml"

# A complete, correct DeepWiki install. Every refusal case below is this minus
# exactly one thing, so a case can never pass because of a second omission.
COMPLETE="\
$RUNTIME \
--set deepwiki.enabled=true \
--set deepwiki.env.ELITEA_DEEPWIKI_GIT_ALLOWLIST=github.com \
--set main.fileConfig.deepwikiClientMaterial.enabled=true \
--set main.fileConfig.deepwikiClientMaterial.secretName=elitea-main-deepwiki-client-tls \
--set main.env.ELITEA_DEEPWIKI_ENABLED=true \
--set main.env.ELITEA_DEEPWIKI_GIT_ALLOWLIST=github.com \
--set main.env.ELITEA_DEEPWIKI_CLIENT_CERT_FILE=/run/elitea-deepwiki/tls.crt \
--set main.env.ELITEA_DEEPWIKI_CLIENT_KEY_FILE=/run/elitea-deepwiki/tls.key \
--set main.env.ELITEA_DEEPWIKI_CA_FILE=/run/elitea-deepwiki/ca.crt"

failures=0
note() { printf '  %s\n' "$1"; }
fail() { printf 'FAIL: %s\n' "$1" >&2; failures=$((failures + 1)); }

render() { helm template test "$CHART" $GATEWAY_POSTURES "$@" 2>&1; }

# ── 1. The component renders, and renders the things it needs ────────────────

echo "== the enabled component renders its objects =="
manifest="$(render $COMPLETE)" || { fail "a complete install does not render: $manifest"; }

for kind_name in \
  "Deployment/elitea-deepwiki" \
  "Service/elitea-deepwiki-svc" \
  "ServiceAccount/elitea-deepwiki" \
  "Job/elitea-deepwiki-migrate" \
  "Certificate/elitea-deepwiki-server" \
  "Certificate/elitea-deepwiki-facade-client"
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

echo "== the migration Job runs the migration command, not the server =="
args_json="$(printf '%s' "$manifest" \
  | yq eval-all 'select(.kind == "Job" and .metadata.name == "elitea-deepwiki-migrate")
      | .spec.template.spec.containers[0].args | join(" ")' -)"
if [ "$args_json" != "migrate" ]; then
  fail "the migrate Job runs args '$args_json'; the image's default CMD starts the SIDECAR, so a Job that does not override it never migrates anything"
else
  note "args: $args_json"
fi

echo "== the migration Job runs BEFORE the Deployment =="
hook="$(printf '%s' "$manifest" \
  | yq eval-all 'select(.kind == "Job" and .metadata.name == "elitea-deepwiki-migrate")
      | .metadata.annotations["helm.sh/hook"]' -)"
case "$hook" in
  *pre-install*|*pre-upgrade*) note "hook: $hook" ;;
  *) fail "the migrate Job's hook is '$hook'; a pod that starts against an unmigrated database answers every read with a missing-relation error, which reads as a broken service" ;;
esac

echo "== mTLS material reaches the provider container =="
env_names="$(printf '%s' "$manifest" \
  | yq eval-all 'select(.kind == "Deployment" and .metadata.name == "elitea-deepwiki")
      | .spec.template.spec.containers[0].env[].name' -)"
for required in ELITEA_DEEPWIKI_TLS_CERTFILE ELITEA_DEEPWIKI_TLS_KEYFILE ELITEA_DEEPWIKI_TLS_CA_FILE; do
  if ! printf '%s' "$env_names" | grep -qx "$required"; then
    fail "$required is not in the provider's environment; without it the service serves plain HTTP and its own refusal of non-mTLS traffic has nothing to enforce"
  else
    note "$required"
  fi
done

echo "== the facade's client material is mounted where its paths point =="
mount_path="$(printf '%s' "$manifest" \
  | yq eval-all 'select(.kind == "Deployment" and .metadata.name == "elitea-main")
      | .spec.template.spec.containers[]
      | select(.volumeMounts[]?.name == "deepwiki-client-material")
      | .volumeMounts[] | select(.name == "deepwiki-client-material") | .mountPath' -)"
if [ "$mount_path" != "/run/elitea-deepwiki" ]; then
  fail "the material is mounted at '$mount_path' but the three env paths point at /run/elitea-deepwiki; a path outside the mount is a file that does not exist in the container"
else
  note "mountPath: $mount_path"
fi

echo "== the two halves name the SAME git allowlist =="
provider_allowlist="$(printf '%s' "$manifest" \
  | yq eval-all 'select(.kind == "Deployment" and .metadata.name == "elitea-deepwiki")
      | .spec.template.spec.containers[0].env[] | select(.name == "ELITEA_DEEPWIKI_GIT_ALLOWLIST") | .value' -)"
if [ "$provider_allowlist" != "github.com" ]; then
  fail "the provider's allowlist rendered as '$provider_allowlist'"
else
  note "provider: $provider_allowlist"
fi

# ── 2. The refusals still fire ───────────────────────────────────────────────
#
# Each case is COMPLETE minus one thing. `--set x=` clears a value that the
# base already set, which is how a refusal is provoked without rebuilding the
# whole argument list and accidentally omitting something else too.

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

refuses "provider with no git allowlist"        --set deepwiki.env.ELITEA_DEEPWIKI_GIT_ALLOWLIST=
refuses "facade with no git allowlist"          --set main.env.ELITEA_DEEPWIKI_GIT_ALLOWLIST=
refuses "the retired base URL (#1359)"         --set main.env.ELITEA_DEEPWIKI_BASE_URL=https://elitea-deepwiki-svc:8443
refuses "facade with no provider to dial"       --set deepwiki.enabled=false
refuses "facade with no client certificate"     --set main.env.ELITEA_DEEPWIKI_CLIENT_CERT_FILE=
refuses "facade with a path outside the mount"  --set main.env.ELITEA_DEEPWIKI_CA_FILE=/elsewhere/ca.crt
refuses "facade with no material mounted"       --set main.fileConfig.deepwikiClientMaterial.enabled=false
refuses "an unrecognised ENABLED spelling"      --set main.env.ELITEA_DEEPWIKI_ENABLED=ture

# The retired Python engine's settings. Each must be refused by ITS guard,
# which names the upgrade note: a value refused for some other reason (an
# unknown runner, say) would pass `refuses` and prove nothing about the
# message an upgrading operator reads.
refuses_retired() {
  local description="$1"; shift
  local output
  if output="$(render $COMPLETE "$@" 2>&1)"; then
    fail "accepted: $description"
  else
    case "$output" in
      *docs/UPGRADING.md*) note "refused: $description" ;;
      *) fail "refused without the upgrade note ($description): $output" ;;
    esac
  fi
}
refuses_retired "the retired legacy engine runner"  --set deepwiki.engine.runner=legacy
refuses_retired "the retired legacy host runner"    --set deepwiki.env.ELITEA_DEEPWIKI_RUNNER=legacy
refuses_retired "the retired Python image key"      --set deepwiki.engine.image.tag=1.2.3-engine
refuses_retired "the retired Python resources key"  --set deepwiki.engine.resources.limits.memory=4Gi

# The reverse direction: material configured with the facade off is a mounted
# Secret nothing reads, which looks configured and does nothing.
if output="$(render \
      --set main.fileConfig.deepwikiClientMaterial.enabled=true \
      --set main.fileConfig.deepwikiClientMaterial.secretName=s 2>&1)"; then
  fail "accepted: material mounted with the facade off"
else
  note "refused: material mounted with the facade off"
fi

# ── 3. Off by default ────────────────────────────────────────────────────────

echo "== the default install ships the component off =="
default_manifest="$(render)" || { fail "the default install does not render"; }
if printf '%s' "$default_manifest" | grep -q "name: elitea-deepwiki$"; then
  fail "the default install renders DeepWiki objects; it is off by default because the published image refuses every tool"
else
  note "no DeepWiki objects in the default render"
fi

# ── 4. The default is the native engine sidecar ─────────────────────────────
#
# Both runners run the SAME native image; only the sidecar's own
# ELITEA_DEEPWIKI_RUNNER differs. The host always dials the socket (native).

echo "== the provider pod is the Go host plus the engine sidecar over one socket (ADR-0023 H2) =="
containers="$(printf '%s' "$manifest" \
  | yq eval-all 'select(.kind == "Deployment" and .metadata.name == "elitea-deepwiki")
      | .spec.template.spec.containers[].name' - | tr '\n' ' ')"
if [ "$containers" != "elitea-deepwiki engine " ]; then
  fail "the provider pod's containers are '$containers'; expected the host and the engine sidecar"
else
  note "containers: $containers"
fi
host_image="$(printf '%s' "$manifest" \
  | yq eval-all 'select(.kind == "Deployment" and .metadata.name == "elitea-deepwiki")
      | .spec.template.spec.containers[0].image' -)"
case "$host_image" in
  ghcr.io/elitea-ng/elitea-subapp-host:*) note "host image: $host_image" ;;
  *) fail "the host container runs '$host_image', not the sub-application host" ;;
esac
engine_image="$(printf '%s' "$manifest" \
  | yq eval-all 'select(.kind == "Deployment" and .metadata.name == "elitea-deepwiki")
      | .spec.template.spec.containers[1].image' -)"
case "$engine_image" in
  ghcr.io/elitea-ng/elitea-deepwiki-engine-native:*) note "engine image: $engine_image" ;;
  *) fail "the engine sidecar runs '$engine_image', not the native engine" ;;
esac
for container in 0 1; do
  socket_mount="$(printf '%s' "$manifest" \
    | yq eval-all "select(.kind == \"Deployment\" and .metadata.name == \"elitea-deepwiki\")
        | .spec.template.spec.containers[$container].volumeMounts[] | select(.name == \"engine-socket\") | .mountPath" -)"
  if [ "$socket_mount" != "/run/deepwiki" ]; then
    fail "container $container does not mount the engine socket at /run/deepwiki (got '$socket_mount')"
  else
    note "container $container mounts the socket at $socket_mount"
  fi
done
host_socket="$(printf '%s' "$manifest" \
  | yq eval-all 'select(.kind == "Deployment" and .metadata.name == "elitea-deepwiki")
      | .spec.template.spec.containers[0].env[] | select(.name == "ELITEA_DEEPWIKI_ENGINE_SOCKET") | .value' -)"
if [ "$host_socket" != "/run/deepwiki/engine.sock" ]; then
  fail "the host's ELITEA_DEEPWIKI_ENGINE_SOCKET is '$host_socket', which is not inside the shared mount"
else
  note "host socket: $host_socket"
fi
migrate_image="$(printf '%s' "$manifest" \
  | yq eval-all 'select(.kind == "Job" and .metadata.name == "elitea-deepwiki-migrate")
      | .spec.template.spec.containers[0].image' -)"
case "$migrate_image" in
  ghcr.io/elitea-ng/elitea-deepwiki-engine-native:*) note "migrate image: $migrate_image" ;;
  *) fail "the migrate Job runs '$migrate_image'; the migrations are embedded in the native engine, and the host image has none" ;;
esac
echo "== an unavailable runner renders no sidecar =="
solo="$(render $COMPLETE --set deepwiki.env.ELITEA_DEEPWIKI_RUNNER=unavailable)" || fail "runner=unavailable does not render"
solo_containers="$(printf '%s' "$solo" \
  | yq eval-all 'select(.kind == "Deployment" and .metadata.name == "elitea-deepwiki")
      | .spec.template.spec.containers[].name' - | tr '\n' ' ')"
if [ "$solo_containers" != "elitea-deepwiki " ]; then
  fail "runner=unavailable still renders '$solo_containers'"
else
  note "containers: $solo_containers"
fi

# ── 5. The native runner (ADR-0026), the DEFAULT ─────────────────────────────
#
# Nothing set: the Rust image as the sidecar, the host's runner native, the
# binary as the probe, the pod name as the build owner, the database secret,
# and the migrate Job on the same image.

echo "== the default runner renders the native sidecar =="
native="$(render $COMPLETE --set image.tag=9.9.9)" \
  || fail "the default runner does not render: $native"
dw() { printf '%s' "$native" | yq eval-all "select(.kind == \"Deployment\" and .metadata.name == \"elitea-deepwiki\") | $1" -; }
job() { printf '%s' "$native" | yq eval-all "select(.kind == \"Job\" and .metadata.name == \"elitea-deepwiki-migrate\") | $1" -; }
expect() {
  local what="$1" got="$2" want="$3"
  if [ "$got" != "$want" ]; then fail "$what is '$got', expected '$want'"; else note "$what: $got"; fi
}
expect "native containers" "$(dw '.spec.template.spec.containers[].name' | tr '\n' ' ')" "elitea-deepwiki engine "
expect "native engine image" "$(dw '.spec.template.spec.containers[1].image')" "ghcr.io/elitea-ng/elitea-deepwiki-engine-native:9.9.9"
expect "native engine command (the image's own ENTRYPOINT/CMD)" "$(dw '.spec.template.spec.containers[1].command')" "null"
expect "host runner" "$(dw '.spec.template.spec.containers[0].env[] | select(.name == "ELITEA_DEEPWIKI_RUNNER") | .value')" "native"
expect "engine runner" "$(dw '.spec.template.spec.containers[1].env[] | select(.name == "ELITEA_DEEPWIKI_RUNNER") | .value')" "native"
for probe in livenessProbe readinessProbe; do
  expect "native $probe" "$(dw ".spec.template.spec.containers[1].$probe.exec.command | join(\" \")")" "/usr/local/bin/elitea-deepwiki-engine healthcheck"
done
expect "build owner" "$(dw '.spec.template.spec.containers[1].env[] | select(.name == "ELITEA_DEEPWIKI_BUILD_OWNER") | .valueFrom.fieldRef.fieldPath')" "metadata.name"
expect "engine database URL secret" "$(dw '.spec.template.spec.containers[1].env[] | select(.name == "ELITEA_DEEPWIKI_DATABASE_URL") | .valueFrom.secretKeyRef.name')" "elitea-main-db"
expect "engine git allowlist" "$(dw '.spec.template.spec.containers[1].env[] | select(.name == "ELITEA_DEEPWIKI_GIT_ALLOWLIST") | .value')" "github.com"
expect "worker memory cap = 85% of limits.memory 16Gi" "$(dw '.spec.template.spec.containers[1].env[] | select(.name == "ELITEA_DEEPWIKI_WORKER_MEMORY_BYTES") | .value')" "14602888735"
expect "container memory limit" "$(dw '.spec.template.spec.containers[1].resources.limits.memory')" "16Gi"
expect "worker CPU seconds" "$(dw '.spec.template.spec.containers[1].env[] | select(.name == "ELITEA_DEEPWIKI_WORKER_CPU_SECONDS") | .value')" "14400"
expect "engine scratch mount" "$(dw '.spec.template.spec.containers[1].volumeMounts[] | select(.name == "scratch") | .mountPath')" "/var/scratch/deepwiki"
expect "engine socket mount" "$(dw '.spec.template.spec.containers[1].volumeMounts[] | select(.name == "engine-socket") | .mountPath')" "/run/deepwiki"
expect "native migrate image" "$(job '.spec.template.spec.containers[0].image')" "ghcr.io/elitea-ng/elitea-deepwiki-engine-native:9.9.9"
expect "native migrate args" "$(job '.spec.template.spec.containers[0].args | join(" ")')" "migrate"
expect "native migrate command" "$(job '.spec.template.spec.containers[0].command')" "null"
expect "a smaller limit moves the cap with it" \
  "$(render $COMPLETE --set deepwiki.engine.runner=native --set deepwiki.engine.native.resources.limits.memory=6144Mi \
      | yq eval-all 'select(.kind == "Deployment" and .metadata.name == "elitea-deepwiki") | .spec.template.spec.containers[1].env[] | select(.name == "ELITEA_DEEPWIKI_WORKER_MEMORY_BYTES") | .value' -)" \
  "5476083265"

echo "== the native runner's refusals =="
refuses "native with no database URL"            --set deepwiki.engine.runner=native --set postgresql.existingSecret=
refuses "native with an env-only database URL"   --set deepwiki.engine.runner=native --set postgresql.existingSecret= --set deepwiki.env.ELITEA_DEEPWIKI_DATABASE_URL=postgresql://x@db/deepwiki
refuses "an unknown engine runner"               --set deepwiki.engine.runner=rust
refuses "native memory limit below 1Gi"          --set deepwiki.engine.runner=native --set deepwiki.engine.native.resources.limits.memory=512Mi
refuses "native memory limit whose 85% cap is below 1GiB" --set deepwiki.engine.runner=native --set deepwiki.engine.native.resources.limits.memory=1Gi
refuses "native memory limit not in Gi or Mi"    --set deepwiki.engine.runner=native --set deepwiki.engine.native.resources.limits.memory=16G

# The fixture runner: the SAME native image, its canned results. It reads no
# database, so the native runner's database guard does not apply to it.
echo "== the fixture runner runs the native image =="
fixture="$(render $COMPLETE --set deepwiki.engine.runner=fixture --set image.tag=9.9.9)" || fail "runner=fixture does not render: $fixture"
fx() { printf '%s' "$fixture" | yq eval-all "select(.kind == \"Deployment\" and .metadata.name == \"elitea-deepwiki\") | $1" -; }
expect "fixture containers" "$(fx '.spec.template.spec.containers[].name' | tr '\n' ' ')" "elitea-deepwiki engine "
expect "fixture engine image" "$(fx '.spec.template.spec.containers[1].image')" "ghcr.io/elitea-ng/elitea-deepwiki-engine-native:9.9.9"
expect "fixture engine runner" "$(fx '.spec.template.spec.containers[1].env[] | select(.name == "ELITEA_DEEPWIKI_RUNNER") | .value')" "fixture"
expect "fixture host runner" "$(fx '.spec.template.spec.containers[0].env[] | select(.name == "ELITEA_DEEPWIKI_RUNNER") | .value')" "native"
expect "fixture migrate image" "$(printf '%s' "$fixture" | yq eval-all 'select(.kind == "Job" and .metadata.name == "elitea-deepwiki-migrate") | .spec.template.spec.containers[0].image' -)" "ghcr.io/elitea-ng/elitea-deepwiki-engine-native:9.9.9"
if render $COMPLETE --set deepwiki.engine.runner=fixture --set postgresql.existingSecret= >/dev/null 2>&1; then
  note "runner=fixture renders with no database URL secret"
else
  fail "runner=fixture is refused for a missing database URL, which the fixture never reads"
fi

# ── The platform listener: the only way back to elitea-main (#1359 U2/U5) ──
#
# Always on: BOTH containers trust the runtime CA (that one key of the worker's
# material Secret and nothing else) and present the callback client
# certificate; the HOST dials ArtifactObjectService at ARTIFACT_SERVICE_URL,
# the ENGINE ModelService (and its artifact reads) at MODEL_SERVICE_URL, both
# the platform origin; elitea-main admits dns:elitea-subapp-callback to both
# services and the DeepWiki pods on 9446 and NOT on 8080. Refused: DeepWiki
# without the runtime plane or the callback Secret (sidecar or not), the
# retired HTTP callback origin, and both retired switches whatever their value.
echo "== the host and the engine reach elitea-main over gRPC on the platform listener =="
expect "the platform listener port" \
  "$(printf '%s' "$manifest" | yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "elitea-main-config") | .data.ELITEA_RUNTIME_PLATFORM_ADDRESS' -)" \
  ":9446"
ed() { printf '%s' "$manifest" | yq eval-all "select(.kind == \"Deployment\" and .metadata.name == \"elitea-deepwiki\") | $1" -; }
en() { ed ".spec.template.spec.containers[$1].env[] | select(.name == \"$2\") | .value"; }
for c in 0 1; do
  label="$([ "$c" = 0 ] && echo host || echo engine)"
  expect "$label callback CA" "$(en $c ELITEA_DEEPWIKI_CALLBACK_CA_FILE)" "/run/elitea-runtime-ca/runtime-ca.crt"
  expect "$label callback client certificate" "$(en $c ELITEA_DEEPWIKI_CALLBACK_CERT_FILE)" "/run/elitea-callback-client/tls.crt"
  expect "$label callback client key" "$(en $c ELITEA_DEEPWIKI_CALLBACK_KEY_FILE)" "/run/elitea-callback-client/tls.key"
done
expect "host ARTIFACT_SERVICE_URL" "$(en 0 ELITEA_DEEPWIKI_ARTIFACT_SERVICE_URL)" "https://elitea-main:9446"
expect "engine MODEL_SERVICE_URL" "$(en 1 ELITEA_DEEPWIKI_MODEL_SERVICE_URL)" "https://elitea-main:9446"
expect "runtime CA mounted in both containers" "$(ed '.spec.template.spec.containers[].volumeMounts[] | select(.name == "runtime-ca") | .mountPath' | tr '\n' ' ')" "/run/elitea-runtime-ca /run/elitea-runtime-ca "
expect "callback client mounted in both containers" "$(ed '.spec.template.spec.containers[].volumeMounts[] | select(.name == "callback-client") | .mountPath' | tr '\n' ' ')" "/run/elitea-callback-client /run/elitea-callback-client "
expect "only runtime-ca.crt leaves the worker Secret" "$(ed '.spec.template.spec.volumes[] | select(.name == "runtime-ca") | .secret.items[].key' | tr '\n' ' ')" "runtime-ca.crt "
expect "the callback client comes from its own Secret" "$(ed '.spec.template.spec.volumes[] | select(.name == "callback-client") | .secret.secretName')" "elitea-deepwiki-callback-client"
mc() { printf '%s' "$manifest" | yq eval-all "select(.kind == \"ConfigMap\" and .metadata.name == \"elitea-main-config\") | .data.$1" -; }
for var in ELITEA_MODEL_SERVICE_CLIENTS ELITEA_ARTIFACT_SERVICE_CLIENTS; do
  expect "elitea-main's $var admits the callback identity" "$(mc $var | tr ',' '\n' | grep -cx 'dns:elitea-subapp-callback')" "1"
done
expect "no HTTP callback origin is rendered" "$(mc ELITEA_DEEPWIKI_CALLBACK_BASE_URL)" "null"
np() { printf '%s' "$manifest" | yq eval-all "select(.kind == \"NetworkPolicy\" and .metadata.name == \"elitea-main-netpol\") | .spec.ingress[] | select(.ports[0].port == $1) | .from[].podSelector.matchLabels.\"app.kubernetes.io/name\"" - | grep -cx elitea-deepwiki || true; }
expect "elitea-main admits DeepWiki on the platform port" "$(np 9446)" "1"
expect "elitea-main does NOT admit DeepWiki on 8080" "$(np 8080)" "0"
refuses_with() {
  local description="$1" text="$2"; shift 2
  local output
  if output="$(render "$@" 2>&1)"; then
    fail "accepted: $description"
  elif printf '%s' "$output" | grep -q -- "$text"; then
    note "refused: $description"
  else
    fail "refused for another reason ($description): $(printf '%s' "$output" | grep Error | tail -1)"
  fi
}
refuses_with "DeepWiki without the runtime plane" "deepwiki.enabled needs main.runtime.enabled" \
  ${COMPLETE#"$RUNTIME "}
refuses_with "DeepWiki without a callback client Secret" "deepwiki.enabled needs deepwiki.callbackClient.secretName" \
  $COMPLETE --set deepwiki.callbackClient.secretName=
for name in ELITEA_DEEPWIKI_MODEL_SERVICE_URL ELITEA_DEEPWIKI_ARTIFACT_SERVICE_URL ELITEA_DEEPWIKI_CALLBACK_CA_FILE ELITEA_DEEPWIKI_CALLBACK_CERT_FILE ELITEA_DEEPWIKI_CALLBACK_KEY_FILE; do
  refuses_with "deepwiki.env.$name set by hand" "deepwiki.env sets $name" $COMPLETE --set deepwiki.env.$name=x
done
# No sidecar (the host's runner unavailable): no model calls, but the host
# still uploads over ArtifactObjectService, so the runtime plane is still
# required and the host still carries the material.
refuses_with "a host-only DeepWiki without the runtime plane" "deepwiki.enabled needs main.runtime.enabled" \
  --set deepwiki.enabled=true --set deepwiki.env.ELITEA_DEEPWIKI_GIT_ALLOWLIST=github.com --set deepwiki.env.ELITEA_DEEPWIKI_RUNNER=unavailable
hostonly="$(render $RUNTIME --set deepwiki.enabled=true --set deepwiki.env.ELITEA_DEEPWIKI_GIT_ALLOWLIST=github.com --set deepwiki.env.ELITEA_DEEPWIKI_RUNNER=unavailable)" \
  || fail "a host-only DeepWiki (no engine sidecar) beside the runtime plane does not render: $hostonly"
ho() { printf '%s' "$hostonly" | yq eval-all "select(.kind == \"Deployment\" and .metadata.name == \"elitea-deepwiki\") | $1" -; }
expect "host-only: no engine container" "$(ho '.spec.template.spec.containers[].name' | tr '\n' ' ')" "elitea-deepwiki "
expect "host-only: the host still dials ArtifactObjectService" "$(ho '.spec.template.spec.containers[0].env[] | select(.name == "ELITEA_DEEPWIKI_ARTIFACT_SERVICE_URL") | .value')" "https://elitea-main:9446"
retired_with() {
  local description="$1" text="$2"; shift 2
  local output
  if output="$(render $COMPLETE "$@" 2>&1)"; then
    fail "accepted: $description"
  elif printf '%s' "$output" | grep -q -- "$text" && printf '%s' "$output" | grep -q 'docs/UPGRADING.md'; then
    note "refused with the migration note: $description"
  else
    fail "refused for another reason ($description): $(printf '%s' "$output" | grep Error | tail -1)"
  fi
}
retired_with "the retired HTTP callback origin" "ELITEA_DEEPWIKI_CALLBACK_BASE_URL" --set main.env.ELITEA_DEEPWIKI_CALLBACK_BASE_URL=http://elitea-main:8080
retired_with "an EMPTY retired HTTP callback origin" "ELITEA_DEEPWIKI_CALLBACK_BASE_URL" --set main.env.ELITEA_DEEPWIKI_CALLBACK_BASE_URL=
retired_with "callbackViaPlatformListener=true" "callbackViaPlatformListener was removed" --set deepwiki.callbackViaPlatformListener=true
retired_with "callbackViaPlatformListener=false" "callbackViaPlatformListener was removed" --set deepwiki.callbackViaPlatformListener=false
retired_with "the retired callbackViaPlatformEdge key" "callbackViaPlatformEdge was removed" --set deepwiki.callbackViaPlatformEdge=true

# ── The platform gRPC service (elitea.subapp.v1.PlatformOperations) ─────────
#
# Off (every render above): no port, no env, nothing exposed. On: a second
# listener, its allowlist and address in the host's environment, a Service
# port, and a NetworkPolicy rule that admits elitea-main only. Refused: the
# allowlist without mutual TLS, and a port that collides with the SPI's.

echo "== the provider serves one port: gRPC over mutual TLS (#1359) =="
dep() { printf '%s' "$manifest" | yq eval-all "select(.kind == \"Deployment\" and .metadata.name == \"elitea-deepwiki\") | $1" -; }
expect "two container ports: the gRPC listener and the admin port" "$(dep '.spec.template.spec.containers[0].ports[].name' | tr '\n' ' ')" "platform-grpc admin "
expect "the admin address" "$(dep '.spec.template.spec.containers[0].env[] | select(.name == "ELITEA_DEEPWIKI_ADMIN_ADDR") | .value')" ":9090"
expect "the admin container port" "$(dep '.spec.template.spec.containers[0].ports[] | select(.name == "admin") | .containerPort')" "9090"
expect "the default allowlist is elitea-main" "$(dep '.spec.template.spec.containers[0].env[] | select(.name == "ELITEA_DEEPWIKI_PLATFORM_CLIENTS") | .value')" "elitea-main"
expect "the listener address" "$(dep '.spec.template.spec.containers[0].env[] | select(.name == "ELITEA_DEEPWIKI_PLATFORM_GRPC_ADDR") | .value')" ":9443"
# The probes ask the plain-HTTP admin port: the gRPC listener needs a client
# certificate the kubelet does not have, and readiness must carry the owner
# claim (#1359), which a TCP accept cannot.
for probe in livenessProbe readinessProbe; do
  expect "host $probe: httpGet on the admin port" "$(dep ".spec.template.spec.containers[0].$probe.httpGet.port")" "admin"
  expect "host $probe: no tcpSocket" "$(dep ".spec.template.spec.containers[0].$probe.tcpSocket")" "null"
done
expect "host liveness path" "$(dep '.spec.template.spec.containers[0].livenessProbe.httpGet.path')" "/healthz"
expect "host readiness path" "$(dep '.spec.template.spec.containers[0].readinessProbe.httpGet.path')" "/readyz"
expect "the Service exposes the gRPC port only" "$(printf '%s' "$manifest" | yq eval-all 'select(.kind == "Service" and .metadata.name == "elitea-deepwiki-svc") | .spec.ports[] | .name + ":" + (.port | tostring)' - | tr '\n' ' ')" "platform-grpc:9443 "
expect "the NetworkPolicy admits one port" "$(printf '%s' "$manifest" | yq eval-all 'select(.kind == "NetworkPolicy" and .metadata.name == "elitea-deepwiki-netpol") | .spec.ingress[].ports[].port' - | tr '\n' ' ')" "9443 "

echo "== the cancellation fan-out: the host dials NATS as elitea-deepwiki (#1359) =="
expect "NATS URL" "$(dep '.spec.template.spec.containers[0].env[] | select(.name == "ELITEA_DEEPWIKI_NATS_URL") | .value')" "tls://elitea-nats.default.svc.cluster.local:4222"
for f in CA:ca.crt CERT:tls.crt KEY:tls.key; do
  expect "NATS ${f%%:*} file in the mount" "$(dep ".spec.template.spec.containers[0].env[] | select(.name == \"ELITEA_DEEPWIKI_NATS_TLS_${f%%:*}_FILE\") | .value")" "$(dep '.spec.template.spec.containers[0].volumeMounts[] | select(.name == "nats-client-tls") | .mountPath')/${f#*:}"
done
expect "the NATS client Secret is mounted" "$(dep '.spec.template.spec.volumes[] | select(.name == "nats-client-tls") | .secret.secretName')" "elitea-deepwiki-nats-client-tls"
expect "its Certificate names the SUBAPP user" "$(printf '%s' "$manifest" | yq eval-all 'select(.kind == "Certificate" and .metadata.name == "elitea-deepwiki-nats-client") | .spec.uris[0]' -)" "spiffe://elitea.internal/nats/elitea-deepwiki"

echo "== the platform gRPC service is configured, exposed and admitted together =="
grpc="$(render $COMPLETE --set 'deepwiki.platformGrpc.clients={elitea-main,elitea-main.ns.svc}' --set deepwiki.platformGrpc.port=9555)" \
  || fail "the platform gRPC service does not render: $grpc"
gp() { printf '%s' "$grpc" | yq eval-all "select(.kind == \"Deployment\" and .metadata.name == \"elitea-deepwiki\") | $1" -; }
expect "gRPC container port" "$(gp '.spec.template.spec.containers[0].ports[] | select(.name == "platform-grpc") | .containerPort')" "9555"
expect "gRPC allowlist" "$(gp '.spec.template.spec.containers[0].env[] | select(.name == "ELITEA_DEEPWIKI_PLATFORM_CLIENTS") | .value')" "elitea-main,elitea-main.ns.svc"
expect "gRPC address" "$(gp '.spec.template.spec.containers[0].env[] | select(.name == "ELITEA_DEEPWIKI_PLATFORM_GRPC_ADDR") | .value')" ":9555"
expect "the Service port" "$(printf '%s' "$grpc" | yq eval-all 'select(.kind == "Service" and .metadata.name == "elitea-deepwiki-svc") | .spec.ports[] | select(.name == "platform-grpc") | .port' -)" "9555"
expect "the Service targets the named port" "$(printf '%s' "$grpc" | yq eval-all 'select(.kind == "Service" and .metadata.name == "elitea-deepwiki-svc") | .spec.ports[] | select(.name == "platform-grpc") | .targetPort' -)" "platform-grpc"
expect "NetworkPolicy ports" "$(printf '%s' "$grpc" | yq eval-all 'select(.kind == "NetworkPolicy" and .metadata.name == "elitea-deepwiki-netpol") | .spec.ingress[].ports[].port' - | tr '\n' ' ')" "9555 "
expect "NetworkPolicy peers on the gRPC port: elitea-main only" "$(printf '%s' "$grpc" | yq eval-all 'select(.kind == "NetworkPolicy" and .metadata.name == "elitea-deepwiki-netpol") | .spec.ingress[] | select(.ports[0].port == 9555) | .from[].podSelector.matchLabels."app.kubernetes.io/name"' - | tr '\n' ' ')" "elitea-main "
refuses "the provider without mutual TLS" --set deepwiki.mtls.enabled=false
refuses "an empty allowlist: a listener that admits nobody" --set 'deepwiki.platformGrpc.clients={}'
refuses "the retired HTTPS service port" --set deepwiki.service.port=8443
refuses "the platform env set by hand beside the values" --set 'deepwiki.platformGrpc.clients={elitea-main}' --set deepwiki.env.ELITEA_DEEPWIKI_PLATFORM_CLIENTS=elitea-main
refuses "the admin address set by hand beside the values" --set deepwiki.env.ELITEA_DEEPWIKI_ADMIN_ADDR=:9091
refuses "the admin port on the gRPC port" --set deepwiki.admin.port=9443
refuses "an allowlist that does not name elitea-main" --set 'deepwiki.platformGrpc.clients={someone-else}'
# The expected name follows the client material. Chart-issued (the default):
# elitea-main, and a stated main.platformClientName that differs is refused.
# Operator-supplied (main mounts a Secret the chart did not issue): the
# allowlist is checked against main.platformClientName instead.
refuses "a custom main.platformClientName while the chart issues the certificate" --set 'deepwiki.platformGrpc.clients={platform-main}' --set main.platformClientName=platform-main
refuses "operator material whose allowlist does not name main.platformClientName" --set 'deepwiki.platformGrpc.clients={elitea-main}' --set main.platformClientName=platform-main --set main.fileConfig.deepwikiClientMaterial.secretName=my-own-material
operator="$(render $COMPLETE --set 'deepwiki.platformGrpc.clients={platform-main}' --set main.platformClientName=platform-main --set main.fileConfig.deepwikiClientMaterial.secretName=my-own-material)" \
  || fail "operator-supplied client material with its own name does not render: $operator"
expect "operator material: the allowlist carries the operator's name" "$(printf '%s' "$operator" | yq eval-all 'select(.kind == "Deployment" and .metadata.name == "elitea-deepwiki") | .spec.template.spec.containers[0].env[] | select(.name == "ELITEA_DEEPWIKI_PLATFORM_CLIENTS") | .value' -)" "platform-main"

# ── elitea-main's side of the platform service (#1243) ───────────────────────
#
# The caller is configured WITH the service: when the engine serves it and the
# facade is on, elitea-main's ConfigMap carries the target (<service>:<port>)
# without the operator stating it. Off, nothing is derived; an explicit value
# wins; and a target beside a facade that is off is refused, as cmd/elitea-main
# refuses it at boot.

echo "== elitea-main is told where the platform service is =="
main_env() { printf '%s' "$1" | yq eval-all "select(.kind == \"ConfigMap\" and .metadata.name == \"elitea-main-config\") | .data.$2" -; }
expect "the default: derived from the Service and the gRPC port" "$(main_env "$manifest" ELITEA_DEEPWIKI_PLATFORM_GRPC_TARGET)" "elitea-deepwiki-svc:9443"
expect "on: the target is the Service and the platform port" "$(main_env "$grpc" ELITEA_DEEPWIKI_PLATFORM_GRPC_TARGET)" "elitea-deepwiki-svc:9555"
explicit="$(render $COMPLETE --set 'deepwiki.platformGrpc.clients={elitea-main}' --set main.env.ELITEA_DEEPWIKI_PLATFORM_GRPC_TARGET=deepwiki.elsewhere:9443)" \
  || fail "an explicit target does not render: $explicit"
expect "an explicit target wins" "$(main_env "$explicit" ELITEA_DEEPWIKI_PLATFORM_GRPC_TARGET)" "deepwiki.elsewhere:9443"
facade_off="$(render $RUNTIME --set deepwiki.enabled=true --set deepwiki.env.ELITEA_DEEPWIKI_GIT_ALLOWLIST=github.com --set 'deepwiki.platformGrpc.clients={elitea-main}')" \
  || fail "the engine with its service on and the facade off does not render: $facade_off"
expect "facade off: nothing derived for a caller that is not composed" "$(main_env "$facade_off" ELITEA_DEEPWIKI_PLATFORM_GRPC_TARGET)" ""
if render --set main.env.ELITEA_DEEPWIKI_PLATFORM_GRPC_TARGET=elitea-deepwiki-svc:9443 >/dev/null 2>&1; then
  fail "a platform target beside a facade that is off renders"
else
  note "refused: a target with the facade off"
fi

if [ "$failures" -ne 0 ]; then
  echo "render-deepwiki: $failures assertion(s) failed" >&2
  exit 1
fi
echo "render-deepwiki: every assertion passed"
