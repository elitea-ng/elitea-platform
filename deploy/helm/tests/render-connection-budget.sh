#!/usr/bin/env bash
# render-connection-budget.sh — the PostgreSQL connection ceiling (O1a).
#
# elitea-main opens SIX isolated runtime-plane pools per replica when the
# runtime plane is on (36 connections; validateDependencies forbids them
# sharing capacity) plus its primary pool. At autoscaling.maxReplicas: 10 that
# is ~440 against a stock max_connections of 100, and the chart set no pool
# size at all.
#
# Asserts BOTH directions, which is the shape render-llm-path.sh settled on: a
# guard that only ever refuses is indistinguishable from a broken template, and
# one that only ever renders is not a guard.
#
# Usage: deploy/helm/tests/render-connection-budget.sh
# Needs: helm. No cluster, no network.
set -euo pipefail

CHART="deploy/helm/elitea"
# values-standalone.yaml is the profile that turns the runtime plane ON, which
# is the only shape where the 36 pools exist. Using it rather than --set means
# this exercises a configuration somebody actually ships.
RUNTIME_VALUES="$CHART/values-standalone.yaml"

# The gateway refuses to render without its two postures; supply them so the
# failure under test is always THIS guard and never that one.
GATEWAY=(--set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1
         --set llmGateway.egressPosture=public-unrestricted)

failures=0
note() { printf '  %s\n' "$1"; }
fail() { printf 'FAIL: %s\n' "$1" >&2; failures=$((failures + 1)); }

render() { helm template budget "$CHART" -f "$RUNTIME_VALUES" "${GATEWAY[@]}" "$@" 2>&1; }

# ── 1. an unknowable primary pool is refused, not guessed ────────────────────
#
# cmd/elitea-main/database_pool.go returns EARLY when ELITEA_DATABASE_MAX_CONNS
# is unset, so pgx applies max(4, numCPU) — a value that depends on the node the
# pod lands on. A budget computed around that term would report a number it
# cannot stand behind.
# values.yaml now SETS a measured default (6), so this branch is reached only
# when somebody clears it — which is why the test clears it explicitly rather
# than relying on the chart to leave it unset. A gate that depended on the
# absence of a default would go quiet the moment a default was added, which is
# exactly what happened while this file was being written.
echo "== a CLEARED primary pool is refused while the runtime plane is on =="
if output="$(render --set main.env.ELITEA_DATABASE_MAX_CONNS=null)"; then
  fail "the runtime plane rendered with ELITEA_DATABASE_MAX_CONNS cleared"
elif [[ "$output" != *"cannot be computed"* ]]; then
  fail "refused for the wrong reason: $output"
else
  note "refused, and the message names the variable to set"
fi

# The default is present and inside the budget for the shipped profiles. Pinned
# because the arithmetic in every other assertion here is written against it.
echo "== the chart ships a primary pool size =="
if ! grep -q 'ELITEA_DATABASE_MAX_CONNS: "6"' "$CHART/values.yaml"; then
  fail "values.yaml no longer sets ELITEA_DATABASE_MAX_CONNS to 6; the numbers below assume it"
else
  note "values.yaml sets 6"
fi

# ── 2. the arithmetic ────────────────────────────────────────────────────────
echo "== an over-budget topology is refused, and the message shows its working =="
if output="$(render --set main.env.ELITEA_DATABASE_MAX_CONNS=8 --set main.autoscaling.maxReplicas=10)"; then
  fail "10 replicas x (8 + 36) = 440 connections rendered against a budget of 175"
else
  for fragment in "can open 440" "44 per elitea-main replica x 10 replicas" "only 175 are available"; do
    if [[ "$output" != *"$fragment"* ]]; then
      fail "the refusal does not show its working — missing: $fragment"
    fi
  done
  note "refused: 440 = 44 x 10, against 175"
fi

# ── 3. the guard PERMITS a sane topology ─────────────────────────────────────
#
# Without this the whole script would pass against a template that refuses
# everything, which is the failure mode a one-directional gate cannot see.
echo "== a topology inside the budget renders =="
if render --set main.env.ELITEA_DATABASE_MAX_CONNS=4 \
          --set main.autoscaling.enabled=false \
          --set main.replicaCount=1 >/dev/null; then
  note "1 replica x (4 + 36) = 40 renders, inside 175"
else
  fail "the guard refuses a topology well inside the budget, so it is not a guard"
