package repos

// Agent identity bindings (elitea_identity.agent_identity_bindings, shared
// 0181, issue #1112). Every transition runs in one transaction that locks the
// row (SELECT ... FOR UPDATE), applies the domain transition and writes it
// with the epoch it read as a guard, so two concurrent approvals give one
// success and one agentidentity.ErrInvalidTransition.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
)

const (
	agentIdentityBindingUniqueIndex      = "agent_identity_bindings_one_live_per_definition"
	agentIdentityBindingApproverCheck    = "agent_identity_bindings_approver_not_proposer"
	agentIdentityListDefaultLimit        = 50
	agentIdentityListMaxLimit            = 200
	agentIdentityBindingSelectColumnList = `id::text, tenant_project_id, application_id, application_version_id,
       definition_digest, mode, COALESCE(external_agent_identity_id::text, ''), workload_identity,
       state, revocation_epoch, policy_revision_id, proposed_by, COALESCE(approved_by, ''),
       created_at, updated_at`
)

// AgentIdentityBindings is the PostgreSQL store for bindings. It implements
// agentidentity.BindingReader and governance.BindingStore.
type AgentIdentityBindings struct {
	pool *pgxpool.Pool
}

// NewAgentIdentityBindings builds the store over pool.
func NewAgentIdentityBindings(pool *pgxpool.Pool) *AgentIdentityBindings {
	return &AgentIdentityBindings{pool: pool}
}

var _ agentidentity.BindingReader = (*AgentIdentityBindings)(nil)

func (s *AgentIdentityBindings) ready() error {
	if s == nil || s.pool == nil {
		return errors.New("agent identity bindings: no database pool")
	}
	return nil
}

func scanAgentIdentityBinding(row pgx.Row) (agentidentity.AgentIdentityBinding, error) {
	var (
		b      agentidentity.AgentIdentityBinding
		digest []byte
		mode   string
		state  string
	)
	err := row.Scan(&b.ID, &b.TenantProjectID, &b.ApplicationID, &b.ApplicationVersionID,
		&digest, &mode, &b.ExternalAgentIdentityID, &b.WorkloadIdentity,
		&state, &b.RevocationEpoch, &b.PolicyRevisionID, &b.ProposedBy, &b.ApprovedBy,
		&b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentidentity.AgentIdentityBinding{}, agentidentity.ErrBindingNotFound
	}
	if err != nil {
		return agentidentity.AgentIdentityBinding{}, fmt.Errorf("scan agent identity binding: %w", err)
	}
	if len(digest) != len(b.DefinitionDigest) {
		return agentidentity.AgentIdentityBinding{}, errors.New("scan agent identity binding: stored digest is not 32 bytes")
	}
	copy(b.DefinitionDigest[:], digest)
	b.Mode, b.State = agentidentity.IdentityMode(mode), agentidentity.BindingState(state)
	b.CreatedAt, b.UpdatedAt = b.CreatedAt.UTC(), b.UpdatedAt.UTC()
	return b, nil
}

func agentIdentityNullableText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// mapAgentIdentityWriteError turns the two constraints the domain also
// enforces into its typed errors.
func mapAgentIdentityWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "23505" && pgErr.ConstraintName == agentIdentityBindingUniqueIndex:
			return agentidentity.ErrDuplicateBinding
		case pgErr.Code == "23514" && pgErr.ConstraintName == agentIdentityBindingApproverCheck:
			return agentidentity.ErrSelfApproval
		case pgErr.Code == "23503" && strings.Contains(pgErr.ConstraintName, "policy_revision"):
			return fmt.Errorf("%w: the policy revision is not stored", agentidentity.ErrInvalidBinding)
		}
	}
	return err
}

// CreateBinding inserts a proposed binding. A second non-revoked binding for
// the same tenant project, version and digest is ErrDuplicateBinding.
func (s *AgentIdentityBindings) CreateBinding(ctx context.Context, b agentidentity.AgentIdentityBinding) (agentidentity.AgentIdentityBinding, error) {
	if err := s.ready(); err != nil {
		return agentidentity.AgentIdentityBinding{}, err
	}
	row := s.pool.QueryRow(ctx, `
INSERT INTO elitea_identity.agent_identity_bindings (
    id, tenant_project_id, application_id, application_version_id, definition_digest, mode,
    external_agent_identity_id, workload_identity, state, revocation_epoch, policy_revision_id,
    proposed_by, approved_by, created_at, updated_at)
VALUES ($1::uuid, $2, $3, $4, $5, $6, $7::uuid, $8, $9, $10, $11, $12, $13, $14, $15)
RETURNING `+agentIdentityBindingSelectColumnList,
		b.ID, b.TenantProjectID, b.ApplicationID, b.ApplicationVersionID, b.DefinitionDigest[:], string(b.Mode),
		agentIdentityNullableText(b.ExternalAgentIdentityID), b.WorkloadIdentity, string(b.State), b.RevocationEpoch, b.PolicyRevisionID,
		b.ProposedBy, agentIdentityNullableText(b.ApprovedBy), b.CreatedAt, b.UpdatedAt)
	created, err := scanAgentIdentityBinding(row)
	if err != nil {
		return agentidentity.AgentIdentityBinding{}, mapAgentIdentityWriteError(err)
	}
	return created, nil
}

