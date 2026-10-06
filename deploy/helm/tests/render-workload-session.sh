#!/usr/bin/env bash
# render-workload-session.sh — the session that expires and the CronJob that
# is the only thing stopping it (O2b).
#
# elitea_runtime.workload_sessions.expires_at is NOT NULL and NOTHING in the
# product writes it: repos/workload_sessions.go's VerifyActiveSession only
# READS it, requiring `issued_at <= now() < expires_at AND revoked_at IS NULL`.
# The pre-install hook stamps it; the renew CronJob re-stamps it. When it
# lapses, every ClaimCommand is refused while the worker still reports Ready —
# a signature identical to never having provisioned the row at all.
#
# Both directions, because a guard that only refuses is indistinguishable from
# a broken template.
#
# Usage: deploy/helm/tests/render-workload-session.sh
# Needs: helm. No cluster, no network.
set -euo pipefail

CHART="deploy/helm/elitea"

# The worker is OFF in the shipped defaults, and these guards are inside
# `if .Values.worker.enabled`. Turning it on here is not incidental: without
# it every assertion below passes against a guard that never runs, which is
# how this file read green while measuring nothing during development.
BASE=(-f "$CHART/values-standalone.yaml"
      --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1
      --set llmGateway.egressPosture=public-unrestricted
      --set worker.enabled=true)

failures=0
note() { printf '  %s\n' "$1"; }
fail() { printf 'FAIL: %s\n' "$1" >&2; failures=$((failures + 1)); }

# expect <renders|refused> <description> [extra --set flags...]
expect() {
  local want="$1" desc="$2"; shift 2
  local got
  if helm template session "$CHART" "${BASE[@]}" "$@" >/dev/null 2>&1; then got=renders; else got=refused; fi
  if [ "$got" = "$want" ]; then note "$desc ($got)"; else fail "$desc: expected $want, got $got"; fi
}

echo "== the guard runs at all =="
expect renders "the shipped defaults, worker on" 

echo "== renewal cannot be switched off silently =="
expect refused "renew disabled with nothing acknowledged" \
  --set worker.runtimeSession.renew.enabled=false
expect renders "renew disabled, but manual renewal acknowledged" \
  --set worker.runtimeSession.renew.enabled=false \
  --set worker.runtimeSession.renew.acknowledgeManualRenewal=true

echo "== ttl and schedule are linked, as values.yaml says they must be =="
# values.yaml: "If you change `ttl`, re-derive this alongside it; the two are
# not linked automatically." This is that link.
expect refused "ttl 240h against a 10-day schedule (one missed run outlives it)" \
  --set worker.runtimeSession.ttl=240h
expect renders "ttl 480h — exactly two renewal windows" \
  --set worker.runtimeSession.ttl=480h
expect refused "ttl 479h — one hour short of two windows" \
  --set worker.runtimeSession.ttl=479h

echo "== a schedule the guard cannot read is left alone, not guessed =="
# A guard that mis-parses a cron expression and refuses a correct one is worse
# than no guard, so only the day-of-month step form is checked.
expect renders "a list-form schedule with a short ttl is not second-guessed" \
  --set worker.runtimeSession.ttl=240h \
  --set 'worker.runtimeSession.renew.schedule=0 3 1\,15 * *'

echo "== the worker being off does not drag these guards in =="
expect renders "worker disabled" --set worker.enabled=false

if [ "$failures" -ne 0 ]; then
  echo "render-workload-session: $failures assertion(s) failed" >&2
  exit 1
fi
echo "render-workload-session: every assertion passed"
