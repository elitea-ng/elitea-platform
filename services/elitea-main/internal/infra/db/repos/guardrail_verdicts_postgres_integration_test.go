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

// TestGuardrailNoticeVerdictsKeepOnlyWhatTheChatReports pins migration 0247
// (warn and warned are accepted) and the notice read: only blocked, masked,
// warned and error_closed verdicts of the run and project come back.
func TestGuardrailNoticeVerdictsKeepOnlyWhatTheChatReports(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	store, err := ComposeRunSteps(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
INSERT INTO gateway.guardrail_verdicts
       (project_id, user_id, execution_id, route, stage, row_name, detector, detector_type, action, outcome, fail_mode, labels)
VALUES (3, 8, 'exec-gn-1', '/llm/v1/chat/completions', 'input', 'heads-up', 'pii', 'personal_data', 'warn', 'warned', 'open', '{email}'),
       (3, 8, 'exec-gn-1', '/llm/v1/chat/completions', 'input', 'audit', 'regex', 'regex', 'flag', 'flagged', 'open', '{x}'),
       (3, 8, 'exec-gn-1', '/llm/v1/chat/completions', 'input', 'ok', 'pii', 'personal_data', 'mask', 'pass', 'closed', '{}'),
       (3, 8, 'exec-gn-1', '/llm/v1/chat/completions', 'input', 'cloud', 'azure', 'azure_content_safety', 'block', 'error_closed', 'closed', '{detector_error}'),
       (4, 8, 'exec-gn-1', '/llm/v1/chat/completions', 'input', 'other', 'pii', 'personal_data', 'warn', 'warned', 'open', '{email}')`); err != nil {
		t.Fatalf("seed verdicts (the warn action and warned outcome need migration 0247): %v", err)
	}
	got, err := store.GuardrailNoticeVerdicts(ctx, 3, 8, "exec-gn-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Outcome != "warned" || got[0].Row != "heads-up" || got[1].Outcome != "error_closed" {
		t.Fatalf("notices = %+v", got)
	}
	// Another user of the same project reads nothing of this run.
	if other, err := store.GuardrailNoticeVerdicts(ctx, 3, 9, "exec-gn-1", 10); err != nil || len(other) != 0 {
		t.Fatalf("another user's notices = %+v, %v", other, err)
	}
	// Many requests aggregate into one group per entry, with the distinct labels,
	// and a late blocking outcome is not cut off by a small limit.
	for i := 0; i < 30; i++ {
		if _, err := pool.Exec(ctx, `INSERT INTO gateway.guardrail_verdicts
(project_id, user_id, execution_id, route, stage, row_name, detector, detector_type, action, outcome, fail_mode, labels)
VALUES (3, 8, 'exec-gn-2', '/llm', 'input', 'pii', 'pii', 'personal_data', 'mask', 'masked', 'closed', $1)`, []string{"email", "iban"}[i%2:i%2+1]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO gateway.guardrail_verdicts
(project_id, user_id, execution_id, route, stage, row_name, detector, detector_type, action, outcome, fail_mode, labels)
VALUES (3, 8, 'exec-gn-2', '/llm', 'input', 'keys', 'secrets', 'secrets', 'block', 'blocked', 'closed', '{jwt}')`); err != nil {
		t.Fatal(err)
	}
	grouped, err := store.GuardrailNoticeVerdicts(ctx, 3, 8, "exec-gn-2", 5)
	if err != nil || len(grouped) != 2 {
		t.Fatalf("grouped = %+v, %v", grouped, err)
	}
	if g := grouped[0]; g.Row != "pii" || g.Requests != 30 || len(g.Labels) != 2 || g.Labels[0] != "email" || g.Labels[1] != "iban" {
		t.Fatalf("pii group = %+v", g)
	}
	if grouped[1].Outcome != "blocked" {
		t.Fatalf("blocked group = %+v", grouped[1])
	}
}
