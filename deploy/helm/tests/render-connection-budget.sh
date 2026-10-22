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
# Every `helm template` here starts from the pre-S10 feature switches
# (fixtures/features-off.yaml: runtime, worker, vector, Qdrant, DeepWiki and
# Inventory off), prepended so that any values file or --set a render names
# wins; each render turns on what it tests. render-default-on.sh covers the
# shipped defaults.
FEATURES_OFF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/fixtures/features-off.yaml"
_helm_bin="${HELM:-helm}"
helm() {
  if [ "${1:-}" = template ]; then shift; command "$_helm_bin" template -f "$FEATURES_OFF" "$@"
  else command "$_helm_bin" "$@"; fi
}
HELM=helm

CHART="deploy/helm/elitea"
# values-standalone.yaml is the profile that turns the runtime plane ON, which
# is the only shape where the 36 pools exist. Using it rather than --set means
# this exercises a configuration somebody actually ships.
RUNTIME_VALUES="$CHART/values-standalone.yaml"

# The gateway refuses to render without its two postures; supply them so the
# failure under test is always THIS guard and never that one.
GATEWAY=(--set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1
         --set llmGateway.egressPosture=public-unrestricted --set networkPolicies.main.noExternalIngress=true)

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
  fail "10 replicas x (8 + 36) = 440 connections (480 with the gateway and scheduler) rendered against a budget of 395"
else
  # values-standalone.yaml: maxConnections 420 - reserved 25 = 395. Since S11 the
  # budget counts every component at its replica ceiling, so the total is
  # main's 440 plus the gateway's 32 (8 x 4, autoscaling) and the scheduler's
  # 8 (4 x 2, active/active), and the message lists each share.
  for fragment in "can open 480" "44 per elitea-main replica x 10 replicas" "llm-gateway 32 (8 x 4)" "scheduler 8 (4 x 2)" "only 395 are available"; do
    if [[ "$output" != *"$fragment"* ]]; then
      fail "the refusal does not show its working — missing: $fragment"
    fi
  done
  note "refused: 480 = 44 x 10 + 32 + 8, against 395"
fi

# ── 3. the guard PERMITS a sane topology ─────────────────────────────────────
#
# Without this the whole script would pass against a template that refuses
# everything, which is the failure mode a one-directional gate cannot see.
echo "== a topology inside the budget renders =="
if render --set main.env.ELITEA_DATABASE_MAX_CONNS=4 \
          --set main.autoscaling.enabled=false \
          --set main.replicaCount=1 >/dev/null; then
  note "1 replica x (4 + 36) + 40 = 80 renders, inside 395"
else
  fail "the guard refuses a topology well inside the budget, so it is not a guard"
fi

# ── 4. the boundary is where it claims to be ─────────────────────────────────
echo "== the boundary is exact =="
budget_render() {
  render --set "main.env.ELITEA_DATABASE_MAX_CONNS=$1" \
         --set main.autoscaling.enabled=false --set main.replicaCount=1 >/dev/null 2>&1
}
# (319 + 36) + gateway 32 + scheduler 8 = 395, exactly the budget; 320 is one over.
if budget_render 319; then note "395 renders"; else fail "395 was refused; the boundary is off by one"; fi
if budget_render 320; then fail "396 rendered; the boundary is off by one"; else note "396 refused"; fi

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
if output="$(render "${PB[@]}" --set pgbouncer.poolSize=392 2>&1)"; then
  fail "poolSize 392 + reserve 4 = 396 rendered against a server budget of 395"
else
  for fragment in "396 server-side connections" "only 395 are available"; do
    if [[ "$output" != *"$fragment"* ]]; then
      fail "the refusal does not show its working — missing: $fragment"
    fi
  done
  note "refused: 396 = 392 + 4, against 395"
fi
# The other side of the pooler's own boundary: 391 + 4 = 395 passes the
# pooler's guard. The release total (395 + 40) is then refused by the main
# budget, which names the pooler's 395, not the pooler guard.
output="$(render "${PB[@]}" --set pgbouncer.poolSize=391 2>&1)" || true
if [[ "$output" == *"server-side connections for the whole cluster"* ]]; then
  fail "poolSize 391 + reserve 4 = 395 was refused by the pooler guard; the boundary is off by one"
