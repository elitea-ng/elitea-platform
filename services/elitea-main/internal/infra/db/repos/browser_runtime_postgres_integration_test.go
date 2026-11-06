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

func TestBrowserSecondsSinceSumsTheProjectsSettledBrowserRows(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM gateway.llm_request_logs WHERE project_id IN (41, 42)`); err != nil {
		t.Fatal(err)
	}
	monthStart := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		project       int
		at            time.Time
		provider      string
		model         string
		operation     string
		units         any
		countedForP41 bool
	}{
		{41, monthStart.Add(time.Hour), "browser", "engine_seconds", "tool", 120, true},
		{41, monthStart.Add(2 * time.Hour), "browser", "engine_seconds", "tool", 30, true},
		// Last month, another provider, an llm row, another project: not counted.
		{41, monthStart.Add(-time.Minute), "browser", "engine_seconds", "tool", 999, false},
		{41, monthStart.Add(time.Hour), "firecrawl", "scrape", "tool", 5, false},
		{41, monthStart.Add(time.Hour), "browser", "engine_seconds", "llm", nil, false},
		{42, monthStart.Add(time.Hour), "browser", "engine_seconds", "tool", 7, false},
	} {
		if _, err := pool.Exec(ctx, `
INSERT INTO gateway.llm_request_logs
    (occurred_at, project_id, user_id, route, method, status, duration_ms, provider, model, operation, units, unit)
VALUES ($1, $2, 7, '/llm/v1/metered/settle', 'POST', 200, 10, $3, $4, $5, $6, CASE WHEN $6::bigint IS NULL THEN NULL ELSE 'second' END)`,
			row.at, row.project, row.provider, row.model, row.operation, row.units); err != nil {
			t.Fatalf("seed: %v", err)
		}
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
