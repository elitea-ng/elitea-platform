#!/usr/bin/env bash
# render-nats-security.sh — the NATS security posture (#1076), asserted on the
# RENDERED output of all three charts that have to agree about it:
#
#   deploy/helm/nats            the server: NATS CA, server certificate,
#                               TLS + verify_and_map, the permission table,
#                               the ingress NetworkPolicy, its refusals
#   deploy/helm/nats-bootstrap  the bootstrap identity and its connection
#   deploy/helm/elitea          one client certificate per component, the
#                               mounts and env that point at it, tls:// URLs,
#                               and the URL refusals
#
# The agreement is the point. Each chart can be internally fine while the
# certificate a component presents names no user, is signed by an issuer the
# server does not trust, or comes from a pod the NetworkPolicy does not admit.
# Every one of those installs cleanly and then fails every connection.
#
# What this does NOT prove: that a grant in the permission table is the right
# one. libs/go/natsconn/natstest runs every service's real code against the
# rendered config for that (ci-go.yml, ci-gateway.yml).
#
# Needs helm, python3 + PyYAML, and network access the first time (the
# upstream nats subchart is vendored with `helm dependency build`).
set -euo pipefail

DIR="$(cd "$(dirname "$0")/../.." && pwd)"   # deploy/
HELM="${HELM:-helm}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

if ! ls "$DIR"/helm/nats/charts/nats-*.tgz >/dev/null 2>&1; then
  "$HELM" dependency build "$DIR/helm/nats" >/dev/null
fi

NS=elitea
"$HELM" template elitea-nats "$DIR/helm/nats" -n "$NS" -f "$DIR/helm/nats/values-scale1.yaml" > "$TMP/nats-scale1.yaml"
"$HELM" template elitea-nats "$DIR/helm/nats" -n "$NS" -f "$DIR/helm/nats/values-ha.yaml"     > "$TMP/nats-ha.yaml"
# The same HA render on a cluster that serves approver-policy (#1076 F3).
"$HELM" template elitea-nats "$DIR/helm/nats" -n "$NS" -f "$DIR/helm/nats/values-ha.yaml" \
  --api-versions policy.cert-manager.io/v1alpha1/CertificateRequestPolicy > "$TMP/nats-ha-ap.yaml"
"$HELM" template elitea-nats-bootstrap "$DIR/helm/nats-bootstrap" -n "$NS"                    > "$TMP/bootstrap.yaml"
# The command-bus alerts (#1081 review G3): with the Prometheus Operator
# served, the rule renders together with the exporter whose series it reads.
"$HELM" template elitea-nats "$DIR/helm/nats" -n "$NS" -f "$DIR/helm/nats/values-scale1.yaml" \
  --api-versions monitoring.coreos.com/v1 > "$TMP/nats-base-prom.yaml"
"$HELM" template elitea "$DIR/helm/elitea" -n "$NS" \
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://render-only.example.invalid/llm/v1 \
  --set-string llmGateway.egressPosture=public-unrestricted > "$TMP/elitea.yaml"
# The standalone profile: the runtime plane on, so the command bus's
# identities (elitea-main-runtime, elitea-worker) render too.
"$HELM" template elitea "$DIR/helm/elitea" -n "$NS" -f "$DIR/helm/elitea/values-standalone.yaml" --set worker.enabled=true \
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://render-only.example.invalid/llm/v1 \
  --set-string llmGateway.egressPosture=public-unrestricted > "$TMP/elitea-runtime.yaml"

# Schema validation of the NATS renders (R9). The template job in helm-lint.yml
# runs kubeconform on the charts it can template without network; these two
# vendor an upstream subchart, so this suite is where their renders exist.
# KUBECONFORM names the binary; CI sets NATS_REQUIRE_KUBECONFORM=1 so a missing
# binary fails instead of reading as a pass. The approver-policy schema is not
# in the pinned CRD catalog release, so it comes from the catalog commit that
# added it (pinned by SHA).
KUBECONFORM="${KUBECONFORM:-}"
# (Not ${CRD_SCHEMAS:-...}: the template's own braces would end the expansion.)
if [ -z "${CRD_SCHEMAS:-}" ]; then
  CRD_SCHEMAS='https://raw.githubusercontent.com/datreeio/CRDs-catalog/v0.0.12/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'
fi
POLICY_SCHEMAS='https://raw.githubusercontent.com/datreeio/CRDs-catalog/b2fd9c93e43e444946da75401bfa0eac8eeb9a38/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'
: > "$TMP/refusals"
if [ -n "$KUBECONFORM" ]; then
  if "$KUBECONFORM" -strict -summary -kubernetes-version 1.30.0 \
      -schema-location default -schema-location "$CRD_SCHEMAS" -schema-location "$POLICY_SCHEMAS" \
      "$TMP/nats-scale1.yaml" "$TMP/nats-ha.yaml" "$TMP/nats-ha-ap.yaml" "$TMP/bootstrap.yaml" > "$TMP/kubeconform.out" 2>&1; then
    echo "ok	kubeconform: the nats (scale-1, HA, HA + approver-policy) and nats-bootstrap renders are schema-valid" >> "$TMP/refusals"
  else
    echo "KUBECONFORM-FAILED	kubeconform on the NATS renders	$(tr '\n' ' ' < "$TMP/kubeconform.out")" >> "$TMP/refusals"
  fi
elif [ "${NATS_REQUIRE_KUBECONFORM:-0}" = "1" ]; then
  echo "KUBECONFORM-MISSING	kubeconform on the NATS renders	NATS_REQUIRE_KUBECONFORM=1 and KUBECONFORM is unset" >> "$TMP/refusals"
else
  echo "kubeconform: skipped (set KUBECONFORM to a kubeconform binary to validate the NATS renders)" >&2
fi

# Refusals: each render below MUST fail, with the reason named.
refuse() {
  local name="$1" want="$2"; shift 2
  if "$HELM" template "$@" > /dev/null 2> "$TMP/refusal.err"; then
    echo "REFUSAL-NOT-RAISED	$name" >> "$TMP/refusals"
  elif grep -qF -- "$want" "$TMP/refusal.err"; then
    echo "ok	$name" >> "$TMP/refusals"
  else
    echo "WRONG-REASON	$name	$(tail -1 "$TMP/refusal.err")" >> "$TMP/refusals"
  fi
}
EL=(elitea "$DIR/helm/elitea" -n "$NS"
    --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://render-only.example.invalid/llm/v1
    --set-string llmGateway.egressPosture=public-unrestricted)
