package migrations_test

// shared/0225: the reasoning control family defaults and the request log's
// reasoning_tokens (ADR-0038 decisions 3 and 5, #1452).
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"os"
	"testing"
)

const reasoningControlMigration = "shared/0225_model_reasoning_control.sql"

func TestReasoningDefaultsAreSeededInPriorityOrder(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := testContext()
	defer cancel()

	// The first pattern that matches, in the order Main reads the table.
	match := func(name string) string {
		t.Helper()
		var control string
		err := pool.QueryRow(ctx, `
SELECT coalesce((
    SELECT reasoning_control FROM elitea_runtime.model_reasoning_defaults
    WHERE $1 ~* name_pattern ORDER BY priority, name_pattern LIMIT 1), '')`, name).Scan(&control)
		if err != nil {
			t.Fatalf("match %q: %v", name, err)
		}
		return control
	}
	for name, want := range map[string]string{
		"Qwen/Qwen3-8B":                      "toggle",
		"Qwen/Qwen3-235B-A22B-Thinking-2507": "fixed_on",
		"gpt-5.1":                            "effort",
		"o3-mini":                            "",
		"claude-sonnet-4-5":                  "budget",
		"gemini-2.5-pro":                     "fixed_on",
	} {
		if got := match(name); got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}

	// The check refuses a form the table does not know.
	if _, err := pool.Exec(ctx, `INSERT INTO elitea_runtime.model_reasoning_defaults
		(name_pattern, reasoning_control) VALUES ('x', 'adaptive')`); err == nil {
		t.Fatal("an unknown reasoning_control was stored")
	}
}

func TestReasoningControlMigrationIsIdempotentAndKeepsAnOperatorChange(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := testContext()
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE elitea_runtime.model_reasoning_defaults
		SET reasoning_control = 'effort' WHERE name_pattern = '(^|/)qwen'`); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(reasoningControlMigration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("re-apply %s: %v", reasoningControlMigration, err)
	}
	var control string
	if err := pool.QueryRow(ctx, `SELECT reasoning_control FROM elitea_runtime.model_reasoning_defaults
		WHERE name_pattern = '(^|/)qwen'`).Scan(&control); err != nil || control != "effort" {
		t.Fatalf("a re-run must keep the operator's change: %q %v", control, err)
	}
}

func TestRequestLogAndUsageLedgerHaveReasoningTokens(t *testing.T) {
	pool := newMigratedPool(t)
	ctx, cancel := testContext()
	defer cancel()
	for _, table := range []string{"llm_request_logs", "llm_usage_events"} {
		var dataType, defaultValue string
		var nullable string
		if err := pool.QueryRow(ctx, `
SELECT data_type, column_default, is_nullable FROM information_schema.columns
WHERE table_schema = 'gateway' AND table_name = $1 AND column_name = 'reasoning_tokens'`, table).
			Scan(&dataType, &defaultValue, &nullable); err != nil {
			t.Fatalf("%s.reasoning_tokens: %v", table, err)
		}
		if dataType != "bigint" || defaultValue != "0" || nullable != "NO" {
			t.Fatalf("%s.reasoning_tokens is %s default %s nullable %s, want bigint default 0 not null",
				table, dataType, defaultValue, nullable)
		}
	}
}
