#!/usr/bin/env bash
# render-worker-node-recovery.sh — recovery is not a setting (PLAN §8.3).
#
# The Rust worker always runs its durable node attempt journal (an effectful
# node interrupted by a Worker crash stops for recovery instead of running its
# effect again) and model-checkpoint recovery. Its config struct no longer has
# `agent_node_recovery` / `agent_model_checkpoint_recovery`, and
# deny_unknown_fields refuses a runtime.json that still names either
# (services/elitea-worker-rust/src/config.rs). The chart values that wrote them
# are deleted.
#
# The rules held here, each read from the RENDERED chart:
#
#   * runtime.json never carries either recovery key, and always carries
#     agent_checkpoint_connection_path, with agent-checkpoint-connection in the
#     material the init container requires (the journal and the model
#     checkpoints live in the AgentState database behind it);
#   * a values file that still sets worker.runtime.agentNodeRecovery or
#     agentModelCheckpointRecovery — true, false or anything else — fails the
#     render with a message that says the value was removed, even with the
#     worker off: silently ignoring it would read as governing something;
#   * with the journal always on, a Code node is an original-Code visit that
#     elitea-main admits only with original-Code owner recovery
#     (main.runtime.codeOwnerRecovery, always on with Agent dispatch and with
#     no enable bit: a values file that sets one is refused), signing only for
#     its supervisors' audiences. So sandbox runtimes without it are refused, and
#     each execution audience (and a platform_client's) needs its own owner
#     supervisor (_code-nodes.tpl, codeNodes.validateWorker). A preparation
#     audience needs no owner supervisor, only to be a sandbox audience.
#   * the chart's own defaults satisfy all of it: the derived four-language
#     backends render with Main's default owner recovery.
#
# Run: deploy/helm/tests/render-worker-node-recovery.sh
# Needs: helm, python3 with PyYAML. No cluster, no network.
set -euo pipefail

DIR="$(cd "$(dirname "$0")/../.." && pwd)"
REPO_ROOT="$(cd "$DIR/.." && pwd)"
# shellcheck source=../../../scripts/lib/assertion-floor.sh
. "${REPO_ROOT}/scripts/lib/assertion-floor.sh"

HELM="${HELM:-helm}"
CHART="$DIR/helm/elitea"
FIXTURE="$DIR/helm/tests/fixtures/code-material-values.yaml"
PASS=0
FAIL=0

# DERIVED, not written down — see scripts/lib/assertion-floor.sh. Each
# assertion holds exactly one accepting arm, and no comment here spells that
# call followed by a quote.
ASSERTION_SITE_PATTERN='(^|[^[:alnum:]_])ok[[:space:]]+"'
EXPECTED_ASSERTIONS="$(derive_assertion_floor "$0" "$ASSERTION_SITE_PATTERN")"

for tool in "$HELM" python3; do
  command -v "$tool" >/dev/null 2>&1 || { echo "render-worker-node-recovery.sh needs $tool on PATH" >&2; exit 2; }
done

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

ok()  { PASS=$((PASS+1)); echo "  ok: $1"; }
bad() { FAIL=$((FAIL+1)); echo "  FAIL: $1" >&2; }

# values-standalone.yaml supplies the runtime plane the worker requires and
# the fixture the Code sandbox material; the gateway postures are render-only
# values (`.invalid` never resolves).
RENDER=(
  -f "$CHART/values-standalone.yaml" -f "$FIXTURE"
  --set worker.enabled=true
  --set main.runtime.indexIngestDispatch.enabled=false --set-string main.runtime.indexIngestDispatch.commandStream=
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://ci-render-only.example.invalid/llm/v1
  --set-string llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true
)

# render <name> [helm args...] — the worker ConfigMap, Deployment and Main's ConfigMap.
render() {
  local name="$1"
  shift
  "$HELM" template t "$CHART" "${RENDER[@]}" "$@" \
    --show-only templates/worker/configmap-runtime.yaml \
    --show-only templates/worker/deployment.yaml \
    --show-only templates/main/configmap.yaml \
    >"$TMP/$name.yaml" 2>"$TMP/$name.err"
}

# journal_backed <name> — no recovery key in runtime.json; the checkpoint
# connection path is there and the init container requires the file behind it.
journal_backed() {
  python3 - "$TMP/$1.yaml" <<'PY'
import json, sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
cm = next(d for d in docs if d["kind"] == "ConfigMap" and "runtime.json" in d.get("data", {}))
dep = next(d for d in docs if d["kind"] == "Deployment")
config = json.loads(cm["data"]["runtime.json"])
assert "agent_node_recovery" not in config, config
assert "agent_model_checkpoint_recovery" not in config, config
assert config["agent_checkpoint_connection_path"] == "/run/elitea-runtime/agent-checkpoint-connection", config
envs = [e for c in dep["spec"]["template"]["spec"]["initContainers"] for e in c.get("env", [])]
required = " ".join(e.get("value", "") for e in envs).split()
assert "agent-checkpoint-connection" in required, required
PY
}

