package repos

// Mandates (elitea_identity.mandates, shared 0181, issue #1112). Same
// transition contract as agent_identity_bindings.go: one transaction, the
// row locked FOR UPDATE, the epoch read used as a write guard.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
)

const (
	mandateApproverCheck    = "mandates_approver_not_issuer"
	mandateSelectColumnList = `id::text, tenant_project_id, kind, agent_binding_id::text, subject_user_id,
       delegation_id::text, operation_scopes, resource_scopes, not_before, expires_at, state,
       revocation_epoch, policy_revision_id, issued_by, COALESCE(approved_by, ''), created_at, updated_at`
)

// Mandates is the PostgreSQL store for mandates. It implements
// mandate.Reader and governance.MandateStore.
type Mandates struct {
	pool *pgxpool.Pool
}

// NewMandates builds the store over pool.
func NewMandates(pool *pgxpool.Pool) *Mandates { return &Mandates{pool: pool} }

var _ mandate.Reader = (*Mandates)(nil)

func (s *Mandates) ready() error {
	if s == nil || s.pool == nil {
		return errors.New("mandates: no database pool")
	}
	return nil
}

func scanMandate(row pgx.Row) (mandate.Mandate, error) {
	var (
		m           mandate.Mandate
		kind, state string
	)
	err := row.Scan(&m.ID, &m.TenantProjectID, &kind, &m.AgentBindingID, &m.SubjectUserID,
		&m.DelegationID, &m.OperationScopes, &m.ResourceScopes, &m.NotBefore, &m.ExpiresAt, &state,
		&m.RevocationEpoch, &m.PolicyRevisionID, &m.IssuedBy, &m.ApprovedBy, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return mandate.Mandate{}, mandate.ErrMandateNotFound
	}
	if err != nil {
		return mandate.Mandate{}, fmt.Errorf("scan mandate: %w", err)
	}
	m.Kind, m.State = mandate.Kind(kind), mandate.State(state)
	if m.ResourceScopes == nil {
		m.ResourceScopes = []string{}
	}
	m.NotBefore, m.ExpiresAt = m.NotBefore.UTC(), m.ExpiresAt.UTC()
	m.CreatedAt, m.UpdatedAt = m.CreatedAt.UTC(), m.UpdatedAt.UTC()
	return m, nil
}

func mapMandateWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "23514" && pgErr.ConstraintName == mandateApproverCheck:
			return mandate.ErrSelfApproval
		case pgErr.Code == "23503":
			return fmt.Errorf("%w: a referenced binding, delegation or policy revision is not stored", mandate.ErrInvalidMandate)
		case pgErr.Code == "23514":
			return fmt.Errorf("%w: refused by a table constraint", mandate.ErrInvalidMandate)
		}
	}
	return err
}

// CreateMandate inserts a proposed mandate.
func (s *Mandates) CreateMandate(ctx context.Context, m mandate.Mandate) (mandate.Mandate, error) {
	if err := s.ready(); err != nil {
		return mandate.Mandate{}, err
	}
	resources := m.ResourceScopes
	if resources == nil {
		resources = []string{}
	}
	created, err := scanMandate(s.pool.QueryRow(ctx, `
INSERT INTO elitea_identity.mandates (
    id, tenant_project_id, kind, agent_binding_id, subject_user_id, delegation_id,
    operation_scopes, resource_scopes, not_before, expires_at, state, revocation_epoch,
    policy_revision_id, issued_by, approved_by, created_at, updated_at)
VALUES ($1::uuid, $2, $3, $4::uuid, $5, $6::uuid, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
RETURNING `+mandateSelectColumnList,
		m.ID, m.TenantProjectID, string(m.Kind), m.AgentBindingID, m.SubjectUserID, m.DelegationID,
		m.OperationScopes, resources, m.NotBefore, m.ExpiresAt, string(m.State), m.RevocationEpoch,
		m.PolicyRevisionID, m.IssuedBy, agentIdentityNullableText(m.ApprovedBy), m.CreatedAt, m.UpdatedAt))
	if err != nil {
		return mandate.Mandate{}, mapMandateWriteError(err)
	}
	return created, nil
}

