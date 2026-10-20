#!/usr/bin/env bash
# e2e-lane-filter.sh — which ci-web-e2e.yml lanes a pull request reaches.
#
# Reads the pull request's changed paths on stdin, one per line, and prints
# two lines for $GITHUB_OUTPUT:
#
#   reaches-stack=true|false   the stack lanes (E2E, chat/index on Rust,
#                              DeepWiki, conformance, visual) run
#   reaches-model=true|false   the real-model lane runs
#
# ── WHAT IS SKIPPED, AND WHY EACH IS SAFE ───────────────────────────────────
#
# Measured 2026-10-20: Web E2E ran on 97% of pull-request SHAs, because its
# `paths:` names whole trees (`deploy/**`, `apps/elitea-web/**`, every
# service) and a README or a chart edit under them starts six stack lanes. A
# changed path is OUTSIDE the stack, and does not count, when it is:
#
#   deploy/helm/**     the chart. No stack lane renders it; helm-lint.yml and
#                      helm-install-smoke.yml do. EXCEPT every path under it
#                      that a deploy/*.yml compose file references (today
#                      ./helm/nats-bootstrap/files/bootstrap.sh, bind-mounted
#                      by the standalone stacks). That list is READ from the
#                      compose files on every run, never written here, so a
#                      new bind mount reaches the stack without editing this.
#   apps/elitea-desktop/**
#                      the Tauri host. No stack lane runs it, nothing under
#                      apps/elitea-web or deploy imports it; ci-desktop.yml
#                      builds it.
#   *.md at the repository root, or under docs/ or .github/
#                      prose. NOT a .md anywhere else: services/** and
#                      libs/** embed some (`//go:embed templates/README.md`
#                      in elitea-main's brandpackage), apps/elitea-web imports
#                      some with `?raw`, and fixtures are test input.
#
# NOT skipped, with the reason:
#   apps/elitea-web/src/entries/docs/**  vite.config.ts imports from it
#       (entries/docs/mdx/remark-mermaid) and the docs SPA ships in the web image.
#   apps/elitea-web/src/entries/desktop/**  the main app imports from it
#       (useLocalCodingAgent.ts imports its .md with `?raw`).
#
# Anything else reaches the stack. An EMPTY list (a failed diff, an empty pull
# request) reaches everything: "nothing changed" must never read as "nothing
# to test". scripts/ci/e2e-lane-filter-test.sh drives every arm.
#
# COMPOSE_DIR (default deploy) is where the compose files are read; the test
# points it at a fixture.
set -euo pipefail

SKIP_TREE='^(deploy/helm/|apps/elitea-desktop/)'
# A .md is prose only here: the repository root, docs/, .github/.
MD_PROSE='^([^/]+|docs/.+|\.github/.+)\.md$'
COMPOSE_DIR="${COMPOSE_DIR:-deploy}"

# Every deploy/helm path a compose file references, as a repository path.
# A compose file names it relative to deploy/ (`./helm/...`).
helm_used=()
while IFS= read -r ref; do
  [ -n "$ref" ] && helm_used+=("deploy/${ref#./}")
done < <(cat "$COMPOSE_DIR"/*.yml "$COMPOSE_DIR"/*.yaml 2>/dev/null | grep -oE '\./helm/[^[:space:]:"'"'"']+' | sort -u)

used_by_compose() {
  local f="$1" u
  for u in ${helm_used[@]+"${helm_used[@]}"}; do
    # A file, or anything under a referenced directory.
    if [ "$f" = "$u" ] || [[ "$f" == "$u"/* ]]; then
      return 0
    fi
  done
  return 1
}
MODEL='^(services/elitea-llm-gateway/|services/elitea-worker-rust/|libs/rust/llm-wire/|libs/rust/agent-runtime/|libs/rust/connectors/|services/elitea-main/internal/api/v2/executions/|services/elitea-main/internal/llmproxy/|services/elitea-main/internal/runtimecomposition/|apps/elitea-web/src/features/chat-|apps/elitea-web/e2e/|apps/elitea-web/scripts/chat-stream-e2e\.sh|apps/elitea-web/playwright\.config\.ts|deploy/docker-compose\.standalone-(full|rust-agent)\.yml|deploy/scripts/|deploy/ci/|\.github/actions/|\.github/workflows/ci-web-e2e\.yml)'

total=0
reaching=()
while IFS= read -r f; do
  [ -n "$f" ] || continue
  total=$((total + 1))
  if [[ "$f" =~ $SKIP_TREE ]] && ! used_by_compose "$f"; then
    continue
  fi
  if [[ "$f" =~ $MD_PROSE ]]; then
    continue
  fi
  reaching+=("$f")
done

if [ "$total" -eq 0 ]; then
  echo "::warning::no changed path was read; every lane runs" >&2
  echo "reaches-stack=true"
  echo "reaches-model=true"
  exit 0
fi

if [ "${#reaching[@]}" -eq 0 ]; then
  echo "all ${total} changed path(s) are outside the stack (docs, desktop, helm): stack lanes skipped" >&2
  echo "reaches-stack=false"
  echo "reaches-model=false"
  exit 0
fi

echo "${#reaching[@]} of ${total} changed path(s) reach the stack, e.g.:" >&2
printf '  %s\n' "${reaching[@]:0:20}" >&2
echo "reaches-stack=true"
if printf '%s\n' "${reaching[@]}" | grep -qE "$MODEL"; then
  echo "model path touched:" >&2
  printf '%s\n' "${reaching[@]}" | grep -E "$MODEL" | sed 's/^/  /' >&2
  echo "reaches-model=true"
else
  echo "no model-path file changed; the real-model lane is skipped" >&2
  echo "reaches-model=false"
fi
