package repos

// Unattended run chains (ADR-0045 decision 4; shared 0221).
//
// The chain's first run writes the chain row and link 0 in the SAME
// transaction as its admission (a cloud agent admission or a desktop local
// turn start). Each continuation runs ONE statement in its admission
// transaction: it sums the chain's spend from the gateway request log, checks
// the three limits, adds one to the count and writes the link. Under READ
// COMMITTED a second continuation that waits on the row lock re-checks the
// limits against the updated row, so two continuations that race cannot both
// take the last slot. A refused continuation ends the chain as
// limit_reached, in its own short transaction.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

// rowQueryer is what the chain helpers need: a pool or a transaction.
type rowQueryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// runChainsGrantIDKey is the unique constraint on run_chains.grant_id.
const runChainsGrantIDKey = "run_chains_grant_id_key"

// startRunChain writes a chain and its root link. A replay of the same root
// is a no-op.
func startRunChain(ctx context.Context, tx pgx.Tx, start unattended.ChainStart) error {
	grant, err := json.Marshal(start.Grant)
	if err != nil {
		return fmt.Errorf("unattended chain: encode the grant: %w", err)
	}
	origin := start.TriggerOrigin
	if origin == "" {
		origin = "manual"
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.run_chains (
    root_execution_id, project_id, owner_user_id, conversation_uuid, origin, trigger_origin,
    grant_id, grant_json, max_continuations, max_spend_usd, deadline, last_execution_id)
VALUES ($1, $2, $3, $4::uuid, $5, $6, $7::uuid, $8::jsonb, $9, $10,
        clock_timestamp() + make_interval(secs => $11), $1)
ON CONFLICT (root_execution_id) DO NOTHING`,
		start.RootExecutionID, start.ProjectID, start.OwnerUserID, start.ConversationUUID, start.Origin, origin,
		start.Grant.GrantID, string(grant), start.Limits.MaxContinuations, start.Limits.MaxSpendUSD,
		start.Limits.Deadline.Seconds()); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == runChainsGrantIDKey {
			// The client sent a grant id that another chain already has (a
			// replayed request with a new question): a refusal, not a 500.
			return unattended.Refused("grant_id", "", unattended.RefusedInvalid)
		}
		return fmt.Errorf("unattended chain: start: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.run_chain_links (execution_id, root_execution_id, continuation_index, kind, response_message_id)
VALUES ($1, $1, 0, 'root', NULLIF($2, ''))
ON CONFLICT (execution_id) DO NOTHING`, start.RootExecutionID, start.ResponseMessageID); err != nil {
		return fmt.Errorf("unattended chain: root link: %w", err)
	}
	return nil
}

// continueRunChainSQL is the chain check: the spend, the three limits, the
// increment and the link, in one statement.
const continueRunChainSQL = `
WITH chain AS MATERIALIZED (
    SELECT root_execution_id, project_id
    FROM elitea_runtime.run_chains
    WHERE root_execution_id = $1 AND owner_user_id = $2 AND grant_id = $3::uuid AND project_id = $4
      AND conversation_uuid = $8::uuid
), spent AS MATERIALIZED (
    SELECT coalesce(sum(` + requestLogCostSQL + `), 0) AS usd
    FROM chain AS c
    JOIN elitea_runtime.run_chain_links AS k ON k.root_execution_id = c.root_execution_id
    JOIN gateway.llm_request_logs AS l
      ON l.project_id = c.project_id
     AND l.execution_id = k.execution_id
     AND l.execution_id IS NOT NULL
    LEFT JOIN gateway.gateway_models AS m
      ON m.provider = l.provider AND m.model_name = l.model
), bumped AS (
    UPDATE elitea_runtime.run_chains AS r
    SET continuations = r.continuations + 1,
        spend_usd = spent.usd,
        last_execution_id = $5,
        updated_at = clock_timestamp()
    FROM chain, spent
    WHERE r.root_execution_id = chain.root_execution_id
      AND r.end_state IS NULL
      AND r.continuations < r.max_continuations
      AND (r.max_spend_usd = 0 OR spent.usd < r.max_spend_usd)
      AND clock_timestamp() < r.deadline
    RETURNING r.root_execution_id, r.continuations
), linked AS (
    INSERT INTO elitea_runtime.run_chain_links (
        execution_id, root_execution_id, continuation_index, kind, response_message_id)
    SELECT $5, bumped.root_execution_id, bumped.continuations, $6, NULLIF($7, '')
    FROM bumped
    RETURNING continuation_index
)
SELECT continuation_index FROM linked`

