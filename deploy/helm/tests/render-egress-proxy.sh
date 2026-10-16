#!/usr/bin/env bash
# render-egress-proxy.sh — elitea-egress-proxy (ADR-0034 decision 9).
#
# Every assertion reads the RENDERED manifest. Every refusal case is the
# complete install minus exactly one thing.
#
# What is asserted, and why it would fail silently otherwise:
#   * egressProxy off (the default) renders nothing of it;
#   * on, the Deployment runs 2 replicas, non-root, read-only root, all
#     capabilities dropped, no service-account token, with anti-affinity and a
#     zone spread, and a PDB keeps one up;
#   * the proxy env names the mounted key directory, and the deny list the
#     proxy enforces itself equals the ranges the NetworkPolicy excepts;
#   * the NetworkPolicy admits ONLY workspace-run pods of the execution
#     namespace on the proxy port, ONLY adminFrom on the admin port, and
#     allows egress to the public internet minus the deny ranges, plus DNS;
#   * the Service exposes the proxy port only;
#   * the chart-made key Secret holds the PEM keys, and existingSecret
#     replaces it;
#   * the half-configured shapes are refused.
#
# Usage: deploy/helm/tests/render-egress-proxy.sh
# Needs: helm, python3 with PyYAML. No cluster, no network.
set -euo pipefail
cd "$(dirname "$0")/../../.."

chart=deploy/helm/elitea
fixture=deploy/helm/tests/fixtures/egress-proxy-values.yaml
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
base=(-f "$chart/values-standalone.yaml" --namespace platform
  --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1
  --set llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true)

failures=0
note() { printf '  %s\n' "$1"; }
fail() { printf 'FAIL: %s\n' "$1" >&2; failures=$((failures + 1)); }

echo "== egressProxy off (the default)"
helm template t "$chart" "${base[@]}" > "$work/off.yaml"
if grep -v '^[[:space:]]*#' "$work/off.yaml" | grep -q 'elitea-egress-proxy\|ELITEA_EGRESS_PROXY_'; then
  fail "egressProxy is off, but something of it rendered"
else
  note "nothing of elitea-egress-proxy renders"
fi

echo "== egressProxy on"
helm template t "$chart" "${base[@]}" -f "$fixture" > "$work/on.yaml"
helm template t "$chart" "${base[@]}" -f "$fixture" \
  --set egressProxy.publicKeys.keys=null --set egressProxy.publicKeys.existingSecret=operator-keys > "$work/existing.yaml"

if python3 - "$work/on.yaml" "$work/existing.yaml" <<'PY'
import sys
import yaml

def docs(path):
    return [d for d in yaml.safe_load_all(open(path)) if d]

def by(docs, kind, name):
    found = [d for d in docs if d["kind"] == kind and d["metadata"]["name"] == name]
    assert len(found) == 1, f"want one {kind}/{name}, found {len(found)}"
    return found[0]

on = docs(sys.argv[1])
existing = docs(sys.argv[2])
selector = {"app.kubernetes.io/name": "elitea-egress-proxy", "app.kubernetes.io/instance": "t"}

