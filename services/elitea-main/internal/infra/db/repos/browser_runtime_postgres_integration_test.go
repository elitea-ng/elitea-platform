package repos

// The browser runtime sources (ADR-0049, #1596) against a REAL migrated
// database: the month's settled browser seconds of one project, and the
// execution's trigger origin. It SKIPS without ELITEA_TEST_DATABASE_URL;
// each case asserts values a skip could not have produced.

import (
	"context"
	"testing"
	"time"
)

func TestBrowserSecondsSinceSumsTheProjectsLedgerRows(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	for _, table := range []string{"gateway.llm_usage_events", "gateway.llm_request_logs"} {
		if _, err := pool.Exec(ctx, `DELETE FROM `+table+` WHERE project_id IN (41, 42)`); err != nil {
			t.Fatal(err)
		}
	}
	monthStart := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		project   int
		at        time.Time
		provider  string
		model     string
		operation string
		units     any
	}{
		{41, monthStart.Add(time.Hour), "browser", "engine_seconds", "tool", 120},
		{41, monthStart.Add(2 * time.Hour), "browser", "engine_seconds", "tool", 30},
		// Last month, another provider, an llm row, another project, a
		// zero-second refusal: not counted.
		{41, monthStart.Add(-time.Minute), "browser", "engine_seconds", "tool", 999},
		{41, monthStart.Add(time.Hour), "firecrawl", "scrape", "tool", 5},
		{41, monthStart.Add(time.Hour), "browser", "engine_seconds", "llm", nil},
		{42, monthStart.Add(time.Hour), "browser", "engine_seconds", "tool", 7},
		{41, monthStart.Add(time.Hour), "browser", "engine_seconds", "tool", 0},
	} {
		if _, err := pool.Exec(ctx, `
INSERT INTO gateway.llm_usage_events
    (event_id, project_id, user_id, provider, model, cost_usd, period_start, period_end, occurred_at,
     operation, units, unit)
VALUES (gen_random_uuid(), $1, 7, $2, $3, 0, $4, $4 + interval '1 month', $5, $6, $7,
        CASE WHEN $7::bigint IS NULL THEN NULL ELSE 'second' END)`,
			row.project, row.provider, row.model, monthStart, row.at, row.operation, row.units); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// The request log is not the source: a row there (a duplicate settle,
	// or a sampled one) adds nothing.
	if _, err := pool.Exec(ctx, `
INSERT INTO gateway.llm_request_logs
    (occurred_at, project_id, user_id, route, method, status, duration_ms, provider, model, operation, units, unit)
VALUES ($1, 41, 7, '/llm/v1/metered/settle', 'POST', 200, 10, 'browser', 'engine_seconds', 'tool', 500, 'second')`,
		monthStart.Add(time.Hour)); err != nil {
		t.Fatalf("seed the request log: %v", err)
	}
	sources, err := NewBrowserRuntimeSources(pool)
	if err != nil {
		t.Fatal(err)
	}
	used, err := sources.BrowserSecondsSince(ctx, 41, monthStart)
	if err != nil || used != 150 {
		t.Fatalf("used=%d err=%v, want 150", used, err)
	}
	if used, err := sources.BrowserSecondsSince(ctx, 43, monthStart); err != nil || used != 0 {
		t.Fatalf("an empty project used=%d err=%v", used, err)
	}
}

func TestBrowserUnattendedGrantReadsTheRunChain(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	root := "aaaaaaaaaaaaaaaaaaaaaaaaaaaa1596"
	if _, err := pool.Exec(ctx, `DELETE FROM elitea_runtime.run_chains WHERE root_execution_id = $1`, root); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.run_chains (
    root_execution_id, project_id, owner_user_id, conversation_uuid, origin, trigger_origin,
    grant_id, grant_json, max_continuations, max_spend_usd, deadline, last_execution_id)
VALUES ($1, 9, 7, gen_random_uuid(), 'cloud', 'manual', gen_random_uuid(), '{}'::jsonb, 3, 0,
        clock_timestamp() + interval '1 hour', $1)`, root); err != nil {
		t.Fatalf("seed the chain: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.run_chain_links (execution_id, root_execution_id, continuation_index, kind)
VALUES ($1, $1, 0, 'root')`, root); err != nil {
		t.Fatalf("seed the link: %v", err)
	}
	sources, err := NewBrowserRuntimeSources(pool)
	if err != nil {
		t.Fatal(err)
	}
	if granted, err := sources.UnattendedGrant(ctx, root); err != nil || !granted {
		t.Fatalf("granted=%v err=%v", granted, err)
	}
	if granted, err := sources.UnattendedGrant(ctx, "bbbbbbbbbbbbbbbbbbbbbbbbbbbb1596"); err != nil || granted {
		t.Fatalf("an execution without a chain: granted=%v err=%v", granted, err)
	}
}

func TestBrowserTriggerOriginReadsTheJob(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	seedEgressExecution(t, pool, "exec-browser-origin", 9, 0)
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.execution_jobs SET trigger_origin = 'schedule' WHERE execution_id = 'exec-browser-origin'`); err != nil {
		t.Fatal(err)
	}
	sources, err := NewBrowserRuntimeSources(pool)
	if err != nil {
		t.Fatal(err)
	}
	if origin, err := sources.TriggerOrigin(ctx, "exec-browser-origin"); err != nil || origin != "schedule" {
		t.Fatalf("origin=%q err=%v", origin, err)
	}
	if origin, err := sources.TriggerOrigin(ctx, "exec-browser-absent"); err != nil || origin != "manual" {
		t.Fatalf("origin=%q err=%v", origin, err)
	}
	if setting, err := sources.BrowserProjectSetting(ctx, 987654); err != nil || setting.Enabled {
		t.Fatalf("a project without a tenant table: setting=%+v err=%v", setting, err)
	}
}