// Get reads one mandate by id.
func (s *Mandates) Get(ctx context.Context, id string) (mandate.Mandate, error) {
	if err := s.ready(); err != nil {
		return mandate.Mandate{}, err
	}
	if !agentIdentityIsUUID(id) {
		return mandate.Mandate{}, mandate.ErrMandateNotFound
	}
	return scanMandate(s.pool.QueryRow(ctx, `
SELECT `+mandateSelectColumnList+` FROM elitea_identity.mandates WHERE id = $1::uuid`, id))
}

// ListMandates lists mandates, newest first. A state filter reads the
// EFFECTIVE state: `expired` is any non-revoked row past expires_at, and
// the other non-terminal states exclude such rows.
func (s *Mandates) ListMandates(ctx context.Context, filter mandate.ListFilter) ([]mandate.Mandate, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if filter.AgentBindingID != "" && !agentIdentityIsUUID(filter.AgentBindingID) {
		return []mandate.Mandate{}, nil
	}
	limit, offset := agentIdentityListWindow(filter.Limit, filter.Offset)
	rows, err := s.pool.Query(ctx, `
SELECT `+mandateSelectColumnList+`
FROM elitea_identity.mandates
WHERE ($1::bigint = 0 OR tenant_project_id = $1)
  AND ($2::text = '' OR agent_binding_id::text = $2)
  AND (
        $3::text = ''
     OR ($3 = 'revoked' AND state = 'revoked')
     OR ($3 = 'expired' AND state <> 'revoked' AND (state = 'expired' OR expires_at <= now()))
     OR ($3 NOT IN ('revoked', 'expired') AND state = $3 AND expires_at > now())
  )
ORDER BY created_at DESC, id DESC
LIMIT $4 OFFSET $5`, filter.TenantProjectID, filter.AgentBindingID, string(filter.State), limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list mandates: %w", err)
	}
	defer rows.Close()
	out := []mandate.Mandate{}
	for rows.Next() {
		m, err := scanMandate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list mandates: %w", err)
	}
	return out, nil
}

// TransitionMandate locks the row, passes it to apply and writes the result.
func (s *Mandates) TransitionMandate(
	ctx context.Context,
	id string,
	apply func(mandate.Mandate) (mandate.Mandate, error),
) (mandate.Mandate, error) {
	if err := s.ready(); err != nil {
		return mandate.Mandate{}, err
	}
	if !agentIdentityIsUUID(id) {
		return mandate.Mandate{}, mandate.ErrMandateNotFound
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return mandate.Mandate{}, fmt.Errorf("begin mandate transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	current, err := scanMandate(tx.QueryRow(ctx, `
SELECT `+mandateSelectColumnList+` FROM elitea_identity.mandates WHERE id = $1::uuid FOR UPDATE`, id))
	if err != nil {
		return mandate.Mandate{}, err
	}
	next, err := apply(current)
	if err != nil {
		return mandate.Mandate{}, err
	}
	if next.ID != current.ID || next.RevocationEpoch != current.RevocationEpoch+1 {
		return mandate.Mandate{}, errors.New("mandates: a transition must keep the id and bump the epoch by one")
	}
	written, err := scanMandate(tx.QueryRow(ctx, `
UPDATE elitea_identity.mandates
SET state = $2, revocation_epoch = $3, approved_by = $4, policy_revision_id = $5, updated_at = $6
WHERE id = $1::uuid AND revocation_epoch = $7
RETURNING `+mandateSelectColumnList,
		id, string(next.State), next.RevocationEpoch, agentIdentityNullableText(next.ApprovedBy), next.PolicyRevisionID,
		next.UpdatedAt, current.RevocationEpoch))
	if errors.Is(err, mandate.ErrMandateNotFound) {
		return mandate.Mandate{}, mandate.ErrInvalidTransition
	}
	if err != nil {
		return mandate.Mandate{}, mapMandateWriteError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return mandate.Mandate{}, mapMandateWriteError(fmt.Errorf("commit mandate transition: %w", err))
	}
	return written, nil
}
