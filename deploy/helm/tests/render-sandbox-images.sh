#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../../.."
chart=deploy/helm/elitea
base=(-f "$chart/values-standalone.yaml"
  --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1
  --set llmGateway.egressPosture=public-unrestricted)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
image=registry/runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
warm=(--set sandboxImageWarmup.enabled=true
  --set-string sandboxImageWarmup.nodeSelector.sandbox=true
  --set sandboxImageWarmup.images[0].name=python
  --set-string "sandboxImageWarmup.images[0].image=$image")
helm template warm "$chart" "${base[@]}" > "$work/default.yaml"
if rg -q 'kind: DaemonSet' "$work/default.yaml"; then
  echo "Unexpected default DaemonSet" >&2; exit 1
fi
helm template warm "$chart" "${base[@]}" "${warm[@]}" --show-only templates/sandbox/image-warmup.yaml > "$work/warm.yaml"
python3 - "$work/warm.yaml" <<'PY'
import sys, yaml
docs = {doc["kind"]: doc for doc in yaml.safe_load_all(open(sys.argv[1]))}
ds, network = docs["DaemonSet"], docs["NetworkPolicy"]
assert ds["kind"] == "DaemonSet"
pod = ds["spec"]["template"]["spec"]
assert pod["nodeSelector"] == {"sandbox": "true"}
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
if helm template warm "$chart" "${base[@]}" "${warm[@]}" --set-string sandboxImageWarmup.images[0].image=runtime:latest > "$work/invalid" 2>&1; then
  echo "Mutable image accepted" >&2; exit 1
fi
rg -q 'must use registry SHA-256 digests' "$work/invalid"
if helm template warm "$chart" "${base[@]}" --set sandboxImageWarmup.enabled=true --set sandboxImageWarmup.images[0].name=python --set-string "sandboxImageWarmup.images[0].image=$image" > "$work/invalid" 2>&1; then
  echo "Missing sandbox node selection accepted" >&2; exit 1
fi
rg -q 'must select sandbox nodes explicitly' "$work/invalid"
echo 'Sandbox image warmup rendering checks passed'
