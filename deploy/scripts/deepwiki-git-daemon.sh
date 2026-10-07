#!/bin/sh
# The git daemon behind deploy/docker-compose.deepwiki-real-engine.yml.
#
# On first start, turns the read-only corpus mounted at /corpus into a bare
# repository named DEEPWIKI_GIT_REPOSITORY (owner/name, the repository the
# seeded toolkit names) under /repos, then serves /repos over git:// on 9418.
# The engine clones with --depth, which only a smart transport supports —
# hence `git daemon` and not a static HTTP tree.
set -eu

# The base image is plain debian:trixie-slim. Debian's `git` package carries
# `git daemon`; no CA bundle is needed, because the daemon serves git:// only.
if ! git daemon --version >/dev/null 2>&1; then
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends git >/dev/null
  rm -rf /var/lib/apt/lists/*
fi

REPOSITORY="${DEEPWIKI_GIT_REPOSITORY:-acme/e2e-generated}"
BASE=/repos
BARE="${BASE}/${REPOSITORY}.git"

if [ ! -d "${BARE}" ]; then
  work=$(mktemp -d)
  cp -R /corpus/. "${work}/"
  git -C "${work}" init -q -b main
  git -C "${work}" config user.email "deepwiki-e2e@autotest.local"
  git -C "${work}" config user.name "DeepWiki e2e"
  git -C "${work}" add -A
  git -C "${work}" commit -q -m "sample repository for the DeepWiki real-engine run"
  mkdir -p "$(dirname "${BARE}")"
  git clone -q --bare "${work}" "${BARE}"
  # `git daemon` exports only repositories that opt in, or everything with
  # --export-all; the marker is belt and braces for a partial copy.
  touch "${BARE}/git-daemon-export-ok"
  rm -rf "${work}"
fi
touch "${BASE}/.ready"

exec git daemon --reuseaddr --export-all --verbose \
  --base-path="${BASE}" --listen=0.0.0.0 --port=9418 "${BASE}"
