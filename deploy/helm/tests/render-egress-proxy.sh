#!/usr/bin/env bash
# render-egress-proxy.sh — elitea-egress-proxy (ADR-0034 decision 9).
#
# Every assertion reads the RENDERED manifest. Every refusal case is the
# complete install minus exactly one thing.
#
# What is asserted, and why it would fail silently otherwise:
#   * egressProxy off (the default) renders nothing of it;
#   * on, a CPU HPA scales the Deployment from 2 to 6 replicas and the
#     Deployment has no `replicas:` (with autoscaling off it runs
#     replicaCount); the pod is non-root, read-only root, all capabilities
#     dropped, no service-account token, with anti-affinity and a zone spread,
#     and a PDB keeps one up;
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
# Needs: helm, yq, python3 with PyYAML. No cluster, no network.
set -euo pipefail
# Every `helm template` here starts from the pre-S10 feature switches
# (fixtures/features-off.yaml: runtime, worker, vector, Qdrant, DeepWiki and
# Inventory off), prepended so that any values file or --set a render names
# wins; each render turns on what it tests. render-default-on.sh covers the
# shipped defaults.
FEATURES_OFF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/fixtures/features-off.yaml"
_helm_bin="${HELM:-helm}"
helm() {
  if [ "${1:-}" = template ]; then shift; command "$_helm_bin" template -f "$FEATURES_OFF" "$@"
  else command "$_helm_bin" "$@"; fi
}
HELM=helm
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
# The HPA owns the count, so the Deployment states none: a value here is
# written back at each helm upgrade.
assert "replicas" not in spec, spec.get("replicas")
hpa = by(on, "HorizontalPodAutoscaler", "elitea-egress-proxy")
assert hpa["spec"]["scaleTargetRef"]["name"] == "elitea-egress-proxy", hpa["spec"]["scaleTargetRef"]
assert (hpa["spec"]["minReplicas"], hpa["spec"]["maxReplicas"]) == (2, 6), hpa["spec"]
assert hpa["spec"]["metrics"][0]["resource"]["name"] == "cpu", hpa["spec"]["metrics"]
assert hpa["metadata"]["annotations"]["elitea.ai/autoscaling-source"] == "cpu"
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
env = {e["name"]: e.get("value") for e in c["env"]}
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
# The integer limits must reach the proxy as plain integers: Helm reads a
# values-file number as a float, and a bare toString rendered the default
# 2147483648 as "2.147483648e+09", which crash-looped the proxy on 4.56.1.
for name, want in [("MAX_CONNECTIONS", "4096"), ("MAX_HEADER_BYTES", "32768"),
                   ("RUN_MAX_CONNECTIONS", "64"), ("RUN_MAX_BYTES", "2147483648")]:
    assert env["ELITEA_EGRESS_PROXY_" + name] == want, (name, env["ELITEA_EGRESS_PROXY_" + name])
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
    "podSelector": {"matchLabels": {"sandbox.elitea.ai/kind": "workspace-run", "sandbox.elitea.ai/egress": "proxy"}},
}], proxy_in["from"]
assert admin_in["ports"] == [{"port": 9090, "protocol": "TCP"}]
assert admin_in["from"] == [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "monitoring"}}}]
main_rule, dns, internet = nps["egress"]
# The run summaries: elitea-main's pods, on the control port only.
assert main_rule["ports"] == [{"port": 9443, "protocol": "TCP"}], main_rule
assert main_rule["to"][0]["podSelector"]["matchLabels"]["app.kubernetes.io/name"] == "elitea-main", main_rule
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
assert vol["secretName"] == "elitea-egress-token-signing"
# Main runs as a non-root user and the pod sets no fsGroup, so the projected
# file is root-owned: 0400 made it unreadable and Main exited at start
# ("permission denied") on the first deployment that enabled the proxy.
assert "fsGroup" not in (pod.get("securityContext") or {}) or vol["defaultMode"] & 0o040, vol
assert vol["defaultMode"] & 0o004, oct(vol["defaultMode"])
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

echo "== the proxy reports run summaries to elitea-main over mTLS"
helm template t "$chart" "${base[@]}" -f "$fixture" --set egressProxy.audit.enabled=false > "$work/noaudit.yaml"
if python3 - "$work/on.yaml" "$work/noaudit.yaml" <<'PY'
import json
import sys
import yaml

