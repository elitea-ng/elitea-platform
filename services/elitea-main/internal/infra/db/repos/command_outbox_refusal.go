package repos

// A command Main can never prepare (shared 0197, ADR-0044 decision 2): the
// refusal is recorded on the run's outbox row, whose deadline moves to now,
// so the no-authority deadline retirement every command type already runs
// settles the run at its next pass, and writes the refusal (not the deadline
// text) as the run's `execution.failed` event. No retirement or terminal
// error code is added: every terminal projection keyed on the deadline
// retirement (index metadata, notifications, task status) applies as it is.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
)

// The refusals the outbox admits: 0197's egress policy and 0207's
// unapproved connector (ADR-0044 decision 1).
const (
	refusalCodeEgressPolicyInvalid  = "EGRESS_POLICY_INVALID"
	refusalCodeConnectorNotApproved = "CONNECTOR_NOT_APPROVED"
)

// refusalChatErrorCodes are the refusals as a chat answer's `error_code`.
var refusalChatErrorCodes = map[string]string{
	refusalCodeEgressPolicyInvalid:  "egress_policy_invalid",
	refusalCodeConnectorNotApproved: "connector_not_approved",
}

// DispatchRefusals records refused commands (commandbus.DispatchRefusalRecorder).
type DispatchRefusals struct{ pool *pgxpool.Pool }

// NewDispatchRefusals records through pool.
func NewDispatchRefusals(pool *pgxpool.Pool) (*DispatchRefusals, error) {
	if pool == nil {
		return nil, errors.New("dispatch refusals database is required")
	}
	return &DispatchRefusals{pool: pool}, nil
}

// RefuseDispatch records the refusal on the run's never-prepared outbox row
// and moves its deadline to now. An agent run's answer is settled as the
// error at once (the chat meta a stream reload reads), the way a refused
// governed continuation's is. A row already prepared, retired or claimed, or
// a run no longer PENDING/DISPATCHED, is left alone (it is no longer this
// dispatch's to refuse) and answers executionapp.ErrDispatchNotRefused, so
// the producer reports an ordinary error, not a refusal that was never made
// durable.
func (r *DispatchRefusals) RefuseDispatch(ctx context.Context, executionID string, generation uint64, code, message string) error {
	if r == nil || r.pool == nil {
		return errors.New("dispatch refusals database is required")
	}
	if _, known := refusalChatErrorCodes[code]; executionID == "" || generation == 0 || !known ||
		message == "" || len(message) > 1024 || !utf8.ValidString(message) {
		return errors.New("invalid dispatch refusal")
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		return fmt.Errorf("begin dispatch refusal transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	var capability string
	var projectionProjectID int64
	err = tx.QueryRow(ctx, `
UPDATE elitea_runtime.command_outbox AS o
SET refusal_code = $3,
    refusal_message = $4,
    deadline = LEAST(o.deadline, clock_timestamp())
FROM elitea_runtime.execution_jobs AS j
WHERE o.execution_id = $1
  AND o.generation = $2
  AND o.retired_at IS NULL
  AND o.authority_granted_at IS NULL
  AND o.prepared_at IS NULL
  AND j.execution_id = o.execution_id
  AND j.generation = o.generation
  AND j.state IN ('PENDING', 'DISPATCHED')
  AND NOT EXISTS (
      SELECT 1 FROM elitea_runtime.execution_claims AS c
      WHERE c.execution_id = o.execution_id AND c.generation = o.generation
  )
RETURNING j.capability_id, j.projection_project_id`,
		executionID, int64(generation), code, message).Scan(&capability, &projectionProjectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return executionapp.ErrDispatchNotRefused
	}
	if err != nil {
		return fmt.Errorf("record the dispatch refusal: %w", err)
	}
	if capability == executiondomain.AgentApplicationCapability || capability == executiondomain.AgentAdhocCapability {
		if err := projectAgentRefusal(ctx, tx, projectionProjectID, executionID, generation, code, message); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit dispatch refusal transaction: %w", err)
	}
	committed = true
	return nil
}

// projectAgentRefusal settles the run's chat answer as the refusal: not
// streaming, `is_error`, the message as its error and egress_policy_invalid
// (or connector_not_approved) as its error code. The question is kept: the refusal is the answer.
func projectAgentRefusal(ctx context.Context, tx pgx.Tx, projectionProjectID int64, executionID string, generation uint64, code, message string) error {
	schema, err := tenantSchema(strconv.FormatInt(projectionProjectID, 10))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
UPDATE %s.chat_message_group AS response
SET is_streaming = FALSE,
    meta = response.meta || jsonb_build_object(
        'is_error', TRUE,
        'error', $3::text,
        'error_code', $4::text
    ),
    updated_at = (clock_timestamp() AT TIME ZONE 'UTC')
FROM elitea_runtime.agent_execution_jobs AS binding
WHERE binding.execution_id = $1
  AND binding.generation = $2
  AND response.uuid::text = binding.client_message_id
  AND response.task_id = binding.execution_id
  AND response.meta ->> 'execution_generation' = binding.client_execution_generation`, schema),
		executionID, int64(generation), message, refusalChatErrorCodes[code]); err != nil {
		return fmt.Errorf("project the refused agent answer: %w", err)
	}
	return nil
}

// refusalEventBytes is the `execution.failed` payload of a refused run, in
// the {code, safe_message, retryable} shape every producer of that event
// writes.
func refusalEventBytes(code, message string) ([]byte, error) {
	return json.Marshal(struct {
		Code        string `json:"code"`
		SafeMessage string `json:"safe_message"`
		Retryable   bool   `json:"retryable"`
	}{Code: code, SafeMessage: message})
}