# languages <name> — the rendered sandbox_runtimes languages, comma separated.
languages() {
  python3 - "$TMP/$1.yaml" <<'PY'
import json, sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
cm = next(d for d in docs if d["kind"] == "ConfigMap" and "runtime.json" in d.get("data", {}))
config = json.loads(cm["data"]["runtime.json"])
print(",".join(r["language"] for r in config.get("sandbox_runtimes", [])))
PY
}

# main_owner_on <name> — Main's ConfigMap configures owner recovery, which has
# no enable variable any more (PLAN §8.7 X1; Main refuses it).
main_owner_on() {
  grep -qF 'ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG:' "$TMP/$1.yaml" \
    && ! grep -qF 'ELITEA_RUNTIME_CODE_OWNER_RECOVERY_ENABLED' "$TMP/$1.yaml"
}

# refuses <expected message> [helm args...]
refuses() {
  local expected="$1" output
  shift
  if output="$("$HELM" template t "$CHART" "${RENDER[@]}" "$@" 2>&1)"; then
    return 1
  fi
  grep -qF "$expected" <<<"$output"
}

echo "== the default: recovery always on, nothing to say in runtime.json =="
render rust-default \
  && ok "rust renders with the default values" \
  || bad "rust with the default values does not render: $(tail -1 "$TMP/rust-default.err")"
journal_backed rust-default \
  && ok "default: no recovery key in runtime.json, the checkpoint connection is always there" \
  || bad "default: runtime.json carries a recovery key or lacks its checkpoint connection"
[ "$(languages rust-default)" = "python,javascript,typescript,rust" ] && main_owner_on rust-default \
  && ok "default: four derived Code backends, and Main gets owner recovery for them" \
  || bad "default: Code backends $(languages rust-default 2>/dev/null), Main owner recovery $(main_owner_on rust-default && echo on || echo off)"

echo "== the removed values are refused, whatever they say =="
NODE_REMOVED='worker.runtime.agentNodeRecovery was removed: node recovery is always on'
MODEL_REMOVED='worker.runtime.agentModelCheckpointRecovery was removed: model-checkpoint recovery is always on'
refuses "$NODE_REMOVED" --set worker.runtime.agentNodeRecovery=true \
  && ok "agentNodeRecovery=true: refused as removed" \
  || bad "agentNodeRecovery=true: not refused with '$NODE_REMOVED'"
refuses "$NODE_REMOVED" --set worker.runtime.agentNodeRecovery=false \
  && ok "agentNodeRecovery=false: refused as removed, never read as off" \
  || bad "agentNodeRecovery=false: not refused with '$NODE_REMOVED'"
refuses "$NODE_REMOVED" --set-string worker.runtime.agentNodeRecovery=false \
  && ok "agentNodeRecovery as the string \"false\": refused as removed" \
  || bad "agentNodeRecovery as a string: not refused with '$NODE_REMOVED'"
refuses "$MODEL_REMOVED" --set worker.runtime.agentModelCheckpointRecovery=true \
  && ok "agentModelCheckpointRecovery=true: refused as removed" \
  || bad "agentModelCheckpointRecovery=true: not refused with '$MODEL_REMOVED'"
refuses "$MODEL_REMOVED" --set worker.runtime.agentModelCheckpointRecovery=false \
  && ok "agentModelCheckpointRecovery=false: refused as removed" \
  || bad "agentModelCheckpointRecovery=false: not refused with '$MODEL_REMOVED'"
refuses "$NODE_REMOVED" --set worker.enabled=false --set worker.runtime.agentNodeRecovery=false \
  && ok "agentNodeRecovery with the worker off: still refused (a dead value reads as governing something)" \
  || bad "agentNodeRecovery with the worker off: not refused"

echo "== Code nodes need original-Code owner recovery in elitea-main =="
# One explicit Python Code backend with its preparation, as an operator who
# runs supervisors outside this chart writes it.
cat >"$TMP/sandbox.yaml" <<'YAML'
worker:
  runtime:
    sandboxRuntimes:
      - language: python
        target: sandbox-python:9446
        audience: dns:sandbox-python
        image_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        policy_revision: python-js-v1
        timeout_seconds: 120
        preparation:
          target: sandbox-python-preparation:9448
          audience: dns:sandbox-python-preparation
          image_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
          policy_revision: python-preparation-v1
          timeout_seconds: 120
