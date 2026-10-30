package repos

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenant"
)

const refusalTestMessage = `The worker egress allowlist entry "http://bad" is not a host or a host:port. ` +
	`A project administrator must correct it in the project's Worker Egress setting.`

// A refused, never-prepared command is settled by the deadline retirement
// at its next pass, with the refusal (not the deadline text) as its
// `execution.failed` event; a prepared row is never refused.
func TestPostgresDispatchRefusalSettlesTheRunWithItsMessage(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repository, err := NewCommandOutboxRepository(pool, "elitea:runtime:commands")
	if err != nil {
		t.Fatal(err)
	}
	refusals, err := NewDispatchRefusals(pool)
	if err != nil {
		t.Fatal(err)
	}

	frame := postgresValidationFrame(t, "egress-refused")
	seed := seedPostgresValidationExecution(t, pool, frame, runtimedomain.DesiredRunning)
	resetPostgresExecutionToNoAuthority(t, pool, frame, seed, false, runtimedomain.DesiredRunning, time.Now().UTC().Add(time.Hour))
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.command_outbox
SET prepared_signed_envelope_bytes = NULL, prepared_signed_envelope_digest = NULL,
    prepared_signature_profile = NULL, prepared_key_id = NULL, prepared_at = NULL
WHERE outbox_id = $1`, seed.outboxID); err != nil {
		t.Fatal(err)
	}
	prepared := postgresValidationFrame(t, "egress-prepared")
	preparedSeed := seedPostgresValidationExecution(t, pool, prepared, runtimedomain.DesiredRunning)
	resetPostgresExecutionToNoAuthority(t, pool, prepared, preparedSeed, false, runtimedomain.DesiredRunning, time.Now().UTC().Add(time.Hour))

	if err := refusals.RefuseDispatch(ctx, frame.Fence.ExecutionID, 1, "SOMETHING_ELSE", refusalTestMessage); err == nil {
		t.Fatal("a refusal code 0197 does not admit was recorded")
	}
	if err := refusals.RefuseDispatch(ctx, frame.Fence.ExecutionID, 1, refusalCodeEgressPolicyInvalid, refusalTestMessage); err != nil {
		t.Fatal(err)
	}
	var code, message string
	var due bool
	if err := pool.QueryRow(ctx, `
SELECT refusal_code, refusal_message, deadline <= clock_timestamp()
FROM elitea_runtime.command_outbox WHERE outbox_id = $1`, seed.outboxID).Scan(&code, &message, &due); err != nil {
		t.Fatal(err)
	}
	if code != refusalCodeEgressPolicyInvalid || message != refusalTestMessage || !due {
		t.Fatalf("refusal code=%q message=%q due=%v", code, message, due)
	}

	// A prepared command is published as prepared: nothing is recorded, and
	// the refusal says so (an unrecorded refusal is never reported as one).
	if err := refusals.RefuseDispatch(ctx, prepared.Fence.ExecutionID, 1, refusalCodeEgressPolicyInvalid, refusalTestMessage); !errors.Is(err, executionapp.ErrDispatchNotRefused) {
		t.Fatalf("a refusal of a prepared row: %v, want ErrDispatchNotRefused", err)
	}
	assertPostgresCount(t, ctx, pool, 0, `SELECT count(*) FROM elitea_runtime.command_outbox WHERE outbox_id = $1 AND refusal_code IS NOT NULL`, preparedSeed.outboxID)
	// The CHECK refuses the same on a prepared row written directly.
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.command_outbox SET refusal_code = 'EGRESS_POLICY_INVALID', refusal_message = 'x'
WHERE outbox_id = $1`, preparedSeed.outboxID); err == nil {
		t.Fatal("the CHECK admitted a refusal on a prepared row")
	}

	retired, err := repository.RetireNoAuthorityValidation(ctx, 8)
	if err != nil || retired != 1 {
		t.Fatalf("retired=%d err=%v, want the refused run only", retired, err)
	}
	var state, terminal string
	if err := pool.QueryRow(ctx, `
SELECT state, COALESCE(terminal_error_code, '') FROM elitea_runtime.execution_jobs
WHERE execution_id = $1 AND generation = 1`, frame.Fence.ExecutionID).Scan(&state, &terminal); err != nil {
		t.Fatal(err)
	}
	if state != "FAILED" || terminal != retirementCodeDeadlineExceeded {
		t.Fatalf("state=%s terminal=%s", state, terminal)
	}
	want, err := refusalEventBytes(refusalCodeEgressPolicyInvalid, refusalTestMessage)
	if err != nil {
		t.Fatal(err)
	}
	assertPostgresCount(t, ctx, pool, 1, `SELECT count(*) FROM elitea_runtime.execution_replay_events WHERE execution_id = $1 AND event_type = 'execution.failed' AND event_bytes = $2`, frame.Fence.ExecutionID, want)
	assertPostgresCount(t, ctx, pool, 0, `SELECT count(*) FROM elitea_runtime.execution_replay_events WHERE execution_id = $1 AND event_bytes = $2`, frame.Fence.ExecutionID, deadlineRetirementEventBytes)

	// A refusal after the run settled records nothing and says so.
	if err := refusals.RefuseDispatch(ctx, frame.Fence.ExecutionID, 1, refusalCodeEgressPolicyInvalid, "again"); !errors.Is(err, executionapp.ErrDispatchNotRefused) {
		t.Fatalf("a refusal of a settled run: %v, want ErrDispatchNotRefused", err)
	}
}

