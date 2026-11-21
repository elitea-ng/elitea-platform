#!/usr/bin/env bash
# Install a pinned golangci-lint release binary into a directory and put it on PATH.
#
#   GH_TOKEN=... install-golangci-lint.sh <version without v> <sha256 of the linux-amd64 tarball> <bin dir>
#
# Through the API (the release asset endpoint), not the github.com release
# download link, which 504'd; checked against the pinned sha256. curl and jq
# only, so it runs on GitHub-hosted runners and on eng-runner, whose image has
# no gh. Used by ci-go.yml (workspace lint) and ci-gateway.yml (gateway lint),
# which pin different versions on purpose.
set -euo pipefail

version="${1:?usage: install-golangci-lint.sh <version> <sha256> <bin dir>}"
sha256="${2:?usage: install-golangci-lint.sh <version> <sha256> <bin dir>}"
bin="${3:?usage: install-golangci-lint.sh <version> <sha256> <bin dir>}"
: "${GH_TOKEN:?GH_TOKEN must be set to the github.token of the job}"

archive="golangci-lint-${version}-linux-amd64.tar.gz"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

asset="$(curl -sSfL --retry 3 -H "Authorization: Bearer ${GH_TOKEN}" \
  "https://api.github.com/repos/golangci/golangci-lint/releases/tags/v${version}" \
  | jq -er --arg name "${archive}" '.assets[] | select(.name == $name) | .url')"
# curl drops the Authorization header on the redirect to the storage host.
curl -sSfL --retry 3 -H "Authorization: Bearer ${GH_TOKEN}" -H "Accept: application/octet-stream" \
  -o "${tmp}/${archive}" "${asset}"
echo "${sha256}  ${tmp}/${archive}" | sha256sum -c -

mkdir -p "$bin"
tar -xzf "${tmp}/${archive}" -C "$bin" --strip-components=1 "golangci-lint-${version}-linux-amd64/golangci-lint"
if [ -n "${GITHUB_PATH:-}" ]; then echo "$bin" >> "$GITHUB_PATH"; fi
"$bin/golangci-lint" version