NA=(elitea-nats "$DIR/helm/nats" -n "$NS" -f "$DIR/helm/nats/values-scale1.yaml")
refuse "nats: verify_and_map off"             "verify_and_map must be true"  "${NA[@]}" --set nats.config.nats.tls.merge.verify_and_map=false
refuse "nats: TLS off"                        "tls.enabled is false"         "${NA[@]}" --set nats.config.nats.tls.enabled=false
refuse "nats: no_auth_user"                   "no_auth_user"                 "${NA[@]}" --set nats.config.merge.no_auth_user=anyone
refuse "nats: allow_non_tls"                  "allow_non_tls"                "${NA[@]}" --set nats.config.merge.allow_non_tls=true
refuse "nats: users in the global account"    "authorization is set"         "${NA[@]}" --set 'nats.config.merge.authorization.users[0].user=spiffe://elitea.internal/nats/elitea-main'
refuse "nats: a plane's account removed"      "exactly MAIN, GATEWAY, SCHEDULER, RUNTIME and WORKER" "${NA[@]}" --set nats.config.merge.accounts.RUNTIME=null
refuse "nats: the scheduler's account removed" "exactly MAIN, GATEWAY, SCHEDULER, RUNTIME and WORKER" "${NA[@]}" --set nats.config.merge.accounts.SCHEDULER=null
refuse "nats: the worker's account removed"   "exactly MAIN, GATEWAY, SCHEDULER, RUNTIME and WORKER" "${NA[@]}" --set nats.config.merge.accounts.WORKER=null
refuse "nats: an extra account"               "exactly MAIN, GATEWAY, SCHEDULER, RUNTIME and WORKER" "${NA[@]}" --set nats.config.merge.accounts.EXTRA.jetstream=enabled
refuse "nats: WORKER JetStream unbounded"     "max_streams: 1"               "${NA[@]}" --set nats.config.merge.accounts.WORKER.jetstream=enabled
refuse "nats: WORKER JetStream widened"       "max_streams: 1"               "${NA[@]}" --set nats.config.merge.accounts.WORKER.jetstream.max_streams=5
refuse "nats: WORKER exports"                 "account WORKER exports"       "${NA[@]}" --set 'nats.config.merge.accounts.WORKER.exports[0].stream=elitea.>' 
refuse "nats: GATEWAY export widened"         "is neither the soft-alert stream" "${NA[@]}" --set 'nats.config.merge.accounts.GATEWAY.exports[0].stream=gateway.>' --set 'nats.config.merge.accounts.GATEWAY.exports[0].accounts[0]=MAIN'
refuse "nats: SCHEDULER gets JetStream"       "holds NO streams"             "${NA[@]}" --set nats.config.merge.accounts.SCHEDULER.jetstream=enabled
refuse "nats: SCHEDULER exports"              "account SCHEDULER exports"    "${NA[@]}" --set 'nats.config.merge.accounts.SCHEDULER.exports[0].stream=elitea.>'
refuse "nats: MAIN exports"                   "account MAIN exports"         "${NA[@]}" --set 'nats.config.merge.accounts.MAIN.exports[0].stream=elitea.>'
refuse "nats: RUNTIME imports"                "account RUNTIME imports"      "${NA[@]}" --set 'nats.config.merge.accounts.RUNTIME.imports[0].stream.account=GATEWAY' --set 'nats.config.merge.accounts.RUNTIME.imports[0].stream.subject=gateway.events.project.*.events'
# Mutations of one user's grants: a values file derived from values.yaml, so
# the rest of the table stays as shipped.
mutate() {
  local out="$1" expr="$2"
  python3 - "$DIR/helm/nats/values.yaml" "$out" "$expr" <<'PYM'
import sys, yaml
v = yaml.safe_load(open(sys.argv[1]))
accts = v["nats"]["config"]["merge"]["accounts"]
def user(acct, ident):
    return next(u for u in accts[acct]["users"] if u["user"].endswith("/nats/" + ident))
exec(sys.argv[3])
yaml.safe_dump({"nats": {"config": {"merge": {"accounts": accts}}}}, open(sys.argv[2], "w"))
PYM
}
mutate "$TMP/m-bootstrap-purge.yaml" 'user("GATEWAY","elitea-nats-bootstrap-gateway")["permissions"]["publish"]["allow"].append("$JS.API.STREAM.PURGE.GATEWAY_BUDGET")'
refuse "nats: a bootstrap may purge"          "Nobody deletes or purges"     "${NA[@]}" -f "$TMP/m-bootstrap-purge.yaml"
mutate "$TMP/m-gw-create.yaml" 'user("GATEWAY","elitea-llm-gateway")["permissions"]["publish"]["allow"].append("$JS.API.STREAM.CREATE.GATEWAY_BUDGET")'
refuse "nats: a service may create a stream"  "Only the account's bootstrap" "${NA[@]}" -f "$TMP/m-gw-create.yaml"
mutate "$TMP/m-sched-consumer.yaml" 'user("SCHEDULER","elitea-scheduler")["permissions"]["publish"]["allow"].append("$JS.API.CONSUMER.CREATE.GATEWAY_BUDGET_DELTAS.budget-writeback.>")'
refuse "nats: the scheduler may create its consumer" "only the account's bootstrap creates consumers" "${NA[@]}" -f "$TMP/m-sched-consumer.yaml"
# The scheduler's way into GATEWAY is exactly three service subjects on its
# one consumer, imported under its JetStream API prefix (#1076 reply-subject fix).
mutate "$TMP/m-sched-back.yaml" 'accts["GATEWAY"]["users"].append(dict(user("SCHEDULER","elitea-scheduler"))); accts["SCHEDULER"]["users"] = [u for u in accts["SCHEDULER"]["users"] if not u["user"].endswith("/elitea-scheduler")]'
refuse "nats: the scheduler back in GATEWAY"  "The scheduler belongs in SCHEDULER"   "${NA[@]}" -f "$TMP/m-sched-back.yaml"
mutate "$TMP/m-svc-any-consumer.yaml" 'next(e for e in accts["GATEWAY"]["exports"] if "MSG.NEXT" in e.get("service",""))["service"] = "$JS.API.CONSUMER.MSG.NEXT.GATEWAY_BUDGET_DELTAS.*"'
refuse "nats: pull export widened to any consumer" "budget-writeback services" "${NA[@]}" -f "$TMP/m-svc-any-consumer.yaml"
mutate "$TMP/m-svc-to-main.yaml" 'next(e for e in accts["GATEWAY"]["exports"] if "MSG.NEXT" in e.get("service",""))["accounts"] = ["SCHEDULER", "MAIN"]'
refuse "nats: pull export to another account" "budget-writeback services"   "${NA[@]}" -f "$TMP/m-svc-to-main.yaml"
mutate "$TMP/m-svc-api.yaml" 'accts["GATEWAY"]["exports"].append({"service": "$JS.API.CONSUMER.CREATE.GATEWAY_BUDGET_DELTAS.budget-writeback.>", "accounts": ["SCHEDULER"]})'
refuse "nats: a consumer-create service export" "budget-writeback services"  "${NA[@]}" -f "$TMP/m-svc-api.yaml"
mutate "$TMP/m-svc-rt.yaml" 'next(e for e in accts["GATEWAY"]["exports"] if "MSG.NEXT" in e.get("service","")).pop("response_type")'
refuse "nats: pull export loses response_type stream" "budget-writeback services" "${NA[@]}" -f "$TMP/m-svc-rt.yaml"
mutate "$TMP/m-imp-to.yaml" 'next(i for i in accts["SCHEDULER"]["imports"] if "MSG.NEXT" in i["service"]["subject"])["to"] = "$JS.API.CONSUMER.MSG.NEXT.GATEWAY_BUDGET_DELTAS.budget-writeback"'
refuse "nats: SCHEDULER import off the API prefix" "SCHEDULER import"        "${NA[@]}" -f "$TMP/m-imp-to.yaml"
mutate "$TMP/m-imp-other.yaml" 'accts["SCHEDULER"]["imports"].append({"service": {"account": "GATEWAY", "subject": "gateway.budget.delta"}})'
refuse "nats: SCHEDULER imports another subject" "SCHEDULER import"          "${NA[@]}" -f "$TMP/m-imp-other.yaml"
# The worker's way into RUNTIME (#1081 review S1): nine service subjects on its
# three durables, imported under its JetStream API prefix.
mutate "$TMP/m-worker-back.yaml" 'accts["RUNTIME"]["users"].append(dict(user("WORKER","elitea-worker"))); accts["WORKER"]["users"] = [u for u in accts["WORKER"]["users"] if not u["user"].endswith("/elitea-worker")]'
refuse "nats: the worker back in RUNTIME"     "The worker belongs in WORKER"  "${NA[@]}" -f "$TMP/m-worker-back.yaml"
mutate "$TMP/m-worker-extra.yaml" 'accts["WORKER"]["users"].append({"user": "spiffe://elitea.internal/nats/elitea-other"})'
refuse "nats: another user in WORKER"         "holds elitea-worker and its bootstrap only" "${NA[@]}" -f "$TMP/m-worker-extra.yaml"
mutate "$TMP/m-rt-any-consumer.yaml" 'next(e for e in accts["RUNTIME"]["exports"] if "MSG.NEXT.ELITEA_RT_V1_AGENT" in e.get("service",""))["service"] = "$JS.API.CONSUMER.MSG.NEXT.ELITEA_RT_V1_AGENT.*"'
refuse "nats: RUNTIME pull export widened"    "worker-durable services"      "${NA[@]}" -f "$TMP/m-rt-any-consumer.yaml"
mutate "$TMP/m-rt-to-main.yaml" 'next(e for e in accts["RUNTIME"]["exports"] if "MSG.NEXT" in e.get("service",""))["accounts"] = ["WORKER", "MAIN"]'
refuse "nats: RUNTIME export to another account" "worker-durable services"   "${NA[@]}" -f "$TMP/m-rt-to-main.yaml"
mutate "$TMP/m-rt-stream-info.yaml" 'accts["RUNTIME"]["exports"].append({"service": "$JS.API.STREAM.INFO.ELITEA_RT_V1_AGENT", "accounts": ["WORKER"]})'
refuse "nats: RUNTIME exports stream info"    "worker-durable services"      "${NA[@]}" -f "$TMP/m-rt-stream-info.yaml"
mutate "$TMP/m-rt-rt.yaml" 'next(e for e in accts["RUNTIME"]["exports"] if "MSG.NEXT" in e.get("service","")).pop("response_type")'
refuse "nats: RUNTIME pull export loses response_type stream" "worker-durable services" "${NA[@]}" -f "$TMP/m-rt-rt.yaml"
mutate "$TMP/m-rt-missing.yaml" 'accts["RUNTIME"]["exports"] = [e for e in accts["RUNTIME"]["exports"] if "INDEX" not in e["service"]]; accts["WORKER"]["imports"] = [i for i in accts["WORKER"]["imports"] if "INDEX" not in i["service"]["subject"]]'
refuse "nats: a worker durable not exported"  "worker-durable services"      "${NA[@]}" -f "$TMP/m-rt-missing.yaml"
mutate "$TMP/m-wimp-to.yaml" 'next(i for i in accts["WORKER"]["imports"] if "MSG.NEXT" in i["service"]["subject"])["to"] = "$JS.API.CONSUMER.MSG.NEXT.ELITEA_RT_V1_VALIDATE.elitea-configuration-worker-v1"'
refuse "nats: WORKER import off the API prefix" "WORKER import"              "${NA[@]}" -f "$TMP/m-wimp-to.yaml"
mutate "$TMP/m-wimp-other.yaml" 'accts["WORKER"]["imports"].append({"service": {"account": "RUNTIME", "subject": "elitea.rt.v1.agent.d.*"}})'
refuse "nats: WORKER imports a command subject" "WORKER import"             "${NA[@]}" -f "$TMP/m-wimp-other.yaml"
mutate "$TMP/m-main-consumer.yaml" 'user("MAIN","elitea-main")["permissions"]["publish"]["allow"].append("$JS.API.CONSUMER.CREATE.KV_OTHER.>")'
refuse "nats: main may create a consumer off its bucket" "only the account's bootstrap creates consumers" "${NA[@]}" -f "$TMP/m-main-consumer.yaml"
mutate "$TMP/m-dup.yaml" 'accts["RUNTIME"]["users"].append(dict(user("MAIN","elitea-main")))'
refuse "nats: one identity in two accounts"   "is declared in accounts"      "${NA[@]}" -f "$TMP/m-dup.yaml"
mutate "$TMP/m-wrong-bootstrap.yaml" 'accts["MAIN"]["users"].append({"user": "spiffe://elitea.internal/nats/elitea-nats-bootstrap-extra"})'
refuse "nats: another plane's bootstrap"      "is a bootstrap identity in account MAIN" "${NA[@]}" -f "$TMP/m-wrong-bootstrap.yaml"
mutate "$TMP/m-password.yaml" 'user("MAIN","elitea-main")["password"] = "x"'
refuse "nats: a password user"                "carries a password"           "${NA[@]}" -f "$TMP/m-password.yaml"
refuse "nats: NetworkPolicy off, unstated"    "networkPolicy.enabled is false" "${NA[@]}" --set networkPolicy.enabled=false
refuse "nats: alerts with the exporter off"   "nats.promExporter.enabled is false" "${NA[@]}" --set nats.promExporter.enabled=false
HA=(elitea-nats "$DIR/helm/nats" -n "$NS" -f "$DIR/helm/nats/values-ha.yaml")
refuse "nats: HA routes trust the client CA"   "must be /etc/nats-certs/cluster/ca.crt" "${HA[@]}" --set nats.config.cluster.tls.merge.ca_file=/etc/nats-ca-cert/ca.crt
refuse "nats: HA routes reuse the client cert" "ROUTE certificate's own Secret" "${HA[@]}" --set nats.config.cluster.tls.secretName=elitea-nats-server-tls
refuse "nats: HA routes do not verify"         "verify must be true"          "${HA[@]}" --set nats.config.cluster.tls.merge.verify=false
refuse "nats: HA route issuer = client issuer" "same issuer as security.issuerRef" "${HA[@]}" --set security.ca.create=false --set security.issuerRef.name=nats-ca --set security.routeIssuerRef.name=nats-ca
refuse "nats: approver-policy required, not served" "does not serve policy.cert-manager.io" "${NA[@]}" --set security.approverPolicy.enabled=true
refuse "nats: HA routes in plaintext"         "cluster.tls.enabled false"    elitea-nats "$DIR/helm/nats" -n "$NS" -f "$DIR/helm/nats/values-ha.yaml" --set nats.config.cluster.tls.enabled=false
refuse "bootstrap: nats:// URL"               "is not tls://"                elitea-nats-bootstrap "$DIR/helm/nats-bootstrap" --set natsUrl=nats://elitea-nats:4222
refuse "bootstrap: credential URL"            "user information"             elitea-nats-bootstrap "$DIR/helm/nats-bootstrap" --set natsUrl=tls://u:p@elitea-nats:4222
refuse "bootstrap: connectWait eats the deadline" "leaves the Job under a minute" elitea-nats-bootstrap "$DIR/helm/nats-bootstrap" --set connectWait=590
refuse "bootstrap: an account left out"       "must be [main, gateway, runtime, worker]" elitea-nats-bootstrap "$DIR/helm/nats-bootstrap" --set 'accounts={main,gateway,runtime}'
refuse "elitea: plaintext unacknowledged"     "allowPlaintext"               "${EL[@]}" --set nats.tls.enabled=false
refuse "elitea: nats:// gateway URL"          "not tls://"                   "${EL[@]}" --set-string llmGateway.env.GATEWAY_NATS_URL=nats://elitea-nats:4222
refuse "elitea: nats:// main URL"             "not tls://"                   "${EL[@]}" --set-string main.env.ELITEA_EVENTS_NATS_URL=nats://elitea-nats:4222
refuse "elitea: credential in a scheduler URL" "user information"            "${EL[@]}" --set-string scheduler.env.GATEWAY_NATS_URL=tls://u:p@elitea-nats:4222
refuse "elitea: NATS in another namespace, namespaced Issuer" "is a namespaced Issuer" "${EL[@]}" --set nats.namespace=nats-elsewhere
refuse "elitea: renamed NATS client, policy not updated" "would be dropped at the NATS port" "${EL[@]}" --set scheduler.nameOverride=sched
refuse "bootstrap: renamed Job pod, policy not updated" "would time out at the NATS port" elitea-nats-bootstrap "$DIR/helm/nats-bootstrap" --set nameOverride=nb
refuse "elitea: tls:// with TLS off"          "tls:// but nats.tls.enabled is false" "${EL[@]}" --set nats.tls.enabled=false --set nats.tls.allowPlaintext=true --set-string llmGateway.env.GATEWAY_NATS_URL=tls://elitea-nats:4222
# A renamed client renders once the operator states the policy matches.
if "$HELM" template "${EL[@]}" --set scheduler.nameOverride=sched --set nats.networkPolicyClientNamesManaged=true > /dev/null 2> "$TMP/rn.err"; then
  echo "ok	elitea: a renamed client with the policy managed renders" >> "$TMP/refusals"
