#!/usr/bin/env bash
# render-worker-extractor.sh — the document extraction sidecar in the worker
# pod (ADR-0030 I1: elitea-doc-extractor).
#
# The render-indexing-runtime.sh shape: every assertion reads the RENDERED
# manifest, and every refusal case is the complete install minus exactly one
# thing.
#
# What is asserted, and why it would fail silently otherwise:
#   * extractor off (the default) renders no second container and writes no
#     `extractor_socket`: the worker keeps reporting PDF/Office documents
#     `unsupported_type`, as before the sidecar existed;
#   * extractor on renders the container with its image, its socket env, the
#     caps from values, the peer uid (the worker's runAsUser unless listed),
#     a concurrency not below the worker's document concurrency, probes whose
#     timeoutSeconds exceed the probe's own 2 s budget, a read-only root
#     filesystem and no capabilities; BOTH
#     containers mount the same memory-backed emptyDir; and runtime.json's
#     `extractor_socket` names the socket inside that directory — a socket
#     the worker dials in a directory only the sidecar writes to would fail
#     every document `extract_failed` with every pod Ready;
#   * the default memory limit is at least maxInputBytes × concurrency × 6 × 2
#     (Config::memory_limit_for_caps: 2400 MiB at the defaults), so the
#     sidecar's memory fit does NOT lower the 50 MiB cap at the default
#     concurrency — a round 2Gi would, quietly, to ~44 MiB;
#   * the sidecar has a small memory-backed /tmp of its own (xberg may need
#     temp files on a read-only root);
#   * peerUids that do NOT include the sidecar's own runAsUser render as
#     given, and the probes stay the binary's own healthcheck: the service
#     serves Capabilities to its own uid whatever peerUids lists, so the
#     liveness probe cannot be configured into failing;
#   * the sidecar has no Service and no NetworkPolicy of its own: it listens on
#     no port;
#   * the retired Python worker and python indexing runtime,
#     refuse the sidecar at render time.
#
# Usage: deploy/helm/tests/render-worker-extractor.sh
# Needs: helm, python3 + PyYAML. No cluster, no network.
set -euo pipefail

DIR="$(cd "$(dirname "$0")/../.." && pwd)"
REPO_ROOT="$(cd "$DIR/.." && pwd)"
# shellcheck source=../../../scripts/lib/assertion-floor.sh
. "${REPO_ROOT}/scripts/lib/assertion-floor.sh"

HELM="${HELM:-helm}"
CHART="$DIR/helm/elitea"
PASS=0
FAIL=0

ASSERTION_SITE_PATTERN='(^|[^[:alnum:]_])ok[[:space:]]+"'
EXPECTED_ASSERTIONS="$(derive_assertion_floor "$0" "$ASSERTION_SITE_PATTERN")"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

ok()  { PASS=$((PASS+1)); echo "  ok: $1"; }
bad() { FAIL=$((FAIL+1)); echo "  FAIL: $1" >&2; }

RENDER=(
  -f "$CHART/values-standalone.yaml" -f "$CHART/../tests/fixtures/code-material-values.yaml"
  --set worker.enabled=true
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://ci-render-only.example.invalid/llm/v1
  --set-string llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true
)
# A Rust-indexing deployment (render-indexing-runtime.sh explains the replica
# count).
RUST=(
  --set worker.implementation=rust --set worker.indexingRuntime=rust
  --set vector.enabled=true --set main.autoscaling.maxReplicas=3
  --set vector.qdrant.apiKeySecret.name=elitea-qdrant-api-key
)

"$HELM" template t "$CHART" "${RENDER[@]}" "${RUST[@]}" >"$TMP/off.yaml" 2>"$TMP/off.err" \
  && ok "rust indexing without the extractor renders" \
  || bad "rust indexing does not render: $(tail -1 "$TMP/off.err")"
