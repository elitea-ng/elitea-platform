package credentialbroker

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// PostgresStore is the broker's Store over migration 0188 and the security
// audit outbox (0183). It holds the audit appender, so a decision or a
// redemption is never recorded without its event.
type PostgresStore struct {
	pool  *pgxpool.Pool
	audit securityaudit.Appender
}

var _ Store = (*PostgresStore)(nil)

// NewPostgresStore builds the store. Both arguments are required.
func NewPostgresStore(pool *pgxpool.Pool, audit securityaudit.Appender) (*PostgresStore, error) {
	if pool == nil || audit == nil {
		return nil, errors.New("credential broker store: a database pool and a security audit appender are required")
	}
	return &PostgresStore{pool: pool, audit: audit}, nil
}

// AdmitInvocation implements Store.
func (s *PostgresStore) AdmitInvocation(ctx context.Context, run ClaimedRun, inv BoundToolInvocation,
	decide func(context.Context) (securityaudit.SecurityEvent, error)) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.bound_tool_invocations (
    execution_id, generation, invocation_id, tool_call_id, toolkit_id, operation_id,
    resource_ref, arguments_digest, credential_reference_id, tenant_project_id
) VALUES ($1, $2, $3::uuid, $4, $5, $6, $7, $8, $9::uuid, $10)
ON CONFLICT (execution_id, generation, invocation_id) DO NOTHING`,
			run.ExecutionID, int64(run.Generation), inv.InvocationID, inv.ToolCallID, inv.ToolkitID, inv.OperationID,
			inv.ResourceRef, inv.ArgumentsDigest[:], inv.CredentialReferenceID, run.TenantProjectID)
		if err != nil {
			return fmt.Errorf("record the bound tool invocation: %w", err)
		}
		if tag.RowsAffected() == 0 {
			if err := matchInvocation(ctx, tx, run, inv); err != nil {
				return err
			}
		}
		event, err := decide(ctx)
		if err != nil {
			return err
		}
		if err := s.audit.Append(ctx, tx, event); err != nil {
			return fmt.Errorf("%w: %w", ErrAuditFailed, err)
		}
		return nil
	})
}

// matchInvocation compares a replay with the stored row, under its lock.
func matchInvocation(ctx context.Context, tx pgx.Tx, run ClaimedRun, inv BoundToolInvocation) error {
	var (
		stored BoundToolInvocation
		digest []byte
		tenant int64
	)
	err := tx.QueryRow(ctx, `
SELECT invocation_id::text, tool_call_id, toolkit_id, operation_id, resource_ref, arguments_digest,
       credential_reference_id::text, tenant_project_id
FROM elitea_runtime.bound_tool_invocations
WHERE execution_id = $1 AND generation = $2 AND invocation_id = $3::uuid
FOR SHARE`, run.ExecutionID, int64(run.Generation), inv.InvocationID).Scan(
		&stored.InvocationID, &stored.ToolCallID, &stored.ToolkitID, &stored.OperationID, &stored.ResourceRef,
		&digest, &stored.CredentialReferenceID, &tenant)
	if err != nil {
		return fmt.Errorf("read the bound tool invocation: %w", err)
	}
	if len(digest) != len(stored.ArgumentsDigest) {
		return ErrInvocationMismatch
	}
	copy(stored.ArgumentsDigest[:], digest)
	if stored.ToolCallID != inv.ToolCallID || stored.ToolkitID != inv.ToolkitID || stored.OperationID != inv.OperationID ||
		stored.ResourceRef != inv.ResourceRef || stored.CredentialReferenceID != inv.CredentialReferenceID ||
		tenant != run.TenantProjectID ||
		subtle.ConstantTimeCompare(stored.ArgumentsDigest[:], inv.ArgumentsDigest[:]) != 1 {
		return ErrInvocationMismatch
	}
	return nil
}

// RecordRedemption implements Store.
func (s *PostgresStore) RecordRedemption(ctx context.Context, run ClaimedRun, r Redemption, event securityaudit.SecurityEvent) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.credential_redemptions (
    redemption_id, execution_id, generation, invocation_id, decision_id, kind, not_after
) VALUES ($1::uuid, $2, $3, $4::uuid, $5::uuid, $6, $7)`,
			r.RedemptionID, run.ExecutionID, int64(run.Generation), r.InvocationID, r.DecisionID, string(r.Kind), r.NotAfter.UTC(),
		); err != nil {
			return fmt.Errorf("record the credential redemption: %w", err)
		}
		if err := s.audit.Append(ctx, tx, event); err != nil {
			return fmt.Errorf("%w: %w", ErrAuditFailed, err)
		}
		return nil
	})
}

// AgentSession implements Store.
func (s *PostgresStore) AgentSession(ctx context.Context, executionID string, generation uint64) (string, bool, error) {
	var ref string
	err := s.pool.QueryRow(ctx, `
SELECT agent_session_ref FROM elitea_runtime.execution_agent_sessions
WHERE execution_id = $1 AND generation = $2`, executionID, int64(generation)).Scan(&ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read the run's agent session: %w", err)
	}
	return ref, true, nil
}

// LinkAgentSession implements Store. A concurrent first redemption that
// lost the race keeps the winner's session; the loser's session stays open
// and unused until it expires.
func (s *PostgresStore) LinkAgentSession(ctx context.Context, executionID string, generation uint64, ref string) (string, error) {
	var linked string
	err := s.pool.QueryRow(ctx, `
WITH inserted AS (
    INSERT INTO elitea_runtime.execution_agent_sessions (execution_id, generation, agent_session_ref)
    VALUES ($1, $2, $3)
    ON CONFLICT (execution_id, generation) DO NOTHING
    RETURNING agent_session_ref
)
SELECT agent_session_ref FROM inserted
UNION ALL
SELECT agent_session_ref FROM elitea_runtime.execution_agent_sessions
WHERE execution_id = $1 AND generation = $2 AND NOT EXISTS (SELECT 1 FROM inserted)`,
		executionID, int64(generation), ref).Scan(&linked)
	if err != nil {
		return "", fmt.Errorf("link the run's agent session: %w", err)
	}
	return linked, nil
}

// SessionCeiling implements Store: the audience, or else the canonical
// identity, of every target the binding's active, unexpired grants name.
func (s *PostgresStore) SessionCeiling(ctx context.Context, tenantProjectID int64, agentBindingID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
SELECT DISTINCT COALESCE(NULLIF(t.audience, ''), t.target_identity) AS audience
FROM elitea_identity.credential_use_grants AS g
JOIN elitea_identity.target_bindings AS t
  ON t.id = g.target_binding_id AND t.tenant_project_id = g.tenant_project_id
WHERE g.tenant_project_id = $1 AND g.agent_binding_id = $2::uuid AND g.state = 'active'
  AND (g.expires_at IS NULL OR g.expires_at > clock_timestamp())
ORDER BY audience
LIMIT 64`, tenantProjectID, agentBindingID)
	if err != nil {
		return nil, fmt.Errorf("read the session ceiling: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
