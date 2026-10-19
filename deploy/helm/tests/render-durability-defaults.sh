#!/usr/bin/env bash
# render-durability-defaults.sh — durability and the Code runtime are ON by
# default wherever the agent runtime runs (PLAN §8.3, the defaults contract).
#
# The chart is rendered with the agent runtime and the worker on and NOTHING
# but deployment material (fixtures/code-material-values.yaml): no recovery
# switch, no Code consumer setting, no sandbox profile. Every durability and
# Code property must then be on, with the canonical shape the standalone
# compose stack runs (deploy/scripts/code-material.py holds the same table):
#
#   * recovery is not a setting: the removed worker values are absent from
#     values.yaml, runtime.json never carries a recovery key, and
#     agent_checkpoint_connection_path is always there;
#   * the worker's four Code backends, each with dependency preparation, with
#     the CONTRACT targets, audiences, policies, timeouts and platform clients;
#   * Main: original-Code owner recovery (no enable bit, PLAN §8.7 X1; three
#     execution supervisors), the
#     Code platform broker, Code debug artifacts and the Code workspace, and
#     the seven sandbox audiences;
#   * the Kubernetes supervisor with its seven chart-rendered profiles, the
#     execution and preparation namespace boundaries, and the runner warmup;
#   * FU-221 keepalives: pgbouncer (rendered on) and dbInit's role defaults.
#
# And both directions of "never silently off": removing any one piece of
# material FAILS the render with a message naming the value, and setting a
# removed recovery value fails it too. A guard that only checks the happy path
# cannot tell a default that is on from a template that renders anything.
#
# Run: deploy/helm/tests/render-durability-defaults.sh
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
# assertion holds exactly one accepting arm.
ASSERTION_SITE_PATTERN='(^|[^[:alnum:]_])ok[[:space:]]+"'
EXPECTED_ASSERTIONS="$(derive_assertion_floor "$0" "$ASSERTION_SITE_PATTERN")"

for tool in "$HELM" python3; do
  command -v "$tool" >/dev/null 2>&1 || { echo "render-durability-defaults.sh needs $tool on PATH" >&2; exit 2; }
done

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

ok()  { PASS=$((PASS+1)); echo "  ok: $1"; }
bad() { FAIL=$((FAIL+1)); echo "  FAIL: $1" >&2; }

# The agent runtime (values-standalone.yaml: main.runtime + agent dispatch,
# its auth and material), the worker, and the Code material. Indexing is off
# only because it needs a vector store this test is not about; the gateway
# postures are render-only (`.invalid` never resolves).
RENDER=(
  -f "$CHART/values-standalone.yaml" -f "$FIXTURE"
  --namespace platform
  --set worker.enabled=true
  --set main.runtime.indexIngestDispatch.enabled=false --set-string main.runtime.indexIngestDispatch.commandStream=
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://ci-render-only.example.invalid/llm/v1
  --set-string llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true
)
# The pooler's three prerequisites, render-only.
PGBOUNCER=(--set pgbouncer.enabled=true --set pgbouncer.postgresHost=pg.example.invalid
  --set pgbouncer.database=elitea --set pgbouncer.credentialsSecretName=pgbouncer-credentials)

# assert <name> <python body> — run a python check over $TMP/<name>.yaml with
# `docs` (every document), `by(kind, name)` and `runtime` (runtime.json) bound.
assert() {
  local name="$1" body="$2"
  python3 - "$TMP/$name.yaml" "$CHART/values.yaml" <<PY
import json, sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
chart_values = yaml.safe_load(open(sys.argv[2]))
def by(kind, name):
    found = [d for d in docs if d["kind"] == kind and d["metadata"]["name"] == name]
    assert len(found) == 1, (kind, name, len(found))
    return found[0]
def main_env():
    # The ConfigMap elitea-main reads through envFrom.
    dep = by("Deployment", "elitea-main")
    name = dep["spec"]["template"]["spec"]["containers"][0]["envFrom"][0]["configMapRef"]["name"]
    return by("ConfigMap", name)["data"]
runtime = json.loads(by("ConfigMap", "elitea-worker-runtime")["data"]["runtime.json"]) if any(
    d["kind"] == "ConfigMap" and d["metadata"]["name"] == "elitea-worker-runtime" for d in docs) else None
$body
PY
}

# refuses <expected fragment> [helm args...] — the render FAILS, naming it.
refuses() {
  local expected="$1" output
  shift
  if output="$("$HELM" template t "$CHART" "${RENDER[@]}" "$@" 2>&1)"; then
    return 1
  fi
  grep -qF -- "$expected" <<<"$output"
}

