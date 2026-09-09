#!/usr/bin/env bash
# render-worker.sh — the agent worker renders correctly for BOTH implementations.
#
# ── Why this file exists ─────────────────────────────────────────────────────
#
# Every other render test in this directory sets `worker.enabled=false`, and so
# does the template matrix in helm-lint.yml. The worker templates were therefore
# rendered by NOTHING: `helm lint` walks the chart, but a `{{- if
# .Values.worker.enabled }}` body it never enters is a body it never checks.
#
# That was survivable while the worker had one image and one argument list. It
# stopped being survivable when `worker.implementation` arrived (the default is
# python since #865/#866; rust is one values override away), because the two
# implementations do not accept each other's command lines:
#
#   * services/elitea-worker-rust/src/lib.rs matches an EXACT five-token slice.
#     `serve --config <path>` — the Python shape — is not a degraded mode. It is
#     `worker_cli.invalid_arguments` and an immediate exit, which reaches an
#     operator as a CrashLoopBackOff with no configuration error anywhere.
#   * The Rust worker refuses a toolkit-security document without a
#     `toolkit_security` key (src/execution/production.rs), so the second file
#     is mandatory, not optional hardening.
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
  --set runtimeRedis.enabled=true
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://ci-render-only.example.invalid/llm/v1
  --set-string llmGateway.egressPosture=public-unrestricted
)

# Unrolled on purpose, not a loop. The assertion floor counts accepting SITES
# in this file and requires each to run exactly once, so a loop that reuses one
# site for two implementations reports more results than the file holds — which
# is a floor failure, and was one when this file was first written.
render() {
  "$HELM" template t "$CHART" "${RENDER[@]}" --set "worker.implementation=$1" \
    >"$TMP/$1.yaml" 2>"$TMP/$1.err"
}

render rust   && ok "the chart renders with worker.implementation=rust" \
              || bad "rust does not render: $(tail -1 "$TMP/rust.err")"
render python && ok "the chart renders with worker.implementation=python" \
              || bad "python does not render: $(tail -1 "$TMP/python.err")"

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
# The same rule the Python worker reads from ELITEA_SENSITIVE_TOOLS, decoded.
assert policy["toolkit_security"]["sensitive_tools"] == {"*": ["delete_file"]}, policy

# The environment is the OTHER half of the branch, and asserting only the
# argument list left it uncovered: a first draft of this file passed while the
# Rust container was handed the Python env block in full.
env = {e["name"]: e.get("value") for e in c["env"]}
assert env["SSL_CERT_FILE"] == "/run/elitea-runtime/trust-bundle.crt", env
assert env["ELITEA_RUST_LOG"] == "info", env
assert env["ELITEA_RUST_TRACE"] == "info", env
# Nothing in the Rust worker reads these three names. Present, they are dead
# configuration that reads as governing something — and ELITEA_SENSITIVE_TOOLS
# would look like the policy while the file beside it is what actually applies.
for absent in ("ELITEA_DISABLE_SYSTEM_CA", "REQUESTS_CA_BUNDLE", "ELITEA_SENSITIVE_TOOLS"):
    assert absent not in env, f"{absent} is an SDK setting the Rust worker never reads"
PY

echo "== the Python worker is unchanged by any of this =="
python3 - "$TMP/python.yaml" <<'PY' && ok "python: four-token serve, env-carried policy, python image" || bad "python worker shape"
import sys, yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
dep = next(d for d in docs
           if d["kind"] == "Deployment" and d["metadata"]["name"] == "elitea-worker")
cm = next(d for d in docs
          if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "elitea-worker-runtime")
c = next(x for x in dep["spec"]["template"]["spec"]["containers"] if x["name"] == "worker")

assert c["args"] == ["serve", "--config", "/run/elitea/runtime.json"], c["args"]
assert c["image"].startswith("ghcr.io/elitea-ng/elitea-worker-python:"), c["image"]

env = {e["name"]: e.get("value") for e in c["env"]}
assert env["ELITEA_SENSITIVE_TOOLS"] == '{"*":["delete_file"]}', env
# SDK settings. The Rust worker reads neither name, so they must not follow it.
assert env["ELITEA_DISABLE_SYSTEM_CA"] == "1", env
assert "REQUESTS_CA_BUNDLE" in env, env

# The second file is Rust-only: writing it for Python would put an unread
# document in the release and invite it to drift from the env var above.
assert "toolkit-security.json" not in cm["data"], sorted(cm["data"])
PY

echo "== runtime.json is the SAME document for both, and complete =="
python3 - "$TMP/rust.yaml" "$TMP/python.yaml" <<'PY' && ok "runtime.json is byte-identical across implementations" || bad "runtime.json differs between implementations"
import json, sys, yaml

def runtime(path):
    docs = [d for d in yaml.safe_load_all(open(path)) if d]
    cm = next(d for d in docs
              if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "elitea-worker-runtime")
    return cm["data"]["runtime.json"]

rust, python = runtime(sys.argv[1]), runtime(sys.argv[2])
assert rust == python, "the two implementations were given different runtime.json"

# RuntimeDeployConfig in services/elitea-worker-rust/src/config.rs is
# deny_unknown_fields and every field below is required, so a chart that stops
# writing one — or starts writing an extra — fails at Rust startup with
# `worker_serve.invalid_configuration` and nothing more specific. Held here,
# where the message can name the field.
parsed = json.loads(rust)
required = {
    "schema_version", "limits_revision", "workload_session_id", "producer_id",
    "consumer_id", "redis_url", "redis_password_path", "redis_stream",
    "redis_group", "control_target", "output_target", "content_origin",
    "platform_origin", "ca_path", "certificate_path", "private_key_path",
    "ed25519_keyring_path", "spool_root", "spool_key_path",
    "agent_checkpoint_connection_path", "limits",
}
assert set(parsed) == required, (
    f"missing {sorted(required - set(parsed))}, unexpected {sorted(set(parsed) - required)}"
)
assert parsed["schema_version"] == "elitea.runtime-deploy.v1", parsed["schema_version"]
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

# A typo must not silently render the Python shape into the Rust image.
refuses 'worker.implementation must be' --set worker.implementation=golang \
  && ok "an unknown worker.implementation is refused" \
  || bad "an unknown worker.implementation is not refused"

# The Rust worker validates its log levels at startup and exits; a values typo
# must not become a CrashLoopBackOff.
refuses 'worker.logLevel must be one of' --set worker.implementation=rust --set worker.logLevel=verbose \
  && ok "an invalid worker.logLevel is refused" \
  || bad "an invalid worker.logLevel is not refused"

refuses 'worker.traceLevel must be one of' --set worker.implementation=rust --set worker.traceLevel=loud \
  && ok "an invalid worker.traceLevel is refused" \
  || bad "an invalid worker.traceLevel is not refused"

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