// continueRunChain admits one continuation, or answers why it cannot:
// *unattended.ChainLimitError, unattended.ErrChainEnded, or a refusal for a
// chain this caller does not own.
func continueRunChain(ctx context.Context, tx pgx.Tx, request unattended.ChainContinue) (unattended.Chain, error) {
	var index int
	err := tx.QueryRow(ctx, continueRunChainSQL, request.RootExecutionID, request.OwnerUserID,
		request.GrantID, request.ProjectID, request.ExecutionID, request.Kind, request.ResponseMessageID,
		request.ConversationUUID).Scan(&index)
	if err == nil {
		chain, found, err := readRunChainLink(ctx, tx, request.ExecutionID)
		if err != nil {
			return unattended.Chain{}, err
		}
		if !found {
			return unattended.Chain{}, errors.New("unattended chain: the new link is missing")
		}
		return chain, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return unattended.Chain{}, fmt.Errorf("unattended chain: continue: %w", err)
	}
	return unattended.Chain{}, classifyRefusedContinuation(ctx, tx, request)
}

// classifyRefusedContinuation names why the chain statement wrote nothing.
func classifyRefusedContinuation(ctx context.Context, tx pgx.Tx, request unattended.ChainContinue) error {
	var (
		owner, project                 int64
		grantID, conversation          string
		endState                       *string
		continuations, maxContinuation int
		pastDeadline                   bool
		spend, maxSpend                float64
	)
	err := tx.QueryRow(ctx, `
SELECT owner_user_id, project_id, grant_id::text, conversation_uuid::text, end_state, continuations,
       max_continuations, clock_timestamp() >= deadline, spend_usd::float8, max_spend_usd::float8
FROM elitea_runtime.run_chains WHERE root_execution_id = $1`, request.RootExecutionID).Scan(
		&owner, &project, &grantID, &conversation, &endState, &continuations, &maxContinuation, &pastDeadline,
		&spend, &maxSpend)
	if errors.Is(err, pgx.ErrNoRows) {
		return unattended.Refused("chain_root_execution_id", "", unattended.RefusedChainNotFound)
	}
	if err != nil {
		return fmt.Errorf("unattended chain: read refused chain: %w", err)
	}
	switch {
	case owner != request.OwnerUserID || project != request.ProjectID || conversation != request.ConversationUUID:
		// The same answer as a chain that does not exist: a caller learns
		// nothing about another person's chain.
		return unattended.Refused("chain_root_execution_id", "", unattended.RefusedChainNotFound)
	case grantID != request.GrantID:
		return unattended.Refused("grant_id", "", unattended.RefusedInvalid)
	case endState != nil && *endState == unattended.EndLimitReached:
		return &unattended.ChainLimitError{Limit: storedEndLimit(ctx, tx, request.RootExecutionID)}
	case endState != nil:
		return unattended.ErrChainEnded
	case continuations >= maxContinuation:
		return &unattended.ChainLimitError{Limit: unattended.LimitContinuations}
	case pastDeadline:
		return &unattended.ChainLimitError{Limit: unattended.LimitDeadline}
	default:
		return &unattended.ChainLimitError{Limit: unattended.LimitSpend}
	}
}

func storedEndLimit(ctx context.Context, tx pgx.Tx, root string) string {
	var limit *string
	if err := tx.QueryRow(ctx, `SELECT end_limit FROM elitea_runtime.run_chains WHERE root_execution_id = $1`,
		root).Scan(&limit); err != nil || limit == nil {
		return unattended.LimitContinuations
	}
	return *limit
}