dep = by(on, "Deployment", "elitea-egress-proxy")
spec = dep["spec"]
pod = spec["template"]["spec"]
c = pod["containers"][0]
assert spec["replicas"] == 2, spec["replicas"]
assert spec["selector"]["matchLabels"] == selector
assert spec["strategy"]["rollingUpdate"]["maxUnavailable"] == 0
assert pod["automountServiceAccountToken"] is False
assert pod["securityContext"]["runAsNonRoot"] is True and pod["securityContext"]["runAsUser"] == 65532
assert pod["securityContext"]["seccompProfile"]["type"] == "RuntimeDefault"
sc = c["securityContext"]
assert sc["readOnlyRootFilesystem"] is True and sc["allowPrivilegeEscalation"] is False
assert sc["capabilities"]["drop"] == ["ALL"]
anti = pod["affinity"]["podAntiAffinity"]["preferredDuringSchedulingIgnoredDuringExecution"][0]["podAffinityTerm"]
assert anti["topologyKey"] == "kubernetes.io/hostname" and anti["labelSelector"]["matchLabels"] == selector
assert pod["topologySpreadConstraints"][0]["topologyKey"] == "topology.kubernetes.io/zone"
assert c["image"] == "ghcr.io/elitea-ng/elitea-egress-proxy:" + str(c["image"].split(":")[-1])
ports = {p["name"]: p["containerPort"] for p in c["ports"]}
assert ports == {"proxy": 3128, "admin": 9090}, ports
assert c["livenessProbe"]["httpGet"] == {"path": "/healthz", "port": "admin"}
assert c["readinessProbe"]["httpGet"] == {"path": "/readyz", "port": "admin"}
env = {e["name"]: e["value"] for e in c["env"]}
assert env["ELITEA_EGRESS_PROXY_LISTEN_ADDR"] == ":3128"
assert env["ELITEA_EGRESS_PROXY_ADMIN_ADDR"] == ":9090"
keydir = env["ELITEA_EGRESS_PROXY_PUBLIC_KEYS"]
mounts = {m["name"]: m for m in c["volumeMounts"]}
assert mounts["public-keys"]["mountPath"] == keydir and mounts["public-keys"]["readOnly"] is True
volumes = {v["name"]: v for v in pod["volumes"]}
assert volumes["public-keys"]["secret"]["secretName"] == "elitea-egress-proxy-keys"
for name in ["RUN_MAX_CONNECTIONS", "RUN_MAX_BYTES", "IDLE_TIMEOUT", "MAX_CONNECTION_LIFETIME",
             "MAX_CONNECTIONS", "MAX_HEADER_BYTES", "SUMMARY_INTERVAL", "DRAIN_DELAY", "SHUTDOWN_TIMEOUT"]:
    assert env.get("ELITEA_EGRESS_PROXY_" + name), name
assert pod["terminationGracePeriodSeconds"] >= 5 + 60 + 5
deny_env = set(env["ELITEA_EGRESS_PROXY_DENY_CIDRS"].split(","))
for cidr in ["10.244.0.0/16", "fd00:10:244::/56", "10.96.0.0/12", "169.254.0.0/16", "10.0.0.0/8", "fc00::/7"]:
    assert cidr in deny_env, cidr

pdb = by(on, "PodDisruptionBudget", "elitea-egress-proxy")
assert pdb["spec"]["minAvailable"] == 1 and pdb["spec"]["selector"]["matchLabels"] == selector

svc = by(on, "Service", "elitea-egress-proxy")
assert [p["port"] for p in svc["spec"]["ports"]] == [3128], "the admin port must not be on the Service"
assert svc["spec"]["selector"] == selector

sa = by(on, "ServiceAccount", "elitea-egress-proxy")
assert sa["automountServiceAccountToken"] is False

secret = by(on, "Secret", "elitea-egress-proxy-keys")
assert sorted(secret["stringData"]) == ["current.pem", "next.pem"]
for pem in secret["stringData"].values():
    assert pem.startswith("-----BEGIN PUBLIC KEY-----") and pem.rstrip().endswith("-----END PUBLIC KEY-----")
    assert "PRIVATE" not in pem

np = by(on, "NetworkPolicy", "elitea-egress-proxy-netpol")
nps = np["spec"]
assert np["metadata"]["namespace"] == "platform"
assert nps["podSelector"]["matchLabels"] == selector
assert nps["policyTypes"] == ["Ingress", "Egress"]
assert len(nps["ingress"]) == 2, nps["ingress"]
proxy_in, admin_in = nps["ingress"]
assert proxy_in["ports"] == [{"port": 3128, "protocol": "TCP"}]
assert proxy_in["from"] == [{
    "namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "code-execution"}},
    "podSelector": {"matchLabels": {"sandbox.elitea.ai/kind": "workspace-run"}},
}], proxy_in["from"]
assert admin_in["ports"] == [{"port": 9090, "protocol": "TCP"}]
assert admin_in["from"] == [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "monitoring"}}}]
dns, internet = nps["egress"]
assert dns["to"] == [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "kube-system"}},
                      "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}}}]
