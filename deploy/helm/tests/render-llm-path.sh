#!/usr/bin/env bash
# render-llm-path.sh — issues #463, #467, #458 and #464.
#
# Assert that the four charts, and the standalone compose stack, do not ship
# values that turn the LLM path off.
#
# Every assertion reads the RENDERED manifest, never a values file. A values
# file that sets a key proves nothing on its own: the key still has to survive
# into a ConfigMap or an env list, and the container still has to consume it.
#
# NO PATH IS WRITTEN OUT HERE. The ConfigMap name, the envFrom link and the
# container index are all read out of the render, so moving a value to a
# different object cannot leave this gate passing on a manifest that no longer
# carries it. A gate that hardcodes a path stops checking, silently, the moment
# the path moves.
#
# Usage: deploy/helm/tests/render-llm-path.sh
# Needs: helm, yq. No cluster, no network.
set -euo pipefail

# The single chart contains the LLM gateway, which REFUSES to render until an
# operator states its two postures. Every render below therefore supplies them:
# they are render-only values (.invalid is reserved by RFC 2606), and a chart
# that rendered without them would be the defect that refusal exists to stop.
# These suites assert against ONE component, and every extractor below reads
# "the Deployment" or "the ConfigMap" from the render. The single chart renders
# every component, so the suite narrows the render to its subject instead of
# teaching twenty selectors to disambiguate.
ONLY_SCHEDULER="--set main.enabled=false --set web.enabled=false --set llmGateway.enabled=false --set otelCollector.enabled=false --set worker.enabled=false --set runtimeRedis.enabled=false"
ONLY_GATEWAY="--set main.enabled=false --set web.enabled=false --set scheduler.enabled=false --set otelCollector.enabled=false --set worker.enabled=false --set runtimeRedis.enabled=false"
# Chooses the narrowing for one case. A case that states a gateway value is a
# gateway case, and is left to supply (or withhold) the postures itself — the
# suite asserts that withholding them is refused, which a blanket injection
# would quietly satisfy.
narrow_for() {
  case "$*" in
    *llmGateway.*) echo "$ONLY_GATEWAY" ;;
    *scheduler.*) echo "$ONLY_SCHEDULER" ;;
    *) echo "$ONLY_MAIN $GATEWAY_RENDER_POSTURE" ;;
  esac
}
ONLY_MAIN="--set web.enabled=false --set scheduler.enabled=false --set llmGateway.enabled=false --set otelCollector.enabled=false --set worker.enabled=false --set runtimeRedis.enabled=false"
GATEWAY_RENDER_POSTURE="--set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://render-only.example.invalid/llm/v1 --set-string llmGateway.egressPosture=public-unrestricted"

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
MAIN="$REPO/deploy/helm/elitea"
GATEWAY="$REPO/deploy/helm/elitea"
SCHEDULER="$REPO/deploy/helm/elitea"
COMPOSE="$REPO/deploy/docker-compose.standalone-full.yml"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
pass() { echo "ok: $*"; }

for tool in helm yq; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "render-llm-path.sh needs $tool on PATH" >&2
    exit 2
  }
done

# A values set that satisfies the guards this script is about, so a test of ONE
# variable is not passed or failed by a different one.
GUARDS_OK=(
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS="https://elitea.example.com/llm/v1"
  --set-string llmGateway.egressPosture=allowlist
  --set-string llmGateway.env.GATEWAY_EGRESS_ALLOWLIST="vllm.ml.svc.cluster.local:8000"
)

# renders "<description>" <chart> [args...] — the chart must render.
renders() {
  local description="$1" chart="$2"
  shift 2
  if helm template $(narrow_for "$@") test-release "$chart" "$@" >"$WORK/render.yaml" 2>"$WORK/err.txt"; then
    pass "the chart renders $description"
  else
    fail "the chart refuses $description, and it must not:
$(cat "$WORK/err.txt")"
  fi
}

# refuses "<description>" "<expected regexp>" <chart> [args...] — the chart must
# refuse WHILE IT RENDERS, and the message must name the field. An operator has
# to read the reason on the terminal that ran the command, before any object
# reaches the cluster.
refuses() {
  local description="$1" expected="$2" chart="$3"
  shift 3
  local output
  if output="$(helm template $(narrow_for "$@") test-release "$chart" "$@" 2>&1)"; then
    fail "the chart rendered $description, and the deployment would then be wrong at run time"
    return
  fi
  if grep -qE "$expected" <<<"$output"; then
    pass "the chart refuses $description while it renders"
  else
    fail "the chart refuses $description but the message does not name '$expected':
$output"
  fi
}

