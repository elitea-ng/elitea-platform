#!/usr/bin/env bash
# render-worker.sh — the agent worker (the Rust worker, the only one) renders
# correctly.
#
# ── Why this file exists ─────────────────────────────────────────────────────
#
# Every other render test in this directory sets `worker.enabled=false`, and so
# does the template matrix in helm-lint.yml. The worker templates were therefore
# rendered by NOTHING: `helm lint` walks the chart, but a `{{- if
# .Values.worker.enabled }}` body it never enters is a body it never checks.
#
#   * services/elitea-worker-rust/src/lib.rs matches an EXACT five-token slice.
#     Any other command line is `worker_cli.invalid_arguments` and an immediate
#     exit, which reaches an operator as a CrashLoopBackOff with no
#     configuration error anywhere.
#   * The Rust worker refuses a toolkit-security document without a
#     `toolkit_security` key (src/execution/production.rs), so the second file
#     is mandatory, not optional hardening.
#   * The retired `worker.implementation: python` must fail the render rather
#     than silently render the Rust worker for a values file that asks for the
#     removed one.
#
# Both of those are render-time facts, and a render test is the cheapest place
# to hold them. The alternative is finding out at the first chat turn.
#
# Run: deploy/helm/tests/render-worker.sh   (requires helm + python3 + PyYAML)
set -euo pipefail

# deploy/helm/tests -> deploy
DIR="$(cd "$(dirname "$0")/../.." && pwd)"
REPO_ROOT="$(cd "$DIR/.." && pwd)"
# shellcheck source=../../../scripts/lib/assertion-floor.sh
. "${REPO_ROOT}/scripts/lib/assertion-floor.sh"

HELM="${HELM:-helm}"
CHART="$DIR/helm/elitea"
PASS=0
FAIL=0

# DERIVED, not written down — see scripts/lib/assertion-floor.sh. Each
# assertion below holds exactly one accepting arm, so the accepting arms are
# the assertions. The pattern must not match this prose either, which is why
# no comment in this file spells the accepting call followed by a quote.
ASSERTION_SITE_PATTERN='(^|[^[:alnum:]_])ok[[:space:]]+"'
EXPECTED_ASSERTIONS="$(derive_assertion_floor "$0" "$ASSERTION_SITE_PATTERN")"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

ok()  { PASS=$((PASS+1)); echo "  ok: $1"; }
bad() { FAIL=$((FAIL+1)); echo "  FAIL: $1" >&2; }

# values-standalone.yaml supplies the runtime plane the worker requires
# (main.runtime.*, its command stream and auth config). Without it the chart
# refuses long before it reaches the worker, and this file would be asserting
# against a refusal. The gateway postures are render-only values: `.invalid` is
# reserved by RFC 2606 and never resolves.
RENDER=(
  -f "$CHART/values-standalone.yaml"
  --set worker.enabled=true
  --set main.runtime.indexIngestDispatch.enabled=false --set-string main.runtime.indexIngestDispatch.commandStream=
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://ci-render-only.example.invalid/llm/v1
  --set-string llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true
)

"$HELM" template t "$CHART" "${RENDER[@]}" >"$TMP/rust.yaml" 2>"$TMP/rust.err" \
  && ok "the chart renders the worker" \
  || bad "the worker does not render: $(tail -1 "$TMP/rust.err")"

echo "== the Rust worker gets the argument list and files it requires =="
python3 - "$TMP/rust.yaml" <<'PY' && ok "rust: five-token serve, both files mounted, rust image" || bad "rust worker shape"
import json, sys, yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
dep = next(d for d in docs
           if d["kind"] == "Deployment" and d["metadata"]["name"] == "elitea-worker")
cm = next(d for d in docs
          if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "elitea-worker-runtime")
c = next(x for x in dep["spec"]["template"]["spec"]["containers"] if x["name"] == "worker")

# The exact slice src/lib.rs matches. Anything else exits worker_cli.invalid_arguments.
assert c["args"] == [
    "serve", "--config", "/run/elitea/runtime.json",
    "--toolkit-security-config", "/run/elitea/toolkit-security.json",
], f"rust argument list is {c['args']}"

assert c["image"].startswith("ghcr.io/elitea-ng/elitea-worker-rust:"), c["image"]

# The flag is worthless if the file is not actually in the container.
mounts = {m["mountPath"] for m in c["volumeMounts"]}
assert "/run/elitea/toolkit-security.json" in mounts, sorted(mounts)
assert "/run/elitea/runtime.json" in mounts, sorted(mounts)