else
  echo "REFUSED-A-VALID-TOPOLOGY	elitea: renamed client, managed	$(tail -1 "$TMP/rn.err")" >> "$TMP/refusals"
fi
# NATS in another namespace renders with a ClusterIssuer.
if "$HELM" template "${EL[@]}" --set nats.namespace=nats-elsewhere --set nats.tls.issuerRef.kind=ClusterIssuer --set nats.tls.issuerRef.name=nats-ca > /dev/null 2> "$TMP/ci.err"; then
  echo "ok	elitea: NATS elsewhere with a ClusterIssuer renders" >> "$TMP/refusals"
else
  echo "REFUSED-A-VALID-TOPOLOGY	elitea: NATS elsewhere with a ClusterIssuer	$(tail -1 "$TMP/ci.err")" >> "$TMP/refusals"
fi
# And the one acknowledged way out renders.
if "$HELM" template "${EL[@]}" --set nats.tls.enabled=false --set nats.tls.allowPlaintext=true > "$TMP/plain.yaml" 2> "$TMP/plain.err"; then
  echo "ok	elitea: acknowledged plaintext renders" >> "$TMP/refusals"
else
  echo "REFUSED-THE-ESCAPE-HATCH	elitea: acknowledged plaintext	$(tail -1 "$TMP/plain.err")" >> "$TMP/refusals"
