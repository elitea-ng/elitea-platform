#!/usr/bin/env bash
# render-worker-node-recovery.sh — worker.runtime.agentNodeRecovery reaches the
# Rust worker exactly as the operator wrote it (gap G-WORKER-01).
#
# `agent_node_recovery` is the durable node attempt journal: an effectful node
# interrupted by a Worker crash stops for recovery instead of running its
# effect again, and effectful pipeline direct tool nodes are refused without
# it. Under a node-recovery claim elitea-main also grants model checkpoint
# inspection, so the key turns on model-step resume as well. The chart keeps
# it off by default, as it keeps all recovery off, until the pod-replacement
# proofs pass (services/elitea-worker-rust/docs/recovery-guarantees.md, D1);
# compose turns it on (deploy/runtime/worker-runtime.rust.json).
#
# The rules held here, each read from the RENDERED runtime.json:
#
#   * false (the default) writes no key for either implementation: the Rust
#     worker reads a missing key as false (#[serde(default)] in
#     services/elitea-worker-rust/src/config.rs), the Python worker refuses
#     unknown keys, and render-worker.sh requires the default runtime.json to
#     be the same document for both;
#   * true reaches the Rust worker, and true with the Python worker fails the
#     render;
#   * anything that is not a boolean is refused, for this key and for
#     agentModelCheckpointRecovery: a string "false" is truthy in a template
#     and would turn recovery on without a word;
#   * when the journal is on, agent_checkpoint_connection_path is present and
#     agent-checkpoint-connection is in the material the init container
#     requires. The worker refuses node recovery without that path
#     (services/elitea-worker-rust/src/config.rs, validate).
#
#   * with the journal on, a Code node is an original-Code visit that
#     elitea-main must admit, and Main admits it only with original-Code owner
#     recovery (main.runtime.codeOwnerRecovery). So the journal together with
#     sandbox runtimes and owner recovery off is refused: every Code node would
#     fail at admission, before any sandbox dispatch, as the generic "The
#     runtime operation failed." (_code-nodes.tpl, codeNodes.validateWorker).
#     The journal without sandbox runtimes, sandbox runtimes without the
#     journal, and all three together render.
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

# values-standalone.yaml supplies the runtime plane the worker requires; the
# gateway postures are render-only values (`.invalid` never resolves).
RENDER=(
  -f "$CHART/values-standalone.yaml"
  --set worker.enabled=true
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://ci-render-only.example.invalid/llm/v1
  --set-string llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true
)

# render <name> [helm args...] — the worker ConfigMap and Deployment only.
render() {
  local name="$1"
  shift
  "$HELM" template t "$CHART" "${RENDER[@]}" "$@" \
    --show-only templates/worker/configmap-runtime.yaml \
    --show-only templates/worker/deployment.yaml \
    >"$TMP/$name.yaml" 2>"$TMP/$name.err"
}

# node_recovery <name> — the rendered value: true, false or absent.
node_recovery() {
  python3 - "$TMP/$1.yaml" <<'PY'
import json, sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
cm = next(d for d in docs if d["kind"] == "ConfigMap")
config = json.loads(cm["data"]["runtime.json"])
print(json.dumps(config["agent_node_recovery"]) if "agent_node_recovery" in config else "absent")
PY
}

