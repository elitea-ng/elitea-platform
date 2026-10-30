package repos

// The approval decision ledger and unattended runs against a real
// PostgreSQL (ADR-0041 decision 5, #1491). Requires ELITEA_TEST_DATABASE_URL;
// every test skips without it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/executioninterrupt"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// ledgerSecretArgument is in the tool arguments of every fixture. No ledger
// row and no audit event may carry it: the ledger keeps the digest only.
const ledgerSecretArgument = "Disk full"

type approvalLedgerRow struct {
	Path, Phase, Outcome, Action, ActorKind string
	ActorUserID                             *int64
	InterruptID, ToolName, ToolkitName      string
	Digest, Reason, AuditEventID, Origin    string
}

func readApprovalLedger(t *testing.T, pool *pgxpool.Pool, executionID string) []approvalLedgerRow {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT path, phase, outcome, COALESCE(action, ''), actor_kind, actor_user_id, interrupt_id,
       COALESCE(tool_name, ''), COALESCE(toolkit_name, ''), COALESCE(argument_digest, ''),
       COALESCE(reason, ''), audit_event_id::text, trigger_origin
FROM elitea_runtime.approval_ledger WHERE execution_id = $1 ORDER BY id`, executionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []approvalLedgerRow
	for rows.Next() {
		var row approvalLedgerRow
		if err := rows.Scan(&row.Path, &row.Phase, &row.Outcome, &row.Action, &row.ActorKind, &row.ActorUserID,
			&row.InterruptID, &row.ToolName, &row.ToolkitName, &row.Digest, &row.Reason, &row.AuditEventID,
			&row.Origin); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func ledgerOutcomes(rows []approvalLedgerRow) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = row.Phase + ":" + row.Outcome
	}
	return out
}

// assertApprovalAudit checks that every ledger row has exactly one chained
// agent.approval event with the row's identity, and that the chain holds.
func assertApprovalAudit(t *testing.T, pool *pgxpool.Pool, projectID int64, rows []approvalLedgerRow) {
	t.Helper()
	for _, row := range rows {
		var eventType string
		var raw []byte
		if err := pool.QueryRow(t.Context(), `
SELECT event_type, event FROM elitea_runtime.security_audit_outbox WHERE event_id = $1::uuid`,
			row.AuditEventID).Scan(&eventType, &raw); err != nil {
			t.Fatalf("audit event of %s/%s: %v", row.Phase, row.Outcome, err)
		}
		if eventType != string(securityaudit.TypeAgentApproval) {
			t.Fatalf("event type = %q", eventType)
		}
		if strings.Contains(string(raw), ledgerSecretArgument) {
			t.Fatalf("audit event carries the tool arguments: %s", raw)
		}
		var event struct {
			Data struct {
				Action          string `json:"action"`
				PrincipalRef    string `json:"principal_ref"`
				ToolName        string `json:"tool_name"`
				ToolkitName     string `json:"toolkit_name"`
				ArgumentsDigest string `json:"arguments_digest"`
				InvocationID    string `json:"invocation_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatal(err)
		}
		if event.Data.Action != "agent.approval."+row.Outcome || event.Data.InvocationID != row.InterruptID ||
			event.Data.ToolName != row.ToolName || event.Data.ToolkitName != row.ToolkitName ||
			event.Data.ArgumentsDigest != row.Digest {
			t.Fatalf("audit event %+v does not mirror ledger row %+v", event.Data, row)
		}
		wantPrincipal := "system:elitea-main"
		if row.ActorUserID != nil {
			wantPrincipal = fmt.Sprintf("user:%d", *row.ActorUserID)
		}
		if event.Data.PrincipalRef != wantPrincipal {
			t.Fatalf("principal = %q, want %q", event.Data.PrincipalRef, wantPrincipal)
		}
	}
	var events int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM elitea_runtime.security_audit_outbox WHERE event_type = $1`,
		string(securityaudit.TypeAgentApproval)).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events < len(rows) {
		t.Fatalf("audit events = %d, ledger rows = %d", events, len(rows))
	}
	report, err := securityaudit.Verify(t.Context(), pool, securityaudit.SourceOutbox, projectID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if report.Break != nil || report.Checked == 0 {
		t.Fatalf("audit chain: %+v", report)
	}
}

func approvalEventCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var events int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM elitea_runtime.security_audit_outbox WHERE event_type = $1`,
		string(securityaudit.TypeAgentApproval)).Scan(&events); err != nil {
		t.Fatal(err)
	}
	return events
}