"$HELM" template t "$CHART" "${RENDER[@]}" "${RUST[@]}" --set worker.extractor.enabled=true \
  >"$TMP/on.yaml" 2>"$TMP/on.err" \
  && ok "rust indexing with worker.extractor.enabled renders" \
  || bad "the extractor does not render: $(tail -1 "$TMP/on.err")"

echo "== extractor off: nothing of it renders =="
python3 - "$TMP/off.yaml" <<'PY' && ok "off: no doc-extractor container, no extractor_socket, no socket volume" || bad "off: something of the extractor rendered"
import json, sys, yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
dep = next(d for d in docs if d["kind"] == "Deployment" and d["metadata"]["name"] == "elitea-worker")
cm = next(d for d in docs if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "elitea-worker-runtime")
names = [c["name"] for c in dep["spec"]["template"]["spec"]["containers"]]
assert names == ["worker"], names
volumes = [v["name"] for v in dep["spec"]["template"]["spec"]["volumes"]]
assert "extractor-socket" not in volumes, volumes
runtime = json.loads(cm["data"]["runtime.json"])
assert "extractor_socket" not in runtime, sorted(runtime)
PY

echo "== extractor on: the sidecar, the shared socket and the worker's path agree =="
python3 - "$TMP/on.yaml" <<'PY' && ok "on: sidecar container, shared memory emptyDir in both containers, runtime.json names the socket inside it" || bad "on: the sidecar shape is wrong"
import json, sys, yaml

docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
dep = next(d for d in docs if d["kind"] == "Deployment" and d["metadata"]["name"] == "elitea-worker")
cm = next(d for d in docs if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "elitea-worker-runtime")
spec = dep["spec"]["template"]["spec"]
containers = {c["name"]: c for c in spec["containers"]}
assert set(containers) == {"worker", "doc-extractor"}, sorted(containers)
x = containers["doc-extractor"]
assert x["image"].startswith("ghcr.io/elitea-ng/elitea-doc-extractor:"), x["image"]
assert "command" not in x and "args" not in x, "the image's own ENTRYPOINT/CMD (serve) is the sidecar"
env = {e["name"]: e["value"] for e in x["env"]}
assert env["ELITEA_EXTRACTOR_SOCKET"] == "/run/elitea-extract/extractor.sock", env
assert env["ELITEA_EXTRACTOR_MAX_INPUT_BYTES"] == "52428800", env
assert env["ELITEA_EXTRACTOR_MAX_PAGES"] == "1000", env
assert env["ELITEA_EXTRACTOR_TIMEOUT_MILLIS"] == "120000", env
assert env["ELITEA_EXTRACTOR_CONCURRENCY"] == "4", env
assert int(env["ELITEA_EXTRACTOR_CONCURRENCY"]) >= 4, "not below the worker's document_concurrency (IndexRunLimits)"
assert env["ELITEA_EXTRACTOR_SLOT_WAIT_MILLIS"] == "600000", env
assert int(env["ELITEA_EXTRACTOR_SLOT_WAIT_MILLIS"]) > int(env["ELITEA_EXTRACTOR_TIMEOUT_MILLIS"]), "the slot wait is a separate, generous bound"
sc = x["securityContext"]
# The peer check: the default is the worker container's own uid, so the
# two containers of the pod agree without anyone listing a number.
worker_sc = containers["worker"]["securityContext"]
assert env["ELITEA_EXTRACTOR_PEER_UIDS"] == str(worker_sc["runAsUser"]) == "10001", (env["ELITEA_EXTRACTOR_PEER_UIDS"], worker_sc)
for probe in ("livenessProbe", "readinessProbe"):
    p = x[probe]
    assert p["timeoutSeconds"] >= 2 + 2, f"{probe}: the healthcheck's own budget is 2 s; {p}"
    assert p["timeoutSeconds"] == 5 and p["periodSeconds"] == 10 and p["failureThreshold"] == 3, p
    assert p["timeoutSeconds"] < p["periodSeconds"], p