def docs(path):
    return [d for d in yaml.safe_load_all(open(path)) if d]

def by(ds, kind, name):
    found = [d for d in ds if d["kind"] == kind and d["metadata"]["name"] == name]
    assert len(found) == 1, f"want one {kind}/{name}, found {len(found)}"
    return found[0]

on, off = docs(sys.argv[1]), docs(sys.argv[2])
cert = by(on, "Certificate", "elitea-egress-proxy-audit-client")["spec"]
assert cert["dnsNames"] == ["elitea-egress-proxy"] and cert["usages"] == ["client auth"]
assert cert["secretName"] == "elitea-egress-proxy-audit-client-tls"
assert cert["issuerRef"]["name"] == "elitea-runtime-ca"

pod = by(on, "Deployment", "elitea-egress-proxy")["spec"]["template"]["spec"]
c = pod["containers"][0]
env = {e["name"]: e for e in c["env"]}
assert env["ELITEA_EGRESS_PROXY_AUDIT_TARGET"]["value"] == "elitea-main:9443", env["ELITEA_EGRESS_PROXY_AUDIT_TARGET"]
assert env["ELITEA_EGRESS_PROXY_AUDIT_SERVER_NAME"]["value"] == "elitea-main"
assert env["ELITEA_EGRESS_PROXY_AUDIT_CERT_FILE"]["value"] == "/etc/elitea-egress-proxy/audit-tls/tls.crt"
assert env["ELITEA_EGRESS_PROXY_REPLICA_ID"]["valueFrom"]["fieldRef"]["fieldPath"] == "metadata.name"
vols = {v["name"]: v for v in pod["volumes"]}
assert vols["audit-client-tls"]["secret"]["secretName"] == "elitea-egress-proxy-audit-client-tls"
assert pod["terminationGracePeriodSeconds"] >= 5 + 60 + 5 + 10

cm = [d for d in on if d["kind"] == "ConfigMap" and d["metadata"]["name"].endswith("elitea-main-config")][0]
assert cm["data"]["ELITEA_EGRESS_AUDIT_CLIENTS"] == "dns:elitea-egress-proxy", cm["data"].get("ELITEA_EGRESS_AUDIT_CLIENTS")

main_np = [d for d in on if d["kind"] == "NetworkPolicy" and d["metadata"]["name"] == "elitea-main-netpol"]
assert main_np, [d["metadata"]["name"] for d in on if d["kind"] == "NetworkPolicy"]
rules = [r for r in main_np[0]["spec"]["ingress"] if any(
    p.get("podSelector", {}).get("matchLabels", {}).get("app.kubernetes.io/name") == "elitea-egress-proxy" for p in r.get("from", []))]
assert len(rules) == 1 and rules[0]["ports"] == [{"port": 9443, "protocol": "TCP"}], rules

# Off: no certificate, no env, no admitted identity, no peer.
assert not [d for d in off if d["kind"] == "Certificate" and d["metadata"]["name"] == "elitea-egress-proxy-audit-client"]
c_off = by(off, "Deployment", "elitea-egress-proxy")["spec"]["template"]["spec"]["containers"][0]
assert not [e for e in c_off["env"] if "AUDIT" in e["name"]]
cm_off = [d for d in off if d["kind"] == "ConfigMap" and d["metadata"]["name"].endswith("elitea-main-config")][0]
assert "ELITEA_EGRESS_AUDIT_CLIENTS" not in cm_off["data"]
PY
then
  note "the proxy's client certificate, env and egress rule, and elitea-main's admitted identity and ingress, render together"
else
  fail "the egress audit wiring is wrong (assertion above)"
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

echo "== a quoted integer override renders as itself"
if helm template t "$chart" "${base[@]}" -f "$fixture" --set-string egressProxy.limits.runMaxBytes=3000000000 \
    | grep -A1 'name: ELITEA_EGRESS_PROXY_RUN_MAX_BYTES' | grep -qF 'value: "3000000000"'; then
  note "runMaxBytes \"3000000000\" reaches the proxy unchanged"