fi

python3 - "$TMP" "$NS" "$DIR/argocd/applications" <<'PY'
import json, pathlib, re, sys, yaml

tmp, ns, apps = pathlib.Path(sys.argv[1]), sys.argv[2], pathlib.Path(sys.argv[3])
results = []

def check(name, cond, detail=""):
    results.append((name, bool(cond), detail))

def docs(f):
    return [d for d in yaml.safe_load_all(open(tmp / f)) if d]

def conf_json(text):
    # The upstream chart renders JSON with unquoted `<< >>` values and
    # include directives; quote the one variable so json can read the rest.
    text = re.sub(r':\s*\$SERVER_NAME', ': "$SERVER_NAME"', text)
    text = re.sub(r':\s*(\d+)(Gi|Mi|Ki)\b', r': "\1\2"', text)
    return json.loads(text)

IDS = {
    "MAIN": ["elitea-main", "elitea-nats-bootstrap-main"],
    "GATEWAY": ["elitea-llm-gateway", "elitea-nats-bootstrap-gateway"],
    "SCHEDULER": ["elitea-scheduler"],
    "RUNTIME": ["elitea-main-runtime", "elitea-nats-bootstrap-runtime"],
    "WORKER": ["elitea-worker", "elitea-nats-bootstrap-worker"],
}
ALL = [i for ids in IDS.values() for i in ids]
URI = lambda i: f"spiffe://elitea.internal/nats/{i}"
ALERTS = "gateway.events.project.*.events"

# ── the server ────────────────────────────────────────────────────────────
scale1, ha = docs("nats-scale1.yaml"), docs("nats-ha.yaml")
base = docs("nats-base-prom.yaml")
rules = [x for x in base if x["kind"] == "PrometheusRule"]
exporter = [c for x in base if x["kind"] == "StatefulSet" for c in x["spec"]["template"]["spec"]["containers"] if c["name"] == "prom-exporter"]
check("the alert rule renders with the exporter it reads (jsz series, nats_ prefix)", bool(rules) and bool(exporter) and "-jsz=all" in exporter[0].get("args", []) and "-prefix=nats" in exporter[0].get("args", []), exporter[0].get("args") if exporter else None)
dl = [r for x in rules for g in x["spec"]["groups"] for r in g["rules"] if r.get("alert") == "EliteaRuntimeCommandDeadLettered"]
check("the dead-letter alert fires on NEW records (last_seq increase), not on delete markers left until the TTL",
      dl and "increase(nats_stream_last_seq" in dl[0]["expr"] and "KV_ELITEA_RT_V1_DEADLETTER" in dl[0]["expr"] and "total_messages" not in dl[0]["expr"], dl)
def nats_conf(d):
    cm = [x for x in d if x["kind"] == "ConfigMap" and "nats.conf" in x.get("data", {})]
    return conf_json(cm[0]["data"]["nats.conf"])
c1, c3 = nats_conf(scale1), nats_conf(ha)

for label, c in (("scale-1", c1), ("HA", c3)):
    tls = c.get("tls", {})
    check(f"{label}: client port TLS with verify_and_map and the NATS CA", tls.get("verify_and_map") is True and tls.get("ca_file", "").endswith("ca.crt") and tls.get("cert_file"))
    check(f"{label}: no no_auth_user, no allow_non_tls, no users in the global account", not any(k in c for k in ("no_auth_user", "allow_non_tls", "authorization")))
    check(f"{label}: http monitor stays on 8222 (exporter over localhost, kubelet probes)", c.get("http_port") == 8222)
check("both profiles carry the SAME accounts and permission table", c1["accounts"] == c3["accounts"])
accts = c1["accounts"]
check("one account per plane: exactly MAIN, GATEWAY, SCHEDULER, RUNTIME and WORKER", set(accts) == set(IDS), sorted(accts))
users = {}
for acct, a in accts.items():
    if acct == "SCHEDULER":
        check("SCHEDULER: no JetStream (it holds no stream a reply subject could land in)", a.get("jetstream") in (None, "disabled"), a.get("jetstream"))
    elif acct == "WORKER":
        check("WORKER: JetStream bounded to one stream (its dead-letter bucket)", isinstance(a.get("jetstream"), dict) and a["jetstream"].get("max_streams") == 1, a.get("jetstream"))
    else:
        check(f"{acct}: JetStream enabled in the account", a.get("jetstream") == "enabled")
    names = [u["user"] for u in a.get("users", [])]
    check(f"{acct}: declares exactly its plane's identities", set(names) == {URI(i) for i in IDS.get(acct, [])}, sorted(names))
    for u in a.get("users", []):
        users[u["user"]] = u.get("permissions", {})
check("every identity is declared once", len(users) == len(ALL))
for i in ALL:
    sub = users.get(URI(i), {}).get("subscribe", {}).get("allow", [])
    check(f"{i} subscribes to its own inbox prefix", f"_INBOX_{i}.>" in sub, sub)
    others = [s for s in sub if s.startswith("_INBOX")  and s != f"_INBOX_{i}.>"]
    check(f"{i} subscribes to no other inbox", not others, others)
destroy = re.compile(r"^\$JS\.API\.(STREAM\.(DELETE|PURGE|MSG\.DELETE)|ACCOUNT\.PURGE)")
admin = re.compile(r"^\$JS\.API\.STREAM\.(CREATE|UPDATE)")
for i in ALL:
    pub = users.get(URI(i), {}).get("publish", {}).get("allow", [])
    check(f"{i} may not delete or purge a stream", not [p for p in pub if destroy.match(p) or p in (">", "$JS.API.>")])
    consumers = [p for p in pub if re.match(r"^\$JS\.API\.CONSUMER\.(DURABLE\.)?CREATE", p)]
    if i.startswith("elitea-nats-bootstrap-"):
        pass
    elif i == "elitea-main":
        check("elitea-main creates consumers only on its own presence bucket (the watcher)", all(p.startswith("$JS.API.CONSUMER.CREATE.KV_ELITEA_CANVAS_PRESENCE.") for p in consumers), consumers)
    else:
        check(f"{i} may not create a consumer", not consumers, consumers)
    made = [p for p in pub if admin.match(p)]
    if i.startswith("elitea-nats-bootstrap-"):
        check(f"{i} may create and update streams", made, pub)
    else:
        check(f"{i} may not create or update a stream", not made, made)
WB = "GATEWAY_BUDGET_DELTAS.budget-writeback"
check("GATEWAY exports exactly the soft-alert stream to MAIN and the budget-writeback services to SCHEDULER",
      accts["GATEWAY"].get("exports") == [
          {"stream": ALERTS, "accounts": ["MAIN"]},
          {"service": f"$JS.API.CONSUMER.INFO.{WB}", "accounts": ["SCHEDULER"]},
          {"service": f"$JS.API.CONSUMER.MSG.NEXT.{WB}", "response_type": "stream", "accounts": ["SCHEDULER"]},
          {"service": f"$JS.ACK.{WB}.>", "accounts": ["SCHEDULER"]},
      ], accts["GATEWAY"].get("exports"))
check("SCHEDULER imports exactly those services, the API under the scheduler's prefix JS.GATEWAY.API",
      accts["SCHEDULER"].get("imports") == [
          {"service": {"account": "GATEWAY", "subject": f"$JS.API.CONSUMER.INFO.{WB}"}, "to": f"JS.GATEWAY.API.CONSUMER.INFO.{WB}"},
          {"service": {"account": "GATEWAY", "subject": f"$JS.API.CONSUMER.MSG.NEXT.{WB}"}, "to": f"JS.GATEWAY.API.CONSUMER.MSG.NEXT.{WB}"},
          {"service": {"account": "GATEWAY", "subject": f"$JS.ACK.{WB}.>"}},
      ], accts["SCHEDULER"].get("imports"))
