#!/usr/bin/env bash
# render-secure-conf.sh — write the nats.conf the NATS chart renders, for the
# secured permission tests (#1076).
#
#   scripts/nats/render-secure-conf.sh OUT_FILE [VALUES_FILE]
#
# Renders deploy/helm/nats with its scale-1 profile (or VALUES_FILE, e.g.
# values-ha.yaml for the route identity test) through
# `helm template` and extracts data."nats.conf" from the server ConfigMap. The
# tests in libs/go/natsconn/natstest, elitea-main, elitea-scheduler and
# elitea-llm-gateway read OUT_FILE through ELITEA_TEST_NATS_SECURE_CONF and run
# nats-server on it, so the permission table under test is the chart's own and
# not a copy.
#
# Needs helm and network access the first time (the upstream nats subchart is
# vendored by `helm dependency build`), and python3 + PyYAML.
set -euo pipefail

out="${1:?usage: render-secure-conf.sh OUT_FILE [VALUES_FILE]}"
root="$(cd "$(dirname "$0")/../.." && pwd)"
chart="${root}/deploy/helm/nats"
values="${2:-${chart}/values-scale1.yaml}"
HELM="${HELM:-helm}"

if ! ls "${chart}"/charts/nats-*.tgz >/dev/null 2>&1; then
  "$HELM" dependency build "$chart" >/dev/null
fi

rendered="$(mktemp)"
trap 'rm -f "$rendered"' EXIT
"$HELM" template elitea-nats "$chart" --namespace elitea -f "$values" > "$rendered"

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
