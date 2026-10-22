#!/usr/bin/env bash
# vendor-nats.sh — re-vendor the upstream nats subchart into charts/nats/,
# unpacked, at the version Chart.yaml pins, and refresh Chart.lock.
#
# Unpacked rather than a .tgz: the archive is a binary blob
# (scripts/no-binaries-check.sh), and an unpacked chart reviews as a diff.
# Needs helm and network access; nothing else in the repository does.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
HELM="${HELM:-helm}"
version="$(awk '/^  - name: nats$/ { f = 1; next } f && $1 == "version:" { gsub(/"/, "", $2); print $2; exit }' "${here}/Chart.yaml")"
repo="$(awk '/^  - name: nats$/ { f = 1; next } f && $1 == "repository:" { gsub(/"/, "", $2); print $2; exit }' "${here}/Chart.yaml")"
[ -n "$version" ] && [ -n "$repo" ] || { echo "vendor-nats: no nats dependency in Chart.yaml" >&2; exit 1; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
"$HELM" pull nats --repo "$repo" --version "$version" --destination "$tmp"
rm -rf "${here}/charts/nats"
mkdir -p "${here}/charts"
tar xzf "${tmp}/nats-${version}.tgz" -C "${here}/charts"

# Chart.lock: what `helm dependency update` would write, without leaving the
# .tgz files behind. The sibling charts are copied too: the sandboxRuntime
# dependency is file://../elitea-sandbox-runtime (charts/ links to it).
mkdir -p "${tmp}/helm"
cp -R "${here}" "${here}/../elitea-sandbox-runtime" "${tmp}/helm/"
rm -rf "${tmp}/helm/elitea/charts"
"$HELM" repo add elitea-vendor-nats "$repo" --force-update >/dev/null
"$HELM" dependency update "${tmp}/helm/elitea" >/dev/null
cp "${tmp}/helm/elitea/Chart.lock" "${here}/Chart.lock"
echo "vendored nats ${version} into ${here}/charts/nats"