schedp = users.get(URI("elitea-scheduler"), {}).get("publish", {}).get("allow", [])
check("elitea-scheduler publishes only its three imported subjects",
      sorted(schedp) == sorted([f"JS.GATEWAY.API.CONSUMER.INFO.{WB}", f"JS.GATEWAY.API.CONSUMER.MSG.NEXT.{WB}", f"$JS.ACK.{WB}.>"]), schedp)
check("SCHEDULER exports nothing", not accts["SCHEDULER"].get("exports"))
check("MAIN imports exactly GATEWAY's soft-alert stream",
      accts["MAIN"].get("imports") == [{"stream": {"account": "GATEWAY", "subject": ALERTS}}], accts["MAIN"].get("imports"))
mainp = users.get(URI("elitea-main"), {})
check("elitea-main publishes presence on its own family, never the gateway's",
      "elitea.events.project.*.presence" in mainp.get("publish", {}).get("allow", [])
      and not any(p.startswith("gateway.") for p in mainp.get("publish", {}).get("allow", []))
      and "gateway.>" in mainp.get("publish", {}).get("deny", []), mainp.get("publish"))
check("elitea-main reads exactly the two families the SSE route forwards",
      sorted(mainp.get("subscribe", {}).get("allow", [])) == sorted(["elitea.events.project.*.presence", ALERTS, "_INBOX_elitea-main.>"]), mainp.get("subscribe"))
gwp = users.get(URI("elitea-llm-gateway"), {}).get("publish", {}).get("allow", [])
check("the gateway cannot publish on elitea-main's presence family", not any(p.startswith("elitea.") for p in gwp), gwp)
RT_DURABLES = (("ELITEA_RT_V1_VALIDATE", "elitea-configuration-worker-v1"), ("ELITEA_RT_V1_AGENT", "elitea-agent-worker-v1"), ("ELITEA_RT_V1_INDEX", "elitea-index-worker-v1"))
want_rt_exports = []
want_w_imports = []
for st, du in RT_DURABLES:
    want_rt_exports += [
        {"service": f"$JS.API.CONSUMER.INFO.{st}.{du}", "accounts": ["WORKER"]},
        {"service": f"$JS.API.CONSUMER.MSG.NEXT.{st}.{du}", "response_type": "stream", "accounts": ["WORKER"]},
        {"service": f"$JS.ACK.{st}.{du}.>", "accounts": ["WORKER"]},
    ]
    want_w_imports += [
        {"service": {"account": "RUNTIME", "subject": f"$JS.API.CONSUMER.INFO.{st}.{du}"}, "to": f"JS.RUNTIME.API.CONSUMER.INFO.{st}.{du}"},
        {"service": {"account": "RUNTIME", "subject": f"$JS.API.CONSUMER.MSG.NEXT.{st}.{du}"}, "to": f"JS.RUNTIME.API.CONSUMER.MSG.NEXT.{st}.{du}"},
        {"service": {"account": "RUNTIME", "subject": f"$JS.ACK.{st}.{du}.>"}},
    ]
check("RUNTIME exports exactly its three worker durables' INFO, MSG.NEXT and ACK services to WORKER, and imports nothing",
      accts["RUNTIME"].get("exports") == want_rt_exports and not accts["RUNTIME"].get("imports"), accts["RUNTIME"].get("exports"))
check("WORKER imports exactly those services, the API under the workers' prefix JS.RUNTIME.API, and exports nothing",
      accts["WORKER"].get("imports") == want_w_imports and not accts["WORKER"].get("exports"), accts["WORKER"].get("imports"))
check("MAIN neither exports nor does GATEWAY import", not accts["MAIN"].get("exports") and not accts["GATEWAY"].get("imports"))
# The runtime command bus (docs/runtime-command-bus.md): the producer publishes
# commands and cannot consume them; the worker consumes and cannot publish one.
rt_main = users.get(URI("elitea-main-runtime"), {}).get("publish", {})
rt_worker = users.get(URI("elitea-worker"), {}).get("publish", {})
check("elitea-main-runtime publishes the three command routes and the replay wake-up",
      {"elitea.rt.v1.validate.d.*", "elitea.rt.v1.agent.d.*", "elitea.rt.v1.index.d.*", "elitea.rt.v1.replay.wake"} <= set(rt_main.get("allow", [])), rt_main.get("allow"))
check("elitea-main-runtime subscribes the replay wake-up (its replicas carry it)",
      "elitea.rt.v1.replay.wake" in users.get(URI("elitea-main-runtime"), {}).get("subscribe", {}).get("allow", []))
check("elitea-main-runtime may not pull or ack a command",
      not [p for p in rt_main.get("allow", []) if p.startswith(("$JS.ACK", "$JS.API.CONSUMER.MSG.NEXT"))] and "$JS.ACK.>" in rt_main.get("deny", []))
check("elitea-worker may not publish a command or the wake-up (denied, not merely unlisted)",
      "elitea.rt.v1.>" in rt_worker.get("deny", []) and not [p for p in rt_worker.get("allow", []) if p.startswith("elitea.rt.")])
check("elitea-worker publishes exactly its imported durable subjects and its dead-letter bucket's",
      sorted(rt_worker.get("allow", [])) == sorted(
          [f"JS.RUNTIME.API.CONSUMER.INFO.{s}.{d}" for s, d in RT_DURABLES]
          + [f"JS.RUNTIME.API.CONSUMER.MSG.NEXT.{s}.{d}" for s, d in RT_DURABLES]
          + [f"$JS.ACK.{s}.{d}.>" for s, d in RT_DURABLES]
          + ["$KV.ELITEA_RT_V1_DEADLETTER.>", "$JS.API.STREAM.INFO.KV_ELITEA_RT_V1_DEADLETTER"]),
      rt_worker.get("allow"))
check("elitea-worker writes the dead-letter bucket only",
      [p for p in rt_worker.get("allow", []) if p.startswith("$KV.")] == ["$KV.ELITEA_RT_V1_DEADLETTER.>"])
check("JetStream syncs every write before acknowledging it (sync_interval: always)",
      all(c.get("jetstream", {}).get("sync_interval") == "always" for c in (c1, c3)), c1.get("jetstream"))
cl = c3.get("cluster", {})
check("HA: routes are tls:// with peer verification", cl.get("tls", {}).get("verify") is True and all(r.startswith("tls://") for r in cl.get("routes", [])))

def kinds(d, k):
    return [x for x in d if x["kind"] == k]
issuers = {x["metadata"]["name"]: x["spec"] for x in kinds(scale1, "Issuer")}
check("the NATS CA chain renders (selfSigned -> CA cert -> CA Issuer)", "elitea-nats-ca-selfsigned" in issuers and "ca" in issuers.get("elitea-nats-ca", {}))
ca_certs = [x for x in kinds(scale1, "Certificate") if x["spec"].get("isCA")]
check("the CA is its own, not elitea-internal-ca", ca_certs and ca_certs[0]["spec"]["secretName"] == "elitea-nats-ca")
srv = [x for x in kinds(scale1, "Certificate") if not x["spec"].get("isCA") and x["metadata"]["name"].endswith("-server")]
check("the server certificate is issued by the NATS CA Issuer", srv and srv[0]["spec"]["issuerRef"]["name"] == "elitea-nats-ca")
fqdn = f"elitea-nats.{ns}.svc.cluster.local"
check("the server certificate names the client URL host", srv and fqdn in srv[0]["spec"]["dnsNames"])
# The route identity (#1076 F5): its own certificate from its own CA.
hacerts = {x["metadata"]["name"]: x["spec"] for x in kinds(ha, "Certificate")}
haiss = {x["metadata"]["name"]: x["spec"] for x in kinds(ha, "Issuer")}
route = hacerts.get("elitea-nats-route", {})
check("scale-1: no route CA or route certificate", not [x for x in kinds(scale1, "Certificate") if "route" in x["metadata"]["name"]])
check("HA: the route CA is its own chain (not the client CA)", hacerts.get("elitea-nats-route-ca", {}).get("isCA") and haiss.get("elitea-nats-route-ca", {}).get("ca", {}).get("secretName") == "elitea-nats-route-ca")
check("HA: the route certificate is issued by the ROUTE CA", route.get("issuerRef", {}).get("name") == "elitea-nats-route-ca", route.get("issuerRef"))
check("HA: the route certificate covers the headless names and serves both route directions", "*.elitea-nats-headless" in route.get("dnsNames", []) and set(route.get("usages", [])) == {"server auth", "client auth"})
check("HA: the client-port certificate is no route certificate (server auth only, no headless names)",
      hacerts.get("elitea-nats-server", {}).get("usages") == ["server auth"] and not any("headless" in n for n in hacerts.get("elitea-nats-server", {}).get("dnsNames", [])))