// ---------------------------------------------------------------------------
// Per-interrupt path (shared 0157 cards).

func (h *interruptHarness) conversationUUID() string {
	h.t.Helper()
	var id string
	if err := h.pool.QueryRow(h.t.Context(), `SELECT uuid::text FROM p_1.chat_conversations WHERE id = $1`,
		h.conversationID).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// raiseLedgered raises a card as the frame ingest does: the card and its
// ledger projection in one transaction.
func (h *interruptHarness) raiseLedgered(responseID string) (key, interruptID string) {
	h.t.Helper()
	key, raw := h.card("fanout-interrupt-card-v1.json", nil)
	input := h.raiseInput(responseID, raw)
	conversation := h.conversationUUID()
	tx, err := h.pool.Begin(h.t.Context())
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	executor := pgxExecutor{queryer: tx}
	if _, err := raiseExecutionInterrupt(h.t.Context(), executor, input); err != nil {
		h.t.Fatalf("raise: %v", err)
	}
	if err := projectInterruptApprovalRaise(h.t.Context(), executor, input, conversation); err != nil {
		h.t.Fatalf("project raise: %v", err)
	}
	if err := tx.Commit(h.t.Context()); err != nil {
		h.t.Fatal(err)
	}
	card, _, err := domain.ParseCard(raw)
	if err != nil {
		h.t.Fatal(err)
	}
	return key, card.InterruptID
}

func (h *interruptHarness) makeUnattended(responseID, origin string) {
	h.exec(`UPDATE elitea_runtime.execution_jobs SET trigger_origin = $2 WHERE execution_id = $1`,
		h.executions[responseID], origin)
}

func (h *interruptHarness) setRunPolicy(action string) {
	h.exec(`
INSERT INTO elitea_runtime.unattended_run_policies
    (project_id, conversation_uuid, trigger_origin, version_id, owner_user_id, on_approval_required)
VALUES (1, $1::uuid, 'schedule', 41, 8, $2)`, h.conversationUUID(), action)
}

func TestApprovalLedgerInterruptApproveIsOneRowAndOneChainedEvent(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	key, interruptID := h.raiseLedgered(response)
	before := readApprovalLedger(t, h.pool, h.executions[response])
	if got := ledgerOutcomes(before); !slices.Equal(got, []string{"pause:paused"}) {
		t.Fatalf("after raise = %v", got)
	}
	if _, err := h.decide(interruptOwner, response, key, "approve", "approve", ""); err != nil {
		t.Fatal(err)
	}
	rows := readApprovalLedger(t, h.pool, h.executions[response])
	if got := ledgerOutcomes(rows); !slices.Equal(got, []string{"pause:paused", "decision:approved"}) {
		t.Fatalf("after approve = %v", got)
	}
	decision := rows[1]
	if decision.ActorKind != "user" || decision.ActorUserID == nil || *decision.ActorUserID != interruptOwner ||
		decision.Action != "approve" || decision.InterruptID != interruptID || decision.ToolName != "create_issue" ||
		decision.ToolkitName != "jira" || decision.Path != "interrupt" {
		t.Fatalf("decision row = %+v", decision)
	}
	// A 0157 card has no worker digest, and the masked arguments it shows
	// are not digested: both rows record no digest.
	if decision.Digest != "" || rows[0].Digest != "" {
		t.Fatalf("pause digest %q, decision digest %q: a card has no argument digest", rows[0].Digest, decision.Digest)
	}
	if approvalEventCount(t, h.pool) != 2 {
		t.Fatalf("audit events = %d, want 2", approvalEventCount(t, h.pool))
	}
	assertApprovalAudit(t, h.pool, 1, rows)
}

func TestApprovalLedgerInterruptRejectRecordsNoApproval(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	key, _ := h.raiseLedgered(response)
	if _, err := h.decide(interruptOwner, response, key, "reject", "reject", ""); err != nil {
		t.Fatal(err)
	}
	rows := readApprovalLedger(t, h.pool, h.executions[response])
	if got := ledgerOutcomes(rows); !slices.Equal(got, []string{"pause:paused", "decision:rejected"}) {
		t.Fatalf("after reject = %v", got)
	}
	// The worker receives the card's decision: reject, never approve.
	var decision []byte
	if err := h.pool.QueryRow(t.Context(), `SELECT decision_json FROM elitea_runtime.execution_interrupts
WHERE root_response_id = $1::uuid AND interrupt_key = $2`, response, key).Scan(&decision); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(decision), `"action":"reject"`) {
		t.Fatalf("card decision = %s", decision)
	}
	assertApprovalAudit(t, h.pool, 1, rows)
}