assert sorted((p["protocol"], p["port"]) for p in dns["ports"]) == [("TCP", 53), ("UDP", 53)]
assert "ports" not in internet
v4, v6 = internet["to"]
assert v4["ipBlock"]["cidr"] == "0.0.0.0/0" and v6["ipBlock"]["cidr"] == "::/0"
excepted = set(v4["ipBlock"]["except"]) | set(v6["ipBlock"]["except"])
assert excepted == deny_env, (excepted ^ deny_env)
assert all(":" not in x for x in v4["ipBlock"]["except"]) and all(":" in x for x in v6["ipBlock"]["except"])
for cidr in ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "127.0.0.0/8", "100.64.0.0/10",
             "10.244.0.0/16", "10.96.0.0/12"]:
    assert cidr in v4["ipBlock"]["except"], cidr

# existingSecret replaces the chart-made Secret.
assert not [d for d in existing if d["kind"] == "Secret" and d["metadata"]["name"] == "elitea-egress-proxy-keys"]
dep2 = by(existing, "Deployment", "elitea-egress-proxy")
vol2 = {v["name"]: v for v in dep2["spec"]["template"]["spec"]["volumes"]}
assert vol2["public-keys"]["secret"]["secretName"] == "operator-keys"
PY
then
  note "the Deployment, PDB, Service, ServiceAccount, Secret and NetworkPolicy hold together"
else
  fail "the enabled render does not hold together (assertion above)"
fi

echo "== elitea-main signs the run tokens"
if python3 - "$work/on.yaml" "$work/off.yaml" <<'PY'
import sys
import yaml

def docs(path):
    return [d for d in yaml.safe_load_all(open(path)) if d]

def main_container(ds):
    deps = [d for d in ds if d["kind"] == "Deployment" and d["metadata"]["name"].endswith("elitea-main")]
    assert len(deps) == 1, [d["metadata"]["name"] for d in deps]
    pod = deps[0]["spec"]["template"]["spec"]
    return pod, [c for c in pod["containers"] if c["name"] == "elitea-main"][0]

on, off = docs(sys.argv[1]), docs(sys.argv[2])
pod, c = main_container(on)
env = {e["name"]: e.get("value") for e in c["env"]}
assert env["ELITEA_EGRESS_PROXY_URL"] == "http://elitea-egress-proxy.platform.svc.cluster.local:3128", env
assert env["ELITEA_EGRESS_TOKEN_SIGNING_KEY_FILE"] == "/etc/elitea/egress-token/signing.pem"
assert "ELITEA_EGRESS_PROXY_DEFAULT_TURN_WALL" not in env
mounts = {m["name"]: m for m in c["volumeMounts"]}
assert mounts["egress-token-signing-key"]["mountPath"] == "/etc/elitea/egress-token"
assert mounts["egress-token-signing-key"]["readOnly"] is True
vol = {v["name"]: v for v in pod["volumes"]}["egress-token-signing-key"]["secret"]
assert vol["secretName"] == "elitea-egress-token-signing" and vol["defaultMode"] == 0o400
assert vol["items"] == [{"key": "signing.pem", "path": "signing.pem"}]

# The proxy pod never references the signing key Secret.
proxy = [d for d in on if d["kind"] == "Deployment" and d["metadata"]["name"] == "elitea-egress-proxy"][0]
assert "elitea-egress-token-signing" not in yaml.safe_dump(proxy)

