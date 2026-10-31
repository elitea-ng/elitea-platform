package repos

// The guardrail verdicts of a run (ADR-0043 decision 1, migration 0214)
// against a REAL migrated database. It SKIPS without ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"testing"
)

func TestGuardrailVerdictsReadByRunAndProject(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	store, err := ComposeRunSteps(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
INSERT INTO gateway.guardrail_verdicts
       (project_id, user_id, execution_id, route, stage, row_name, detector, detector_type, action, outcome,
        fail_mode, labels, findings, max_score, latency_ms, runs)
VALUES (3, 8, 'exec-gv-1', '/llm/v1/chat/completions', 'output', 'no-secrets', 'secrets', 'secrets', 'mask', 'masked',
        'closed', '{github-token}', 1, 0, 4, 3),
       (4, 8, 'exec-gv-1', '/llm/v1/chat/completions', 'input', 'other-project', 'secrets', 'secrets', 'block', 'pass',
        'closed', '{}', 0, 0, 1, 1)`); err != nil {
		t.Fatalf("seed verdicts: %v", err)
	}
	// The table refuses an outcome the gateway never writes.
	if _, err := pool.Exec(ctx, `INSERT INTO gateway.guardrail_verdicts (stage, action, outcome, fail_mode)
VALUES ('output', 'mask', 'leaked', 'closed')`); err == nil {
		t.Fatal("an unknown outcome was stored")
	}
	verdicts, err := store.GuardrailVerdicts(ctx, 3, "exec-gv-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 1 {
		t.Fatalf("verdicts = %+v; want the one of project 3", verdicts)
	}
	v := verdicts[0]
	if v.Outcome != "masked" || v.Row != "no-secrets" || len(v.Labels) != 1 || v.Labels[0] != "github-token" || v.Runs != 3 {
		t.Fatalf("verdict = %+v", v)
	}
}