policy = json.loads(cm["data"]["toolkit-security.json"])
# production.rs refuses a document without this key, even an empty one.
assert "toolkit_security" in policy, policy
# worker.runtime.sensitiveTools, decoded. `remove_index` (the indexing families' index delete, ADR-0030 I1) needs a
# human's approval like `delete_file` does.
assert policy["toolkit_security"]["sensitive_tools"] == {"*": ["delete_file", "remove_index"]}, policy

# The environment: only what the Rust worker reads.
env = {e["name"]: e.get("value") for e in c["env"]}
assert env["SSL_CERT_FILE"] == "/run/elitea-runtime/trust-bundle.crt", env
assert env["ELITEA_RUST_LOG"] == "info", env
assert env["ELITEA_RUST_TRACE"] == "info", env
# The retired Python worker's SDK settings. Nothing in the Rust worker reads
# these names; present, they are dead configuration that reads as governing
# something — and ELITEA_SENSITIVE_TOOLS would look like the policy while the
# file beside it is what actually applies.
for absent in ("ELITEA_DISABLE_SYSTEM_CA", "REQUESTS_CA_BUNDLE", "ELITEA_SENSITIVE_TOOLS"):
    assert absent not in env, f"{absent} is an SDK setting the Rust worker never reads"
PY

echo "== runtime.json is complete =="
python3 - "$TMP/rust.yaml" <<'PY' && ok "runtime.json carries exactly the required fields" || bad "runtime.json fields"
import json, sys, yaml

def runtime(path):
    docs = [d for d in yaml.safe_load_all(open(path)) if d]
    cm = next(d for d in docs
              if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "elitea-worker-runtime")
    return cm["data"]["runtime.json"]

rust = runtime(sys.argv[1])

# RuntimeDeployConfig in services/elitea-worker-rust/src/config.rs is
# deny_unknown_fields and every field below is required, so a chart that stops
# writing one — or starts writing an extra — fails at Rust startup with
# `worker_serve.invalid_configuration` and nothing more specific. Held here,
# where the message can name the field.
parsed = json.loads(rust)
required = {
    "schema_version", "limits_revision", "workload_session_id", "producer_id",
    "consumer_id", "nats_url", "nats_ca_path", "nats_certificate_path",
    "nats_private_key_path", "nats_stream", "nats_consumer",
    "control_target", "output_target", "content_origin",
    "platform_origin", "ca_path", "certificate_path", "private_key_path",
    "ed25519_keyring_path", "spool_root", "spool_key_path",
    "agent_checkpoint_connection_path", "limits",
}
assert set(parsed) == required, (
    f"missing {sorted(required - set(parsed))}, unexpected {sorted(set(parsed) - required)}"
)
assert parsed["schema_version"] == "elitea.runtime-deploy.v1", parsed["schema_version"]
PY

echo "== the Rust worker binds the configuration-validation route =="
# configuration.validate.v1 is dispatched on ELITEA_RT_V1_VALIDATE, which the
# runtime plane refuses to share with agent or index dispatch. The chart's one
# worker Deployment binds one stream, so serving validation from Rust is a
# release of this chart whose worker is pointed at that route. The pair is
# fixed by the command bus, and the Rust worker's own pair check
# (valid_route_pair) must accept exactly what is rendered here.
"$HELM" template t "$CHART" "${RENDER[@]}" \
    --set worker.runtime.natsStream=ELITEA_RT_V1_VALIDATE \
    --set worker.runtime.natsConsumer=elitea-configuration-worker-v1 \
    >"$TMP/rust-validate.yaml" 2>"$TMP/rust-validate.err" \
  && python3 - "$TMP/rust-validate.yaml" <<'PY' && ok "rust: renders bound to the validation stream and durable" || bad "rust validate route: $(tail -1 "$TMP/rust-validate.err")"
import json, sys, yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
cm = next(d for d in docs
          if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "elitea-worker-runtime")
runtime = json.loads(cm["data"]["runtime.json"])
assert runtime["nats_stream"] == "ELITEA_RT_V1_VALIDATE", runtime["nats_stream"]
assert runtime["nats_consumer"] == "elitea-configuration-worker-v1", runtime["nats_consumer"]
# The Rust bootstrap opens the agent-state database whatever the stream
# (src/bootstrap.rs load_agentstate_options), so the path stays.
assert runtime["agent_checkpoint_connection_path"], runtime
dep = next(d for d in docs
           if d["kind"] == "Deployment" and d["metadata"]["name"] == "elitea-worker")
c = next(x for x in dep["spec"]["template"]["spec"]["containers"] if x["name"] == "worker")
assert c["image"].startswith("ghcr.io/elitea-ng/elitea-worker-rust:"), c["image"]
PY

