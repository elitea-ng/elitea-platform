#!/usr/bin/env bash
# render-secure-conf.sh — write the nats.conf the elitea chart's bundled NATS
# server renders, for the secured permission tests (#1076).
#
#   scripts/nats/render-secure-conf.sh OUT_FILE [VALUES_FILE]
#
# Renders deploy/helm/elitea (nats.enabled, the scale-1 default; or with
# VALUES_FILE layered on, e.g. values-nats-ha.yaml for the route identity
# test) through `helm template` and extracts data."nats.conf" from the server
# ConfigMap. The
# tests in libs/go/natsconn/natstest, elitea-main, elitea-scheduler and
# elitea-llm-gateway read OUT_FILE through ELITEA_TEST_NATS_SECURE_CONF and run
# nats-server on it, so the permission table under test is the chart's own and
# not a copy.
#
# Needs helm and python3 + PyYAML, and network for the upstream nats subchart:
# it runs deploy/helm/elitea/build-deps.sh (`helm dependency build`) first.
set -euo pipefail

out="${1:?usage: render-secure-conf.sh OUT_FILE [VALUES_FILE]}"
root="$(cd "$(dirname "$0")/../.." && pwd)"
chart="${root}/deploy/helm/elitea"
HELM="${HELM:-helm}"
extra=()
if [ -n "${2:-}" ]; then extra=(-f "$2"); fi

bash "${chart}/build-deps.sh" >/dev/null
rendered="$(mktemp)"
trap 'rm -f "$rendered"' EXIT
# The platform's own refusals run on every render, so state the two fields the
# chart has no default for; neither reaches nats.conf.
"$HELM" template elitea "$chart" --namespace elitea ${extra[@]+"${extra[@]}"} \
  --set-string llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://render-only.example.invalid/llm/v1 \
  --set-string llmGateway.egressPosture=public-unrestricted \
  --set networkPolicies.main.noExternalIngress=true \
  --show-only charts/nats/templates/config-map.yaml > "$rendered"

python3 - "$rendered" "$out" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
cms = [d for d in docs if d.get("kind") == "ConfigMap" and "nats.conf" in (d.get("data") or {})]
if len(cms) != 1:
    sys.exit(f"expected exactly one ConfigMap carrying nats.conf, found {len(cms)}")
conf = cms[0]["data"]["nats.conf"]
for needle in ('"verify_and_map": true', '"accounts"', '"MAIN"', '"GATEWAY"', '"SCHEDULER"', '"RUNTIME"', '"WORKER"', '"users"'):
    if needle not in conf:
        sys.exit(f"the rendered nats.conf lacks {needle}: this is not the secured profile")
open(sys.argv[2], "w").write(conf)
PY
echo "wrote ${out}"
