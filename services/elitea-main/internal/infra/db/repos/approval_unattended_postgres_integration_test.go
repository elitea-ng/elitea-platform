package repos

// Unattended approval edge cases against a real PostgreSQL (ADR-0041
// decision 5, #1491): the platform as the decider of a fan-out skip, the
// fallback to fail for a run without an owner, an expiry that fails, and
// the trigger origin of an attended run. Requires ELITEA_TEST_DATABASE_URL;
// every test skips without it.

import (
	"slices"
	"testing"
)

// setPlatformUnattendedDefault saves approvals.unattended_default. The
// platform_config table is in the bootstrap schema, not the ledgered corpus.
func (h *interruptHarness) setPlatformUnattendedDefault(action string) {
	h.exec(`
CREATE TABLE IF NOT EXISTS centry.platform_config (
    section VARCHAR(64) NOT NULL, key VARCHAR(128) NOT NULL, value JSONB NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT now(), updated_by VARCHAR(255), PRIMARY KEY (section, key))`)
	h.exec(`
INSERT INTO centry.platform_config (section, key, value, updated_at)
VALUES ('approvals', 'unattended_default', to_jsonb($1::text), now() AT TIME ZONE 'UTC')
ON CONFLICT (section, key) DO UPDATE SET value = EXCLUDED.value`, action)
}

// makeOwnerless gives the run an actor that is not a user id and no run
// policy, so Main cannot resolve the run's owner.
func (h *interruptHarness) makeOwnerless(responseID string) {
	h.exec(`UPDATE elitea_runtime.execution_jobs SET actor_id = 'service:scheduler' WHERE execution_id = $1`,
		h.executions[responseID])
}

func (h *interruptHarness) unattendedIntents(responseID string) int {
	h.t.Helper()
	var count int
	if err := h.pool.QueryRow(h.t.Context(), `
SELECT count(*) FROM elitea_runtime.unattended_approval_actions WHERE execution_id = $1`,
		h.executions[responseID]).Scan(&count); err != nil {
		h.t.Fatal(err)
	}
	return count
}

func TestUnattendedFanOutSkipNamesThePlatformAsTheDecider(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	h.makeUnattended(response, "schedule")
	h.setRunPolicy("skip")
	key, _ := h.raiseLedgered(response)
	var decidedBy *int64
	var system bool
	if err := h.pool.QueryRow(t.Context(), `SELECT decided_by, decided_by_system FROM elitea_runtime.execution_interrupts
WHERE root_response_id = $1::uuid AND interrupt_key = $2`, response, key).Scan(&decidedBy, &system); err != nil {
		t.Fatal(err)
	}
	if decidedBy != nil || !system {
		t.Fatalf("card decided_by=%v decided_by_system=%v, want the platform", decidedBy, system)
	}
	var actor *int64
	var systemActor bool
	if err := h.pool.QueryRow(t.Context(), `SELECT actor_id, system_actor FROM elitea_runtime.execution_interrupt_audit
WHERE root_response_id = $1::uuid AND interrupt_key = $2 AND transition = 'DECIDED'`, response, key).
		Scan(&actor, &systemActor); err != nil {
		t.Fatal(err)
	}
	if actor != nil || !systemActor {
		t.Fatalf("audit actor_id=%v system_actor=%v, want the platform", actor, systemActor)
	}
	rows := readApprovalLedger(t, h.pool, h.executions[response])
	if got := ledgerOutcomes(rows); !slices.Equal(got, []string{"pause:paused", "decision:unattended_skipped"}) {
		t.Fatalf("ledger = %v", got)
	}
	if rows[1].ActorKind != "system" || rows[1].ActorUserID != nil {
		t.Fatalf("skipped row = %+v, want the platform", rows[1])
	}
	// A person's decision still needs its person: the relaxed checks admit
	// the platform only with its flag.
	if _, err := h.pool.Exec(t.Context(), `UPDATE elitea_runtime.execution_interrupts SET decided_by_system = FALSE
WHERE root_response_id = $1::uuid AND interrupt_key = $2`, response, key); err == nil {
		t.Fatal("a decided card without a decider was accepted")
	}
}