assert sc["readOnlyRootFilesystem"] is True and sc["allowPrivilegeEscalation"] is False, sc
assert sc["capabilities"] == {"drop": ["ALL"]}, sc
assert sc["runAsUser"] == 10001 and sc["runAsNonRoot"] is True, sc
assert x["livenessProbe"]["exec"]["command"] == ["/usr/local/bin/elitea-doc-extractor", "healthcheck"], x["livenessProbe"]
assert x["readinessProbe"]["exec"]["command"] == ["/usr/local/bin/elitea-doc-extractor", "healthcheck"], x["readinessProbe"]
assert "ports" not in x, "the sidecar listens on no port"

# The memory limit against the caps: fit_memory lowers the input cap to
# limit / 2 / concurrency / 6, so the limit must be at least
# cap × concurrency × 6 × 2 for the configured cap to stand (the service's
# Config::memory_limit_for_caps; its unit test pins the same arithmetic).
def mebibytes(quantity):
    units = {"Mi": 1, "Gi": 1024}
    for suffix, factor in units.items():
        if quantity.endswith(suffix):
            return int(quantity[: -len(suffix)]) * factor
    raise AssertionError(f"unexpected memory quantity {quantity!r}")
limit_mib = mebibytes(x["resources"]["limits"]["memory"])
needed_mib = int(env["ELITEA_EXTRACTOR_MAX_INPUT_BYTES"]) * int(env["ELITEA_EXTRACTOR_CONCURRENCY"]) * 6 * 2 // (1024 * 1024)
assert needed_mib == 2400, needed_mib
assert x["resources"]["limits"]["memory"] == "2560Mi", x["resources"]
assert limit_mib >= needed_mib, f"the default limit {limit_mib} MiB would make fit_memory lower the {env['ELITEA_EXTRACTOR_MAX_INPUT_BYTES']}-byte cap (needs {needed_mib} MiB)"
assert mebibytes("2Gi") < needed_mib, "the figure this assertion exists for: a round 2Gi does not fit"

volume = next(v for v in spec["volumes"] if v["name"] == "extractor-socket")
assert volume["emptyDir"] == {"medium": "Memory", "sizeLimit": "1Mi"}, volume
# The sidecar's own small memory-backed /tmp, and not the worker's tmp.
tmp_volume = next(v for v in spec["volumes"] if v["name"] == "extractor-tmp")
assert tmp_volume["emptyDir"] == {"medium": "Memory", "sizeLimit": "64Mi"}, tmp_volume
tmp_mount = next(m for m in x["volumeMounts"] if m["name"] == "extractor-tmp")
assert tmp_mount["mountPath"] == "/tmp", tmp_mount
assert all(m["name"] != "extractor-tmp" for m in containers["worker"]["volumeMounts"]), "the worker does not share the sidecar's /tmp"
worker_mount = next(m for m in containers["worker"]["volumeMounts"] if m["name"] == "extractor-socket")
sidecar_mount = next(m for m in x["volumeMounts"] if m["name"] == "extractor-socket")
assert worker_mount["mountPath"] == sidecar_mount["mountPath"] == "/run/elitea-extract", (worker_mount, sidecar_mount)
assert "readOnly" not in worker_mount or worker_mount["readOnly"] is False, "connecting to a socket needs write access to it"

runtime = json.loads(cm["data"]["runtime.json"])
assert runtime["extractor_socket"] == "/run/elitea-extract/extractor.sock", runtime.get("extractor_socket")
assert runtime["extractor_socket"].startswith(worker_mount["mountPath"] + "/"), "the socket must be inside the shared directory"

# No Service, NetworkPolicy or other object of its own.
others = [d for d in docs if "doc-extractor" in d["metadata"]["name"] or "extractor" in d["metadata"]["name"]]
assert others == [], [(d["kind"], d["metadata"]["name"]) for d in others]
PY

