#!/usr/bin/env bash
# render-network-policies.sh — the default-deny NetworkPolicies of the elitea
# chart (templates/main/networkpolicy.yaml, templates/worker/networkpolicy.yaml,
# templates/sandbox/supervisor-networkpolicy.yaml).
#
# What is enforced, asserted on the RENDERED manifests:
#
#   elitea-main      HTTP 8080 admits only the provider pods that call back
#                    directly and the operator-declared peers; the control,
#                    output and content listeners admit the worker pods only;
#                    the platform listener (#1359, which replaced the
#                    platform-edge proxy) admits the worker and, when its
#                    callback hop uses it, DeepWiki — and no edge pod or
#                    platform-edge policy is rendered any more.
#   worker           no inbound traffic at all; egress is not restricted.
#   sandbox          the supervisor ports admit the worker and elitea-main only.
#   deepwiki         one port (#1359), the gRPC listener that serves the
#                    provider SPI and the platform service: declared, served,
#                    exposed and admitted together, to elitea-main only; no
#                    HTTP SPI port is left.
#   inventory        the same shape as deepwiki.
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
WORKER=(--set worker.enabled=true --set main.runtime.indexIngestDispatch.enabled=false --set-string main.runtime.indexIngestDispatch.commandStream=)
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
  --set main.env.ELITEA_DEEPWIKI_GIT_ALLOWLIST=github.com
  --set main.env.ELITEA_DEEPWIKI_CLIENT_CERT_FILE=/run/elitea-deepwiki/tls.crt
  --set main.env.ELITEA_DEEPWIKI_CLIENT_KEY_FILE=/run/elitea-deepwiki/tls.key
  --set main.env.ELITEA_DEEPWIKI_CA_FILE=/run/elitea-deepwiki/ca.crt)