elif [[ "$output" != *"395 PgBouncer"* ]]; then
  fail "poolSize 391: the release budget does not count the pooler's 395: $output"
else
  note "395 = 391 + 4 passes the pooler guard, and the release budget counts it"
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

echo "== a literal 0 reaches the validator (sprig default would mask it) =="
if output="$(render "${PB[@]}" --set pgbouncer.maxPreparedStatements=0 2>&1)"; then
  fail "maxPreparedStatements=0 rendered: the validator saw a default instead of the 0 the ConfigMap emits"
elif [[ "$output" != *"pgbouncer.maxPreparedStatements is 0"* ]]; then
  fail "refused for the wrong reason: $output"
else
  note "refused: the validator and the ConfigMap read the same 0"
fi

# The client-side refusal must not have been the server-side one: 48 + 4 = 52
# (+ 40 for the gateway and scheduler) is inside 395, so only the client term
# can have fired.
if render "${PB[@]}" --set main.env.ELITEA_DATABASE_MAX_CONNS=4 >/dev/null 2>&1; then
  note "a pooler topology inside both budgets renders (52 server, 160 client)"
else
  fail "the guard refuses a pooler topology inside both budgets, so it is not a guard"
fi

# ── the migration keeps the direct DSN when main is pointed at the pooler ────
#
# main.secrets.DATABASE_URL is the operator's switch that puts elitea-main's
# pods behind the pooler. The migrate Job reads main.secrets too, and before
# this check it followed main through the pooler.
POOLED=(--set postgresql.existingSecret=direct-db --set postgresql.key=database-url
        --set main.secrets.DATABASE_URL.secretName=pooled-db
        --set main.secrets.DATABASE_URL.key=database-url)
output="$(render "${PB[@]}" "${POOLED[@]}" --set main.env.ELITEA_DATABASE_MAX_CONNS=4 \
  --show-only templates/main/migrate-job.yaml 2>&1)" || true
if [[ "$output" == *"pooled-db"* ]]; then
  fail "the migrate Job reads the pooled DSN"
elif [[ "$output" != *"direct-db"* ]]; then
  fail "the migrate Job does not read postgresql.existingSecret: $output"
else
  note "the migrate Job keeps the direct DSN behind a pooled elitea-main"
fi
output="$(render "${PB[@]}" "${POOLED[@]}" --set main.env.ELITEA_DATABASE_MAX_CONNS=4 \
  --show-only templates/main/deployment.yaml 2>&1)" || true
if [[ "$output" != *"pooled-db"* ]]; then
  fail "elitea-main's pods lost the pooled DSN"
else
  note "elitea-main's pods read the pooled DSN"
fi

# ── 7. every component at its replica ceiling: the shipped defaults ──────
#
# The default install runs every component, and every one autoscales. The
# budget counts each at its ceiling. The defaults must fit the default server
# (600 - 25 reserved = 575): 568 = main 352 (44 x 8) + gateway 32 (8 x 4) +
# scheduler 8 (4 x 2) + deepwiki host 15 (5 x 3) + deepwiki engine 48 (16 x 3)
# + inventory host 9 (3 x 3) + inventory engine 24 (8 x 3) + worker 80 (8 x 10).
OPERATOR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/fixtures/operator-values.yaml"
# No features-off here: these renders are the shipped defaults.
default_render() { command "$_helm_bin" template budget "$CHART" -f "$OPERATOR" "$@" 2>&1; }
echo "== the defaults at every ceiling fit the default server =="
if output="$(default_render)"; then
  note "the default install renders inside 575"
else
  fail "the default install does not fit its own default server: $(printf '%s' "$output" | grep -m1 Error)"
fi
# The breakdown: render against a server that is too small, and read the rows.
output="$(default_render --set postgresql.maxConnections=100)" || true
for fragment in "can open 568" "44 per elitea-main replica x 8 replicas" "llm-gateway 32 (8 x 4)" "scheduler 8 (4 x 2)" \
    "deepwiki host 15 (5 x 3)" "deepwiki engine 48 (16 x 3)" "inventory host 9 (3 x 3)" "inventory engine 24 (8 x 3)" "worker AgentState 80 (8 x 10)"; do
  if [[ "$output" != *"$fragment"* ]]; then
    fail "the default breakdown is missing: $fragment"
  fi