func TestApprovalLedgerInterruptRacingApproversHaveOneWinner(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	key, _ := h.raiseLedgered(response)
	const racers = 12
	errs := make([]error, racers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range racers {
		wg.Go(func() {
			<-start
			actor, action := interruptOwner, "approve"
			if i%2 == 1 {
				actor, action = interruptAsker, "reject"
			}
			_, errs[i] = h.decide(actor, response, key, fmt.Sprintf("racer-%d", i), action, "")
		})
	}
	close(start)
	wg.Wait()
	winners := 0
	for _, err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, domain.ErrAlreadyResolved):
		default:
			t.Fatalf("racer error = %v", err)
		}
	}
	rows := readApprovalLedger(t, h.pool, h.executions[response])
	decisions := 0
	for _, row := range rows {
		if row.Phase == approvalPhaseDecision {
			decisions++
		}
	}
	if winners != 1 || decisions != 1 {
		t.Fatalf("winners=%d decision rows=%d", winners, decisions)
	}
	assertApprovalAudit(t, h.pool, 1, rows)
}

func TestApprovalLedgerInterruptOpenPausesMatchPendingCards(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	first, _ := h.raiseLedgered(response)
	_, secondID := h.raiseLedgered(response)
	if _, err := h.decide(interruptOwner, response, first, "first", "approve", ""); err != nil {
		t.Fatal(err)
	}
	var pending, open []string
	rows, err := h.pool.Query(t.Context(), `SELECT interrupt_id FROM elitea_runtime.execution_interrupts
WHERE root_response_id = $1::uuid AND state = 'PENDING' ORDER BY interrupt_id`, response)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		pending = append(pending, id)
	}
	rows.Close()
	if err := h.pool.QueryRow(t.Context(), `
SELECT COALESCE(array_agg(pause.interrupt_id ORDER BY pause.interrupt_id), '{}')
FROM elitea_runtime.approval_ledger AS pause
WHERE pause.response_message_id = $1 AND pause.phase = 'pause'
  AND NOT EXISTS (SELECT 1 FROM elitea_runtime.approval_ledger AS decided
                  WHERE decided.execution_id = pause.execution_id AND decided.interrupt_id = pause.interrupt_id
                    AND decided.phase = 'decision')`, response).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pending, open) || !slices.Equal(open, []string{secondID}) {
		t.Fatalf("pending cards %v, open ledger pauses %v", pending, open)
	}
	// A Stop closes the rest: no pause stays open.
	tx, err := h.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := closeOpenCards(t.Context(), pgxExecutor{queryer: tx}, response, domain.StateCancelled,
		domain.StateCancelled, domain.Closer{ActorUserID: interruptOwner}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	all := readApprovalLedger(t, h.pool, h.executions[response])
	if got := ledgerOutcomes(all); !slices.Equal(got,
		[]string{"pause:paused", "pause:paused", "decision:approved", "decision:cancelled"}) {
		t.Fatalf("after stop = %v", got)
	}
	assertApprovalAudit(t, h.pool, 1, all)
}

func TestApprovalLedgerInterruptUnattendedFailEndsTheRun(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	h.makeUnattended(response, "schedule") // no run policy: the platform default, fail
	h.raiseLedgered(response)
	rows := readApprovalLedger(t, h.pool, h.executions[response])
	if got := ledgerOutcomes(rows); !slices.Equal(got, []string{"pause:paused", "decision:unattended_failed"}) {
		t.Fatalf("ledger = %v", got)
	}
	if rows[1].ActorKind != "system" || rows[1].Reason != ApprovalRequiredUnattendedCode || rows[1].Origin != "schedule" {
		t.Fatalf("failed row = %+v", rows[1])
	}
	var desired, reason string
	if err := h.pool.QueryRow(t.Context(), `SELECT desired_state, COALESCE(cancel_reason, '')
FROM elitea_runtime.execution_jobs WHERE execution_id = $1`, h.executions[response]).Scan(&desired, &reason); err != nil {
		t.Fatal(err)
	}
	if reason != ApprovalRequiredUnattendedCode {
		t.Fatalf("desired=%s cancel_reason=%q", desired, reason)
	}
	assertApprovalAudit(t, h.pool, 1, rows)
}