func TestUnattendedRootPauseWithoutAnOwnerFails(t *testing.T) {
	h := newInterruptHarness(t)
	h.setPlatformUnattendedDefault("skip")
	response := h.newResponse()
	h.makeUnattended(response, "schedule")
	h.makeOwnerless(response)
	if outcome := h.pauseRootResponse(response); outcome != rootPauseFailed {
		t.Fatalf("outcome = %v, want failed", outcome)
	}
	rows := readApprovalLedger(t, h.pool, h.executions[response])
	if got := ledgerOutcomes(rows); !slices.Equal(got, []string{"pause:paused", "pause:paused",
		"decision:unattended_failed", "decision:unattended_failed"}) {
		t.Fatalf("ledger = %v", got)
	}
	for _, row := range rows[2:] {
		if row.Reason != ApprovalRequiredUnattendedCode || row.ActorKind != "system" {
			t.Fatalf("failed row = %+v", row)
		}
	}
	if meta := h.responseMeta(response); meta["error_code"] != ApprovalRequiredUnattendedCode {
		t.Fatalf("meta = %v", meta)
	}
	if intents := h.unattendedIntents(response); intents != 0 {
		t.Fatalf("intents = %d, want none", intents)
	}
	assertApprovalAudit(t, h.pool, 1, rows)
}

func TestUnattendedInterruptPauseWithoutAnOwnerFails(t *testing.T) {
	h := newInterruptHarness(t)
	h.setPlatformUnattendedDefault("notify_and_wait")
	response := h.newResponse()
	h.makeUnattended(response, "webhook")
	h.makeOwnerless(response)
	h.raiseLedgered(response)
	rows := readApprovalLedger(t, h.pool, h.executions[response])
	if got := ledgerOutcomes(rows); !slices.Equal(got, []string{"pause:paused", "decision:unattended_failed"}) {
		t.Fatalf("ledger = %v", got)
	}
	if rows[1].Reason != ApprovalRequiredUnattendedCode {
		t.Fatalf("failed row = %+v", rows[1])
	}
	var reason string
	if err := h.pool.QueryRow(t.Context(), `SELECT COALESCE(cancel_reason, '') FROM elitea_runtime.execution_jobs
WHERE execution_id = $1`, h.executions[response]).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != ApprovalRequiredUnattendedCode {
		t.Fatalf("cancel_reason = %q", reason)
	}
	if intents := h.unattendedIntents(response); intents != 0 {
		t.Fatalf("intents = %d, want none", intents)
	}
	assertApprovalAudit(t, h.pool, 1, rows)
}

func TestExpireUnattendedWaitsAFailingWaitDoesNotBlockTheOthers(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	h.bindLiveClaim(response)
	h.makeUnattended(response, "webhook")
	h.setRunPolicy("notify_and_wait")
	h.raiseLedgered(response)
	h.exec(`UPDATE elitea_runtime.unattended_approval_actions SET expires_at = clock_timestamp() - interval '1 second'
WHERE execution_id = $1`, h.executions[response])
	// A wait that can never be expired (its project does not exist), and
	// that expired first, so the sweep reads it first.
	h.exec(`
INSERT INTO elitea_runtime.unattended_approval_actions (
    execution_id, interrupt_id, project_id, response_message_id, conversation_uuid, path, action,
    owner_user_id, trigger_origin, state, expires_at)
VALUES ('execution-broken-wait', 'interrupt-broken', 999999, gen_random_uuid()::text, gen_random_uuid(),
        'interrupt', 'notify_and_wait', 8, 'schedule', 'WAITING', clock_timestamp() - interval '1 hour')`)
	store, err := NewUnattendedApprovalsRepository(h.pool)
	if err != nil {
		t.Fatal(err)
	}
	expired, err := store.ExpireUnattendedWaits(t.Context(), 16)
	if err == nil || expired != 1 {
		t.Fatalf("expired=%d err=%v, want 1 and the broken wait's error", expired, err)
	}
	var state, code string
	if err := h.pool.QueryRow(t.Context(), `SELECT state, COALESCE(failure_code, '')
FROM elitea_runtime.unattended_approval_actions WHERE execution_id = 'execution-broken-wait'`).
		Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "FAILED" || code != unattendedExpiryFailedCode {
		t.Fatalf("broken wait state=%s failure_code=%q", state, code)
	}
	rows := readApprovalLedger(t, h.pool, h.executions[response])
	if got := ledgerOutcomes(rows); !slices.Equal(got,
		[]string{"pause:paused", "notice:unattended_waiting", "decision:timed_out"}) {
		t.Fatalf("the later wait was not expired: %v", got)
	}
	// The broken wait is not read again.
	if again, err := store.ExpireUnattendedWaits(t.Context(), 16); err != nil || again != 0 {
		t.Fatalf("second pass expired=%d err=%v", again, err)
	}
}

func TestAttendedPauseKeepsItsTriggerOrigin(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	h.makeUnattended(response, "api") // a person's API call: attended
	if outcome := h.pauseRootResponse(response); outcome != rootPauseWaits {
		t.Fatalf("outcome = %v", outcome)
	}
	rows := readApprovalLedger(t, h.pool, h.executions[response])
	if len(rows) != 2 || rows[0].Origin != "api" || rows[1].Origin != "api" {
		t.Fatalf("ledger = %+v, want origin api", rows)
	}
}