// An agent run's chat answer is settled as the refusal at once: not
// streaming, is_error, the message and egress_policy_invalid.
func TestPostgresDispatchRefusalSettlesTheAgentAnswer(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	const (
		questionID     = "20000000-0000-4000-8000-000000001490"
		questionItem   = "40000000-0000-4000-8000-000000001490"
		responseID     = "30000000-0000-4000-8000-000000001490"
		conversationID = "10000000-0000-4000-8000-000000000031"
		executionID    = "execution-egress-refused"
	)
	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	responseMessageID := insertPostgresCurrentApplicationTurn(t, sqlcgen.New(tx),
		mustCurrentPGUUID(t, conversationID), questionID, questionItem, responseID, "list my repositories", executionID)
	insertPostgresCurrentAgentCancelBinding(t, tx, responseMessageID, questionID, executionID,
		"agent.execute.application.v1", "PENDING", "RUNNING")
	if _, err := tx.Exec(t.Context(), `
INSERT INTO elitea_runtime.command_outbox (
    outbox_id, execution_id, generation, stream_name, resource_class,
    isolation_class, priority, deadline, limits_revision
) VALUES ('outbox-'||$1, $1, 1, 'elitea:runtime:commands', 'agent', 'project', 1,
          clock_timestamp() + interval '1 hour', 'limits-v1')`, executionID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	refusals, err := NewDispatchRefusals(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := refusals.RefuseDispatch(t.Context(), executionID, 1, refusalCodeEgressPolicyInvalid, refusalTestMessage); err != nil {
		t.Fatal(err)
	}
	var isStreaming bool
	var isError, errorText, errorCode *string
	if err := pool.QueryRow(t.Context(), `
SELECT is_streaming, meta ->> 'is_error', meta ->> 'error', meta ->> 'error_code'
FROM p_1.chat_message_group WHERE uuid = $1`, responseMessageID).Scan(&isStreaming, &isError, &errorText, &errorCode); err != nil {
		t.Fatal(err)
	}
	if isStreaming || isError == nil || *isError != "true" || errorText == nil || *errorText != refusalTestMessage ||
		errorCode == nil || *errorCode != refusalChatErrorCodes[refusalCodeEgressPolicyInvalid] {
		t.Fatalf("answer streaming=%v is_error=%v error=%v code=%v", isStreaming, isError, errorText, errorCode)
	}
}
