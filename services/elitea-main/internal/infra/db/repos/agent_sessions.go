package repos

// Run-scoped agent sessions (elitea_identity.agent_sessions, shared 0186,
// issue #1151). A row is keyed on the sha256 of the opaque session ref; the
// ref itself is never stored. Every OpenSession and Credential call appends
// one securityaudit.TypeAgentSession event: an opened session in the
// session's own transaction, everything else in a transaction of its own.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const agentSessionSelectColumnList = `id::text, tenant_project_id, execution_id, generation, agent_binding_id::text,
       agent_binding_epoch, external_agent_identity_id::text, external_generation, external_epoch,
       security_profile, user_evidence_kind, delegation_id, mandate_id, evidence_issuer, evidence_tenant_id,
       evidence_subject, evidence_audience, ceiling_audiences, ceiling_scopes, worker_claim_ref,
       policy_revision_id, state, expires_at, created_at`

// AgentSessions is the PostgreSQL agentidentity.SessionStore.
type AgentSessions struct {
	pool  *pgxpool.Pool
	audit securityaudit.Appender
}

// NewAgentSessions builds the store. A nil audit makes every call fail.
func NewAgentSessions(pool *pgxpool.Pool, audit securityaudit.Appender) *AgentSessions {
	return &AgentSessions{pool: pool, audit: audit}
}

var _ agentidentity.SessionStore = (*AgentSessions)(nil)

func (s *AgentSessions) ready() error {
	if s == nil || s.pool == nil {
		return errors.New("agent sessions: no database pool")
	}
	if s.audit == nil {
		return errors.New("agent sessions: no security audit appender")
	}
	return nil
}

func scanAgentSession(row pgx.Row) (agentidentity.AgentSession, error) {
	var (
		a                        agentidentity.AgentSession
		profile, evidence, state string
	)
	err := row.Scan(&a.ID, &a.TenantProjectID, &a.ExecutionID, &a.Generation, &a.AgentBindingID,
		&a.AgentBindingEpoch, &a.ExternalAgentIdentityID, &a.ExternalGeneration, &a.ExternalEpoch,
		&profile, &evidence, &a.DelegationID, &a.MandateID, &a.EvidenceIssuer, &a.EvidenceTenantID,
		&a.EvidenceSubject, &a.EvidenceAudience, &a.Ceiling.Audiences, &a.Ceiling.Scopes, &a.WorkerClaimRef,
		&a.PolicyRevisionID, &state, &a.ExpiresAt, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentidentity.AgentSession{}, agentidentity.ErrSessionNotFound
	}
	if err != nil {
		return agentidentity.AgentSession{}, fmt.Errorf("scan agent session: %w", err)
	}
	a.Profile, a.EvidenceKind = agentidentity.SecurityProfile(profile), agentidentity.EvidenceKind(evidence)
	a.State = agentidentity.SessionState(state)
	a.ExpiresAt, a.CreatedAt = a.ExpiresAt.UTC(), a.CreatedAt.UTC()
	return a, nil
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// CreateSession inserts the session and its audit event in one transaction.
func (s *AgentSessions) CreateSession(ctx context.Context, a agentidentity.AgentSession, refDigest [32]byte, event securityaudit.SecurityEvent) error {
	if err := s.ready(); err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin agent session create: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
INSERT INTO elitea_identity.agent_sessions (
    id, ref_digest, tenant_project_id, execution_id, generation, agent_binding_id, agent_binding_epoch,
    external_agent_identity_id, external_generation, external_epoch, security_profile, user_evidence_kind,
    delegation_id, mandate_id, evidence_issuer, evidence_tenant_id, evidence_subject, evidence_audience,
    ceiling_audiences, ceiling_scopes, worker_claim_ref, policy_revision_id, state, expires_at, created_at, updated_at)
VALUES ($1::uuid, $2, $3, $4, $5, $6::uuid, $7, $8::uuid, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18,
        $19, $20, $21, $22, $23, $24, $25, $25)`,
		a.ID, refDigest[:], a.TenantProjectID, a.ExecutionID, a.Generation, a.AgentBindingID, a.AgentBindingEpoch,
		a.ExternalAgentIdentityID, a.ExternalGeneration, a.ExternalEpoch, string(a.Profile), string(a.EvidenceKind),
		a.DelegationID, a.MandateID, a.EvidenceIssuer, a.EvidenceTenantID, a.EvidenceSubject, a.EvidenceAudience,
		nonNilStrings(a.Ceiling.Audiences), nonNilStrings(a.Ceiling.Scopes), a.WorkerClaimRef, a.PolicyRevisionID,
		string(a.State), a.ExpiresAt, a.CreatedAt); err != nil {
		return fmt.Errorf("insert agent session: %w", err)
	}
	if err := s.audit.Append(ctx, tx, event); err != nil {
		return fmt.Errorf("audit agent session open: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit agent session create: %w", err)
	}
	return nil
}

// SessionByID reads one session by its id, without locking it. The Entra
// agent identity adapter (#1152) reads the execution, generation and
// delegation of a session the broker already claimed.
func (s *AgentSessions) SessionByID(ctx context.Context, id string) (agentidentity.AgentSession, error) {
	if err := s.ready(); err != nil {
		return agentidentity.AgentSession{}, err
	}
	return scanAgentSession(s.pool.QueryRow(ctx, `
SELECT `+agentSessionSelectColumnList+`
FROM elitea_identity.agent_sessions WHERE id = $1::uuid`, id))
}

// ClaimSession locks the session row, passes it to check and writes the
// worker claim check returned when it changed.
func (s *AgentSessions) ClaimSession(ctx context.Context, refDigest [32]byte, check func(agentidentity.AgentSession) (agentidentity.AgentSession, error)) (agentidentity.AgentSession, error) {
	if err := s.ready(); err != nil {
		return agentidentity.AgentSession{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return agentidentity.AgentSession{}, fmt.Errorf("begin agent session claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, err := scanAgentSession(tx.QueryRow(ctx, `
SELECT `+agentSessionSelectColumnList+`
FROM elitea_identity.agent_sessions WHERE ref_digest = $1 FOR UPDATE`, refDigest[:]))
	if err != nil {
		return agentidentity.AgentSession{}, err
	}
	next, err := check(current)
	if err != nil {
		return agentidentity.AgentSession{}, err
	}
	if next.ID != current.ID {
		return agentidentity.AgentSession{}, errors.New("agent sessions: a claim must keep the session id")
	}
	if next.WorkerClaimRef != current.WorkerClaimRef {
		// Bind once: an empty claim becomes the presented one, never another.
		tag, err := tx.Exec(ctx, `
UPDATE elitea_identity.agent_sessions SET worker_claim_ref = $2, updated_at = clock_timestamp()
WHERE id = $1::uuid AND worker_claim_ref = ''`, current.ID, next.WorkerClaimRef)
		if err != nil {
			return agentidentity.AgentSession{}, fmt.Errorf("bind agent session worker claim: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return agentidentity.AgentSession{}, errors.New("agent sessions: the worker claim is already bound")
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return agentidentity.AgentSession{}, fmt.Errorf("commit agent session claim: %w", err)
	}
	return next, nil
}

// AppendEvent appends one event in its own transaction.
func (s *AgentSessions) AppendEvent(ctx context.Context, event securityaudit.SecurityEvent) error {
	if err := s.ready(); err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin agent session audit: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.audit.Append(ctx, tx, event); err != nil {
		return fmt.Errorf("audit agent session: %w", err)
	}
	return tx.Commit(ctx)
}