# ---------------------------------------------------------------------------
# Readers. Each one DERIVES its path from the render.
# ---------------------------------------------------------------------------

# configValue <key> <rendered file> — read one key out of the ConfigMap that the
# Deployment's first container actually consumes through envFrom. The link is
# verified, not assumed: a correct ConfigMap that no container reads leaves the
# process with nothing.
#
# The ConfigMap is selected BY THE REFERENCE THE CONTAINER MAKES, never by "the
# only ConfigMap in the render". A render can hold more than one — the
# authentication document is a second one since issue #444 — and a bare
# selector then returns two names, so every read below reported a missing link.
configValue() {
  local key="$1" file="$2" config_name env_from
  env_from="$(yq eval-all \
    'select(.kind == "Deployment") | .spec.template.spec.containers[0].envFrom[].configMapRef.name' \
    "$file")"
  config_name="$(yq eval-all \
    "select(.kind == \"ConfigMap\") | select(.metadata.name == \"$env_from\") | .metadata.name" "$file")"
  if [ -z "$config_name" ] || [ "$config_name" != "$env_from" ]; then
    echo "__NO_ENVFROM_LINK__"
    return
  fi
  yq eval-all \
    "select(.kind == \"ConfigMap\") | select(.metadata.name == \"$env_from\") | .data.$key // \"\"" "$file"
}

# projectIdCopies <rendered file> — EVERY copy of the public project id in a
# render, whichever object carries it and whichever of its four names it is
# under. Deliberately path-free and object-free: the defect this section exists
# for is a copy that disagrees, so a reader that looked in two named places
# would stop seeing the third the moment it moved.
#
# Two shapes carry it: a ConfigMap `KEY: "1"` line, and a container env pair
# (`- name: KEY` followed by `value: "1"`). Empty values are kept, so "one side
# set and the other dark" is visible rather than filtered away.
projectIdCopies() {
  sed -n -E 's/^[[:space:]]*(ELITEA_AI_PROJECT_ID|VITE_PUBLIC_PROJECT_ID):[[:space:]]*"?([^"]*)"?[[:space:]]*$/\2/p' "$1"
  grep -A 1 -E '^[[:space:]]*- name: (ELITEA_AI_PROJECT_ID|VITE_PUBLIC_PROJECT_ID)$' "$1" |
    sed -n -E 's/^[[:space:]]*value:[[:space:]]*"?([^"]*)"?[[:space:]]*$/\1/p'
}

# distinctProjectIds <rendered file> — the non-empty copies, deduplicated, on one
# line. More than one word here is a deployment whose surfaces disagree.
distinctProjectIds() {
  projectIdCopies "$1" | grep -v '^$' | sort -u | tr '\n' ' ' | sed -E 's/ $//'
}

# deployEnv <key> <rendered file> — read one plain env entry off the Deployment's
# first container. The gateway chart has no ConfigMap; it renders env inline.
deployEnv() {
  local key="$1" file="$2"
  yq eval-all \
    "select(.kind == \"Deployment\") | .spec.template.spec.containers[0].env[] | select(.name == \"$key\") | .value // \"\"" \
    "$file"
}

# composeEnv <service> <key> — read one env value out of the compose file.
composeEnv() {
  yq ".services.\"$1\".environment.\"$2\" // \"\"" "$COMPOSE"
}

echo "=== #463 — LLM_GATEWAY_URL, the public /llm path ========================="

# The OFF posture is coherent: with no URL there is no material either, so the
# chart never renders a half-configured pod.
helm template ${GATEWAY_RENDER_POSTURE} ${ONLY_MAIN} test-release "$MAIN" >"$WORK/main-default.yaml"
for key in LLM_GATEWAY_URL LLM_GATEWAY_CLIENT_CERT LLM_GATEWAY_CLIENT_KEY LLM_GATEWAY_CA_FILE; do
  actual="$(configValue "$key" "$WORK/main-default.yaml")"
  if [ "$actual" = "__NO_ENVFROM_LINK__" ]; then
    fail "the elitea-main container does not consume the rendered ConfigMap, so no value below reaches the process"
  elif [ -z "$actual" ]; then
    pass "a default install renders $key empty, which is the stated OFF posture"
  else
    fail "a default install renders $key=\"$actual\", so the OFF posture is not coherent"
  fi
done