echo "== the agent runtime with only its material renders =="
"$HELM" template t "$CHART" "${RENDER[@]}" >"$TMP/full.yaml" 2>"$TMP/full.err" \
  && ok "the chart renders with the runtime, the worker and only the material fixture" \
  || bad "the default agent runtime does not render: $(tail -2 "$TMP/full.err" | tr '\n' ' ')"
"$HELM" template t "$CHART" "${RENDER[@]}" "${PGBOUNCER[@]}" >"$TMP/pgbouncer.yaml" 2>"$TMP/pgbouncer.err" \
  && ok "the same render with the pooler on" \
  || bad "the default agent runtime with pgbouncer does not render: $(tail -2 "$TMP/pgbouncer.err" | tr '\n' ' ')"

echo "== recovery is not a setting =="
assert full '
worker_runtime = chart_values["worker"]["runtime"]
for key in ("agentNodeRecovery", "agentModelCheckpointRecovery"):
    assert key not in worker_runtime, f"values.yaml still has worker.runtime.{key}"
for key in ("agent_node_recovery", "agent_model_checkpoint_recovery"):
    assert key not in runtime, f"runtime.json carries {key}"
' && ok "the removed recovery values are gone from values.yaml and runtime.json" \
  || bad "a recovery switch survives in values.yaml or runtime.json"
assert full '
assert runtime["agent_checkpoint_connection_path"] == "/run/elitea-runtime/agent-checkpoint-connection", runtime
dep = by("Deployment", "elitea-worker")
envs = [e for c in dep["spec"]["template"]["spec"]["initContainers"] for e in c.get("env", [])]
assert "agent-checkpoint-connection" in " ".join(e.get("value", "") for e in envs).split()
' && ok "agent_checkpoint_connection_path is present and its material is required" \
  || bad "the worker has no checkpoint connection for the always-on journal"

echo "== the worker: four Code backends, each with preparation (CONTRACT) =="
assert full '
DENO = "sha256:" + "1" * 64
RUST = "sha256:" + "2" * 64
NATIVE = {"os": "linux", "arch": "amd64", "abi": "gnu"}
def platform(target, audience, digest, policy, timeout):
    return {"target": target, "audience": audience, "image_digest": digest, "policy_revision": policy,
            "timeout_seconds": timeout, "max_calls": 32, "max_total_bytes": 1048576}
want = [
    {"language": "python", "target": "elitea-sandbox-deno:9446", "audience": "dns:elitea-sandbox-deno",
     "image_digest": DENO, "policy_revision": "deno-v1", "timeout_seconds": 60,
     "platform_client": platform("elitea-sandbox-deno:9446", "dns:elitea-sandbox-deno", DENO, "deno-v1", 60),
     "preparation": {"target": "elitea-sandbox-preparation:9448", "audience": "dns:elitea-sandbox-preparation",
                     "image_digest": DENO, "policy_revision": "python-preparation-v1", "timeout_seconds": 120}},
    {"language": "javascript", "target": "elitea-sandbox-deno-native:9450", "audience": "dns:elitea-sandbox-deno-native",
     "image_digest": DENO, "policy_revision": "deno-v1", "timeout_seconds": 60,
     "platform_client": platform("elitea-sandbox-deno-native:9450", "dns:elitea-sandbox-deno-native", DENO, "deno-v1", 60),
     "preparation": {"native_platform": NATIVE, "target": "elitea-sandbox-javascript-preparation:9449",
                     "audience": "dns:elitea-sandbox-javascript-preparation", "image_digest": DENO,
                     "policy_revision": "deno-preparation-v1", "timeout_seconds": 120}},
    {"language": "typescript", "target": "elitea-sandbox-deno-native:9450", "audience": "dns:elitea-sandbox-deno-native",
     "image_digest": DENO, "policy_revision": "deno-v1", "timeout_seconds": 60,
     "platform_client": platform("elitea-sandbox-deno-native:9450", "dns:elitea-sandbox-deno-native", DENO, "deno-v1", 60),
     "preparation": {"native_platform": NATIVE, "target": "elitea-sandbox-typescript-preparation:9451",
                     "audience": "dns:elitea-sandbox-typescript-preparation", "image_digest": DENO,
                     "policy_revision": "deno-preparation-v1", "timeout_seconds": 120}},
    {"language": "rust", "target": "elitea-sandbox-rust:9447", "audience": "dns:elitea-sandbox-rust",
     "image_digest": RUST, "policy_revision": "rust-v1", "timeout_seconds": 180,
     "preparation": {"native_platform": NATIVE, "target": "elitea-sandbox-rust-preparation:9452",
                     "audience": "dns:elitea-sandbox-rust-preparation", "image_digest": RUST,
                     "policy_revision": "cargo-preparation-v1", "timeout_seconds": 300}},
]
assert runtime["sandbox_runtimes"] == want, json.dumps(runtime["sandbox_runtimes"], indent=1)
' && ok "sandbox_runtimes: python, javascript, typescript, rust, each with preparation, as the CONTRACT writes them" \
  || bad "the worker's sandbox_runtimes differ from the CONTRACT"