else
  fail "a quoted runMaxBytes did not render as the same integer"
fi

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
# The floor is at least two: autoscaling.minReplicas while the HPA owns the
# count, else replicaCount. The refusal names the field that applies.
expect_refusal "an autoscaler floor of one" "egressProxy.autoscaling.minReplicas must be at least 2" \
  --set egressProxy.autoscaling.minReplicas=1
expect_refusal "one replica with autoscaling off" "egressProxy.replicaCount must be at least 2" \
  --set egressProxy.autoscaling.enabled=false --set egressProxy.replicaCount=1
if helm template t "$chart" "${base[@]}" -f "$fixture" --set egressProxy.replicaCount=1 >/dev/null 2>&1; then
  note "replicaCount 1 renders while the HPA (floor 2) owns the count"
else
  fail "replicaCount 1 is refused while autoscaling.minReplicas is 2; the HPA, not replicaCount, sets the floor"
fi
fixed="$(helm template t "$chart" "${base[@]}" -f "$fixture" --set egressProxy.autoscaling.enabled=false)" \
  || fail "egressProxy with autoscaling off does not render"
fixed_replicas="$(printf '%s' "$fixed" | yq eval-all 'select(.kind == "Deployment" and .metadata.name == "elitea-egress-proxy") | .spec.replicas' -)"
fixed_hpa="$(printf '%s' "$fixed" | yq eval-all 'select(.kind == "HorizontalPodAutoscaler" and .metadata.name == "elitea-egress-proxy") | .metadata.name' -)"
if [ "$fixed_replicas" = "2" ] && [ -z "$fixed_hpa" ]; then
  note "autoscaling off: replicas 2 and no HPA"
else
  fail "autoscaling off: replicas '$fixed_replicas' (want 2), HPA '$fixed_hpa' (want none)"
fi
# The integer limits: a unit suffix, a fraction, a leading zero (octal to
# sprig's int64), hex, and zero are refused by name instead of reaching the
# proxy as a different number.
expect_refusal "a unit-suffixed limit" "egressProxy.limits.runMaxBytes must be a whole number" \
  --set-string egressProxy.limits.runMaxBytes=2Gi
expect_refusal "a fractional limit" "egressProxy.limits.runMaxConnections must be a whole number" \
  --set egressProxy.limits.runMaxConnections=0.5
expect_refusal "a leading-zero limit" "egressProxy.limits.maxConnections must be a whole number" \
  --set-string egressProxy.limits.maxConnections=0100
expect_refusal "a hex limit" "egressProxy.limits.runMaxBytes must be a whole number" \
  --set-string egressProxy.limits.runMaxBytes=0x10
expect_refusal "a zero limit" "egressProxy.limits.maxHeaderBytes must be at least 1" \
  --set egressProxy.limits.maxHeaderBytes=0
expect_refusal "a grace period shorter than the drain" "terminationGracePeriodSeconds must be at least" \
  --set egressProxy.terminationGracePeriodSeconds=30
expect_refusal "a grace period without the audit's last reports" "plus 10 for the last audit reports" \
  --set egressProxy.terminationGracePeriodSeconds=75
expect_refusal "the audit identity is elitea-vector's" "must differ from vector.mtls.clientDnsName" \
  --set vector.enabled=true --set qdrant.enabled=true --set postgresql.maxConnections=1000 \
  --set egressProxy.audit.clientDnsName=elitea-vector
# The default values keep elitea-main's runtime plane off.
if out="$(helm template t "$chart" --namespace platform -f "$fixture" \
    --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1 \
    --set llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true 2>&1)"; then
  fail "the audit without elitea-main's runtime rendered"
elif printf '%s' "$out" | grep -qF "egressProxy.audit.enabled needs main.runtime.enabled"; then
  note "refused: the audit without elitea-main's runtime"
else
  fail "the audit without the runtime was refused for the wrong reason: $(printf '%s' "$out" | tail -1)"
fi

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