func TestApprovalLedgerInterruptUnattendedSkipRefusesTheStep(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	h.makeUnattended(response, "schedule")
	h.setRunPolicy("skip")
	key, _ := h.raiseLedgered(response)
	rows := readApprovalLedger(t, h.pool, h.executions[response])
	if got := ledgerOutcomes(rows); !slices.Equal(got, []string{"pause:paused", "decision:unattended_skipped"}) {
		t.Fatalf("ledger = %v", got)
	}
	if rows[1].ActorKind != "system" || rows[1].Action != "block_with_comment" {
		t.Fatalf("skipped row = %+v", rows[1])
	}
	var state string
	var decision []byte
	if err := h.pool.QueryRow(t.Context(), `SELECT state, decision_json FROM elitea_runtime.execution_interrupts
WHERE root_response_id = $1::uuid AND interrupt_key = $2`, response, key).Scan(&state, &decision); err != nil {
		t.Fatal(err)
	}
	if state != "DECIDED" || !strings.Contains(string(decision), "block_with_comment") ||
		strings.Contains(string(decision), `"approve"`) {
		t.Fatalf("card state=%s decision=%s", state, decision)
	}
	// A person who arrives late cannot approve the skipped step.
	if _, err := h.decide(interruptOwner, response, key, "late", "approve", ""); !errors.Is(err, domain.ErrAlreadyResolved) {
		t.Fatalf("late approve error = %v", err)
	}
	assertApprovalAudit(t, h.pool, 1, rows)
}

func TestApprovalLedgerInterruptUnattendedNotifyAndWaitThenTimeout(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	h.bindLiveClaim(response)
	h.makeUnattended(response, "webhook")
	h.setRunPolicy("notify_and_wait")
	key, _ := h.raiseLedgered(response)
	rows := readApprovalLedger(t, h.pool, h.executions[response])
	if got := ledgerOutcomes(rows); !slices.Equal(got, []string{"pause:paused", "notice:unattended_waiting"}) {
		t.Fatalf("ledger = %v", got)
	}
	var notified int
	if err := h.pool.QueryRow(t.Context(), `SELECT count(*) FROM centry.notifications
WHERE user_id = 8 AND event_type = $1`, ApprovalNotificationEventType).Scan(&notified); err != nil {
		t.Fatal(err)
	}
	if notified != 1 {
		t.Fatalf("owner notifications = %d", notified)
	}
	h.exec(`UPDATE elitea_runtime.unattended_approval_actions SET expires_at = clock_timestamp() - interval '1 second'
WHERE execution_id = $1`, h.executions[response])
	store, err := NewUnattendedApprovalsRepository(h.pool)
	if err != nil {
		t.Fatal(err)
	}
	expired, err := store.ExpireUnattendedWaits(t.Context(), 16)
	if err != nil || expired != 1 {
		t.Fatalf("expired=%d err=%v", expired, err)
	}
	rows = readApprovalLedger(t, h.pool, h.executions[response])
	if got := ledgerOutcomes(rows); !slices.Equal(got,
		[]string{"pause:paused", "notice:unattended_waiting", "decision:timed_out"}) {
		t.Fatalf("after expiry = %v", got)
	}
	var state, reason string
	if err := h.pool.QueryRow(t.Context(), `SELECT state FROM elitea_runtime.execution_interrupts
WHERE root_response_id = $1::uuid AND interrupt_key = $2`, response, key).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := h.pool.QueryRow(t.Context(), `SELECT COALESCE(cancel_reason, '') FROM elitea_runtime.execution_jobs
WHERE execution_id = $1`, h.executions[response]).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if state != "CANCELLED" || reason != ApprovalTimedOutCode {
		t.Fatalf("card state=%s cancel_reason=%q", state, reason)
	}
	// A second pass finds nothing.
	if again, err := store.ExpireUnattendedWaits(t.Context(), 16); err != nil || again != 0 {
		t.Fatalf("second pass expired=%d err=%v", again, err)
	}
	assertApprovalAudit(t, h.pool, 1, rows)
}