done
note "568 = 352 + 32 + 8 + 15 + 48 + 9 + 24 + 80"
# The boundary at the defaults: 568 + 25 = 593 renders, 592 is one short.
if default_render --set postgresql.maxConnections=593 >/dev/null; then note "593 (568 available) renders"; else fail "593 was refused; the boundary is off by one"; fi
if default_render --set postgresql.maxConnections=592 >/dev/null; then fail "592 (567 available) rendered; the boundary is off by one"; else note "592 refused"; fi

# The worker row is agentstate_max_connections x the worker's ceiling. Unset,
# the worker uses min(8, delivery_max_concurrency), and so does the count. A
# pool above delivery_max_concurrency is refused: the worker refuses it at
# start.
echo "== the worker row: agentstate_max_connections x worker max replicas =="
if output="$(default_render --set worker.runtime.limits.agentstate_max_connections=9)"; then
  fail "a worker pool of 9 (90 for ten replicas, 578 in all) rendered against 575"
elif [[ "$output" != *"worker AgentState 90 (9 x 10)"* ]] || [[ "$output" != *"can open 578"* ]]; then
  fail "the worker pool of 9 was refused, but not as 90 (9 x 10) in 578: $output"
else
  note "refused: a worker pool of 9 is 90 (9 x 10), 578 in all"
fi
output="$(default_render --set worker.runtime.limits.agentstate_max_connections=null --set postgresql.maxConnections=100)" || true
[[ "$output" == *"worker AgentState 80 (8 x 10)"* ]] && note "unset: min(8, 32) = 8 per worker" || fail "an unset worker pool is not counted as min(8, 32): $output"
output="$(default_render --set worker.runtime.limits.agentstate_max_connections=null --set worker.runtime.limits.delivery_max_concurrency=4 --set postgresql.maxConnections=100)" || true
[[ "$output" == *"worker AgentState 40 (4 x 10)"* ]] && note "unset: min(8, 4) = 4 per worker" || fail "an unset worker pool is not counted as min(8, 4): $output"
if output="$(default_render --set worker.runtime.limits.agentstate_max_connections=5 --set worker.runtime.limits.delivery_max_concurrency=4)"; then
  fail "a worker pool above delivery_max_concurrency rendered"
elif [[ "$output" != *"agentstate_max_connections (5) is above delivery_max_concurrency (4)"* ]]; then
  fail "a worker pool above delivery_max_concurrency was refused for the wrong reason: $output"
else
  note "refused: a worker pool above delivery_max_concurrency"
fi
if default_render --set worker.runtime.limits.agentstate_max_connections=4 --set worker.runtime.limits.delivery_max_concurrency=4 >/dev/null; then
  note "a worker pool equal to delivery_max_concurrency renders"
else
  fail "a worker pool equal to delivery_max_concurrency is refused"
fi

# The Inventory host opens its invocation store on the engine's database
# (ELITEA_INVENTORY_DATABASE_MAX_CONNS + 1 for its listener) on each replica,
# whenever the database secret exists. Without that secret there is no row.
echo "== the inventory host row follows the database secret =="
output="$(default_render --set inventory.env.ELITEA_INVENTORY_DATABASE_MAX_CONNS=3 --set postgresql.maxConnections=100)" || true
[[ "$output" == *"inventory host 12 (4 x 3)"* ]] && note "a host pool of 3 is (3 + 1) x 3 = 12" || fail "a host pool of 3 is not counted as 12 (4 x 3): $output"
output="$(default_render --set inventory.secrets.ELITEA_INVENTORY_DATABASE_URL=null --set postgresql.existingSecret= \
  --set inventory.autoscaling.enabled=false --set deepwiki.autoscaling.enabled=false --set postgresql.maxConnections=100)" || true
if [[ "$output" == *"inventory host"* ]]; then
  fail "the inventory host is counted with no database secret: $output"
elif [[ "$output" != *"worker AgentState"* ]]; then
  fail "no database secret: the render did not reach the budget: $output"
else
  note "no database secret: no inventory host row"
fi

if [ "$failures" -ne 0 ]; then
  echo "render-connection-budget: $failures assertion(s) failed" >&2
  exit 1
fi
echo "render-connection-budget: every assertion passed"
