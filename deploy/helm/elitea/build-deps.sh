#!/usr/bin/env bash
# build-deps.sh — fetch the elitea chart's dependencies into charts/, exactly
# as Chart.lock pins them.
#
#   deploy/helm/elitea/build-deps.sh
#
# The chart's dependencies are NOT committed (charts/ is gitignored):
#   nats                    the upstream chart, from the repository Chart.yaml
#                           names (nats.enabled, on by default)
#   qdrant                  the upstream chart, likewise (qdrant.enabled, on
#                           by default)
#   elitea-sandbox-runtime  our chart, file://../elitea-sandbox-runtime
#                           (sandboxRuntime.enabled, off by default)
# Anything that lints, templates, packages or installs the chart from a clone
# runs this first: every CI job that does, kind-stack.sh, the Taskfile's
# helm:* tasks and scripts/nats/render-secure-conf.sh. The published OCI chart
# carries them all as .tgz (helm package includes charts/), and Argo CD builds
# them itself for a git source, so neither needs this.
#
# `helm dependency build` refuses a Chart.lock that is out of sync with
# Chart.yaml. To bump a dependency: change its version in Chart.yaml, then
# `helm dependency update deploy/helm/elitea` and commit Chart.lock.
# Air-gapped: mirror the published OCI chart (it already carries every
# dependency) rather than building from a clone. Needs helm and network access
# to every https repository Chart.yaml names.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
HELM="${HELM:-helm}"

# Every https repository Chart.yaml's dependencies name, each once. file://
# dependencies (our own charts) need no repo entry.
repos=()
while IFS= read -r url; do
  [ -n "$url" ] && repos+=("$url")
done < <(awk '$1 == "repository:" { gsub(/"/, "", $2); if ($2 ~ /^https:\/\//) print $2 }' "${here}/Chart.yaml" | sort -u)
[ "${#repos[@]}" -gt 0 ] || { echo "build-deps: no https dependency repository in Chart.yaml" >&2; exit 1; }

retry() {  # the repositories are public GitHub Pages sites: retry, don't rerun
  local n
  for n in 1 2 3; do
    if "$@"; then return 0; fi
    echo "build-deps: attempt ${n} failed: $*" >&2
    sleep $((n * 5))
  done
  return 1
}
# helm resolves a dependency's https repository through a repo entry with the
# same URL; the name is ours (elitea-dep-<host-and-path>). Chart.lock still
# pins the version.
for url in "${repos[@]}"; do
  name="elitea-dep-$(printf '%s' "${url#https://}" | tr -c 'A-Za-z0-9' '-' | sed 's/-*$//')"
  retry "$HELM" repo add "$name" "$url" --force-update >/dev/null
done
# A stale archive of our own sandbox-runtime chart would otherwise survive a
# local edit to it: rebuild from scratch every time.
rm -rf "${here}/charts"
retry "$HELM" dependency build "${here}" >/dev/null
"$HELM" dependency list "${here}" 2>/dev/null
