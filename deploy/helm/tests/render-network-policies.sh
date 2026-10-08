#!/usr/bin/env bash
# render-network-policies.sh — the default-deny NetworkPolicies of the elitea
# chart (templates/main/networkpolicy.yaml, templates/worker/networkpolicy.yaml,
# templates/sandbox/supervisor-networkpolicy.yaml).
#
# What is enforced, asserted on the RENDERED manifests:
#
#   elitea-main      HTTP 8080 admits only the platform edge, the provider pods
#                    that call back directly, and the operator-declared
#                    peers; the runtime listeners admit the worker pods only.
#   platform edge    ingress 443 from the worker, elitea-main and (when the
#                    callback hop goes through the edge) DeepWiki; egress only
#                    to elitea-main's HTTP port and cluster DNS.
#   worker           no inbound traffic at all; egress is not restricted.
#   sandbox          the supervisor ports admit the worker and elitea-main only.
#
# A policy is only as good as the labels it selects, so every selector here is
# matched against the labels of the pod templates the chart ACTUALLY renders:
# a policy that selects no pod, or another workload's pods, fails the script.
# The two guards (disabled without externallyManaged; ingress enabled without a
# declared peer) are asserted in both directions.
#
# Usage: deploy/helm/tests/render-network-policies.sh
# Needs: helm, python3 with PyYAML. No cluster, no network.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CHART="$REPO/deploy/helm/elitea"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

for tool in helm python3; do
  command -v "$tool" >/dev/null 2>&1 || { echo "render-network-policies.sh needs $tool on PATH" >&2; exit 2; }
done

POSTURE=(--set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://render-only.example.invalid/llm/v1
         --set-string llmGateway.egressPosture=public-unrestricted)
NS=(--namespace elitea)
# The install-time decision the chart demands: nothing outside the chart reaches
# elitea-main. Renders that exercise other things state it; the guard section
# below renders without it.
NOEXT=(--set networkPolicies.main.noExternalIngress=true)
STANDALONE=(-f "$CHART/values-standalone.yaml")
WORKER=(--set worker.enabled=true)
SUPERVISOR=(--set sandboxKubernetes.enabled=true
  --set sandboxKubernetes.executionNamespace=code-execution
  --set sandboxKubernetes.supervisor.enabled=true
  --set sandboxKubernetes.supervisor.materialSecret=sandbox-material
  --set 'sandboxKubernetes.supervisor.profiles[0].file=deno.json'
  --set 'sandboxKubernetes.supervisor.profiles[0].port=9446'
  --set 'sandboxKubernetes.supervisor.profiles[1].file=rust.json'
  --set 'sandboxKubernetes.supervisor.profiles[1].port=9447'
  --set sandboxKubernetes.supervisor.image=registry/supervisor@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa)
DEEPWIKI=(--set deepwiki.enabled=true
  --set deepwiki.env.ELITEA_DEEPWIKI_GIT_ALLOWLIST=github.com
  --set main.fileConfig.deepwikiClientMaterial.enabled=true
  --set main.fileConfig.deepwikiClientMaterial.secretName=elitea-main-deepwiki-client-tls
  --set main.env.ELITEA_DEEPWIKI_ENABLED=true
  --set main.env.ELITEA_DEEPWIKI_BASE_URL=https://elitea-deepwiki-svc:8443
  --set main.env.ELITEA_DEEPWIKI_GIT_ALLOWLIST=github.com
  --set main.env.ELITEA_DEEPWIKI_CLIENT_CERT_FILE=/run/elitea-deepwiki/tls.crt
  --set main.env.ELITEA_DEEPWIKI_CLIENT_KEY_FILE=/run/elitea-deepwiki/tls.key
  --set main.env.ELITEA_DEEPWIKI_CA_FILE=/run/elitea-deepwiki/ca.crt)