rtls = c3.get("cluster", {}).get("tls", {})
check("HA: routes verify peers against the ROUTE CA in the route Secret's mount", rtls.get("ca_file") == "/etc/nats-certs/cluster/ca.crt" and rtls.get("cert_file", "").startswith("/etc/nats-certs/cluster/"), rtls)
hasts = kinds(ha, "StatefulSet")[0]["spec"]["template"]["spec"]
check("HA: the StatefulSet mounts the route Secret as the cluster TLS", {v["name"]: v.get("secret", {}).get("secretName") for v in hasts["volumes"]}.get("cluster-tls") == route.get("secretName"))
sts = kinds(scale1, "StatefulSet")[0]
mounted = {v.get("secret", {}).get("secretName") for v in sts["spec"]["template"]["spec"]["volumes"]}
check("the server mounts the certificate Secret it is issued into", srv[0]["spec"]["secretName"] in mounted)

pod_labels = sts["spec"]["template"]["metadata"]["labels"]
for label, d in (("scale-1", scale1), ("HA", ha)):
    nps = kinds(d, "NetworkPolicy")
    check(f"{label}: an ingress NetworkPolicy renders", len(nps) == 1)
    if not nps:
        continue
    np = nps[0]["spec"]
    sel = np["podSelector"]["matchLabels"]
    check(f"{label}: it selects the NATS pods", all(pod_labels.get(k) == v for k, v in sel.items()))
    rules = {r["ports"][0]["port"]: r for r in np["ingress"]}
    names = {p["podSelector"]["matchLabels"]["app.kubernetes.io/name"] for p in rules.get(4222, {}).get("from", []) if "podSelector" in p}
    check(f"{label}: 4222 admits exactly the five client workloads", names == {"elitea-main", "elitea-llm-gateway", "elitea-scheduler", "nats-bootstrap", "elitea-worker-python"}, sorted(names))
    mon = [p.get("podSelector", {}).get("matchLabels", {}) for p in rules.get(8222, {}).get("from", [])]
    check(f"{label}: 8222 (monitoring) admits the KEDA operator only", mon == [{"app": "keda-operator"}], mon)
    if label == "HA":
        check("HA: 6222 is admitted only from the NATS pods", 6222 in rules and all("namespaceSelector" not in p for p in rules[6222]["from"]))

# ── approver-policy (#1076 F3) ─────────────────────────────────────────────
check("no CertificateRequestPolicy where the API is not served (enabled: auto)", not kinds(ha, "CertificateRequestPolicy"))
ap = docs("nats-ha-ap.yaml")
pols = {x["metadata"]["name"]: x["spec"] for x in kinds(ap, "CertificateRequestPolicy")}
check("approver-policy renders where the API is served", len(pols) == 5, sorted(pols))
cli = pols.get("elitea-nats-elitea-client-identities", {})
check("client identities: exactly the permission table's URI SANs, required", sorted(cli.get("allowed", {}).get("uris", {}).get("values", [])) == sorted(users) and cli["allowed"]["uris"].get("required") is True)
check("client identities: no DNS names, no CA, client auth, from the client issuer",
      "dnsNames" not in cli.get("allowed", {}) and cli.get("allowed", {}).get("isCA") is False
      and "server auth" not in cli.get("allowed", {}).get("usages", []) and cli.get("selector", {}).get("issuerRef", {}).get("name") == "elitea-nats-ca")
srvpol = pols.get("elitea-nats-elitea-server", {})
check("server: exactly the server certificate's names", sorted(srvpol.get("allowed", {}).get("dnsNames", {}).get("values", [])) == sorted(hacerts["elitea-nats-server"]["dnsNames"]))
rpol = pols.get("elitea-nats-elitea-routes", {})
check("routes: exactly the route certificate's names, from the route issuer",
      sorted(rpol.get("allowed", {}).get("dnsNames", {}).get("values", [])) == sorted(route.get("dnsNames", []))
      and rpol.get("selector", {}).get("issuerRef", {}).get("name") == "elitea-nats-route-ca")
check("the self-signed issuer may sign only the two CAs", sorted(p["allowed"]["commonName"]["value"] for n, p in pols.items() if p["allowed"].get("isCA")) == ["elitea-nats-ca", "elitea-nats-route-ca"])
role = kinds(ap, "ClusterRole")
check("cert-manager may `use` exactly these policies", role and sorted(role[0]["rules"][0]["resourceNames"]) == sorted(pols) and role[0]["rules"][0]["verbs"] == ["use"])
binding = kinds(ap, "ClusterRoleBinding")
check("the binding names cert-manager's service account", binding and binding[0]["subjects"] == [{"kind": "ServiceAccount", "name": "cert-manager", "namespace": "cert-manager"}])

# ── the bootstrap ─────────────────────────────────────────────────────────
bs = docs("bootstrap.yaml")
bcerts = {c["spec"]["commonName"]: c for c in kinds(bs, "Certificate")}
check("the bootstrap issues one certificate per account", set(bcerts) == {f"elitea-nats-bootstrap-{a}" for a in ("main", "gateway", "runtime", "worker")}, sorted(bcerts))
for name, c in bcerts.items():
    check(f"{name}: URI SAN names its account's bootstrap user", c["spec"]["uris"] == [URI(name)] and URI(name) in users)
    check(f"{name}: issued by the NATS CA Issuer", c["spec"]["issuerRef"]["name"] == "elitea-nats-ca")
jobmeta = kinds(bs, "Job")[0]["metadata"].get("annotations", {})
def dur_hours(d):
    m = re.fullmatch(r"(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?", d or "")
    return (int(m.group(1) or 0) + int(m.group(2) or 0) / 60 + int(m.group(3) or 0) / 3600) if m and d else 1e9
for name, c in bcerts.items():
    ann = c["metadata"].get("annotations", {})
    check(f"{name}: a hook in the Job's own phase (exists only while an install runs)", ann.get("helm.sh/hook") == jobmeta.get("helm.sh/hook"), ann.get("helm.sh/hook"))
    check(f"{name}: ordered before the Job", int(ann.get("helm.sh/hook-weight", "0")) < int(jobmeta.get("helm.sh/hook-weight", "0")))
    check(f"{name}: deleted with the Job, so nothing renews it", "hook-succeeded" in ann.get("helm.sh/hook-delete-policy", "") and "before-hook-creation" in ann.get("helm.sh/hook-delete-policy", ""))
    check(f"{name}: lives at most an hour", dur_hours(c["spec"].get("duration")) <= 1, c["spec"].get("duration"))
job = kinds(bs, "Job")[0]["spec"]["template"]["spec"]
env = {e["name"]: e.get("value") for e in job["containers"][0]["env"]}
check("the bootstrap dials tls:// with no credential", env.get("NATS_URL", "").startswith("tls://") and "@" not in env.get("NATS_URL", ""))
check("the bootstrap connects to every account", env.get("NATS_BOOTSTRAP_ACCOUNTS") == "main gateway runtime worker", env.get("NATS_BOOTSTRAP_ACCOUNTS"))
check("the bootstrap presents no single identity (NATS_TLS_*_FILE)", not any(k.startswith("NATS_TLS_") and k.endswith("_FILE") for k in env))
check("the bootstrap Job pod is what the NetworkPolicy admits", kinds(bs, "Job")[0]["spec"]["template"]["metadata"]["labels"].get("app.kubernetes.io/name") == "nats-bootstrap")
bvols = {v["name"]: v.get("secret", {}).get("secretName") for v in job["volumes"]}
bmnts = {m["name"]: m["mountPath"] for m in job["containers"][0]["volumeMounts"]}
for a in ("main", "gateway", "runtime"):
    c = bcerts.get(f"elitea-nats-bootstrap-{a}")
    check(f"the bootstrap mounts {a}'s certificate Secret at NATS_TLS_DIR/{a}",
          c and bvols.get(f"nats-client-tls-{a}") == c["spec"]["secretName"] and bmnts.get(f"nats-client-tls-{a}") == f"{env.get('NATS_TLS_DIR')}/{a}")