# The chart must expose the key at all. An absent key cannot be set by an
# operator, and the whole defect class this file is about began that way.
if yq eval-all 'select(.kind == "ConfigMap") | .data | has("LLM_GATEWAY_URL")' \
  "$WORK/main-default.yaml" | grep -qx true; then
  pass "the chart exposes LLM_GATEWAY_URL, so an operator can set it"
else
  fail "the chart does not expose LLM_GATEWAY_URL at all"
fi

# The half-configured combinations are what produce a CrashLoopBackOff:
# internal/llmproxy/proxy.go always builds an mTLS transport, so a URL with no
# readable client certificate is a fatal boot error.
refuses "a gateway URL with no client certificate" \
  "LLM_GATEWAY_CLIENT_CERT" "$MAIN" \
  --set-string main.env.LLM_GATEWAY_URL="https://gw:8083"

refuses "a gateway URL with no CA bundle" \
  "LLM_GATEWAY_CA_FILE" "$MAIN" \
  --set-string main.env.LLM_GATEWAY_URL="https://gw:8083" \
  --set-string main.env.LLM_GATEWAY_CLIENT_CERT=/run/certs/client.crt \
  --set-string main.env.LLM_GATEWAY_CLIENT_KEY=/run/certs/client.key

refuses "certificate material with no gateway URL" \
  "LLM_GATEWAY_URL" "$MAIN" \
  --set-string main.env.LLM_GATEWAY_CLIENT_CERT=/run/certs/client.crt

# The old `secrets:` wiring put PEM TEXT in these variables. The code reads them
# as file paths, so that shape could never work.
refuses "certificate text where a file path belongs" \
  "absolute file path" "$MAIN" \
  --set-string main.env.LLM_GATEWAY_URL="https://gw:8083" \
  --set-string main.env.LLM_GATEWAY_CLIENT_CERT="-----BEGIN CERTIFICATE-----" \
  --set-string main.env.LLM_GATEWAY_CLIENT_KEY=/run/certs/client.key \
  --set-string main.env.LLM_GATEWAY_CA_FILE=/run/certs/ca.crt

refuses "a gateway URL with no scheme" \
  "missing scheme or host" "$MAIN" \
  --set-string main.env.LLM_GATEWAY_URL="gw:8083" \
  --set-string main.env.LLM_GATEWAY_CLIENT_CERT=/run/certs/client.crt \
  --set-string main.env.LLM_GATEWAY_CLIENT_KEY=/run/certs/client.key \
  --set-string main.env.LLM_GATEWAY_CA_FILE=/run/certs/ca.crt

# The ON posture reaches the container environment. This is the direction the
# refusals above would otherwise never let anyone test.
helm template ${GATEWAY_RENDER_POSTURE} ${ONLY_MAIN} test-release "$MAIN" -f "$MAIN/values-standalone.yaml" >"$WORK/main-standalone.yaml"
url="$(configValue LLM_GATEWAY_URL "$WORK/main-standalone.yaml")"
if [ -n "$url" ] && [ "$url" != "__NO_ENVFROM_LINK__" ]; then
  pass "values-standalone.yaml renders LLM_GATEWAY_URL=\"$url\" into the consumed ConfigMap"
else
  fail "values-standalone.yaml renders no LLM_GATEWAY_URL, so /llm answers 503 llm_gateway_not_configured"
