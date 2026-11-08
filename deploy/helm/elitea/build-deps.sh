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
if ! retry "$HELM" dependency build "${here}" >/dev/null; then
  # The upstream charts' archives live on github.com release download links,
  # which answer 504 for long stretches while the API serves the same asset.
  # Fetch every pinned archive ourselves instead: the URL and sha256 digest
  # come from the repository index helm just cached, a GitHub release asset
  # goes through `gh release download` (the API; needs GH_TOKEN), and every
  # archive must match its digest. Our file:// charts are packaged locally.
  echo "build-deps: helm dependency build failed; fetching the pinned archives directly" >&2
  cache="$("$HELM" env HELM_REPOSITORY_CACHE)"
  rm -rf "${here}/charts"; mkdir -p "${here}/charts"
  # name<TAB>version<TAB>repository, one row per Chart.lock dependency.
  rows="$(awk '
    $1 == "-" && $2 == "name:" { n = $3; next }
    $1 == "name:" { n = $2; next }
    $1 == "repository:" { r = $2; next }
    $1 == "version:" && n != "" { print n "\t" $2 "\t" r; n = "" }
  ' "${here}/Chart.lock")"
  while IFS=$'\t' read -r dep ver url; do
    [ -n "$dep" ] || continue
    case "$url" in
      file://*)
        "$HELM" package "${here}/${url#file://}" --destination "${here}/charts" >/dev/null
        continue ;;
    esac
    name="elitea-dep-$(printf '%s' "${url#https://}" | tr -c 'A-Za-z0-9' '-' | sed 's/-*$//')"
    index="${cache}/${name}-index.yaml"
    # The entry for this chart and version: its digest and first URL.
    read -r digest asset < <(awk -v chart="$dep" -v want="$ver" '
      /^  [^ -][^:]*:$/ { cur = $1; sub(/:$/, "", cur); next }
      cur != chart { next }
      /^  - / { dg = ""; u = ""; inurls = 0 }
      /^    digest: / { dg = $2 }
      /^    urls:/ { inurls = 1; next }
      inurls && /^    - / { if (u == "") u = $2; next }
      /^    [a-z]/ { inurls = 0 }
      /^    version: / { v = $2; gsub(/"/, "", v); if (v == want) { print dg, u; exit } }
    ' "$index")
    [ -n "${digest:-}" ] && [ -n "${asset:-}" ] || { echo "build-deps: ${dep} ${ver} not in ${index}" >&2; exit 1; }
    # A relative URL is relative to the repository.
    case "$asset" in https://*) ;; *) asset="${url%/}/${asset}" ;; esac
    out="${here}/charts/${dep}-${ver}.tgz"
    if [[ "$asset" =~ ^https://github\.com/([^/]+/[^/]+)/releases/download/([^/]+)/([^/]+)$ ]] && [ -n "${GH_TOKEN:-}" ]; then
      retry gh release download "${BASH_REMATCH[2]}" --repo "${BASH_REMATCH[1]}" \
        --pattern "${BASH_REMATCH[3]}" --output "$out" --clobber
    else
      retry curl -fsSL --retry 5 --retry-all-errors --retry-delay 5 -o "$out" "$asset"
    fi
    echo "${digest}  ${out}" | sha256sum -c - >/dev/null \
      || { echo "build-deps: ${dep} ${ver} does not match its index digest" >&2; exit 1; }
  done <<< "$rows"
fi
"$HELM" dependency list "${here}" 2>/dev/null
