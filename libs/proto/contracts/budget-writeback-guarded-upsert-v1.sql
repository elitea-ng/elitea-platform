-- The guarded HEALTHY accumulator UPSERT, as two writers send it.
--   - services/elitea-scheduler/internal/budgetwriteback/store.go upsertSQL
--     (the write-back consumer);
--   - services/elitea-llm-gateway/internal/failmode/store.go healthyUpsertSQL
--     (PersistHealthyDelta, [human decision] 2026-10-16, "Healthy write").
-- The two modules cannot import each other. A parity test in EACH reads this
-- file and compares it with its own statement, whitespace-normalised and with
-- these comment lines removed. Change all three together. The divisor is
-- NanoUSD (1e9) rendered.
-- $1 project_id, $2 org_id, $3 scope, $4 scope_id, $5 period_start,
-- $6 period_end, $7 delta_nano.
INSERT INTO gateway.llm_budget_accumulators AS acc
		(project_id, org_id, scope, scope_id, period_start, period_end,
		 accumulated_cost, outage_mode, reconciled, last_updated)
	VALUES ($1, $2, $3, $4, to_timestamp($5), to_timestamp($6),
		$7::numeric / 1000000000, false, false, now())
	ON CONFLICT (scope, scope_id, period_start) DO UPDATE SET
		accumulated_cost = acc.accumulated_cost + EXCLUDED.accumulated_cost,
		last_updated = now()
	WHERE NOT (acc.outage_mode AND NOT acc.reconciled)