// readRunChainLink answers the chain of one execution.
func readRunChainLink(ctx context.Context, queryer rowQueryer, executionID string) (unattended.Chain, bool, error) {
	var (
		chain    unattended.Chain
		grant    []byte
		endState *string
		response *string
		deadline time.Time
		created  time.Time
		limits   struct {
			continuations int
			spend         float64
		}
	)
	err := queryer.QueryRow(ctx, `
SELECT k.root_execution_id, k.execution_id, k.continuation_index, k.kind, k.response_message_id,
       c.project_id, c.owner_user_id, c.conversation_uuid::text, c.origin, c.trigger_origin,
       c.grant_id::text, c.grant_json, c.max_continuations, c.max_spend_usd::float8, c.deadline,
       c.created_at, c.continuations, c.end_state
FROM elitea_runtime.run_chain_links AS k
JOIN elitea_runtime.run_chains AS c ON c.root_execution_id = k.root_execution_id
WHERE k.execution_id = $1`, executionID).Scan(
		&chain.RootExecutionID, &chain.ExecutionID, &chain.ContinuationIndex, &chain.Kind, &response,
		&chain.ProjectID, &chain.OwnerUserID, &chain.ConversationUUID, &chain.Origin, &chain.TriggerOrigin,
		&chain.GrantID, &grant, &limits.continuations, &limits.spend, &deadline,
		&created, &chain.Continuations, &endState)
	if errors.Is(err, pgx.ErrNoRows) {
		return unattended.Chain{}, false, nil
	}
	if err != nil {
		if undefinedColumnOrTable(err) {
			return unattended.Chain{}, false, nil
		}
		return unattended.Chain{}, false, fmt.Errorf("unattended chain: read link: %w", err)
	}
	if err := json.Unmarshal(grant, &chain.Grant); err != nil {
		return unattended.Chain{}, false, fmt.Errorf("unattended chain: decode the grant: %w", err)
	}
	chain.Limits = unattended.Limits{
		MaxContinuations: limits.continuations, MaxSpendUSD: limits.spend,
		Deadline: deadline.Sub(created).Round(time.Second),
	}
	chain.DeadlineAt = deadline
	if endState != nil {
		chain.EndState = *endState
	}
	if response != nil {
		chain.ResponseMessageID = *response
	}
	return chain, true, nil
}

// endRunChain writes a chain's end state once. It answers false when the
// chain already ended or does not exist.
func endRunChain(ctx context.Context, queryer rowQueryer, root, state, limit string) (bool, error) {
	if !unattended.ValidEndState(state) {
		return false, errors.New("unattended chain: unknown end state")
	}
	if state != unattended.EndLimitReached {
		limit = ""
	}
	var ended string
	err := queryer.QueryRow(ctx, `
UPDATE elitea_runtime.run_chains
SET end_state = $2, end_limit = NULLIF($3, ''), ended_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE root_execution_id = $1 AND end_state IS NULL
RETURNING root_execution_id`, root, state, limit).Scan(&ended)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("unattended chain: end: %w", err)
	}
	return true, nil
}

// RunChainsRepository reads and ends chains outside an admission.
type RunChainsRepository struct {
	pool *pgxpool.Pool
}

// NewRunChainsRepository builds the repository.
func NewRunChainsRepository(pool *pgxpool.Pool) *RunChainsRepository {
	return &RunChainsRepository{pool: pool}
}

// ChainOfExecution answers the chain an execution is a link of.
func (r *RunChainsRepository) ChainOfExecution(ctx context.Context, executionID string) (unattended.Chain, bool, error) {
	return readRunChainLink(ctx, r.pool, executionID)
}

// EndChain writes a chain's end state once (idempotent: a second call
// answers false and changes nothing).
func (r *RunChainsRepository) EndChain(ctx context.Context, root, state, limit string) (bool, error) {
	return endRunChain(ctx, r.pool, root, state, limit)
}

// EndRefusedChain ends a chain whose continuation a limit refused. The
// refused admission rolled back, so this runs in its own statement.
func (r *RunChainsRepository) EndRefusedChain(ctx context.Context, root string, refusal *unattended.ChainLimitError) error {
	if refusal == nil {
		return nil
	}
	_, err := endRunChain(ctx, r.pool, root, unattended.EndLimitReached, refusal.Limit)
	return err
}