// ---------------------------------------------------------------------------
// Root (coordinator) path: chat_message_group.meta.hitl_interrupts.

const rootLedgerPauses = `[
	{"interrupt_id":"interrupt-root-1","available_actions":["approve","reject","block_with_comment"],
	 "guardrail_type":"sensitive_tool","tool_name":"create_issue","toolkit_name":"jira","toolkit_type":"jira",
	 "argument_digest":"` + rootLedgerWorkerDigest + `",
	 "tool_args":{"project":"OPS","summary":"Disk full"}},
	{"interrupt_id":"interrupt-root-2","available_actions":["approve","reject","block_with_comment"],
	 "guardrail_type":"sensitive_tool","tool_name":"delete_issue","toolkit_name":"jira","toolkit_type":"jira",
	 "tool_args":{"key":"OPS-1","reason":"Disk full"}}
]`

// rootLedgerWorkerDigest is the worker's digest of the unmasked arguments
// of interrupt-root-1. interrupt-root-2 has none, as from an earlier worker.
const rootLedgerWorkerDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// pauseRootResponse writes a paused root response in the interrupt harness:
// the meta projection and its ledger projection in one transaction, as the
// terminal projection does. It returns the projection outcome.
func (h *interruptHarness) pauseRootResponse(responseID string) rootPauseOutcome {
	h.t.Helper()
	executionID := h.executions[responseID]
	tx, err := h.pool.Begin(h.t.Context())
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(h.t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		h.t.Fatal(err)
	}
	executor := pgxExecutor{queryer: tx}
	expected := outputapp.ExpectedAgentExecution{
		ProjectionProjectID: "1", ExecutionID: executionID, ClientMessageID: responseID,
		ClientStreamID: h.conversationUUID(),
	}
	outcome, err := projectRootApprovalPause(h.t.Context(), executor, expected, json.RawMessage(rootLedgerPauses), false)
	if err != nil {
		h.t.Fatalf("project root pause: %v", err)
	}
	var groupID int64
	if err := tx.QueryRow(h.t.Context(), `SELECT id FROM chat_message_group WHERE uuid = $1::uuid`, responseID).
		Scan(&groupID); err != nil {
		h.t.Fatal(err)
	}
	if outcome == rootPauseFailed {
		if err := finalizeUnattendedApprovalFailure(h.t.Context(), executor, groupID, "thread-1", []byte("[]"), nil); err != nil {
			h.t.Fatal(err)
		}
	} else if _, err := tx.Exec(h.t.Context(), `
UPDATE chat_message_group SET is_streaming = FALSE, task_id = $2,
    meta = meta || jsonb_build_object('thread_id', 'thread-1', 'hitl_interrupts', $3::jsonb)
WHERE id = $1`, groupID, executionID, rootLedgerPauses); err != nil {
		h.t.Fatal(err)
	}
	if err := tx.Commit(h.t.Context()); err != nil {
		h.t.Fatal(err)
	}
	return outcome
}

func (h *interruptHarness) responseMeta(responseID string) map[string]any {
	h.t.Helper()
	var raw []byte
	if err := h.pool.QueryRow(h.t.Context(), `SELECT meta FROM p_1.chat_message_group WHERE uuid = $1::uuid`,
		responseID).Scan(&raw); err != nil {
		h.t.Fatal(err)
	}
	meta := map[string]any{}
	if err := json.Unmarshal(raw, &meta); err != nil {
		h.t.Fatal(err)
	}
	return meta
}

func TestApprovalLedgerRootPauseDigestsAndMatchesTheProjection(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	if outcome := h.pauseRootResponse(response); outcome != rootPauseWaits {
		t.Fatalf("outcome = %v", outcome)
	}
	rows := readApprovalLedger(t, h.pool, h.executions[response])
	if got := ledgerOutcomes(rows); !slices.Equal(got, []string{"pause:paused", "pause:paused"}) {
		t.Fatalf("ledger = %v", got)
	}
	meta := h.responseMeta(response)
	pending, _ := meta["hitl_interrupts"].([]any)
	var projected []string
	for _, item := range pending {
		projected = append(projected, item.(map[string]any)["interrupt_id"].(string))
	}
	ledgered := []string{rows[0].InterruptID, rows[1].InterruptID}
	if !slices.Equal(projected, ledgered) {
		t.Fatalf("meta pauses %v, ledger pauses %v", projected, ledgered)
	}
	// The worker's digest is kept; without it the masked arguments are
	// not digested, and the row records no digest.
	if rows[0].Digest != rootLedgerWorkerDigest || rows[0].ToolName != "create_issue" || rows[0].Origin != "manual" {
		t.Fatalf("pause row = %+v, want digest %s", rows[0], rootLedgerWorkerDigest)
	}
	if rows[1].Digest != "" || rows[1].ToolName != "delete_issue" {
		t.Fatalf("pause row without a worker digest = %+v", rows[1])
	}
	assertApprovalAudit(t, h.pool, 1, rows)
}