# journal_backed <name> — the worker can open the journal: the connection path
# is in runtime.json and the init container requires the file behind it.
journal_backed() {
  python3 - "$TMP/$1.yaml" <<'PY'
import json, sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
cm = next(d for d in docs if d["kind"] == "ConfigMap")
dep = next(d for d in docs if d["kind"] == "Deployment")
config = json.loads(cm["data"]["runtime.json"])
assert config["agent_checkpoint_connection_path"] == "/run/elitea-runtime/agent-checkpoint-connection", config
envs = [e for c in dep["spec"]["template"]["spec"]["initContainers"] for e in c.get("env", [])]
required = " ".join(e.get("value", "") for e in envs).split()
assert "agent-checkpoint-connection" in required, required
PY
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

echo "== the default: no key, so recovery is off =="
render rust-default --set worker.implementation=rust \
  && ok "rust renders with the default values" \
  || bad "rust with the default values does not render: $(tail -1 "$TMP/rust-default.err")"
[ "$(node_recovery rust-default)" = "absent" ] \
  && ok "rust, default: agent_node_recovery is absent (the worker reads false)" \
  || bad "rust, default: agent_node_recovery is $(node_recovery rust-default), want absent"

render python-default --set worker.implementation=python \
  && ok "python renders with the default values" \
  || bad "python with the default values does not render: $(tail -1 "$TMP/python-default.err")"
[ "$(node_recovery python-default)" = "absent" ] \
  && ok "python, default: agent_node_recovery is absent" \
  || bad "python, default: agent_node_recovery is $(node_recovery python-default); the Python worker refuses unknown keys"

echo "== an explicit value is the operator's =="
render rust-false --set worker.implementation=rust --set worker.runtime.agentNodeRecovery=false \
  && [ "$(node_recovery rust-false)" = "absent" ] \
  && ok "rust, false: agent_node_recovery is absent (the worker reads false)" \
  || bad "rust, false: got $(node_recovery rust-false 2>/dev/null || tail -1 "$TMP/rust-false.err")"
render rust-true --set worker.implementation=rust --set worker.runtime.agentNodeRecovery=true \
  && [ "$(node_recovery rust-true)" = "true" ] && journal_backed rust-true \
  && ok "rust, true: agent_node_recovery is true with its checkpoint connection" \
  || bad "rust, true: got $(node_recovery rust-true 2>/dev/null || tail -1 "$TMP/rust-true.err")"
render python-false --set worker.implementation=python --set worker.runtime.agentNodeRecovery=false \
  && [ "$(node_recovery python-false)" = "absent" ] \
  && ok "python, false: renders, and agent_node_recovery is absent" \
  || bad "python, false: got $(node_recovery python-false 2>/dev/null || tail -1 "$TMP/python-false.err")"
refuses 'require the Rust worker' --set worker.implementation=python --set worker.runtime.agentNodeRecovery=true \
  && ok "python, true: the render is refused (the Python worker has no journal)" \
  || bad "python, true: the render was not refused with 'require the Rust worker'"

echo "== a value that is not a boolean is refused =="
refuses 'worker.runtime.agentNodeRecovery must be true or false' \
  --set worker.implementation=rust --set worker.runtime.agentNodeRecovery=null \
  && ok "rust, null: refused, not read as off" \
  || bad "rust, null: not refused"
refuses 'worker.runtime.agentNodeRecovery must be true or false' \
  --set worker.implementation=rust --set-string worker.runtime.agentNodeRecovery=false \
  && ok "rust, the string \"false\": refused, not read as truthy" \
  || bad "rust, the string \"false\": not refused"
refuses 'worker.runtime.agentNodeRecovery must be true or false' \
  --set worker.implementation=rust --set worker.runtime.agentNodeRecovery=1 \
  && ok "rust, the number 1: refused" \
  || bad "rust, the number 1: not refused"
refuses 'worker.runtime.agentModelCheckpointRecovery must be true or false' \
  --set worker.implementation=rust --set-string worker.runtime.agentModelCheckpointRecovery=false \
  && ok "rust, agentModelCheckpointRecovery as the string \"false\": refused too" \
  || bad "rust, agentModelCheckpointRecovery as the string \"false\": not refused"

echo "== Code nodes with the journal need original-Code owner recovery in elitea-main =="
# One Python Code backend, as render-worker-sandbox.sh writes it.
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
YAML
# Main's original-Code owner for that backend's supervisor audience.
cat >"$TMP/owner.yaml" <<'YAML'
main:
  runtime:
    sandboxAudiences: [dns:sandbox-python]
    codeOwnerRecovery:
      enabled: true
      mainWorkloadIdentity: spiffe://elitea.invalid/main
      supervisors:
        - audience: dns:sandbox-python
          httpsOrigin: https://sandbox-python:9446
YAML
CODE_REFUSAL='Code nodes would be refused without original-Code owner recovery'

# code_backed <name> — the worker gets both the journal and the Code backend.
code_backed() {
  python3 - "$TMP/$1.yaml" <<'PY'
import json, sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
cm = next(d for d in docs if d["kind"] == "ConfigMap")
config = json.loads(cm["data"]["runtime.json"])
assert config["agent_node_recovery"] is True, config
assert [p["language"] for p in config["sandbox_runtimes"]] == ["python"], config
PY
}

# main_owner_on [helm args...] — Main's ConfigMap turns owner recovery on.
main_owner_on() {
  "$HELM" template t "$CHART" "${RENDER[@]}" "$@" --show-only templates/main/configmap.yaml 2>/dev/null \
    | grep -qF 'ELITEA_RUNTIME_CODE_OWNER_RECOVERY_ENABLED: "true"'
}

refuses "$CODE_REFUSAL" --set worker.implementation=rust --set worker.runtime.agentNodeRecovery=true \
  -f "$TMP/sandbox.yaml" \
  && ok "rust, journal and sandbox runtimes, owner recovery off: refused" \
  || bad "rust, journal and sandbox runtimes, owner recovery off: not refused with '$CODE_REFUSAL'"
refuses "$CODE_REFUSAL" --set worker.implementation=rust --set worker.runtime.agentNodeRecovery=true \
  -f "$TMP/sandbox.yaml" --set main.runtime.codeOwnerRecovery.enabled=false \
  && ok "rust, journal and sandbox runtimes, owner recovery explicitly false: refused" \
  || bad "rust, journal and sandbox runtimes, owner recovery explicitly false: not refused with '$CODE_REFUSAL'"
render rust-journal-no-code --set worker.implementation=rust --set worker.runtime.agentNodeRecovery=true \
  && [ "$(node_recovery rust-journal-no-code)" = "true" ] \
  && ok "rust, journal without sandbox runtimes, owner recovery off: renders" \
  || bad "rust, journal without sandbox runtimes: got $(node_recovery rust-journal-no-code 2>/dev/null || tail -1 "$TMP/rust-journal-no-code.err")"
render rust-code-no-journal --set worker.implementation=rust -f "$TMP/sandbox.yaml" \
  && [ "$(node_recovery rust-code-no-journal)" = "absent" ] \
  && ok "rust, sandbox runtimes without the journal, owner recovery off: renders" \
  || bad "rust, sandbox runtimes without the journal: got $(node_recovery rust-code-no-journal 2>/dev/null || tail -1 "$TMP/rust-code-no-journal.err")"
render rust-journal-code-owner --set worker.implementation=rust --set worker.runtime.agentNodeRecovery=true \
  -f "$TMP/sandbox.yaml" -f "$TMP/owner.yaml" \
  && code_backed rust-journal-code-owner \
  && main_owner_on --set worker.implementation=rust --set worker.runtime.agentNodeRecovery=true \
    -f "$TMP/sandbox.yaml" -f "$TMP/owner.yaml" \
  && ok "rust, journal, sandbox runtimes and owner recovery: renders, and Main gets owner recovery" \
  || bad "rust, journal, sandbox runtimes and owner recovery: $(tail -1 "$TMP/rust-journal-code-owner.err")"
refuses 'require the Rust worker' --set worker.implementation=python --set worker.runtime.agentNodeRecovery=true \
  -f "$TMP/sandbox.yaml" \
  && ok "python, journal and sandbox runtimes: refused as Rust-only, not as a Code owner gap" \
  || bad "python, journal and sandbox runtimes: not refused with 'require the Rust worker'"

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
