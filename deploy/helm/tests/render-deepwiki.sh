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

# A complete, correct DeepWiki install. Every refusal case below is this minus
# exactly one thing, so a case can never pass because of a second omission.
COMPLETE="\
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
refuses "facade with no callback origin"        --set main.env.ELITEA_DEEPWIKI_CALLBACK_BASE_URL=
refuses "facade with a plain-http base URL"     --set main.env.ELITEA_DEEPWIKI_BASE_URL=http://elitea-deepwiki-svc:8443
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

# ── The callback hop through platform-edge (ADR-0027) ────────────────────────
#
# On: elitea-main's callback origin becomes the edge, and BOTH containers trust
# the runtime CA — that one key of the worker's material Secret and nothing
# else. Refused: the flag without the edge, and the flag beside an explicit
# origin that is not the edge. Off (every render above): neither appears.

echo "== deepwiki.callbackViaPlatformEdge routes the callback hop through the edge =="
EDGE="-f $CHART/values-standalone.yaml --set worker.enabled=true --set deepwiki.callbackViaPlatformEdge=true --set main.env.ELITEA_DEEPWIKI_CALLBACK_BASE_URL="
edge="$(render $COMPLETE $EDGE)" || fail "the callback hop through the edge does not render: $edge"
expect "callback origin in elitea-main" \
  "$(printf '%s' "$edge" | yq eval-all 'select(.kind == "ConfigMap" and .metadata.name == "elitea-main-config") | .data.ELITEA_DEEPWIKI_CALLBACK_BASE_URL' -)" \
  "https://elitea-platform-edge"
ed() { printf '%s' "$edge" | yq eval-all "select(.kind == \"Deployment\" and .metadata.name == \"elitea-deepwiki\") | $1" -; }
expect "host callback CA" "$(ed '.spec.template.spec.containers[0].env[] | select(.name == "ELITEA_DEEPWIKI_CALLBACK_CA_FILE") | .value')" "/run/elitea-runtime-ca/runtime-ca.crt"
expect "engine callback CA" "$(ed '.spec.template.spec.containers[1].env[] | select(.name == "ELITEA_DEEPWIKI_CALLBACK_CA_FILE") | .value')" "/run/elitea-runtime-ca/runtime-ca.crt"
expect "runtime CA mounted in both containers" "$(ed '.spec.template.spec.containers[].volumeMounts[] | select(.name == "runtime-ca") | .mountPath' | tr '\n' ' ')" "/run/elitea-runtime-ca /run/elitea-runtime-ca "
expect "only runtime-ca.crt leaves the worker Secret" "$(ed '.spec.template.spec.volumes[] | select(.name == "runtime-ca") | .secret.items[].key' | tr '\n' ' ')" "runtime-ca.crt "
expect "the flag off mounts nothing" "$(printf '%s' "$manifest" | grep -c 'runtime-ca\|CALLBACK_CA_FILE')" "0"
if render $COMPLETE --set deepwiki.callbackViaPlatformEdge=true >/dev/null 2>&1; then
  fail "callbackViaPlatformEdge renders with no platform-edge to dial"
else
  note "refused without worker.platformEdge"
fi
if render $COMPLETE -f $CHART/values-standalone.yaml --set worker.enabled=true --set deepwiki.callbackViaPlatformEdge=true >/dev/null 2>&1; then
  fail "callbackViaPlatformEdge renders beside an explicit non-edge callback origin"
else
  note "refused beside http://elitea-main:8080"
fi

# ── The platform gRPC service (elitea.subapp.v1.PlatformOperations) ─────────
#
# Off (every render above): no port, no env, nothing exposed. On: a second
# listener, its allowlist and address in the host's environment, a Service
# port, and a NetworkPolicy rule that admits elitea-main only. Refused: the
# allowlist without mutual TLS, and a port that collides with the SPI's.