echo "== the guards refuse what the workers cannot run =="
# Returns 0 only when the render FAILED and failed for the stated reason. A
# refusal for some other reason is not this assertion passing — it is the chart
# refusing earlier, with the guard under test never reached.
#
# It reports nothing itself: each caller owns its own accepting site, because
# the floor requires one site per assertion and this helper runs three times.
refuses() {
  local expect="$1"; shift
  local out
  if out="$("$HELM" template t "$CHART" "${RENDER[@]}" "$@" 2>&1)"; then
    echo "    rendered instead of being refused" >&2
    return 1
  fi
  printf '%s' "$out" | grep -q "$expect" && return 0
  echo "    refused, but not for the stated reason: $(printf '%s' "$out" | tail -1)" >&2
  return 1
}

# The removed Python worker must not silently become the Rust one.
refuses 'worker.implementation=python is no longer supported' --set worker.implementation=python \
  && ok "the retired worker.implementation=python is refused" \
  || bad "the retired worker.implementation=python is not refused"
refuses 'is no longer supported' --set worker.implementation=golang \
  && ok "an unknown worker.implementation is refused" \
  || bad "an unknown worker.implementation is not refused"
"$HELM" template t "$CHART" "${RENDER[@]}" --set worker.implementation=rust >/dev/null 2>"$TMP/explicit-rust.err" \
  && ok "an explicit worker.implementation=rust still renders (old values files)" \
  || bad "worker.implementation=rust does not render: $(tail -1 "$TMP/explicit-rust.err")"

# The Rust worker validates its log levels at startup and exits; a values typo
# must not become a CrashLoopBackOff.
refuses 'worker.logLevel must be one of' --set worker.logLevel=verbose \
  && ok "an invalid worker.logLevel is refused" \
  || bad "an invalid worker.logLevel is not refused"

refuses 'worker.traceLevel must be one of' --set worker.traceLevel=loud \
  && ok "an invalid worker.traceLevel is refused" \
  || bad "an invalid worker.traceLevel is not refused"

"$HELM" template t "$CHART" "${RENDER[@]}" --set-string worker.runtime.publicBaseUrl=https://elitea.example.com >"$TMP/public.yaml" 2>/dev/null
python3 - "$TMP/public.yaml" <<'PY' && ok "rust: worker.runtime.publicBaseUrl reaches runtime.json as public_base_url, beside the untouched platform_origin" || bad "publicBaseUrl rendering"
import json, sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
cm = next(d for d in docs if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "elitea-worker-runtime")
runtime = json.loads(cm["data"]["runtime.json"])
assert runtime["public_base_url"] == "https://elitea.example.com", runtime
assert runtime["platform_origin"] == "https://elitea-platform-edge", runtime
PY

echo "== the worker rolls after elitea-main (Argo CD sync waves) =="
python3 - "$TMP/rust.yaml" <<'PY' && ok "worker Deployment in a later wave than elitea-main; what it mounts is not later" || bad "worker rollout wave"
import sys, yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
def wave(d):
    return int(((d.get("metadata") or {}).get("annotations") or {}).get("argocd.argoproj.io/sync-wave", "0"))
def find(kind, name):
    return next(d for d in docs if d["kind"] == kind and d["metadata"]["name"] == name)

worker = find("Deployment", "elitea-worker")
main = find("Deployment", "elitea-main")
# elitea-main verifies the claim a new worker names on the artifact object API;
# a new worker must not run against an older elitea-main (PR #1293).
assert wave(worker) > wave(main), (wave(worker), wave(main))

# A Deployment ahead of what it mounts would never become ready: every
# ConfigMap and Secret the worker mounts must be in its wave or earlier.
spec = worker["spec"]["template"]["spec"]
mounted = set()
for v in spec.get("volumes", []):
    if "configMap" in v:
        mounted.add(("ConfigMap", v["configMap"]["name"]))
    if "secret" in v:
        mounted.add(("Secret", v["secret"]["secretName"]))
    for src in (v.get("projected") or {}).get("sources", []):
        if "configMap" in src:
            mounted.add(("ConfigMap", src["configMap"]["name"]))
        if "secret" in src:
            mounted.add(("Secret", src["secret"]["name"]))
later = sorted(f"{k}/{n}" for k, n in mounted
               for d in docs if d["kind"] == k and d["metadata"]["name"] == n and wave(d) > wave(worker))
assert not later, later
PY

echo
RAN=$((PASS+FAIL))
echo "worker render assertions: ${RAN} ran, ${PASS} passed, ${FAIL} failed"
# -ne, not -lt: too low means an assertion stopped running, too high means a
# site ran more than once and the floor no longer describes this file.
if [ "$RAN" -ne "$EXPECTED_ASSERTIONS" ]; then
  echo "FAIL: ${RAN} assertion(s) ran, and this file holds ${EXPECTED_ASSERTIONS} assertion site(s)." >&2
  exit 1
fi
[ "$FAIL" -eq 0 ]