func TestApprovalLedgerRootUnattendedFailEndsWithTypedError(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	h.makeUnattended(response, "schedule")
	if outcome := h.pauseRootResponse(response); outcome != rootPauseFailed {
		t.Fatalf("outcome = %v", outcome)
	}
	rows := readApprovalLedger(t, h.pool, h.executions[response])
	if got := ledgerOutcomes(rows); !slices.Equal(got, []string{"pause:paused", "pause:paused",
		"decision:unattended_failed", "decision:unattended_failed"}) {
		t.Fatalf("ledger = %v", got)
	}
	meta := h.responseMeta(response)
	if meta["error_code"] != ApprovalRequiredUnattendedCode || meta["hitl_interrupts"] != nil {
		t.Fatalf("meta = %v", meta)
	}
	assertApprovalAudit(t, h.pool, 1, rows)
}

func TestApprovalLedgerRootUnattendedSkipQueuesARefusal(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	h.makeUnattended(response, "schedule")
	h.setRunPolicy("skip")
	if outcome := h.pauseRootResponse(response); outcome != rootPauseWaits {
		t.Fatalf("outcome = %v", outcome)
	}
	store, err := NewUnattendedApprovalsRepository(h.pool)
	if err != nil {
		t.Fatal(err)
	}
	targets, err := store.ClaimUnattendedSkips(t.Context(), 16, 0)
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets=%+v err=%v", targets, err)
	}
	target := targets[0]
	if target.OwnerUserID != 8 || target.ResponseMessageID != response || len(target.Decisions) != 2 {
		t.Fatalf("target = %+v", target)
	}
	for _, decision := range target.Decisions {
		if decision.Action != "block_with_comment" || !strings.Contains(decision.Value, "no approver") {
			t.Fatalf("skip decision = %+v", decision)
		}
	}
	// The worker failing the skip ends the run with the typed error.
	if err := store.FailUnattendedSkip(t.Context(), target, "continuation_refused"); err != nil {
		t.Fatal(err)
	}
	rows := readApprovalLedger(t, h.pool, h.executions[response])
	if got := ledgerOutcomes(rows); !slices.Equal(got, []string{"pause:paused", "pause:paused",
		"decision:unattended_failed", "decision:unattended_failed"}) {
		t.Fatalf("ledger = %v", got)
	}
	if meta := h.responseMeta(response); meta["error_code"] != ApprovalRequiredUnattendedCode {
		t.Fatalf("meta = %v", meta)
	}
	assertApprovalAudit(t, h.pool, 1, rows)
}

func TestApprovalLedgerRootUnattendedWaitTimesOut(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	h.makeUnattended(response, "schedule")
	h.setRunPolicy("notify_and_wait")
	if outcome := h.pauseRootResponse(response); outcome != rootPauseWaits {
		t.Fatalf("outcome = %v", outcome)
	}
	h.exec(`UPDATE elitea_runtime.unattended_approval_actions SET expires_at = clock_timestamp() - interval '1 second'
WHERE execution_id = $1`, h.executions[response])
	store, err := NewUnattendedApprovalsRepository(h.pool)
	if err != nil {
		t.Fatal(err)
	}
	if expired, err := store.ExpireUnattendedWaits(t.Context(), 16); err != nil || expired != 1 {
		t.Fatalf("expired=%d err=%v", expired, err)
	}
	rows := readApprovalLedger(t, h.pool, h.executions[response])
	if got := ledgerOutcomes(rows); !slices.Equal(got, []string{"pause:paused", "pause:paused",
		"notice:unattended_waiting", "notice:unattended_waiting", "decision:timed_out", "decision:timed_out"}) {
		t.Fatalf("ledger = %v", got)
	}
	if meta := h.responseMeta(response); meta["error_code"] != ApprovalTimedOutCode || meta["hitl_interrupts"] != nil {
		t.Fatalf("meta = %v", meta)
	}
	assertApprovalAudit(t, h.pool, 1, rows)
}