DEEPWIKI_DIRECT=(--set main.env.ELITEA_DEEPWIKI_CALLBACK_BASE_URL=http://elitea-main:8080)
INVENTORY=(--set inventory.enabled=true
  --set inventory.env.ELITEA_INVENTORY_GIT_ALLOWLIST=github.com
  --set inventory.secrets.ELITEA_INVENTORY_DATABASE_URL.secretName=elitea-inventory-db
  --set inventory.secrets.ELITEA_INVENTORY_DATABASE_URL.key=url
  --set main.fileConfig.inventoryClientMaterial.enabled=true
  --set main.fileConfig.inventoryClientMaterial.secretName=elitea-main-inventory-client-tls
  --set main.env.ELITEA_INVENTORY_ENABLED=true
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
# check <name> <worker> <supervisor> <deepwiki: none|direct|platform> <inventory: 0|1>
check() {
  python3 - "$WORK/$1.yaml" "$1" "$2" "$3" "$4" "$5" "${EXPECT_GRPC:-}" "${EXPECT_INV_GRPC:-}" <<'PY'
import sys, yaml

path, name, worker, sup, deepwiki, inventory = sys.argv[1:7]
expect_grpc = sys.argv[7] if len(sys.argv) > 7 else ""
expect_inv_grpc = sys.argv[8] if len(sys.argv) > 8 else ""
worker, sup, inventory = worker == "1", sup == "1", inventory == "1"
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

MAIN, WORKER, SUP = "Deployment/elitea-main", "Deployment/elitea-worker", "Deployment/elitea-sandbox-supervisor"
DW, INV = "Deployment/elitea-deepwiki", "Deployment/elitea-inventory"

expected = {"elitea-main-netpol": MAIN}
if worker:
    expected["elitea-worker-netpol"] = WORKER
if sup:
    expected["elitea-sandbox-supervisor-netpol"] = SUP
if deepwiki != "none":
    expected["elitea-deepwiki-netpol"] = DW
if inventory:
    expected["elitea-inventory-netpol"] = INV
check(sorted(policies) == sorted(expected), f"policy set {sorted(policies)} != {sorted(expected)}")

for pname, target in expected.items():
    pol = policies.get(pname)
    if pol is None:
        continue
    sel = pol["spec"]["podSelector"].get("matchLabels") or {}
    check(bool(sel), f"{pname}: empty podSelector would select every pod")
    # A provider's one-shot migration Job carries the provider's selector
    # labels: the same workload family, not a second pod to protect.
    selected = [n for n in selects(sel) if not n.endswith("-migrate")]
    check(selected == [target], f"{pname}: selects {selected}, want only {target}")
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
    # No platform-edge pod exists any more (#1359): nothing named after it
    # may render, and nothing reaches 8080 on its behalf.
    check(not any("platform-edge" in n for n in pods), "a platform-edge workload is still rendered")
    want_http = sorted(([DW] if deepwiki == "direct" else []) + ([INV] if inventory else []))
    http = by.get((8080,))
    check(http is not None or not want_http, "main: no rule for 8080")
    check(http is None or want_http, "main: 8080 rule without a peer")
    if http:
        got = peer_targets(http, "main:8080")
        check(got == want_http, f"main:8080 admits {got}, want {want_http}")
        check(WORKER not in got, "main:8080 must not admit the worker")
    # runtime listeners: exactly the ports the Deployment declares. control,
    # output and content admit the worker only; the platform listener admits
    # the worker and, when its callback uses it, DeepWiki.
    ctr = deployments["elitea-main"]["spec"]["template"]["spec"]["containers"][0]["ports"]
    rt = sorted(p["containerPort"] for p in ctr if p["name"].startswith("runtime-") and p["name"] != "runtime-platform")
    plat = [p["containerPort"] for p in ctr if p["name"] == "runtime-platform"]
    check(bool(rt) == bool(plat), f"main: declares runtime ports {rt} but platform listener {plat}")
    rts = [k for k in by if k != (8080,) and k != tuple(plat)]
    # The providers' native engines make their model calls over ModelService
    # on the platform listener (#1359 U2) whatever the artifact hop does, so
    # both are admitted there whenever the listener exists (every composed
    # install here runs the native engine).
    want_platform = sorted(([WORKER] if worker else [])
                           + ([DW] if deepwiki != "none" and plat else [])
                           + ([INV] if inventory and plat else []))
    if rt and worker:
        check(rts == [tuple(rt)], f"main: runtime rule ports {rts}, want {[tuple(rt)]}")
        if rts:
            check(peer_targets(by[rts[0]], "main:runtime") == [WORKER], "main: runtime listeners must admit the worker only")
    else:
        check(not rts, f"main: unexpected rules for ports {rts} (runtime={rt}, worker={worker})")
    platform_rule = by.get(tuple(plat)) if plat else None
    if want_platform:
        check(platform_rule is not None, f"main: no rule for the platform listener {plat}")
        if platform_rule:
            got = peer_targets(platform_rule, "main:platform")
            check(got == want_platform, f"main:platform admits {got}, want {want_platform}")
    else:
        check(platform_rule is None, f"main: platform listener rule without a peer")
    check(sorted(k for r in by for k in r) == sorted(([8080] if want_http else []) + (rt if worker else []) + (plat if want_platform else [])),
          "main: ports admitted are not exactly 8080 + runtime + platform")

# ── the providers (deepwiki, inventory) ─────────────────────────────────────
# One port each (#1359): the gRPC listener (provider SPI + platform service),
# declared, configured, exposed and admitted together, to elitea-main only.
for key, want_port in (("deepwiki", expect_grpc), ("inventory", expect_inv_grpc)):
    pol = policies.get(f"elitea-{key}-netpol")
    if not pol:
        continue
    spec = pol["spec"]
    check(spec["policyTypes"] == ["Ingress"], f"{key}: policyTypes must be Ingress only")
    by = rules_by_ports(spec["ingress"], key)
    ctr = deployments[f"elitea-{key}"]["spec"]["template"]["spec"]["containers"][0]
    declared = {p["name"]: p["containerPort"] for p in ctr["ports"]}
    grpc_port = declared.get("platform-grpc")
    check(list(declared) == ["platform-grpc"], f"{key}: container ports {declared}, want the gRPC listener only (no HTTP SPI)")
    check(sorted(by) == [(grpc_port,)], f"{key}: ports admitted {sorted(by)}, want [({grpc_port},)]")
    for k, rule in by.items():
        check(peer_targets(rule, f"{key}:{k}") == [MAIN], f"{key}:{k} must admit elitea-main only")
    env = {e["name"]: e.get("value") for e in ctr.get("env", [])}
    service = [d for d in docs if d["kind"] == "Service" and d["metadata"]["name"] == f"elitea-{key}-svc"][0]
    service_ports = {p["name"]: p for p in service["spec"]["ports"]}
    K = key.upper()
    check(env.get(f"ELITEA_{K}_PLATFORM_CLIENTS"), f"{key}: the gRPC port is declared with no allowed clients")
    check(env.get(f"ELITEA_{K}_PLATFORM_GRPC_ADDR") == f":{grpc_port}", f"{key}: the gRPC address is {env.get(f'ELITEA_{K}_PLATFORM_GRPC_ADDR')}")
    check(list(service_ports) == ["platform-grpc"] and service_ports["platform-grpc"]["targetPort"] == "platform-grpc",
          f"{key}: the Service ports are {list(service_ports)}, want platform-grpc only")
    if want_port:
        check(str(grpc_port) == want_port, f"{key}: gRPC port {grpc_port}, expected {want_port}")

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

run() { # run <name> <worker> <supervisor> <deepwiki> <inventory> -- <helm args>
  local name="$1" w="$2" s="$3" d="$4" i="$5"; shift 6
  # values-kind.yaml states the decision itself; every other profile does not.
  if [ "$name" = kind ]; then render "$name" "$@" || return 0; else render "$name" "${NOEXT[@]}" "$@" || return 0; fi
  if out="$(check "$name" "$w" "$s" "$d" "$i" 2>&1)"; then pass "$out"; else fail "$out"; fi
}

echo "== profiles =="
run default      0 0 none 0 --
run standalone   0 0 none 0 -- "${STANDALONE[@]}"
run auth-minimal 0 0 none 0 -- -f "$CHART/values-auth-minimal.yaml"
run staging      0 0 none 0 -- -f "$CHART/values-staging.yaml"
# values-kind.yaml runs DeepWiki with its callback on elitea-main:8080.
run kind         0 0 direct 0 -- -f "$REPO/deploy/kind/values-kind.yaml"
echo "== composed installs =="
run worker       1 0 none 0 -- "${STANDALONE[@]}" "${WORKER[@]}"
run full         1 1 direct 1 -- "${STANDALONE[@]}" "${WORKER[@]}" "${SUPERVISOR[@]}" "${DEEPWIKI[@]}" "${DEEPWIKI_DIRECT[@]}" "${INVENTORY[@]}"
run full-via-platform 1 1 platform 1 -- "${STANDALONE[@]}" "${WORKER[@]}" "${SUPERVISOR[@]}" "${DEEPWIKI[@]}" --set deepwiki.callbackViaPlatformListener=true --set main.env.ELITEA_DEEPWIKI_CALLBACK_BASE_URL= "${INVENTORY[@]}"
# The one gRPC port follows platformGrpc.port everywhere it appears.
EXPECT_GRPC=9555 run deepwiki-grpc-port 0 0 direct 0 -- "${STANDALONE[@]}" "${DEEPWIKI[@]}" "${DEEPWIKI_DIRECT[@]}" --set deepwiki.platformGrpc.port=9555
EXPECT_INV_GRPC=9555 run inventory-grpc-port 0 0 none 1 -- "${STANDALONE[@]}" "${INVENTORY[@]}" --set inventory.platformGrpc.port=9555

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
refuses "a values file that still turns platform-edge on" "worker.platformEdge was removed" \
  "${STANDALONE[@]}" "${NOEXT[@]}" "${WORKER[@]}" --set worker.platformEdge.enabled=true
refuses "noExternalIngress with main.ingress.enabled (gateway-api)" "contradicts main.ingress.enabled" \
  "${STANDALONE[@]}" "${NOEXT[@]}" --set main.ingress.enabled=true --set networkPolicies.main.ingressFrom[0].podSelector.matchLabels.x=y --set main.ingress.gatewayApi=true \
  --set main.ingress.gateway.name=shared-gateway --set main.ingress.gateway.namespace=gateway-system
refuses "deepwiki without mutual TLS" "deepwiki.mtls.enabled is false, but the provider serves gRPC over mutual TLS only" \
  "${STANDALONE[@]}" "${NOEXT[@]}" "${DEEPWIKI[@]}" "${DEEPWIKI_DIRECT[@]}" --set deepwiki.mtls.enabled=false
refuses "inventory without mutual TLS" "inventory.mtls.enabled is false, but the provider serves gRPC over mutual TLS only" \
  "${STANDALONE[@]}" "${NOEXT[@]}" "${INVENTORY[@]}" --set inventory.mtls.enabled=false
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
assert rt and all(gw not in r["from"] and extra not in r["from"] for r in rt), "operator peers must not reach the runtime or platform listeners"
for n in ("elitea-sandbox-supervisor-netpol",):
    assert any(probe in r["from"] for r in pols[n]["spec"]["ingress"]), n + " lacks probeFrom"
print("peers present: 8080 admits gateway, extra and probe peers; runtime and platform listeners do not; supervisor admits probeFrom")
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