fi

# ── 4. the boundary is where it claims to be ─────────────────────────────────
echo "== the boundary is exact =="
budget_render() {
  render --set "main.env.ELITEA_DATABASE_MAX_CONNS=$1" \
         --set main.autoscaling.enabled=false --set main.replicaCount=1 >/dev/null 2>&1
}
if budget_render 139; then note "175 renders"; else fail "175 was refused; the boundary is off by one"; fi
if budget_render 140; then fail "176 rendered; the boundary is off by one"; else note "176 refused"; fi

# ── 5. the runtime plane OFF does not pay for pools it does not open ─────────
#
# Measured on the E2E stack: one replica with the runtime plane off uses 6
# connections, not 40. A guard that counted the runtime pools unconditionally
# would refuse a default install that is nowhere near the ceiling.
echo "== the default install, with no runtime plane, is unaffected =="
if helm template budget "$CHART" "${GATEWAY[@]}" >/dev/null 2>&1; then
  note "default install renders"
else
  fail "the default install no longer renders"
fi

# ── 6. the pooler (issue #964) ──────────────────────────────────────────────
#
# With pgbouncer.enabled the guard splits: the server-side term becomes
# poolSize + reservePoolSize ONCE for the whole cluster, and a client-side
# term keeps every replica's pools under pgbouncer.maxClientConn. values-
# standalone.yaml ships the pooler OFF (its Postgres is operator-supplied), so
# each assertion below turns it on explicitly with the three values the
# component refuses without.
PB=(--set pgbouncer.enabled=true
    --set pgbouncer.postgresHost=pg.example.invalid
    --set pgbouncer.database=elitea
    --set pgbouncer.credentialsSecretName=pgbouncer-credentials)

echo "== a pooler without its three prerequisites is refused, named by field =="
if output="$(render "${PB[@]}" --set pgbouncer.postgresHost=null 2>&1)"; then
  fail "the pooler rendered without a postgresHost"
elif [[ "$output" != *"pgbouncer.enabled=true needs pgbouncer.postgresHost"* ]]; then
  fail "refused for the wrong reason: $output"
else
  note "refused, and the message names the missing field"
fi

echo "== a pooler block set while the component is off is refused =="
if output="$(render --set pgbouncer.postgresHost=pg.example.invalid 2>&1)"; then
  fail "a dead pgbouncer.postgresHost rendered while pgbouncer.enabled is false"
elif [[ "$output" != *"pgbouncer.enabled is false"* ]]; then
  fail "refused for the wrong reason: $output"
else
  note "refused: the dead setting is loud"
fi

echo "== a pooler pool that outgrows the server budget is refused =="
if output="$(render "${PB[@]}" --set pgbouncer.poolSize=180 2>&1)"; then
  fail "poolSize 180 + reserve 4 = 184 rendered against a server budget of 175"
else
  for fragment in "184 server-side connections" "only 175 are available"; do
    if [[ "$output" != *"$fragment"* ]]; then
      fail "the refusal does not show its working — missing: $fragment"
    fi
  done
  note "refused: 184 = 180 + 4, against 175"
fi

echo "== a replica fan-out that outgrows the pooler's client ceiling is refused =="
if output="$(render "${PB[@]}" --set main.env.ELITEA_DATABASE_MAX_CONNS=64 --set pgbouncer.maxClientConn=300 2>&1)"; then
  fail "4 replicas x (64 + 36) = 400 client connections rendered under a maxClientConn of 300"
else
  for fragment in "400 client connections" "100 per replica x 4" "pgbouncer.maxClientConn allows 300"; do
    if [[ "$output" != *"$fragment"* ]]; then
      fail "the refusal does not show its working — missing: $fragment"
    fi
  done
  note "refused: 400 = 100 x 4, against 300"
fi

# The client-side refusal must not have been the server-side one: 48 + 4 = 52
# is inside 175, so only the client term can have fired.
if render "${PB[@]}" --set main.env.ELITEA_DATABASE_MAX_CONNS=4 >/dev/null 2>&1; then
  note "a pooler topology inside both budgets renders (52 server, 160 client)"
else
  fail "the guard refuses a pooler topology inside both budgets, so it is not a guard"
fi

if [ "$failures" -ne 0 ]; then
  echo "render-connection-budget: $failures assertion(s) failed" >&2
  exit 1
fi
echo "render-connection-budget: every assertion passed"