# ── the platform ──────────────────────────────────────────────────────────
el = docs("elitea.yaml")
certs = {c["spec"]["commonName"]: c for c in kinds(el, "Certificate") if c["spec"].get("uris")}
want = {"elitea-main", "elitea-llm-gateway", "elitea-scheduler"}
check("the platform issues a NATS certificate for each client", set(certs) == want, sorted(certs))
for ident, c in certs.items():
    check(f"{ident}: URI SAN names its user in the permission table", c["spec"]["uris"] == [URI(ident)] and URI(ident) in users)
    check(f"{ident}: issued by the NATS CA Issuer, not elitea-internal-ca", c["spec"]["issuerRef"]["name"] == "elitea-nats-ca")
deps = {d["metadata"]["name"]: d for d in kinds(el, "Deployment")}
cms = {d["metadata"]["name"]: d.get("data", {}) for d in kinds(el, "ConfigMap")}
prefix = {"elitea-main": ("ELITEA_EVENTS", "elitea-main-config", "ELITEA_EVENTS_NATS_URL"),
          "elitea-llm-gateway": ("GATEWAY", None, "GATEWAY_NATS_URL"),
          "elitea-scheduler": ("GATEWAY", "elitea-scheduler-config", "GATEWAY_NATS_URL")}
for ident, (pfx, cm, urlvar) in prefix.items():
    d = deps.get(ident)
    if not d:
        check(f"{ident}: Deployment renders", False)
        continue
    spec = d["spec"]["template"]["spec"]
    ctr = spec["containers"][0]
    np_names = {p["podSelector"]["matchLabels"]["app.kubernetes.io/name"]
                for r in kinds(scale1, "NetworkPolicy")[0]["spec"]["ingress"] if r["ports"][0]["port"] == 4222
                for p in r["from"] if "podSelector" in p}
    check(f"{ident}: pod label is one the NATS NetworkPolicy admits", d["spec"]["template"]["metadata"]["labels"].get("app.kubernetes.io/name") in np_names)
    vol = [v for v in spec.get("volumes", []) if v["name"] == "nats-client-tls"]
    mnt = [m for m in ctr.get("volumeMounts", []) if m["name"] == "nats-client-tls"]
    check(f"{ident}: mounts the Secret its Certificate writes", vol and vol[0]["secret"]["secretName"] == certs[ident]["spec"]["secretName"] and mnt)
    envmap = {e["name"]: e.get("value") for e in ctr.get("env", [])}
    if cm:
        envmap.update(cms.get(cm, {}))
    files = [envmap.get(f"{pfx}_NATS_TLS_{k}_FILE") for k in ("CA", "CERT", "KEY")]
    base = mnt[0]["mountPath"] if mnt else "<unmounted>"
    check(f"{ident}: {pfx}_NATS_TLS_* point into the mount", files == [f"{base}/ca.crt", f"{base}/tls.crt", f"{base}/tls.key"], files)
    url = envmap.get(urlvar, "")
    check(f"{ident}: {urlvar} is tls://{fqdn}:4222, no credential", url == f"tls://{fqdn}:4222", url)

# ── the runtime command bus's identities (standalone profile) ─────────────
rt = docs("elitea-runtime.yaml")
rtcerts = {c["spec"]["commonName"]: c for c in kinds(rt, "Certificate") if c["spec"].get("uris")}
check("runtime on: elitea-main-runtime gets its own NATS certificate", "elitea-main-runtime" in rtcerts, sorted(rtcerts))
mc = rtcerts.get("elitea-main-runtime")
if mc:
    check("elitea-main-runtime: URI SAN names its user in the permission table", mc["spec"]["uris"] == [URI("elitea-main-runtime")] and URI("elitea-main-runtime") in users)
    check("elitea-main-runtime: issued by the NATS CA Issuer", mc["spec"]["issuerRef"]["name"] == "elitea-nats-ca")
    rdeps = {d["metadata"]["name"]: d for d in kinds(rt, "Deployment")}
    rcms = {d["metadata"]["name"]: d.get("data", {}) for d in kinds(rt, "ConfigMap")}
    md = rdeps.get("elitea-main")
    spec = md["spec"]["template"]["spec"] if md else {}
    vol = [v for v in spec.get("volumes", []) if v["name"] == "runtime-nats-client-tls"]
    mnt = [m for m in (spec.get("containers") or [{}])[0].get("volumeMounts", []) if m["name"] == "runtime-nats-client-tls"]
    check("elitea-main mounts the runtime identity's Secret", vol and vol[0]["secret"]["secretName"] == mc["spec"]["secretName"] and mnt)
    env = {}
    for d in rcms.values():
        env.update({k: v for k, v in d.items() if k.startswith("ELITEA_RUNTIME_NATS")})
    base = mnt[0]["mountPath"] if mnt else "<unmounted>"
    check("ELITEA_RUNTIME_NATS_TLS_* point into its mount",
          [env.get(f"ELITEA_RUNTIME_NATS_TLS_{k}_FILE") for k in ("CA", "CERT", "KEY")] == [f"{base}/ca.crt", f"{base}/tls.crt", f"{base}/tls.key"], env)
    check(f"ELITEA_RUNTIME_NATS_URL is tls://{fqdn}:4222, no credential", env.get("ELITEA_RUNTIME_NATS_URL") == f"tls://{fqdn}:4222", env.get("ELITEA_RUNTIME_NATS_URL"))

wc = rtcerts.get("elitea-worker")
check("worker on: elitea-worker gets its NATS certificate", wc is not None and wc["spec"]["uris"] == [URI("elitea-worker")] and URI("elitea-worker") in users, sorted(rtcerts))
if wc:
    wd = {d["metadata"]["name"]: d for d in kinds(rt, "Deployment")}.get("elitea-worker")
    wspec = wd["spec"]["template"]["spec"] if wd else {}
    wvol = [v for v in wspec.get("volumes", []) if v["name"] == "nats-client-tls"]
    check("the worker mounts the Secret its Certificate writes", wvol and wvol[0]["secret"]["secretName"] == wc["spec"]["secretName"])
    check("the worker pod label is one the NATS NetworkPolicy admits", wd and wd["spec"]["template"]["metadata"]["labels"].get("app.kubernetes.io/name") == "elitea-worker-python")
    rj = [json.loads(d["data"]["runtime.json"]) for d in kinds(rt, "ConfigMap") if "runtime.json" in (d.get("data") or {})]
    check("runtime.json dials tls:// with the mounted identity", rj and rj[0]["nats_url"] == f"tls://{fqdn}:4222" and rj[0]["nats_certificate_path"].endswith("/tls.crt"), rj[0] if rj else None)

# ── the env names the code builds (R7) ────────────────────────────────────
# natsconn.FromEnv / EnvNames build <PREFIX>_NATS_TLS_{CA,CERT,KEY}_FILE by
# concatenation, so no grep of the code finds them and the env-drift gate
# cannot compare them with the chart. Read every prefix the services pass to
# natsconn (a string literal or a constant), and require the rendered chart
# to set all three names for each.
root = pathlib.Path(sys.argv[3]).parents[2]
call = re.compile(r"natsconn\.(?:FromEnv|EnvNames)\(\s*(\"[A-Z_]+\"|[A-Za-z_][A-Za-z0-9_.]*)")
prefixes, sites = set(), 0
for go in (root / "services").rglob("*.go"):
    if go.name.endswith("_test.go") or "/vendor/" in str(go):
        continue
    text = go.read_text(errors="replace")
    for m in call.finditer(text):
        sites += 1
        arg = m.group(1)
        if arg.startswith('"'):
            prefixes.add(arg.strip('"'))
            continue
        pkgdir, name = (root / "libs/go/natsconn", arg.split(".", 1)[1]) if arg.startswith("natsconn.") else (go.parent, arg)
        found = None
        for src in pkgdir.glob("*.go"):
            hit = re.search(r"\b" + re.escape(name) + r"\s*=\s*\"([A-Z_]+)\"", src.read_text(errors="replace"))
            if hit:
                found = hit.group(1)
                break
        check(f"env prefix {arg} ({go.relative_to(root)}) resolves to a string", found, arg)
        if found:
            prefixes.add(found)