echo "== peer uids listed in values =="
"$HELM" template t "$CHART" "${RENDER[@]}" "${RUST[@]}" --set worker.extractor.enabled=true \
  --set 'worker.extractor.peerUids={10001,20002}' >"$TMP/peers.yaml" 2>"$TMP/peers.err" \
  && python3 - "$TMP/peers.yaml" <<'PY' && ok "listed peerUids render comma-separated" || bad "listed peerUids: $(tail -1 "$TMP/peers.err")"
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
dep = next(d for d in docs if d["kind"] == "Deployment" and d["metadata"]["name"] == "elitea-worker")
x = next(c for c in dep["spec"]["template"]["spec"]["containers"] if c["name"] == "doc-extractor")
env = {e["name"]: e["value"] for e in x["env"]}
assert env["ELITEA_EXTRACTOR_PEER_UIDS"] == "10001,20002", env
PY

echo "== peer uids that do not include the sidecar's own uid =="
# A worker image that runs as another uid: Extract is served to 20002 only,
# and the probes are untouched — the service always serves Capabilities to
# its own uid (runAsUser), so the health probe cannot be configured into
# failing. The render must show the uids as given, the sidecar still running
# as the chart's runAsUser, and the probe commands unchanged.
"$HELM" template t "$CHART" "${RENDER[@]}" "${RUST[@]}" --set worker.extractor.enabled=true \
  --set 'worker.extractor.peerUids={20002}' >"$TMP/other-peer.yaml" 2>"$TMP/other-peer.err" \
  && python3 - "$TMP/other-peer.yaml" <<'PY' && ok "peerUids without runAsUser render as given and the probes stay the binary's own healthcheck" || bad "peerUids without runAsUser: $(tail -1 "$TMP/other-peer.err")"
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
dep = next(d for d in docs if d["kind"] == "Deployment" and d["metadata"]["name"] == "elitea-worker")
x = next(c for c in dep["spec"]["template"]["spec"]["containers"] if c["name"] == "doc-extractor")
env = {e["name"]: e["value"] for e in x["env"]}
assert env["ELITEA_EXTRACTOR_PEER_UIDS"] == "20002", env
assert x["securityContext"]["runAsUser"] == 10001, x["securityContext"]
assert "10001" not in env["ELITEA_EXTRACTOR_PEER_UIDS"].split(","), "the sidecar's own uid is deliberately NOT a configured peer here"
for probe in ("livenessProbe", "readinessProbe"):
    assert x[probe]["exec"]["command"] == ["/usr/local/bin/elitea-doc-extractor", "healthcheck"], x[probe]
PY

echo "== refusals =="
# refused <reason> <extra args...>: 0 when the chart refuses for that reason.
refused() {
  local reason="$1"
  shift
  if "$HELM" template t "$CHART" "${RENDER[@]}" "$@" >"$TMP/refused.yaml" 2>"$TMP/refused.err"; then
    return 1
  fi
  grep -qF "$reason" "$TMP/refused.err"
}
# Unrolled, like render-worker.sh: the floor counts accepting sites.
refused "is no longer supported" --set worker.implementation=python --set worker.extractor.enabled=true \
  && ok "the retired Python worker is refused, extractor or not" \
  || bad "the retired Python worker with the extractor: $(tail -1 "$TMP/refused.err")"
refused "is no longer supported" --set worker.implementation=rust --set worker.indexingRuntime=python --set worker.extractor.enabled=true \
  && ok "the retired python indexing runtime is refused, extractor or not" \
  || bad "python indexing with the extractor: $(tail -1 "$TMP/refused.err")"

echo
RAN=$((PASS+FAIL))
echo "worker extractor render assertions: ${RAN} ran, ${PASS} passed, ${FAIL} failed"
if [ "$RAN" -ne "$EXPECTED_ASSERTIONS" ]; then
  echo "FAIL: ${RAN} assertion(s) ran, and this file holds ${EXPECTED_ASSERTIONS} assertion site(s)." >&2
  exit 1
fi
[ "$FAIL" -eq 0 ]