// TestApprovalLedgerRootResumeRecordsDecisionsAndRacesHaveOneWinner drives
// the chat continuation (the REST decision path) with several approvers at
// once. One resume wins; every other gets the typed conflict; the ledger
// has one decision row per pause.
func TestApprovalLedgerRootResumeRecordsDecisionsAndRacesHaveOneWinner(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	conversationRaw := "10000000-0000-4000-8000-000000000031"
	questionID := "20000000-0000-4000-8000-000000000091"
	pausedExecution := "execution-ledger-root-paused"

	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	queries := sqlcgen.New(tx)
	responsePG := insertPostgresCurrentApplicationTurn(t, queries, mustCurrentPGUUID(t, conversationRaw), questionID,
		"30000000-0000-4000-8000-000000000091", "40000000-0000-4000-8000-000000000091",
		"file two issues", pausedExecution)
	responseID := responsePG.String()
	if _, err := projectRootApprovalPause(t.Context(), pgxExecutor{queryer: tx}, outputapp.ExpectedAgentExecution{
		ProjectionProjectID: "1", ExecutionID: pausedExecution, ClientMessageID: responseID,
		ClientStreamID: conversationRaw,
	}, json.RawMessage(rootLedgerPauses), false); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
UPDATE chat_message_group SET is_streaming = FALSE,
    meta = meta || jsonb_build_object('thread_id', 'thread-ledger-1', 'execution_generation', $2::text,
        'hitl_interrupt', ($3::jsonb -> 0), 'hitl_interrupts', $3::jsonb)
WHERE uuid = $1`, responseID, questionID, rootLedgerPauses); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	turn := agentexecutionapp.CurrentContinueTurn{
		ProjectID: 1, ActorUserID: 11, ConversationUUID: conversationRaw, TargetParticipantID: 21,
		Kind: agentexecutionapp.CurrentRegenerationApplication, ApplicationID: 31, ApplicationVersionID: 41,
		QuestionID: questionID, ResponseMessageID: responseID, ExecutionGeneration: questionID,
		ThreadID: "thread-ledger-1",
		HITLDecisions: json.RawMessage(`[{"interrupt_id":"interrupt-root-1","action":"approve"},
			{"interrupt_id":"interrupt-root-2","action":"reject"}]`),
	}
	const racers = 6
	errs := make([]error, racers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range racers {
		wg.Go(func() {
			<-start
			errs[i] = func() error {
				tx, err := pool.BeginTx(context.Background(), pgx.TxOptions{})
				if err != nil {
					return err
				}
				defer func() { _ = tx.Rollback(context.Background()) }()
				if err := tenant.BindProject(context.Background(), tx, tenant.Project{ID: 1}); err != nil {
					return err
				}
				if err := resumeCurrentAgentHITLWithLedger(context.Background(), tx, sqlcgen.New(tx),
					fmt.Sprintf("execution-ledger-root-resumed-%d", i), turn, "manual"); err != nil {
					return err
				}
				return tx.Commit(context.Background())
			}()
		})
	}
	close(start)
	wg.Wait()
	winners := 0
	for _, err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, agentexecutionapp.ErrCurrentAgentHITLAlreadyResolved):
		default:
			t.Fatalf("racer error = %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d", winners)
	}
	rows := readApprovalLedger(t, pool, pausedExecution)
	if got := ledgerOutcomes(rows); !slices.Equal(got,
		[]string{"pause:paused", "pause:paused", "decision:approved", "decision:rejected"}) {
		t.Fatalf("ledger = %v", got)
	}
	for _, row := range rows[2:] {
		if row.ActorUserID == nil || *row.ActorUserID != 11 || row.Path != "root" {
			t.Fatalf("decision row = %+v", row)
		}
	}
	if rows[2].Digest != rows[0].Digest || rows[3].Digest != rows[1].Digest {
		t.Fatalf("decision digests do not match their pauses: %+v", rows)
	}
	assertApprovalAudit(t, pool, 1, rows)
}