echo "== a proxy-mode workspace run reaches only the proxy port"
runs=(-f deploy/helm/tests/fixtures/cloud-executor-values.yaml --set sandboxKubernetes.enabled=true --set sandboxKubernetes.workspaceRuns.enabled=true --set sandboxKubernetes.executionNamespace=code-execution)
helm template t "$chart" "${base[@]}" -f "$fixture" "${runs[@]}" > "$work/runs.yaml"
helm template t "$chart" "${base[@]}" "${runs[@]}" > "$work/runs-noproxy.yaml"
# Direct mode alongside: both policies, each selecting its own label.
helm template t "$chart" "${base[@]}" -f "$fixture" "${runs[@]}" \
  --set sandboxKubernetes.workspaceRuns.egress.enabled=true \
  --set egressProxy.networkPolicy.podCidrs=null --set egressProxy.networkPolicy.serviceCidrs=null \
  --set 'sandboxKubernetes.workspaceRuns.egress.podCidrs[0]=10.244.0.0/16' \
  --set 'sandboxKubernetes.workspaceRuns.egress.serviceCidrs[0]=10.96.0.0/12' > "$work/both.yaml"
if python3 - "$work/runs.yaml" "$work/runs-noproxy.yaml" "$work/both.yaml" <<'PY'
import sys
import yaml

def docs(path):
    return [d for d in yaml.safe_load_all(open(path)) if d]

def policies(ds):
    return {d["metadata"]["name"]: d for d in ds if d["kind"] == "NetworkPolicy"}

runs, noproxy, both = (docs(p) for p in sys.argv[1:4])
found = policies(runs)
p = found["elitea-workspace-run-proxy-egress"]
assert p["metadata"]["namespace"] == "code-execution"
spec = p["spec"]
assert spec["podSelector"] == {"matchLabels": {"sandbox.elitea.ai/kind": "workspace-run", "sandbox.elitea.ai/egress": "proxy"}}
assert spec["policyTypes"] == ["Egress"]
assert spec["egress"] == [{
    "to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "platform"}},
            "podSelector": {"matchLabels": {"app.kubernetes.io/name": "elitea-egress-proxy", "app.kubernetes.io/instance": "t"}}}],
    "ports": [{"protocol": "TCP", "port": 3128}],
}], spec["egress"]
# No DNS, no ipBlock: only the proxy pods.
assert "53" not in yaml.safe_dump(spec) and "ipBlock" not in yaml.safe_dump(spec)
# Deny-all is unchanged; the direct-mode policy does not render without workspaceRuns.egress.
assert found["elitea-code-deny-network"]["spec"]["egress"] == []
assert "elitea-workspace-run-egress" not in found
assert "elitea-workspace-run-proxy-egress" not in policies(noproxy)

found = policies(both)
direct = found["elitea-workspace-run-egress"]["spec"]["podSelector"]["matchLabels"]
proxy = found["elitea-workspace-run-proxy-egress"]["spec"]["podSelector"]["matchLabels"]
assert direct["sandbox.elitea.ai/egress"] == "public" and proxy["sandbox.elitea.ai/egress"] == "proxy"
# The proxy took the cluster ranges from workspaceRuns.egress.
dep = [d for d in both if d["kind"] == "Deployment" and d["metadata"]["name"] == "elitea-egress-proxy"][0]
env = {e["name"]: e.get("value") for e in dep["spec"]["template"]["spec"]["containers"][0]["env"]}
deny = set(env["ELITEA_EGRESS_PROXY_DENY_CIDRS"].split(","))
assert {"10.244.0.0/16", "10.96.0.0/12"} <= deny, deny
np = [r for r in found["elitea-egress-proxy-netpol"]["spec"]["egress"] if "ipBlock" in r["to"][0]][0]["to"][0]["ipBlock"]["except"]
assert "10.244.0.0/16" in np and "10.96.0.0/12" in np
PY
then
  note "proxy mode: one policy, the proxy pods on 3128 only; direct mode keeps its own; the CIDRs are shared"
else
  fail "the workspace-run proxy-mode policy is wrong (assertion above)"
fi
expect_refusal "different cluster ranges in the two places" "name different cluster ranges" \
  -f deploy/helm/tests/fixtures/cloud-executor-values.yaml --set sandboxKubernetes.enabled=true --set sandboxKubernetes.workspaceRuns.enabled=true \
  --set 'sandboxKubernetes.workspaceRuns.egress.podCidrs[0]=10.0.0.0/16'

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