fi
for key in LLM_GATEWAY_CLIENT_CERT LLM_GATEWAY_CLIENT_KEY LLM_GATEWAY_CA_FILE; do
  value="$(configValue "$key" "$WORK/main-standalone.yaml")"
  case "$value" in
    /*) pass "values-standalone.yaml renders $key as the absolute path $value" ;;
    *) fail "values-standalone.yaml renders $key=\"$value\", and llmproxy opens it as a file" ;;
  esac
done

# The three certificate names must NOT come back as secretKeyRef entries. That
# shape sets the variable to the CONTENT of a Secret key, and the process then
# tries to open a file whose name is a PEM block.
for key in LLM_GATEWAY_CLIENT_CERT LLM_GATEWAY_CLIENT_KEY LLM_GATEWAY_CA_FILE; do
  if [ -z "$(yq eval-all \
    "select(.kind == \"Deployment\") | .spec.template.spec.containers[0].env[]? | select(.name == \"$key\") | .valueFrom.secretKeyRef.name" \
    "$WORK/main-standalone.yaml")" ]; then
    pass "$key is not wired as a secretKeyRef, which cannot carry a file path"
  else
    fail "$key is wired as a secretKeyRef; that sets the variable to PEM text and llmproxy then opens it as a file name"
  fi
done

echo
echo "=== #467 — the three guard variables ===================================="

# elitea-main, guard #1 at upsert time. DEPLOYMENT_URL alone arms it, because
# selfref.go appends "/llm" itself.
refuses "the Configurations plane with no self-origin and no deployment URL" \
  "ELITEA_SELF_LLM_ORIGINS" "$MAIN" \
  -f "$MAIN/values-standalone.yaml" --set-string main.env.DEPLOYMENT_URL=""

renders "the Configurations plane with only DEPLOYMENT_URL set" "$MAIN" \
  -f "$MAIN/values-standalone.yaml" \
  --set-string main.env.DEPLOYMENT_URL="https://elitea.example.com" \
  --set-string main.env.ELITEA_SELF_LLM_ORIGINS=""

renders "the Configurations plane with only ELITEA_SELF_LLM_ORIGINS set" "$MAIN" \
  -f "$MAIN/values-standalone.yaml" \
  --set-string main.env.DEPLOYMENT_URL="" \
  --set-string main.env.ELITEA_SELF_LLM_ORIGINS="https://elitea.example.com/llm/v1"

# And the value reaches the container, not only the values file.
helm template ${GATEWAY_RENDER_POSTURE} ${ONLY_MAIN} test-release "$MAIN" -f "$MAIN/values-standalone.yaml" \
  --set-string main.env.DEPLOYMENT_URL="" \
  --set-string main.env.ELITEA_SELF_LLM_ORIGINS="https://elitea.example.com/llm/v1" \
  >"$WORK/main-selforigins.yaml"
if [ "$(configValue ELITEA_SELF_LLM_ORIGINS "$WORK/main-selforigins.yaml")" = "https://elitea.example.com/llm/v1" ]; then
  pass "ELITEA_SELF_LLM_ORIGINS reaches the container environment"
else
  fail "ELITEA_SELF_LLM_ORIGINS does not reach the container environment"
fi

# The gateway, guard #1 at request time. There is no legitimate empty posture.
refuses "an empty GATEWAY_SELF_LLM_ORIGINS" \
  "GATEWAY_SELF_LLM_ORIGINS" "$GATEWAY" \
  --set-string llmGateway.egressPosture=public-unrestricted

# The gateway, the egress allowlist. The posture must be STATED, because the
# empty value is permissive for public hosts and closed for private ones.
refuses "an unstated egress posture" \
  "egressPosture must be one of" "$GATEWAY" \
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS="https://elitea.example.com/llm/v1"

refuses "the allowlist posture with no allowlist" \
  "needs env.GATEWAY_EGRESS_ALLOWLIST" "$GATEWAY" \
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS="https://elitea.example.com/llm/v1" \
  --set-string llmGateway.egressPosture=allowlist

refuses "an allowlist that contradicts the stated posture" \
  "contradicts" "$GATEWAY" \
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS="https://elitea.example.com/llm/v1" \
  --set-string llmGateway.egressPosture=public-unrestricted \
  --set-string llmGateway.env.GATEWAY_EGRESS_ALLOWLIST="vllm.ml.svc.cluster.local:8000"

refuses "an egress posture that is not one of the two modes" \
  "egressPosture must be one of" "$GATEWAY" \
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS="https://elitea.example.com/llm/v1" \
  --set-string llmGateway.egressPosture=off

# Both valid postures render, and both reach the container. A guard that
# refuses everything also passes a test that only checks refusal.
helm template ${ONLY_GATEWAY} test-release "$GATEWAY" "${GUARDS_OK[@]}" >"$WORK/gw-allowlist.yaml"
if [ "$(deployEnv GATEWAY_EGRESS_ALLOWLIST "$WORK/gw-allowlist.yaml")" = "vllm.ml.svc.cluster.local:8000" ]; then
  pass "the allowlist posture puts the private model host in the container environment"
else
  fail "the allowlist posture does not carry GATEWAY_EGRESS_ALLOWLIST into the container"
fi
if [ "$(deployEnv GATEWAY_SELF_LLM_ORIGINS "$WORK/gw-allowlist.yaml")" = "https://elitea.example.com/llm/v1" ]; then
  pass "GATEWAY_SELF_LLM_ORIGINS reaches the container environment"
else
  fail "GATEWAY_SELF_LLM_ORIGINS does not reach the container environment"
fi

helm template ${ONLY_GATEWAY} test-release "$GATEWAY" \
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS="https://elitea.example.com/llm/v1" \
  --set-string llmGateway.egressPosture=public-unrestricted >"$WORK/gw-public.yaml"
if [ -z "$(deployEnv GATEWAY_EGRESS_ALLOWLIST "$WORK/gw-public.yaml")" ]; then
  pass "the public-unrestricted posture renders an empty allowlist, as it states"
else
  fail "the public-unrestricted posture renders a non-empty allowlist"
fi

echo
echo "=== G5 — the public project id is ONE value ============================"

# The compose stack. It is not a chart, so no template can check it: the two
# services must hold the SAME value, and the test must fail when one holds it
# and the other does not.
main_id="$(composeEnv elitea-main ELITEA_AI_PROJECT_ID)"
gw_id="$(composeEnv elitea-llm-gateway ELITEA_AI_PROJECT_ID)"
if [ -z "$main_id" ] || [ -z "$gw_id" ]; then
  fail "docker-compose.standalone-full.yml gives ELITEA_AI_PROJECT_ID to only one service (elitea-main=\"$main_id\", elitea-llm-gateway=\"$gw_id\"); the gateway then serves a different model set from the one the picker offers"
elif [ "$main_id" != "$gw_id" ]; then
  fail "docker-compose.standalone-full.yml sets different values for ELITEA_AI_PROJECT_ID (elitea-main=\"$main_id\", elitea-llm-gateway=\"$gw_id\")"
else
  pass "docker-compose.standalone-full.yml gives both services the same ELITEA_AI_PROJECT_ID (\"$main_id\")"
fi

# The chart. The comparison this suite used to make over two renders now lives
# in the chart itself (`elitea.aiProjectId` in templates/_helpers.tpl), because
# the four copies are in ONE chart and a check that runs at `helm template` time
# runs for every operator instead of only in this repository's CI. What is
# asserted here is that the check is armed, and that the single value reaches
# every copy.
FULL_RENDER=(
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS="https://elitea.example.com/llm/v1"
  --set-string llmGateway.egressPosture=public-unrestricted
)

# One value, set once, reaches every copy in the render.
helm template test-release "$MAIN" "${FULL_RENDER[@]}" \
  --set-string platform.aiProjectId=7 >"$WORK/single-value.yaml"
copies="$(projectIdCopies "$WORK/single-value.yaml" | grep -c .)"
distinct="$(distinctProjectIds "$WORK/single-value.yaml")"
if [ "$copies" -lt 3 ]; then
  fail "the render carries only $copies copies of the public project id; this assertion would pass on a render that had lost them"
elif [ "$distinct" = "7" ]; then
  pass "platform.aiProjectId=7 reaches all $copies copies in the render, and they agree"
else
  fail "platform.aiProjectId=7 rendered these distinct ids: $distinct"
fi

# The default render still leaves elitea-main's copy EMPTY. It must: with the
# Configurations plane off, elitea-main refuses to start when the variable is
# present at all, so a chart that invented a value here would stop every default
# install.
helm template test-release "$MAIN" "${FULL_RENDER[@]}" >"$WORK/no-value.yaml"
main_default="$(configValue ELITEA_AI_PROJECT_ID "$WORK/no-value.yaml")"
if [ -z "$main_default" ] || [ "$main_default" = "__NO_ENVFROM_LINK__" ]; then
  pass "a default install still leaves ELITEA_AI_PROJECT_ID empty, so it boots with the Configurations plane off"
else
  fail "a default install renders ELITEA_AI_PROJECT_ID=\"$main_default\"; elitea-main refuses to start on that with ELITEA_CONFIGURATIONS_ENABLED off"
fi

# ...and the SPA still gets "1", which is the value this chart shipped as the
# web default and every reference deployment uses. Empty there is not a safe
# fallback: it makes every public-project check in the SPA false, silently.
web_default="$(distinctProjectIds "$WORK/no-value.yaml")"
if [ "$web_default" = "1" ]; then
  pass "a default install still renders VITE_PUBLIC_PROJECT_ID=\"1\" for the SPA"
else
  fail "a default install renders these public project ids: \"$web_default\", expected only \"1\""
fi

# The refusals. Each pair below is a deployment that installs cleanly, reports
# every pod Ready, and then resolves no shared credential at all.
refuses "elitea-main and the gateway naming different public projects" \
  "name different public projects" "$MAIN" \
  --set-string main.env.ELITEA_AI_PROJECT_ID=1 \
  --set-string llmGateway.env.ELITEA_AI_PROJECT_ID=2

refuses "the SPA naming a different public project from elitea-main" \
  "name different public projects" "$MAIN" \
  --set-string main.env.ELITEA_AI_PROJECT_ID=2 \
  --set-string web.env.VITE_PUBLIC_PROJECT_ID=1

refuses "platform.aiProjectId contradicting a component key" \
  "name different public projects" "$MAIN" \
  --set-string platform.aiProjectId=3 \
  --set-string main.env.ELITEA_AI_PROJECT_ID=4

refuses "a public project id that is not a project id" \
  "must be a positive project id" "$MAIN" \
  --set-string platform.aiProjectId=public

# Backward compatibility. A values file that still sets both component keys, to
# the same value, must keep rendering — every values file in this repository is
# that shape, and breaking them to fix a naming problem would be the wrong
# trade.
helm template test-release "$MAIN" "${FULL_RENDER[@]}" \
  --set-string main.env.ELITEA_AI_PROJECT_ID=1 \
  --set-string llmGateway.env.ELITEA_AI_PROJECT_ID=1 \
  --set-string web.env.VITE_PUBLIC_PROJECT_ID=1 >"$WORK/legacy-values.yaml" 2>"$WORK/legacy-err.txt"
legacy_distinct="$(distinctProjectIds "$WORK/legacy-values.yaml")"
if [ "$legacy_distinct" = "1" ]; then
  pass "a values file that still sets all three keys to one value keeps rendering"
else
  fail "a values file that sets all three keys to \"1\" rendered these ids: \"$legacy_distinct\"
$(cat "$WORK/legacy-err.txt")"
fi

echo
echo "=== #464 — the budget accumulator writers ==============================="

helm template ${ONLY_SCHEDULER} test-release "$SCHEDULER" >"$WORK/scheduler.yaml"
while read -r key expected; do
  [ -z "$key" ] && continue
  actual="$(configValue "$key" "$WORK/scheduler.yaml")"
  if [ "$actual" = "$expected" ]; then
    pass "a default scheduler install renders $key=\"$expected\""
  else
    fail "a default scheduler install renders $key=\"$actual\", expected \"$expected\"; nothing then fills gateway.llm_budget_accumulators"
  fi
done <<'EXPECTED'
BUDGET_WRITEBACK_ENABLED true
PRICE_SYNC_ENABLED true
EXPECTED

# cmd/elitea-scheduler starts the consumer only when the flag AND the URL are
# both set, so the flag alone changes nothing.
nats_url="$(configValue GATEWAY_NATS_URL "$WORK/scheduler.yaml")"
if [ -z "$nats_url" ]; then
  fail "a default scheduler install renders GATEWAY_NATS_URL empty, so the write-back consumer never starts whatever BUDGET_WRITEBACK_ENABLED says"
else
  pass "a default scheduler install renders GATEWAY_NATS_URL=\"$nats_url\""
  # NATS is installed into a DIFFERENT namespace from this workload, and a
  # short name does not resolve across namespaces.
  case "$nats_url" in
    *.svc.cluster.local:*) pass "GATEWAY_NATS_URL is an FQDN, so it resolves from another namespace" ;;
    *) fail "GATEWAY_NATS_URL is \"$nats_url\", which is not an FQDN; NATS runs in elitea-gateway and this workload does not" ;;
  esac
fi

# The opposite direction: an operator who turns collection off must still get a
# rendered manifest, because that is a supported posture. What must NOT happen
# is the flag reading "on" while the URL is empty.
renders "a deliberate collection-off scheduler install" "$SCHEDULER" \
  --set-string scheduler.env.BUDGET_WRITEBACK_ENABLED=false \
  --set-string scheduler.env.PRICE_SYNC_ENABLED=false
helm template ${ONLY_SCHEDULER} test-release "$SCHEDULER" \
  --set-string scheduler.env.BUDGET_WRITEBACK_ENABLED=false >"$WORK/scheduler-off.yaml"
if [ "$(configValue BUDGET_WRITEBACK_ENABLED "$WORK/scheduler-off.yaml")" = "false" ]; then
  pass "collection can be turned off deliberately, and the rendered value says so"
else
  fail "BUDGET_WRITEBACK_ENABLED=false does not reach the rendered ConfigMap"
fi

echo
if [ "$failures" -eq 0 ]; then
  echo "render-llm-path: all checks passed"
else
  echo "render-llm-path: $failures check(s) failed" >&2
  exit 1
fi