check("the code passes natsconn a prefix at three or more call sites (else this check reads nothing)", sites >= 3, sites)
rendered_env = set()
for d in kinds(el, "Deployment"):
    for c in d["spec"]["template"]["spec"]["containers"]:
        rendered_env |= {e["name"] for e in c.get("env", [])}
for data in cms.values():
    rendered_env |= set(data)
# The runtime plane's producer identity (ELITEA_RUNTIME) renders only with the
# runtime plane on: the standalone profile's render.
for d in kinds(docs("elitea-runtime.yaml"), "Deployment"):
    for c in d["spec"]["template"]["spec"]["containers"]:
        rendered_env |= {e["name"] for e in c.get("env", [])}
for d in kinds(docs("elitea-runtime.yaml"), "ConfigMap"):
    rendered_env |= set(d.get("data", {}) or {})
for p in sorted(prefixes):
    names = [f"{p}_NATS_TLS_{k}_FILE" for k in ("CA", "CERT", "KEY")]
    check(f"the chart renders all three of {p}_NATS_TLS_*_FILE the code builds", all(n in rendered_env for n in names), [n for n in names if n not in rendered_env])
check("the prefixes the code uses are the ones this suite expects", prefixes == {"ELITEA_EVENTS", "GATEWAY", "ELITEA_RUNTIME"}, sorted(prefixes))
# The scheduler opens JetStream with natsconn.SchedulerGatewayJSAPIPrefix; the
# SCHEDULER imports must map GATEWAY's consumer API under that same prefix,
# or every bind answers "JetStream not enabled for account".
hit = re.search(r'SchedulerGatewayJSAPIPrefix\s*=\s*"([^"]+)"', (root / "libs/go/natsconn/natsconn.go").read_text())
code_prefix = hit.group(1) if hit else None
tos = [i.get("to", "") for i in accts["SCHEDULER"].get("imports", []) if "$JS.API." in i["service"]["subject"]]
check("the SCHEDULER imports use the scheduler code's JetStream API prefix", code_prefix and tos and all(t.startswith(code_prefix + ".") for t in tos), (code_prefix, tos))
# Likewise the workers (#1081 review S1): natsconn.WorkerRuntimeJSAPIPrefix,
# the Rust worker's and the Python worker's own constants, and the WORKER
# imports must all name the same prefix, or a secured worker's bind answers
# "JetStream not enabled" / no responders.
hit = re.search(r'WorkerRuntimeJSAPIPrefix\s*=\s*"([^"]+)"', (root / "libs/go/natsconn/natsconn.go").read_text())
wprefix = hit.group(1) if hit else None
wtos = [i.get("to", "") for i in accts["WORKER"].get("imports", []) if "$JS.API." in i["service"]["subject"]]
check("the WORKER imports use natsconn.WorkerRuntimeJSAPIPrefix", wprefix and wtos and all(t.startswith(wprefix + ".") for t in wtos), (wprefix, wtos))
py = re.search(r'RUNTIME_API_PREFIX\s*=\s*"([^"]+)"', (root / "services/elitea-worker-python/src/elitea_worker/transport/nats_jetstream.py").read_text())
rs = re.search(r'RUNTIME_API_PREFIX:\s*&str\s*=\s*"([^"]+)"', (root / "services/elitea-worker-rust/src/transport/command_bus.rs").read_text())
check("the Python worker's prefix is the chart's", py and py.group(1) == wprefix, py and py.group(1))
check("the Rust worker's prefix is the chart's", rs and rs.group(1) == wprefix, rs and rs.group(1))

# ── one NATS server version everywhere (R9) ───────────────────────────────
# The chart pins the server; CI's plaintext suites, the secured tests' server
# binary and compose must run the same one, or a test passes on a server the
# cluster does not run.
pins = {yaml.safe_load(open(root / "deploy/helm/nats" / f))["nats"]["container"]["image"]["tag"] for f in ("values-scale1.yaml", "values-ha.yaml")}
check("both NATS profiles pin one server image", len(pins) == 1, pins)
pin = next(iter(pins))
image_re = re.compile(r"\bnats:(2\.[0-9][0-9A-Za-z.\-]*)")
seen = {}
for rel in (".github/workflows", "deploy", "scripts"):
    for f in (root / rel).rglob("*"):
        if f.suffix not in (".yml", ".yaml", ".sh") or "/charts/" in str(f) or "/helm/" in str(f):
            continue
        for m in image_re.finditer(f.read_text(errors="replace")):
            seen.setdefault(m.group(1), set()).add(str(f.relative_to(root)))
check("CI, compose and the test scripts run the chart's NATS image (found some)", seen, seen)
for tag, files in sorted(seen.items()):
    check(f"nats:{tag} is the chart's pin nats:{pin}", tag == pin, sorted(files))

# ── the Argo CD sample ────────────────────────────────────────────────────
def app(f):
    return yaml.safe_load(open(apps / f))
nsset = {app(f)["spec"]["destination"]["namespace"] for f in ("nats.yaml", "nats-bootstrap.yaml", "elitea.yaml")}
check("argocd: NATS, its bootstrap and the platform share one namespace (the NATS CA Issuer is namespaced)", len(nsset) == 1, nsset)
bparams = {p["name"]: p["value"] for p in app("nats-bootstrap.yaml")["spec"]["source"]["helm"]["parameters"]}
check("argocd: the bootstrap dials tls://", bparams.get("natsUrl", "").startswith("tls://"))

# Docker retains the store when its container is replaced, as the PVC does.
for filename in ("docker-compose.yml", "docker-compose.standalone-full.yml"):
    compose = yaml.safe_load((root / "deploy" / filename).read_text())
    server = compose["services"]["nats"]
    volumes = [entry.split(":") for entry in server.get("volumes", [])]
    stores = [entry for entry in volumes if len(entry) >= 2 and entry[1] == "/data"]
    check(f"{filename}: JetStream has one declared named volume", len(stores) == 1 and stores[0][0] in compose.get("volumes", {}), stores)
    check(f"{filename}: the local store config is mounted read-only", ["./runtime/nats-local.conf", "/etc/nats-config/nats.conf", "ro"] in volumes, volumes)
    check(f"{filename}: NATS loads the mounted config", server["command"] == ["-c", "/etc/nats-config/nats.conf"], server["command"])
secure = yaml.safe_load((root / "deploy/docker-compose.nats-secure.yml").read_text())["services"]["nats"]
check("the secure overlay preserves persistent JetStream storage", not any(entry.split(":")[0] == "/data" for entry in secure.get("tmpfs", [])), secure.get("tmpfs"))
local_config = (root / "deploy/runtime/nats-local.conf").read_text()
check("local JetStream writes sync before acknowledgment", re.search(r"^\s*sync_interval:\s*always\s*$", local_config, re.M) is not None, "sync_interval")
check("local JetStream uses the persistent mount", re.search(r'^\s*store_dir:\s*"/data"\s*$', local_config, re.M) is not None, "store_dir")

# ── report ────────────────────────────────────────────────────────────────
for line in open(tmp / "refusals"):
    status, name, *rest = line.rstrip("\n").split("\t")
    results.append((f"refusal: {name}", status == "ok", " ".join(rest) or status))
failed = [r for r in results if not r[1]]
for name, ok, detail in results:
    print(("  ok: " if ok else "  FAIL: ") + name + ("" if ok or not detail else f" — {detail}"))
FLOOR = 60
print(f"render-nats-security: {len(results)} assertion(s), {len(failed)} failed")
if len(results) < FLOOR:
    sys.exit(f"only {len(results)} assertion(s) ran, under the floor of {FLOOR}: an assertion stopped running")
sys.exit(1 if failed else 0)
PY