YAML
# Main's original-Code owner for that backend's execution audience; the
# preparation audience is a sandbox audience, not an owner supervisor.
cat >"$TMP/owner.yaml" <<'YAML'
main:
  runtime:
    sandboxAudiences: [dns:sandbox-python, dns:sandbox-python-preparation]
    codeOwnerRecovery:
      mainWorkloadIdentity: spiffe://elitea.invalid/main
      supervisors:
        - audience: dns:sandbox-python
          httpsOrigin: https://sandbox-python:9446
YAML
CODE_REFUSAL='Code nodes would be refused without original-Code owner recovery'
OWNER_REMOVED='main.runtime.codeOwnerRecovery.enabled was removed: Code owner recovery is always on with Agent dispatch'
# Owner recovery has no enable bit (PLAN §8.7 X1): it is off only where Agent
# dispatch is off, and explicit Code backends there are refused.
NO_DISPATCH=(--set main.runtime.agentExecutionDispatch.enabled=false --set-string main.runtime.agentExecutionDispatch.commandStream=)

refuses "$CODE_REFUSAL" -f "$TMP/sandbox.yaml" "${NO_DISPATCH[@]}" \
  && ok "explicit sandbox runtimes without Agent dispatch (no owner recovery): refused" \
  || bad "explicit sandbox runtimes without Agent dispatch: not refused with '$CODE_REFUSAL'"
refuses "$OWNER_REMOVED" --set main.runtime.codeOwnerRecovery.enabled=false \
  && ok "codeOwnerRecovery.enabled=false: refused as removed, never read as off" \
  || bad "codeOwnerRecovery.enabled=false: not refused with '$OWNER_REMOVED'"
render rust-code-owner -f "$TMP/sandbox.yaml" -f "$TMP/owner.yaml" \
  && journal_backed rust-code-owner && [ "$(languages rust-code-owner)" = "python" ] \
  && main_owner_on rust-code-owner \
  && ok "explicit sandbox runtimes with owner recovery: renders, the explicit list replaces the derived one" \
  || bad "explicit sandbox runtimes with owner recovery: $(tail -1 "$TMP/rust-code-owner.err")"
# A second backend whose supervisor audience has no owner supervisor: Main
# signs original-Code intents only for its owner supervisors' audiences.
cat >"$TMP/sandbox-two.yaml" <<'YAML'
worker:
  runtime:
    sandboxRuntimes:
      - language: python
        target: sandbox-python:9446
        audience: dns:sandbox-python
        image_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        policy_revision: python-js-v1
        timeout_seconds: 120
      - language: rust
        target: sandbox-rust:9447
        audience: dns:sandbox-rust
        image_digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
        policy_revision: rust-v1
        timeout_seconds: 120
YAML
refuses 'no main.runtime.codeOwnerRecovery supervisor for audience "dns:sandbox-rust"' \
  -f "$TMP/sandbox-two.yaml" -f "$TMP/owner.yaml" \
  && ok "owner recovery for one of two execution audiences: refused for the uncovered audience" \
  || bad "owner recovery missing one execution audience: not refused for dns:sandbox-rust"
# A platform_client audience is an execution audience too.
cat >"$TMP/sandbox-platform.yaml" <<'YAML'
worker:
  runtime:
    sandboxRuntimes:
      - language: python
        target: sandbox-python:9446
        audience: dns:sandbox-python
        image_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        policy_revision: python-js-v1
        timeout_seconds: 120
        platform_client:
          target: sandbox-broker:9453
          audience: dns:sandbox-broker
          image_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
          policy_revision: python-js-v1
          timeout_seconds: 120
          max_calls: 32
          max_total_bytes: 1048576
YAML
refuses 'no main.runtime.codeOwnerRecovery supervisor for audience "dns:sandbox-broker"' \
  -f "$TMP/sandbox-platform.yaml" -f "$TMP/owner.yaml" \
  && ok "a platform_client audience without an owner supervisor: refused" \
  || bad "a platform_client audience without an owner supervisor: not refused for dns:sandbox-broker"
refuses 'preparation audience "dns:sandbox-python-preparation" is not in main.runtime.sandboxAudiences' \
  -f "$TMP/sandbox.yaml" -f "$TMP/owner.yaml" --set-json 'main.runtime.sandboxAudiences=["dns:sandbox-python"]' \
  && ok "a preparation audience Main does not sign for: refused (it needs the audience, not an owner)" \
  || bad "a preparation audience missing from sandboxAudiences: not refused"

echo
RAN=$((PASS+FAIL))
echo "worker node recovery render assertions: ${RAN} ran, ${PASS} passed, ${FAIL} failed"
# -ne, not -lt: too low means an assertion stopped running, too high means a
# site ran more than once and the floor no longer describes this file.
if [ "$RAN" -ne "$EXPECTED_ASSERTIONS" ]; then
  echo "FAIL: ${RAN} assertion(s) ran, and this file holds ${EXPECTED_ASSERTIONS} assertion site(s)." >&2
  exit 1
fi
[ "$FAIL" -eq 0 ]
