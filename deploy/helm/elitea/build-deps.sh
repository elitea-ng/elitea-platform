#!/usr/bin/env bash
# build-deps.sh — fetch the elitea chart's dependencies into charts/, exactly
# as Chart.lock pins them.
#
#   deploy/helm/elitea/build-deps.sh
#
# The chart's two dependencies are NOT committed (charts/ is gitignored):
#   nats                    the upstream chart, from the repository Chart.yaml
#                           names (nats.enabled, on by default)
#   elitea-sandbox-runtime  our chart, file://../elitea-sandbox-runtime
#                           (sandboxRuntime.enabled, off by default)
# Anything that lints, templates, packages or installs the chart from a clone
# runs this first: every CI job that does, kind-stack.sh, the Taskfile's
# helm:* tasks and scripts/nats/render-secure-conf.sh. The published OCI chart
# carries both as .tgz (helm package includes charts/), and Argo CD builds
# them itself for a git source, so neither needs this.
#
# `helm dependency build` refuses a Chart.lock that is out of sync with
# Chart.yaml. To bump nats: change its version in Chart.yaml, then
# `helm dependency update deploy/helm/elitea` and commit Chart.lock.
# Air-gapped: mirror the published OCI chart (it already carries nats) rather
# than building from a clone. Needs helm and network access to the nats
# repository.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
HELM="${HELM:-helm}"
repo="$(awk '/^  - name: nats$/ { f = 1; next } f && $1 == "repository:" { gsub(/"/, "", $2); print $2; exit }' "${here}/Chart.yaml")"
[ -n "$repo" ] || { echo "build-deps: no nats dependency in Chart.yaml" >&2; exit 1; }

# helm resolves a dependency's https repository through a repo entry with the
# same URL; the name is ours. Chart.lock still pins the version.
retry() {  # the nats repository is a public GitHub Pages site: retry, don't rerun
  local n
  for n in 1 2 3; do
    if "$@"; then return 0; fi
    echo "build-deps: attempt ${n} failed: $*" >&2
    sleep $((n * 5))
  done
  return 1
}
retry "$HELM" repo add elitea-nats "$repo" --force-update >/dev/null
# A stale archive of our own sandbox-runtime chart would otherwise survive a
# local edit to it: rebuild from scratch every time.
rm -rf "${here}/charts"
retry "$HELM" dependency build "${here}" >/dev/null
"$HELM" dependency list "${here}" 2>/dev/null