DEEPWIKI_DIRECT=(--set main.env.ELITEA_DEEPWIKI_CALLBACK_BASE_URL=http://elitea-main:8080)
INVENTORY=(--set inventory.enabled=true
  --set main.fileConfig.inventoryClientMaterial.enabled=true
  --set main.fileConfig.inventoryClientMaterial.secretName=elitea-main-inventory-client-tls
  --set main.env.ELITEA_INVENTORY_ENABLED=true
  --set main.env.ELITEA_INVENTORY_BASE_URL=https://elitea-inventory-svc:8443
  --set main.env.ELITEA_INVENTORY_CALLBACK_BASE_URL=http://elitea-main:8080
  --set main.env.ELITEA_INVENTORY_GIT_ALLOWLIST=github.com
  --set main.env.ELITEA_INVENTORY_CLIENT_CERT_FILE=/run/elitea-inventory/tls.crt
  --set main.env.ELITEA_INVENTORY_CLIENT_KEY_FILE=/run/elitea-inventory/tls.key
  --set main.env.ELITEA_INVENTORY_CA_FILE=/run/elitea-inventory/ca.crt)

failures=0
fail() { echo "FAIL: $*" >&2; failures=$((failures + 1)); }
pass() { echo "ok: $*"; }

render() { # render <name> <args...> -> $WORK/<name>.yaml
  local name="$1"; shift
  helm template elitea "$CHART" "${NS[@]}" "${POSTURE[@]}" "$@" > "$WORK/$name.yaml" 2> "$WORK/$name.err" \
    || { fail "$name does not render: $(tail -3 "$WORK/$name.err" | tr '\n' ' ')"; return 1; }
}

# ── 1. The policies, against the pods the chart renders ──────────────────────
# check <name> <edge> <worker> <supervisor> <deepwiki: none|direct|edge> <inventory: 0|1>
check() {
  python3 - "$WORK/$1.yaml" "$1" "$2" "$3" "$4" "$5" "$6" <<'PY'
import sys, yaml

path, name, edge, worker, sup, deepwiki, inventory = sys.argv[1:8]
edge, worker, sup, inventory = edge == "1", worker == "1", sup == "1", inventory == "1"
docs = [d for d in yaml.safe_load_all(open(path)) if d]
bad = []
count = 0

def check(cond, msg):
    global count
    count += 1
    if not cond:
        bad.append(msg)

# Every pod template the chart renders, keyed by workload.
pods = {}
for d in docs:
    kind = d["kind"]
    if kind in ("Deployment", "StatefulSet", "Job"):
        pods[f'{kind}/{d["metadata"]["name"]}'] = d["spec"]["template"]["metadata"].get("labels", {})
    elif kind == "CronJob":
        pods[f'CronJob/{d["metadata"]["name"]}'] = d["spec"]["jobTemplate"]["spec"]["template"]["metadata"].get("labels", {})
    elif kind == "Pod":
        pods[f'Pod/{d["metadata"]["name"]}'] = d["metadata"].get("labels", {})

def selects(match_labels):
    return sorted(n for n, l in pods.items() if all(l.get(k) == v for k, v in match_labels.items()))

policies = {d["metadata"]["name"]: d for d in docs if d["kind"] == "NetworkPolicy"
            and d["metadata"]["name"].endswith("-netpol")}
deployments = {d["metadata"]["name"]: d for d in docs if d["kind"] == "Deployment"}

MAIN, WORKER, EDGE, SUP = "Deployment/elitea-main", "Deployment/elitea-worker", "Deployment/elitea-platform-edge", "Deployment/elitea-sandbox-supervisor"
DW, INV = "Deployment/elitea-deepwiki", "Deployment/elitea-inventory"

expected = {"elitea-main-netpol": MAIN}
if worker:
    expected["elitea-worker-netpol"] = WORKER
if edge:
    expected["elitea-platform-edge-netpol"] = EDGE
if sup:
    expected["elitea-sandbox-supervisor-netpol"] = SUP
check(sorted(policies) == sorted(expected), f"policy set {sorted(policies)} != {sorted(expected)}")

for pname, target in expected.items():
    pol = policies.get(pname)
    if pol is None:
        continue
    sel = pol["spec"]["podSelector"].get("matchLabels") or {}
    check(bool(sel), f"{pname}: empty podSelector would select every pod")
    check(selects(sel) == [target], f"{pname}: selects {selects(sel)}, want only {target}")
    check(pol["metadata"].get("namespace", "elitea") == "elitea", f"{pname}: wrong namespace")

def peer_targets(rule, where):
    """Resolve each from-peer to the workload(s) it matches; every peer must be an
    in-namespace pod peer that selects exactly one real rendered workload."""
    out = []
    for p in rule.get("from", []):
        ns = (p.get("namespaceSelector") or {}).get("matchLabels") or {}
        ps = (p.get("podSelector") or {}).get("matchLabels") or {}
        check(ns == {"kubernetes.io/metadata.name": "elitea"}, f"{where}: peer {p} is not pinned to the release namespace")
        # A provider's one-shot migration Job carries the provider's selector
        # labels; it is the same workload family, not a second client.
        t = [n for n in (selects(ps) if ps else []) if not n.endswith("-migrate")]
        check(len(t) == 1, f"{where}: peer {p} selects {t}, want exactly one rendered workload")
        out += t
    return sorted(out)

def rules_by_ports(rules, where):
    out = {}
    for r in rules:
        check(len(r.get("from", r.get("to", []))) > 0, f"{where}: rule {r} has no peers (an empty peer list admits everyone)")
        key = tuple(sorted(int(p["port"]) for p in r.get("ports", [])))
        check(bool(key), f"{where}: rule {r} has no ports (admits every port)")
        out[key] = r
    return out

# ── elitea-main ──────────────────────────────────────────────────────────────
main = policies.get("elitea-main-netpol")
if main:
    spec = main["spec"]
    check(spec["policyTypes"] == ["Ingress"], "main: policyTypes must be Ingress only")
    by = rules_by_ports(spec.get("ingress") or [], "main")
    want_http = sorted(([EDGE] if edge else []) + ([DW] if deepwiki == "direct" else []) + ([INV] if inventory else []))
    http = by.get((8080,))
    check(http is not None or not want_http, "main: no rule for 8080")
    check(http is None or want_http, "main: 8080 rule without a peer")
    if http:
        got = peer_targets(http, "main:8080")
        check(got == want_http, f"main:8080 admits {got}, want {want_http}")
        check(WORKER not in got, "main:8080 must not admit the worker")
    # runtime listeners: exactly the ports the Deployment declares, worker only
    ctr = deployments["elitea-main"]["spec"]["template"]["spec"]["containers"][0]["ports"]
    rt = sorted(p["containerPort"] for p in ctr if p["name"].startswith("runtime-"))
    rts = [k for k in by if k != (8080,)]
    if rt and worker:
        check(rts == [tuple(rt)], f"main: runtime rule ports {rts}, want {[tuple(rt)]}")
        if rts:
            check(peer_targets(by[rts[0]], "main:runtime") == [WORKER], "main: runtime listeners must admit the worker only")
    else:
        check(not rts, f"main: unexpected rules for ports {rts} (runtime={rt}, worker={worker})")
    check(sorted(k for r in by for k in r) == sorted(([8080] if want_http else []) + (rt if worker else [])), "main: ports admitted are not exactly 8080 + runtime")

# ── platform edge ────────────────────────────────────────────────────────────
ep = policies.get("elitea-platform-edge-netpol")
if ep:
    spec = ep["spec"]
    check(spec["policyTypes"] == ["Ingress", "Egress"], "edge: policyTypes must be Ingress+Egress")
    by = rules_by_ports(spec["ingress"], "edge")
    check(list(by) == [(443,)], f"edge: ingress ports {list(by)}, want only 443")
    if (443,) in by:
        got = peer_targets(by[(443,)], "edge:443")
        want = sorted([WORKER, MAIN] + ([DW] if deepwiki == "edge" else []))
        check(got == want, f"edge:443 admits {got}, want {want}")
    eg = spec["egress"]
    check(len(eg) == 2, f"edge: {len(eg)} egress rules, want main + DNS only")
    to_main = [r for r in eg if (r.get("ports") or [{}])[0].get("port") == 8080]
    dns = [r for r in eg if (r.get("ports") or [{}])[0].get("port") == 53]
    check(len(to_main) == 1 and len(dns) == 1, "edge: egress must be exactly main:8080 and DNS:53")
    if to_main:
        check(peer_targets({"from": to_main[0]["to"]}, "edge egress") == [MAIN], "edge: egress must reach elitea-main only")
        check([p["port"] for p in to_main[0]["ports"]] == [8080], "edge: egress to main limited to the http port")
    if dns:
        check(sorted((p["protocol"], p["port"]) for p in dns[0]["ports"]) == [("TCP", 53), ("UDP", 53)], "edge: DNS ports")
        check(dns[0]["to"] == [{"namespaceSelector": {}, "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}}}], "edge: DNS peer shape")

# ── worker ───────────────────────────────────────────────────────────────────
wp = policies.get("elitea-worker-netpol")
if wp:
    spec = wp["spec"]
    check(spec["policyTypes"] == ["Ingress"], "worker: policyTypes must be Ingress only (egress is not restricted)")
    check(not spec.get("ingress"), "worker: ingress must have no rules (deny all inbound)")
    check("egress" not in spec, "worker: must carry no egress rules")

# ── sandbox supervisor ───────────────────────────────────────────────────────
sp = policies.get("elitea-sandbox-supervisor-netpol")
if sp:
    spec = sp["spec"]
    check(spec["policyTypes"] == ["Ingress"], "supervisor: policyTypes must be Ingress only")
    by = rules_by_ports(spec["ingress"], "supervisor")
    check(list(by) == [(9446, 9447)], f"supervisor: ports {list(by)}, want the profile ports")
    if by:
        got = peer_targets(next(iter(by.values())), "supervisor")
        check(got == sorted([WORKER, MAIN]), f"supervisor admits {got}, want worker + main")

if bad:
    print(f"{name}: {len(bad)} failed of {count}")
    for b in bad:
        print("  FAIL:", b)
    sys.exit(1)
print(f"{name}: {count} assertions, {len(policies)} policies")
PY
}

run() { # run <name> <edge> <worker> <supervisor> <deepwiki> <inventory> -- <helm args>
  local name="$1" e="$2" w="$3" s="$4" d="$5" i="$6"; shift 7
  # values-kind.yaml states the decision itself; every other profile does not.
  if [ "$name" = kind ]; then render "$name" "$@" || return 0; else render "$name" "${NOEXT[@]}" "$@" || return 0; fi
  if out="$(check "$name" "$e" "$w" "$s" "$d" "$i" 2>&1)"; then pass "$out"; else fail "$out"; fi
}

echo "== profiles =="
run default      0 0 0 none 0 --
run standalone   0 0 0 none 0 -- "${STANDALONE[@]}"
run auth-minimal 0 0 0 none 0 -- -f "$CHART/values-auth-minimal.yaml"
run staging      0 0 0 none 0 -- -f "$CHART/values-staging.yaml"
# values-kind.yaml runs DeepWiki with its callback on elitea-main:8080.
run kind         0 0 0 direct 0 -- -f "$REPO/deploy/kind/values-kind.yaml"
echo "== composed installs =="
run worker-edge  1 1 0 none 0 -- "${STANDALONE[@]}" "${WORKER[@]}"
run full         1 1 1 direct 1 -- "${STANDALONE[@]}" "${WORKER[@]}" "${SUPERVISOR[@]}" "${DEEPWIKI[@]}" "${DEEPWIKI_DIRECT[@]}" "${INVENTORY[@]}"
run full-via-edge 1 1 1 edge 1 -- "${STANDALONE[@]}" "${WORKER[@]}" "${SUPERVISOR[@]}" "${DEEPWIKI[@]}" --set deepwiki.callbackViaPlatformEdge=true --set main.env.ELITEA_DEEPWIKI_CALLBACK_BASE_URL= "${INVENTORY[@]}"

# ── 2. Guards ────────────────────────────────────────────────────────────────
echo "== guards =="
refuses() { # refuses <label> <message fragment> <helm args...>
  local label="$1" want="$2"; shift 2
  if helm template elitea "$CHART" "${NS[@]}" "${POSTURE[@]}" "$@" > /dev/null 2> "$WORK/refusal.err"; then
    fail "$label: rendered, but must be refused"
  elif grep -qF -- "$want" "$WORK/refusal.err"; then
    pass "$label: refused ($want)"
  else
    fail "$label: refused for another reason: $(tail -1 "$WORK/refusal.err")"
  fi
}
count_policies() { python3 -c '
import sys, yaml
print(sum(1 for d in yaml.safe_load_all(open(sys.argv[1])) if d and d["kind"] == "NetworkPolicy" and d["metadata"]["name"].endswith("-netpol")))' "$1"; }

refuses "default values without the ingress decision" "explicit decision about who may reach elitea-main" \
  "${STANDALONE[@]}" --set networkPolicies.main.noExternalIngress=false
refuses "an unfilled placeholder peer" "unfilled placeholder" "${STANDALONE[@]}" \
  -f <(printf 'networkPolicies:\n  main:\n    extraIngressFrom:\n      - namespaceSelector:\n          matchLabels:\n            kubernetes.io/metadata.name: ""\n')
refuses "noExternalIngress with main.ingress.enabled (gateway-api)" "contradicts main.ingress.enabled" \
  "${STANDALONE[@]}" "${NOEXT[@]}" --set main.ingress.enabled=true --set networkPolicies.main.ingressFrom[0].podSelector.matchLabels.x=y --set main.ingress.gatewayApi=true \
  --set main.ingress.gateway.name=shared-gateway --set main.ingress.gateway.namespace=gateway-system
refuses "enabled=false without externallyManaged" "networkPolicies.externallyManaged" \
  "${STANDALONE[@]}" "${NOEXT[@]}" "${WORKER[@]}" --set networkPolicies.enabled=false
if render off-managed "${STANDALONE[@]}" "${WORKER[@]}" "${SUPERVISOR[@]}" --set networkPolicies.enabled=false --set networkPolicies.externallyManaged=true; then
  n="$(count_policies "$WORK/off-managed.yaml")"
  [ "$n" = "0" ] && pass "enabled=false + externallyManaged=true renders no chart-owned policies" || fail "enabled=false rendered $n policies"
fi

# A peer that is NOT in the chart: the gateway / ingress controller.
cat > "$WORK/peers.yaml" <<'YAML'
networkPolicies:
  main:
    ingressFrom:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: gateway-system
        podSelector:
          matchLabels:
            gateway.example/name: edge
    extraIngressFrom:
      - ipBlock:
          cidr: 192.0.2.0/24
  probeFrom:
    - ipBlock:
        cidr: 198.51.100.0/24
YAML
# An ingress carries outside traffic, so these renders never inherit a
# profile's noExternalIngress=true.
GWAPI=(--set networkPolicies.main.noExternalIngress=false --set main.ingress.enabled=true --set main.ingress.gatewayApi=true
       --set main.ingress.gateway.name=shared-gateway --set main.ingress.gateway.namespace=gateway-system)
PLAIN=(--set networkPolicies.main.noExternalIngress=false --set main.ingress.enabled=true --set main.ingress.gatewayApi=false
       --set main.ingress.identityHeadersStrippedByController=true)
# extraIngressFrom satisfies the install-time decision, so what is refused here is
# the ingress-specific rule: the Ingress / HTTPRoute needs ingressFrom itself.
EXTRA=(--set 'networkPolicies.main.extraIngressFrom[0].ipBlock.cidr=192.0.2.0/24')
refuses "gateway-api ingress without ingressFrom" "main.ingress.enabled=true with networkPolicies.enabled=true needs networkPolicies.main.ingressFrom" "${STANDALONE[@]}" "${GWAPI[@]}" "${EXTRA[@]}"
refuses "plain ingress without ingressFrom"       "main.ingress.enabled=true with networkPolicies.enabled=true needs networkPolicies.main.ingressFrom" "${STANDALONE[@]}" "${PLAIN[@]}" "${EXTRA[@]}"
for variant in gwapi plain none; do
  case "$variant" in gwapi) args=("${GWAPI[@]}");; plain) args=("${PLAIN[@]}");; none) args=();; esac
  if render "ingress-$variant" "${STANDALONE[@]}" "${WORKER[@]}" "${SUPERVISOR[@]}" ${args[@]+"${args[@]}"} -f "$WORK/peers.yaml"; then
    if out="$(python3 - "$WORK/ingress-$variant.yaml" <<'PY' 2>&1
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
pols = {d["metadata"]["name"]: d for d in docs if d["kind"] == "NetworkPolicy"}
gw = {"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "gateway-system"}},
      "podSelector": {"matchLabels": {"gateway.example/name": "edge"}}}
extra = {"ipBlock": {"cidr": "192.0.2.0/24"}}
probe = {"ipBlock": {"cidr": "198.51.100.0/24"}}
http = next(r for r in pols["elitea-main-netpol"]["spec"]["ingress"] if [p["port"] for p in r["ports"]] == [8080])
assert gw in http["from"] and extra in http["from"] and probe in http["from"], http["from"]
rt = [r for r in pols["elitea-main-netpol"]["spec"]["ingress"] if r is not http]
assert rt and all(gw not in r["from"] and extra not in r["from"] for r in rt), "operator peers must not reach the runtime listeners"
for n in ("elitea-platform-edge-netpol", "elitea-sandbox-supervisor-netpol"):
    assert any(probe in r["from"] for r in pols[n]["spec"]["ingress"]), n + " lacks probeFrom"
print("peers present: 8080 admits gateway, extra and probe peers; runtime listeners do not; edge and supervisor admit probeFrom")
PY
)"; then pass "ingress ($variant) with ingressFrom: $out"; else fail "ingress ($variant) peers: $out"; fi
  fi
done

echo
if [ "$failures" -ne 0 ]; then
  echo "$failures failure(s)" >&2
  exit 1
fi
echo "all NetworkPolicy checks passed"
