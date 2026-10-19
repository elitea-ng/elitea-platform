#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."
chart=deploy/helm/elitea
base=(-f "$chart/values-standalone.yaml" -f "$chart/../tests/fixtures/code-material-values.yaml"
  --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1
  --set llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
# The warmup renders with the Code sandbox, wherever the agent runtime runs
# (main.runtime + agent dispatch + the worker; PLAN §8.3).
worker=(--set worker.enabled=true --set main.runtime.indexIngestDispatch.enabled=false
  --set-string main.runtime.indexIngestDispatch.commandStream=)
image=registry/runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
warm=(--set sandboxImageWarmup.enabled=true
  --set-string sandboxImageWarmup.nodeSelector.sandbox=true
  --set sandboxImageWarmup.images[0].name=python
  --set-string "sandboxImageWarmup.images[0].image=$image")
helm template warm "$chart" "${base[@]}" > "$work/default.yaml"
helm template warm "$chart" "${base[@]}" \
  --set-string 'main.runtime.sandboxAudiences[0]=dns:elitea-sandbox-deno' \
  --set-string 'main.runtime.sandboxAudiences[1]=dns:elitea-sandbox-deno-native' \
  --set-string 'main.runtime.sandboxAudiences[2]=dns:elitea-sandbox-rust' \
  --set-string 'main.runtime.sandboxAudiences[3]=spiffe://elitea.test/sandbox/rust' > "$work/audiences.yaml"
python3 - "$work/default.yaml" "$work/audiences.yaml" <<'PYTEST'
import sys, yaml
# The agent runtime grants the seven canonical audiences by default; an
# explicit list is kept exactly (with the owner supervisors' audiences in it).
canonical = ",".join("dns:elitea-sandbox-" + n for n in (
    "deno", "rust", "deno-native", "preparation", "javascript-preparation",
    "typescript-preparation", "rust-preparation"))
explicit = "dns:elitea-sandbox-deno,dns:elitea-sandbox-deno-native,dns:elitea-sandbox-rust,spiffe://elitea.test/sandbox/rust"
for filename, expected in zip(sys.argv[1:], [canonical, explicit]):
    configs = [doc.get("data", {}) for doc in yaml.safe_load_all(open(filename))
               if doc and doc.get("kind") == "ConfigMap"]
    values = [config["ELITEA_RUNTIME_SANDBOX_AUDIENCES"] for config in configs
              if "ELITEA_RUNTIME_SANDBOX_AUDIENCES" in config]
    assert values == [expected], values
PYTEST
# No worker, no Code sandbox: no warmup either.
if grep -Fq 'kind: DaemonSet' "$work/default.yaml"; then
  echo "Unexpected DaemonSet without the agent runtime's worker" >&2; exit 1
fi
# The default with the worker: the two runner images, on the Code Pods' nodes.
helm template warm "$chart" "${base[@]}" "${worker[@]}" \
  --show-only templates/sandbox/image-warmup.yaml > "$work/default-warm.yaml"
python3 - "$work/default-warm.yaml" <<'PYDEFAULT'
import sys, yaml
docs = {doc["kind"]: doc for doc in yaml.safe_load_all(open(sys.argv[1]))}
pod = docs["DaemonSet"]["spec"]["template"]["spec"]
assert pod["nodeSelector"] == {"elitea.ai/sandbox": "true"}, pod["nodeSelector"]
images = {c["name"]: c["image"] for c in pod["containers"]}
assert images == {
    "runner-deno": "registry.example/elitea-code-runner@sha256:" + "1" * 64,
    "runner-rust": "registry.example/elitea-code-runner-rust@sha256:" + "2" * 64,
}, images
PYDEFAULT
helm template warm "$chart" "${base[@]}" "${worker[@]}" "${warm[@]}" \
  --show-only templates/sandbox/image-warmup.yaml > "$work/warm.yaml"
python3 - "$work/warm.yaml" <<'PY'
import sys, yaml
docs = {doc["kind"]: doc for doc in yaml.safe_load_all(open(sys.argv[1]))}
ds, network = docs["DaemonSet"], docs["NetworkPolicy"]
assert ds["kind"] == "DaemonSet"
pod = ds["spec"]["template"]["spec"]
assert pod["nodeSelector"] == {"sandbox": "true"}
assert [c["name"] for c in pod["containers"]] == ["python"]
assert pod["automountServiceAccountToken"] is False
assert not pod.get("volumes")
assert pod["securityContext"]["runAsNonRoot"]
container = pod["containers"][0]
assert container["imagePullPolicy"] == "IfNotPresent"
assert container["securityContext"]["readOnlyRootFilesystem"]
assert container["securityContext"]["allowPrivilegeEscalation"] is False
assert network["spec"]["egress"] == []
assert network["spec"]["ingress"] == []
PY
if helm template warm "$chart" "${base[@]}" "${worker[@]}" "${warm[@]}" --set-string sandboxImageWarmup.images[0].image=runtime:latest > "$work/invalid" 2>&1; then
  echo "Mutable image accepted" >&2; exit 1
fi
grep -Fq 'must use registry SHA-256 digests' "$work/invalid"
if helm template warm "$chart" "${base[@]}" "${worker[@]}" --set sandboxKubernetes.nodeSelector=null \
  --set sandboxImageWarmup.images[0].name=python --set-string "sandboxImageWarmup.images[0].image=$image" > "$work/invalid" 2>&1; then
  echo "Missing sandbox node selection accepted" >&2; exit 1
fi
grep -Fq 'sandbox nodes explicitly' "$work/invalid"
echo 'Sandbox image warmup rendering checks passed'