// Get reads one binding by id.
func (s *AgentIdentityBindings) Get(ctx context.Context, id string) (agentidentity.AgentIdentityBinding, error) {
	if err := s.ready(); err != nil {
		return agentidentity.AgentIdentityBinding{}, err
	}
	if !agentIdentityIsUUID(id) {
		return agentidentity.AgentIdentityBinding{}, agentidentity.ErrBindingNotFound
	}
	return scanAgentIdentityBinding(s.pool.QueryRow(ctx, `
SELECT `+agentIdentityBindingSelectColumnList+`
FROM elitea_identity.agent_identity_bindings WHERE id = $1::uuid`, id))
}

// ActiveForDefinition reads the active binding of one agent version. Until
// #1151 it reads the stored state only (see agentidentity.BindingReader).
func (s *AgentIdentityBindings) ActiveForDefinition(ctx context.Context, tenantProjectID, versionID int64, digest [32]byte) (agentidentity.AgentIdentityBinding, error) {
	if err := s.ready(); err != nil {
		return agentidentity.AgentIdentityBinding{}, err
	}
	return scanAgentIdentityBinding(s.pool.QueryRow(ctx, `
SELECT `+agentIdentityBindingSelectColumnList+`
FROM elitea_identity.agent_identity_bindings
WHERE tenant_project_id = $1 AND application_version_id = $2 AND definition_digest = $3
  AND state = 'active'`, tenantProjectID, versionID, digest[:]))
}

func agentIdentityListWindow(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = agentIdentityListDefaultLimit
	}
	if limit > agentIdentityListMaxLimit {
		limit = agentIdentityListMaxLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// ListBindings lists bindings, newest first.
func (s *AgentIdentityBindings) ListBindings(ctx context.Context, filter agentidentity.ListFilter) ([]agentidentity.AgentIdentityBinding, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	limit, offset := agentIdentityListWindow(filter.Limit, filter.Offset)
	rows, err := s.pool.Query(ctx, `
SELECT `+agentIdentityBindingSelectColumnList+`
FROM elitea_identity.agent_identity_bindings
WHERE ($1::bigint = 0 OR tenant_project_id = $1)
  AND ($2::text = '' OR state = $2)
ORDER BY created_at DESC, id DESC
LIMIT $3 OFFSET $4`, filter.TenantProjectID, string(filter.State), limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list agent identity bindings: %w", err)
	}
	defer rows.Close()
	out := []agentidentity.AgentIdentityBinding{}
	for rows.Next() {
		b, err := scanAgentIdentityBinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list agent identity bindings: %w", err)
	}
	return out, nil
}

// TransitionBinding locks the row, passes it to apply and writes the result.
// apply must bump the epoch by exactly one; anything else is refused. The
// UPDATE is guarded by the epoch it read as well as by the lock.
func (s *AgentIdentityBindings) TransitionBinding(
	ctx context.Context,
	id string,
	apply func(agentidentity.AgentIdentityBinding) (agentidentity.AgentIdentityBinding, error),
) (agentidentity.AgentIdentityBinding, error) {
	if err := s.ready(); err != nil {
		return agentidentity.AgentIdentityBinding{}, err
	}
	if !agentIdentityIsUUID(id) {
		return agentidentity.AgentIdentityBinding{}, agentidentity.ErrBindingNotFound
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return agentidentity.AgentIdentityBinding{}, fmt.Errorf("begin binding transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	current, err := scanAgentIdentityBinding(tx.QueryRow(ctx, `
SELECT `+agentIdentityBindingSelectColumnList+`
FROM elitea_identity.agent_identity_bindings WHERE id = $1::uuid FOR UPDATE`, id))
	if err != nil {
		return agentidentity.AgentIdentityBinding{}, err
	}
	next, err := apply(current)
	if err != nil {
		return agentidentity.AgentIdentityBinding{}, err
	}
	if next.ID != current.ID || next.RevocationEpoch != current.RevocationEpoch+1 {
		return agentidentity.AgentIdentityBinding{}, errors.New("agent identity bindings: a transition must keep the id and bump the epoch by one")
	}
	written, err := scanAgentIdentityBinding(tx.QueryRow(ctx, `
UPDATE elitea_identity.agent_identity_bindings
SET state = $2, revocation_epoch = $3, approved_by = $4, policy_revision_id = $5, updated_at = $6
WHERE id = $1::uuid AND revocation_epoch = $7
RETURNING `+agentIdentityBindingSelectColumnList,
		id, string(next.State), next.RevocationEpoch, agentIdentityNullableText(next.ApprovedBy), next.PolicyRevisionID,
		next.UpdatedAt, current.RevocationEpoch))
	if errors.Is(err, agentidentity.ErrBindingNotFound) {
		return agentidentity.AgentIdentityBinding{}, agentidentity.ErrInvalidTransition
	}
	if err != nil {
		return agentidentity.AgentIdentityBinding{}, mapAgentIdentityWriteError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return agentidentity.AgentIdentityBinding{}, mapAgentIdentityWriteError(fmt.Errorf("commit binding transition: %w", err))
	}
	return written, nil
}

// agentIdentityIsUUID keeps a malformed path id from reaching the uuid cast, which
// would be a 22P02 error rather than "not found".
func agentIdentityIsUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, c := range value {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}