echo "== the platform gRPC service is off unless clients are named =="
expect "off: no gRPC container port" "$(printf '%s' "$manifest" | yq eval-all 'select(.kind == "Deployment" and .metadata.name == "elitea-deepwiki") | .spec.template.spec.containers[0].ports[].name' - | tr '\n' ' ')" "http "
expect "off: no gRPC env" "$(printf '%s' "$manifest" | yq eval-all 'select(.kind == "Deployment" and .metadata.name == "elitea-deepwiki") | .spec.template.spec.containers[0].env[].name' - | grep -c 'PLATFORM_CLIENTS\|PLATFORM_GRPC_ADDR' || true)" "0"
expect "off: the Service exposes the SPI only" "$(printf '%s' "$manifest" | yq eval-all 'select(.kind == "Service" and .metadata.name == "elitea-deepwiki-svc") | .spec.ports[].name' - | tr '\n' ' ')" "https "
expect "off: the NetworkPolicy admits one port" "$(printf '%s' "$manifest" | yq eval-all 'select(.kind == "NetworkPolicy" and .metadata.name == "elitea-deepwiki-netpol") | .spec.ingress | length' -)" "1"

echo "== the platform gRPC service is configured, exposed and admitted together =="
grpc="$(render $COMPLETE --set 'deepwiki.platformGrpc.clients={elitea-main,elitea-main.ns.svc}' --set deepwiki.platformGrpc.port=9555)" \
  || fail "the platform gRPC service does not render: $grpc"
gp() { printf '%s' "$grpc" | yq eval-all "select(.kind == \"Deployment\" and .metadata.name == \"elitea-deepwiki\") | $1" -; }
expect "gRPC container port" "$(gp '.spec.template.spec.containers[0].ports[] | select(.name == "platform-grpc") | .containerPort')" "9555"
expect "gRPC allowlist" "$(gp '.spec.template.spec.containers[0].env[] | select(.name == "ELITEA_DEEPWIKI_PLATFORM_CLIENTS") | .value')" "elitea-main,elitea-main.ns.svc"
expect "gRPC address" "$(gp '.spec.template.spec.containers[0].env[] | select(.name == "ELITEA_DEEPWIKI_PLATFORM_GRPC_ADDR") | .value')" ":9555"
expect "the Service port" "$(printf '%s' "$grpc" | yq eval-all 'select(.kind == "Service" and .metadata.name == "elitea-deepwiki-svc") | .spec.ports[] | select(.name == "platform-grpc") | .port' -)" "9555"
expect "the Service targets the named port" "$(printf '%s' "$grpc" | yq eval-all 'select(.kind == "Service" and .metadata.name == "elitea-deepwiki-svc") | .spec.ports[] | select(.name == "platform-grpc") | .targetPort' -)" "platform-grpc"
expect "NetworkPolicy ports" "$(printf '%s' "$grpc" | yq eval-all 'select(.kind == "NetworkPolicy" and .metadata.name == "elitea-deepwiki-netpol") | .spec.ingress[].ports[].port' - | tr '\n' ' ')" "8080 9555 "
expect "NetworkPolicy peers on the gRPC port: elitea-main only" "$(printf '%s' "$grpc" | yq eval-all 'select(.kind == "NetworkPolicy" and .metadata.name == "elitea-deepwiki-netpol") | .spec.ingress[] | select(.ports[0].port == 9555) | .from[].podSelector.matchLabels."app.kubernetes.io/name"' - | tr '\n' ' ')" "elitea-main "
refuses "platform clients without mutual TLS" --set 'deepwiki.platformGrpc.clients={elitea-main}' --set deepwiki.mtls.enabled=false
refuses "a platform port that is the SPI's" --set 'deepwiki.platformGrpc.clients={elitea-main}' --set deepwiki.platformGrpc.port=8080
refuses "the platform env set by hand beside the values" --set 'deepwiki.platformGrpc.clients={elitea-main}' --set deepwiki.env.ELITEA_DEEPWIKI_PLATFORM_CLIENTS=elitea-main

if [ "$failures" -ne 0 ]; then
  echo "render-deepwiki: $failures assertion(s) failed" >&2
  exit 1
fi
echo "render-deepwiki: every assertion passed"