echo "== Main: the Code consumers and the seven audiences =="
assert full '
env = main_env()
for name in ("PLATFORM", "DEBUG_ARTIFACTS", "WORKSPACE"):
    assert env.get(f"ELITEA_RUNTIME_CODE_{name}_ENABLED") == "true", name
# Owner recovery has no enable bit (PLAN §8.7 X1): Main refuses the variable,
# and its configuration is what turns it on.
assert "ELITEA_RUNTIME_CODE_OWNER_RECOVERY_ENABLED" not in env
assert env.get("ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG")
' && ok "Main: PLATFORM/DEBUG_ARTIFACTS/WORKSPACE_ENABLED true, owner recovery configured with no enable variable" \
  || bad "a Main Code consumer is off by default, or the removed owner enable variable is rendered"
assert full '
env = main_env()
owner = json.loads(env["ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG"])
assert owner["main_workload_identity"] == "dns:elitea-code-owner-main", owner
assert owner["supervisors"] == [
    {"audience": "dns:elitea-sandbox-deno", "https_origin": "https://elitea-sandbox-deno:9446"},
    {"audience": "dns:elitea-sandbox-deno-native", "https_origin": "https://elitea-sandbox-deno-native:9450"},
    {"audience": "dns:elitea-sandbox-rust", "https_origin": "https://elitea-sandbox-rust:9447"},
], owner
assert (owner["certificate_chain_path"], owner["private_key_path"]) == (
    "/run/elitea-runtime/code-owner-client.crt", "/run/elitea-runtime/code-owner-client.key"), owner
workspace = json.loads(env["ELITEA_RUNTIME_CODE_WORKSPACE_CONFIG"])
assert workspace == {"revision": 1, "repository_capabilities": ["github"], "egress_allowlist": ["api.github.com:443"],
                     "policy": {"revision": 1, "max_files": 1024, "max_file_bytes": 1048576, "max_total_bytes": 16777216,
                                "max_manifest_bytes": 524288, "max_path_bytes": 255, "max_depth": 16,
                                "max_projections": 64, "max_acquisition_seconds": 300}}, workspace
platform = json.loads(env["ELITEA_RUNTIME_CODE_PLATFORM_CONFIG"])
assert platform == {"revision": 1, "content_keys_file": "/run/elitea-runtime/code-platform-content-keys.json",
                    "broker_policies": [{"revision": 1, "max_calls": 32, "max_total_bytes": 1048576}]}, platform
assert env["ELITEA_CODE_DEBUG_AGENTSTATE_DSN_FILE"] == "/run/elitea-runtime/agent-checkpoint-connection"
' && ok "Main owner recovery (three execution supervisors), workspace, platform and debug carry the canonical configuration" \
  || bad "a Main Code consumer configuration differs from the CONTRACT"
assert full '
audiences = main_env()["ELITEA_RUNTIME_SANDBOX_AUDIENCES"].split(",")
assert audiences == ["dns:elitea-sandbox-" + n for n in ("deno", "rust", "deno-native", "preparation",
    "javascript-preparation", "typescript-preparation", "rust-preparation")], audiences
' && ok "ELITEA_RUNTIME_SANDBOX_AUDIENCES is the seven canonical audiences" \
  || bad "Main does not sign for the seven sandbox audiences"