_, c_off = main_container(off)
assert not [e for e in c_off.get("env", []) if e["name"].startswith("ELITEA_EGRESS")]
assert "egress-token-signing-key" not in {m["name"] for m in c_off["volumeMounts"]}
PY
then
  note "elitea-main gets the proxy URL and only the signing key; the proxy never mounts it"
else
  fail "elitea-main's egress wiring is wrong (assertion above)"
fi

expect_refusal() {
  local description="$1" fragment="$2"
  shift 2
  local out
  if out="$(helm template t "$chart" "${base[@]}" -f "$fixture" "$@" 2>&1)"; then
    fail "$description: rendered, expected a refusal"
  elif ! printf '%s' "$out" | grep -qF "$fragment"; then
    fail "$description: refused for the wrong reason: $(printf '%s' "$out" | tail -1)"
  else
    note "refused: $description"
  fi
}

echo "== refusals"
expect_refusal "no public keys" "needs the run-token public keys" --set egressProxy.publicKeys.keys=null
expect_refusal "keys and existingSecret together" "set existingSecret or keys, not both" \
  --set egressProxy.publicKeys.existingSecret=operator-keys
expect_refusal "a private key in the public key set" "must be a PEM PUBLIC KEY block" \
  --set-string 'egressProxy.publicKeys.keys.current\.pem=-----BEGIN PRIVATE KEY-----'
expect_refusal "no pod CIDRs" "needs the cluster's podCidrs and serviceCidrs" \
  --set egressProxy.networkPolicy.podCidrs=null
expect_refusal "no service CIDRs" "needs the cluster's podCidrs and serviceCidrs" \
  --set egressProxy.networkPolicy.serviceCidrs=null
expect_refusal "no sandbox namespace" "needs the sandbox namespace" \
  --set sandboxKubernetes.executionNamespace=
expect_refusal "no client pod labels" "clientPodLabels must name" \
  --set egressProxy.networkPolicy.clientPodLabels=null
expect_refusal "a malformed deny CIDR" "is not an explicit CIDR block" \
  --set 'egressProxy.networkPolicy.denyCidrs[0]=metadata.internal'
expect_refusal "one replica" "replicaCount must be at least 2" --set egressProxy.replicaCount=1
expect_refusal "a grace period shorter than the drain" "terminationGracePeriodSeconds must be at least" \
  --set egressProxy.terminationGracePeriodSeconds=30

expect_refusal "no signing key for elitea-main" "needs elitea-main's run-token signing key" \
  --set egressProxy.signingKey.existingSecret=
expect_refusal "the signing key Secret is the public key Secret" "must not be egressProxy.publicKeys.existingSecret" \
  --set egressProxy.publicKeys.keys=null --set egressProxy.publicKeys.existingSecret=elitea-egress-token-signing

echo "== a signing key without the proxy is refused"
if out="$(helm template t "$chart" "${base[@]}" --set egressProxy.signingKey.existingSecret=x 2>&1)"; then
  fail "a signing key with egressProxy off rendered"
elif printf '%s' "$out" | grep -qF "egressProxy.enabled is false"; then
  note "refused: a signing key with the proxy off"
else
  fail "refused for the wrong reason: $(printf '%s' "$out" | tail -1)"
fi

echo "== clientNamespace overrides the execution namespace"
helm template t "$chart" "${base[@]}" -f "$fixture" --set egressProxy.networkPolicy.clientNamespace=workspaces \
  --show-only templates/egressProxy/networkpolicy.yaml > "$work/ns.yaml"
if grep -q 'kubernetes.io/metadata.name: workspaces' "$work/ns.yaml"; then
  note "the proxy port admits the named namespace"
else
  fail "egressProxy.networkPolicy.clientNamespace is not used"
fi

if [ "$failures" -ne 0 ]; then
  printf '%d failure(s)\n' "$failures" >&2
  exit 1
fi
echo "OK: elitea-egress-proxy renders, holds together, and refuses the half-configured shapes."
