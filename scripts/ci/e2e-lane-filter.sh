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
#   deploy/helm/**                     the chart. No stack lane renders it;
#                                      helm-lint.yml and helm-install-smoke.yml do.
#   apps/elitea-desktop/**             the Tauri host. No stack lane runs it;
#                                      ci-desktop.yml does.
#   apps/elitea-web/src/entries/docs/** the embedded docs SPA. No journey opens
#                                      /docs; ci-web.yml's `npm run build`
#                                      builds it (build:docs), so a broken
#                                      page still fails a gate.
#   *.md                               prose — EXCEPT under apps/elitea-web/src/
#                                      (the app imports .md with `?raw`, e.g.
#                                      entries/desktop/agents/local-coding-agent.md)
#                                      and under any test, fixture, testdata,
#                                      e2e or conformance path, where a .md
#                                      file is test input.
#
# NOT skipped: apps/elitea-web/src/entries/desktop/**. It is not separable —
# the main app imports from it (useLocalCodingAgent.ts) — so it reaches the
# web image like any other source.
#
# Anything else reaches the stack. An EMPTY list (a failed diff, an empty pull
# request) reaches everything: "nothing changed" must never read as "nothing
# to test". scripts/ci/e2e-lane-filter-test.sh drives every arm.
set -euo pipefail

SKIP_TREE='^(deploy/helm/|apps/elitea-desktop/|apps/elitea-web/src/entries/docs/)'
MD_KEEP='^(apps/elitea-web/src/|conformance/)|(^|/)(tests?|fixtures|testdata|e2e)/'
MODEL='^(services/elitea-llm-gateway/|services/elitea-worker-rust/|libs/rust/llm-wire/|libs/rust/agent-runtime/|libs/rust/connectors/|services/elitea-main/internal/api/v2/executions/|services/elitea-main/internal/llmproxy/|services/elitea-main/internal/runtimecomposition/|apps/elitea-web/src/features/chat-|apps/elitea-web/e2e/|apps/elitea-web/scripts/chat-stream-e2e\.sh|apps/elitea-web/playwright\.config\.ts|deploy/docker-compose\.standalone-(full|rust-agent)\.yml|deploy/scripts/|deploy/ci/|\.github/actions/|\.github/workflows/ci-web-e2e\.yml)'

total=0
reaching=()
while IFS= read -r f; do
  [ -n "$f" ] || continue
  total=$((total + 1))
  if [[ "$f" =~ $SKIP_TREE ]]; then
    continue
  fi
  if [[ "$f" == *.md && ! "$f" =~ $MD_KEEP ]]; then
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