echo "== the supervisor: seven chart-rendered profiles =="
assert full '
dep = by("Deployment", "elitea-sandbox-supervisor")
spec = dep["spec"]["template"]["spec"]
args = spec["containers"][0]["args"]
names = ["deno", "rust", "denonative", "prep", "jsprep", "tsprep", "rsprep"]
assert args == [x for n in names for x in ("--config", f"/run/elitea-sandbox-profiles/{n}.json")], args
assert [c["name"] for c in spec["initContainers"]] == ["prepare-material", "prepare-profiles", "prepare-content-staging"]
assert spec["initContainers"][1]["args"] == ["--prepare-material", "/run/projected-profiles", "/run/elitea-sandbox-profiles"]
volumes = {v["name"]: v for v in spec["volumes"]}
assert volumes["projected-material"]["secret"]["secretName"] == "elitea-sandbox-material"
assert volumes["projected-profiles"]["configMap"]["name"] == "elitea-sandbox-profiles"
assert spec["containers"][0]["image"] == "registry.example/elitea-sandbox-supervisor@sha256:" + "3" * 64
' && ok "the supervisor Deployment starts seven profiles from the rendered ConfigMap, material from elitea-sandbox-material" \
  || bad "the supervisor Deployment is not the seven-profile default"
assert full '
configs = {k[:-5]: json.loads(v) for k, v in by("ConfigMap", "elitea-sandbox-profiles")["data"].items()}
MIB = 1048576
table = {  # name: port, service, purpose, languages, policy, runner digest, timeout, memory MiB, native MiB
    "deno": (9446, "elitea-sandbox-deno", "execution", ["python"], "deno-v1", "1", 60, 512, None),
    "rust": (9447, "elitea-sandbox-rust", "execution", ["rust"], "rust-v1", "2", 180, 2048, 1024),
    "denonative": (9450, "elitea-sandbox-deno-native", "execution", ["javascript", "typescript"], "deno-v1", "1", 60, 1024, 512),
    "prep": (9448, "elitea-sandbox-preparation", "preparation", ["python"], "python-preparation-v1", "1", 120, 512, None),
    "jsprep": (9449, "elitea-sandbox-javascript-preparation", "preparation", ["javascript"], "deno-preparation-v1", "1", 120, 1024, 512),
    "tsprep": (9451, "elitea-sandbox-typescript-preparation", "preparation", ["typescript"], "deno-preparation-v1", "1", 120, 1024, 512),
    "rsprep": (9452, "elitea-sandbox-rust-preparation", "preparation", ["rust"], "cargo-preparation-v1", "2", 300, 2048, 1024),
}
assert sorted(configs) == sorted(table), sorted(configs)
for name, (port, svc, purpose, langs, policy, digit, timeout, memory, native) in table.items():
    c = configs[name]
    execution = purpose == "execution"
    assert c["listen_address"] == f"0.0.0.0:{port}" and c["audience"] == f"dns:{svc}", (name, c)
    assert c["purpose"] == purpose and c["languages"] == langs and c["policy_revision"] == policy, (name, c)
    assert c["image_digest"] == "sha256:" + digit * 64 and c["backend"]["image"].endswith(c["image_digest"]), (name, c)
    assert c["timeout_seconds"] == timeout and c["memory_bytes"] == memory * MIB, (name, c)
    assert c["concurrency"] == (4 if execution else 1) and c["database_connections"] == (4 if execution else 2), (name, c)
    assert c["backend"]["kind"] == "kubernetes", (name, c)
    assert c["backend"]["namespace"] == ("elitea-code-execution" if execution else "elitea-code-preparation"), (name, c)
    if native:
        assert c["native_platform"] == {"os": "linux", "arch": "amd64", "abi": "gnu"}, (name, c)
        assert c["native_workspace_bytes"] == native * MIB, (name, c)
    else:
        assert "native_platform" not in c and "native_workspace_bytes" not in c, (name, c)
    assert c.get("code_owner_requester") == ("dns:elitea-code-owner-main" if execution else None), (name, c)
    assert c.get("code_platform_profile", False) == (name in ("deno", "denonative")), (name, c)
    content = c["dependency_content"]
    assert content["origin"] == "https://elitea-main:9445" and content["capacity"] == 1 and content["timeout_seconds"] == 30, (name, c)
    assert content["certificate_path"] == f"/run/elitea-sandbox/{svc}-content-client.crt", (name, c)
    assert content["staging_root"] == f"/run/elitea-sandbox-content/{name}/private", (name, c)
    assert c["certificate_path"] == f"/run/elitea-sandbox/{svc}.crt", (name, c)
    assert "preparation_network" not in c, (name, c)
staging = [v for v in by("Deployment", "elitea-sandbox-supervisor")["spec"]["template"]["spec"]["volumes"] if v["name"].startswith("content-")]
assert [(v["name"], v["emptyDir"]["sizeLimit"]) for v in staging] == [(f"content-{n}", "256Mi") for n in table], staging
' && ok "the seven rendered profiles match the canonical table, each with its own 256 MiB content staging" \
  || bad "a rendered supervisor profile differs from the CONTRACT table"
assert full '
services = {d["metadata"]["name"] for d in docs if d["kind"] == "Service"}
for svc in ("elitea-sandbox-deno", "elitea-sandbox-rust", "elitea-sandbox-deno-native", "elitea-sandbox-preparation",
            "elitea-sandbox-javascript-preparation", "elitea-sandbox-typescript-preparation", "elitea-sandbox-rust-preparation"):
    assert svc in services, svc
rule = [r for r in by("NetworkPolicy", "elitea-main-netpol")["spec"]["ingress"]
        if [p["port"] for p in r["ports"]] == [9445] and any(
            f.get("podSelector", {}).get("matchLabels", {}).get("app.kubernetes.io/component") == "sandbox-supervisor"
            for f in r["from"])]
assert len(rule) == 1, "the supervisor cannot reach the content listener"
' && ok "each profile has its Service, and the supervisor reaches Main's content listener" \
  || bad "a profile Service or the supervisor's content route is missing"

echo "== the namespace boundaries and the warmup =="
assert full '
by("Namespace", "elitea-code-execution")
deny = [d for d in docs if d["kind"] == "NetworkPolicy" and d["metadata"]["name"] == "elitea-code-deny-network"][0]
assert deny["metadata"]["namespace"] == "elitea-code-execution" and deny["spec"]["egress"] == []
' && ok "the execution namespace boundary renders (elitea-code-execution, deny-all)" \
  || bad "the execution namespace boundary is missing"
assert full '
ns = by("Namespace", "elitea-code-preparation")
policy = [d for d in docs if d["kind"] == "NetworkPolicy" and d["metadata"].get("namespace") == "elitea-code-preparation"][0]
assert policy["spec"]["egress"][1]["to"] == [{"ipBlock": {"cidr": "192.0.2.0/24"}}], policy["spec"]
' && ok "the preparation boundary renders (elitea-code-preparation, egress to the resolver CIDRs only)" \
  || bad "the preparation boundary is missing"
assert full '
pod = by("DaemonSet", "elitea-sandbox-image-warmup")["spec"]["template"]["spec"]
assert sorted(c["image"] for c in pod["containers"]) == [
    "registry.example/elitea-code-runner-rust@sha256:" + "2" * 64,
    "registry.example/elitea-code-runner@sha256:" + "1" * 64], pod["containers"]
assert pod["nodeSelector"] == {"elitea.ai/sandbox": "true"}
' && ok "the runner image warmup pulls both runner images onto the sandbox nodes" \
  || bad "the runner image warmup is missing or incomplete"

echo "== FU-221 keepalives =="
assert pgbouncer '
ini = by("ConfigMap", "t-pgbouncer-config")["data"]["pgbouncer.ini"] if any(
    d["kind"] == "ConfigMap" and d["metadata"]["name"] == "t-pgbouncer-config" for d in docs) else next(
    d["data"]["pgbouncer.ini"] for d in docs if d["kind"] == "ConfigMap" and "pgbouncer.ini" in d.get("data", {}))
settings = dict(l.split(" = ", 1) for l in ini.splitlines() if " = " in l and not l.lstrip().startswith(";"))
settings = {k.strip(): v.strip() for k, v in settings.items()}
assert (settings["tcp_keepalive"], settings["tcp_keepidle"], settings["tcp_keepintvl"], settings["tcp_keepcnt"]) == ("1", "20", "5", "3"), settings
' && ok "pgbouncer keeps TCP keepalives on both sides (1/20/5/3)" \
  || bad "pgbouncer.ini lacks the FU-221 keepalives"
assert full '
job = by("Job", "elitea-db-init")
script = job["spec"]["template"]["spec"]["containers"][0]["command"][-1]
for line in ("SET tcp_keepalives_idle = 20", "SET tcp_keepalives_interval = 5", "SET tcp_keepalives_count = 3"):
    assert line in script, line
' && ok "dbInit sets the application role's tcp_keepalives defaults (20/5/3)" \
  || bad "dbInit does not set the role keepalives"

echo "== never silently off: missing material fails the render, naming the value =="
refuses 'sandboxKubernetes.runnerImages.deno is required' --set sandboxKubernetes.runnerImages.deno= \
  && ok "no deno runner image: refused, naming sandboxKubernetes.runnerImages.deno" \
  || bad "a missing deno runner image did not fail the render by name"
refuses 'sandboxKubernetes.runnerImages.rust is required' --set-string sandboxKubernetes.runnerImages.rust=registry.example/runner:latest \
  && ok "a rust runner image by tag: refused, naming sandboxKubernetes.runnerImages.rust" \
  || bad "a mutable rust runner image did not fail the render by name"
refuses 'sandboxKubernetes.supervisor.image is required' --set sandboxKubernetes.supervisor.image= \
  && ok "no supervisor image: refused, naming sandboxKubernetes.supervisor.image" \
  || bad "a missing supervisor image did not fail the render by name"
refuses 'sandboxKubernetes.supervisor.contentStagingInitImage is required' --set sandboxKubernetes.supervisor.contentStagingInitImage= \
  && ok "no staging init image: refused, naming sandboxKubernetes.supervisor.contentStagingInitImage" \
  || bad "a missing staging init image did not fail the render by name"
refuses 'sandboxKubernetes.preparation.resolverCidrs is required' --set-json 'sandboxKubernetes.preparation.resolverCidrs=[]' \
  && ok "no resolver CIDRs: refused, naming sandboxKubernetes.preparation.resolverCidrs" \
  || bad "missing resolver CIDRs did not fail the render by name"
refuses 'sandboxKubernetes.supervisor.materialSecret is required' --set sandboxKubernetes.supervisor.materialSecret= \
  && ok "no supervisor material Secret: refused, naming sandboxKubernetes.supervisor.materialSecret" \
  || bad "a missing supervisor material Secret did not fail the render by name"
refuses 'set worker.runtime.sandboxRuntimes to the supervisors you deploy yourself' --set sandboxKubernetes.enabled=false \
  && ok "the Kubernetes sandbox off with no explicit backends: refused, not a Code-less worker" \
  || bad "turning the sandbox off left the worker without Code silently"

echo "== the removed recovery values fail the render =="
refuses 'worker.runtime.agentNodeRecovery was removed: node recovery is always on' --set worker.runtime.agentNodeRecovery=false \
  && ok "worker.runtime.agentNodeRecovery: refused as removed" \
  || bad "worker.runtime.agentNodeRecovery was accepted"
refuses 'worker.runtime.agentModelCheckpointRecovery was removed: model-checkpoint recovery is always on' --set worker.runtime.agentModelCheckpointRecovery=true \
  && ok "worker.runtime.agentModelCheckpointRecovery: refused as removed" \
  || bad "worker.runtime.agentModelCheckpointRecovery was accepted"
refuses 'main.runtime.codeOwnerRecovery.enabled was removed: Code owner recovery is always on with Agent dispatch' --set main.runtime.codeOwnerRecovery.enabled=true \
  && ok "main.runtime.codeOwnerRecovery.enabled: refused as removed" \
  || bad "main.runtime.codeOwnerRecovery.enabled was accepted"

echo "== no worker: Main still composes its Code consumers, and needs no sandbox material =="
"$HELM" template t "$CHART" -f "$CHART/values-standalone.yaml" --namespace platform \
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://ci-render-only.example.invalid/llm/v1 \
  --set-string llmGateway.egressPosture=public-unrestricted >"$TMP/standalone.yaml" 2>"$TMP/standalone.err" \
  && assert standalone '
assert runtime is None
env = main_env()
assert env["ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG"] and "ELITEA_RUNTIME_CODE_OWNER_RECOVERY_ENABLED" not in env
assert not any(d["metadata"]["name"].startswith("elitea-sandbox") or d["metadata"]["name"].startswith("elitea-code")
               for d in docs), [d["metadata"]["name"] for d in docs if "sandbox" in d["metadata"]["name"]]
' && ok "values-standalone.yaml (worker off) renders without material: Main owner recovery on, no sandbox object" \
  || bad "values-standalone.yaml without the worker: $(tail -1 "$TMP/standalone.err")"

echo
RAN=$((PASS+FAIL))
echo "durability defaults render assertions: ${RAN} ran, ${PASS} passed, ${FAIL} failed"
if [ "$RAN" -ne "$EXPECTED_ASSERTIONS" ]; then
  echo "FAIL: ${RAN} assertion(s) ran, and this file holds ${EXPECTED_ASSERTIONS} assertion site(s)." >&2
  exit 1
fi
[ "$FAIL" -eq 0 ]
