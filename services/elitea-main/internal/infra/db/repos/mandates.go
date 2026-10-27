package repos

// Mandates (elitea_identity.mandates, shared 0181, issue #1112). Same
// transition contract as agent_identity_bindings.go: one transaction, the
// row locked FOR UPDATE, the epoch read used as a write guard, and a
// securityaudit.TypeMandateChanged event appended in that transaction (an
// append error rolls the change back).
//
// Every write locks the mandate's binding FOR SHARE first, and its
// delegation FOR SHARE when it names one, and hands them to the caller's
// check inside the transaction. A binding revoke or suspend (FOR UPDATE)
// therefore waits for an in-flight mandate approval and then cascades over
// it, or runs first and the approval sees the stopped binding: a mandate is
// never left active on a revoked binding. The binding is locked before the
// mandate here and in the binding store's cascade, so the two never
// deadlock.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const (
	mandateApproverCheck    = "mandates_approver_not_issuer"
	mandateSelectColumnList = `id::text, tenant_project_id, kind, agent_binding_id::text, subject_user_id,
       delegation_id::text, operation_scopes, resource_scopes, not_before, expires_at, state,
       revocation_epoch, policy_revision_id, issued_by, COALESCE(approved_by, ''), last_transition_by,
       created_at, updated_at`
)

// Mandates is the PostgreSQL store for mandates. It implements
// mandate.Reader and governance.MandateStore.
type Mandates struct {
	pool  *pgxpool.Pool
	audit securityaudit.Appender
}

// NewMandates builds the store over pool. audit records every change; a nil
// audit makes every call fail rather than write unaudited.
func NewMandates(pool *pgxpool.Pool, audit securityaudit.Appender) *Mandates {
	return &Mandates{pool: pool, audit: audit}
}

var _ mandate.Reader = (*Mandates)(nil)

func (s *Mandates) ready() error {
	if s == nil || s.pool == nil {
		return errors.New("mandates: no database pool")
	}
	if s.audit == nil {
		return errors.New("mandates: no security audit appender")
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
		&m.RevocationEpoch, &m.PolicyRevisionID, &m.IssuedBy, &m.ApprovedBy, &m.LastTransitionBy,
		&m.CreatedAt, &m.UpdatedAt)
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

// lockMandateBinding locks a mandate's binding FOR SHARE. A binding that
// does not exist is mandate.ErrBindingNotUsable.
func lockMandateBinding(ctx context.Context, tx pgx.Tx, bindingID string) (agentidentity.AgentIdentityBinding, error) {
	if !agentIdentityIsUUID(bindingID) {
		return agentidentity.AgentIdentityBinding{}, mandate.ErrBindingNotUsable
	}
	binding, err := scanAgentIdentityBinding(tx.QueryRow(ctx, `
SELECT `+agentIdentityBindingSelectColumnList+`
FROM elitea_identity.agent_identity_bindings WHERE id = $1::uuid FOR SHARE`, bindingID))
	if errors.Is(err, agentidentity.ErrBindingNotFound) {
		return agentidentity.AgentIdentityBinding{}, mandate.ErrBindingNotUsable
	}
	return binding, err
}

// lockMandateDelegation reads the delegation id names FOR SHARE, or nil when
// there is no such row.
func lockMandateDelegation(ctx context.Context, tx pgx.Tx, id string) (*mandate.Delegation, error) {
	if !agentIdentityIsUUID(id) {
		return nil, nil
	}
	var (
		d     mandate.Delegation
		state string
	)
	err := tx.QueryRow(ctx, `
SELECT id::text, tenant_project_id, agent_binding_id::text, state, expires_at, revocation_epoch
FROM elitea_identity.delegations WHERE id = $1::uuid FOR SHARE`, id).
		Scan(&d.ID, &d.TenantProjectID, &d.AgentBindingID, &state, &d.ExpiresAt, &d.RevocationEpoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read delegation: %w", err)
	}
	d.State, d.ExpiresAt = mandate.State(state), d.ExpiresAt.UTC()
	return &d, nil
}

// CreateMandate inserts a proposed mandate and its audit event. It locks the
// binding (and the delegation, when named) first and passes them to check;
// a check error writes nothing.
func (s *Mandates) CreateMandate(
	ctx context.Context,
	m mandate.Mandate,
	check func(binding agentidentity.AgentIdentityBinding, delegation *mandate.Delegation) error,
) (mandate.Mandate, error) {
	if err := s.ready(); err != nil {
		return mandate.Mandate{}, err
	}
	if check == nil {
		return mandate.Mandate{}, errors.New("mandates: a binding check is required")
	}
	resources := m.ResourceScopes
	if resources == nil {
		resources = []string{}
	}
	if m.LastTransitionBy == "" {
		m.LastTransitionBy = m.IssuedBy
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return mandate.Mandate{}, fmt.Errorf("begin mandate create: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	binding, err := lockMandateBinding(ctx, tx, m.AgentBindingID)
	if err != nil {
		return mandate.Mandate{}, err
	}
	var delegation *mandate.Delegation
	if m.DelegationID != nil {
		if delegation, err = lockMandateDelegation(ctx, tx, *m.DelegationID); err != nil {
			return mandate.Mandate{}, err
		}
	}
	if err := check(binding, delegation); err != nil {
		return mandate.Mandate{}, err
	}
	created, err := scanMandate(tx.QueryRow(ctx, `
INSERT INTO elitea_identity.mandates (
    id, tenant_project_id, kind, agent_binding_id, subject_user_id, delegation_id,
    operation_scopes, resource_scopes, not_before, expires_at, state, revocation_epoch,
    policy_revision_id, issued_by, approved_by, last_transition_by, created_at, updated_at)
VALUES ($1::uuid, $2, $3, $4::uuid, $5, $6::uuid, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
RETURNING `+mandateSelectColumnList,
		m.ID, m.TenantProjectID, string(m.Kind), m.AgentBindingID, m.SubjectUserID, m.DelegationID,
		m.OperationScopes, resources, m.NotBefore, m.ExpiresAt, string(m.State), m.RevocationEpoch,
		m.PolicyRevisionID, m.IssuedBy, agentIdentityNullableText(m.ApprovedBy), m.LastTransitionBy, m.CreatedAt, m.UpdatedAt))
	if err != nil {
		return mandate.Mandate{}, mapMandateWriteError(err)
	}
	if err := s.audit.Append(ctx, tx, mandateAuditEvent(created, "")); err != nil {
		return mandate.Mandate{}, fmt.Errorf("audit mandate create: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return mandate.Mandate{}, mapMandateWriteError(fmt.Errorf("commit mandate create: %w", err))
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

// TransitionMandate locks the mandate's binding FOR SHARE, then the mandate
// FOR UPDATE, passes both to apply and writes the result with its audit
// event.
func (s *Mandates) TransitionMandate(
	ctx context.Context,
	id string,
	apply func(current mandate.Mandate, binding agentidentity.AgentIdentityBinding) (mandate.Mandate, error),
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

	// agent_binding_id never changes, so an unlocked read of it is enough to
	// take the binding lock before the mandate lock.
	var bindingID string
	err = tx.QueryRow(ctx, `SELECT agent_binding_id::text FROM elitea_identity.mandates WHERE id = $1::uuid`, id).Scan(&bindingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return mandate.Mandate{}, mandate.ErrMandateNotFound
	}
	if err != nil {
		return mandate.Mandate{}, fmt.Errorf("read mandate binding: %w", err)
	}
	binding, err := lockMandateBinding(ctx, tx, bindingID)
	if err != nil {
		return mandate.Mandate{}, err
	}
	current, err := scanMandate(tx.QueryRow(ctx, `
SELECT `+mandateSelectColumnList+` FROM elitea_identity.mandates WHERE id = $1::uuid FOR UPDATE`, id))
	if err != nil {
		return mandate.Mandate{}, err
	}
	next, err := apply(current, binding)
	if err != nil {
		return mandate.Mandate{}, err
	}
	written, err := writeMandateTransition(ctx, tx, current, next)
	if err != nil {
		return mandate.Mandate{}, err
	}
	if err := s.audit.Append(ctx, tx, mandateAuditEvent(written, "")); err != nil {
		return mandate.Mandate{}, fmt.Errorf("audit mandate transition: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return mandate.Mandate{}, mapMandateWriteError(fmt.Errorf("commit mandate transition: %w", err))
	}
	return written, nil
}

// writeMandateTransition writes next over current, which the caller locked.
// next must keep the id, bump the epoch by one and name its actor.
func writeMandateTransition(ctx context.Context, tx pgx.Tx, current, next mandate.Mandate) (mandate.Mandate, error) {
	if next.ID != current.ID || next.RevocationEpoch != current.RevocationEpoch+1 {
		return mandate.Mandate{}, errors.New("mandates: a transition must keep the id and bump the epoch by one")
	}
	if strings.TrimSpace(next.LastTransitionBy) == "" {
		return mandate.Mandate{}, fmt.Errorf("%w: the actor is unknown", mandate.ErrInvalidMandate)
	}
	written, err := scanMandate(tx.QueryRow(ctx, `
UPDATE elitea_identity.mandates
SET state = $2, revocation_epoch = $3, approved_by = $4, policy_revision_id = $5, updated_at = $6,
    last_transition_by = $8
WHERE id = $1::uuid AND revocation_epoch = $7
RETURNING `+mandateSelectColumnList,
		current.ID, string(next.State), next.RevocationEpoch, agentIdentityNullableText(next.ApprovedBy), next.PolicyRevisionID,
		next.UpdatedAt, current.RevocationEpoch, next.LastTransitionBy))
	if errors.Is(err, mandate.ErrMandateNotFound) {
		return mandate.Mandate{}, mandate.ErrInvalidTransition
	}
	if err != nil {
		return mandate.Mandate{}, mapMandateWriteError(err)
	}
	return written, nil
}

// mandateAuditEvent is the TypeMandateChanged event of one write. cascade is
// the reason code when a binding change caused it, empty otherwise. The
// action follows the stored state the write left.
func mandateAuditEvent(m mandate.Mandate, cascade string) securityaudit.SecurityEvent {
	action := "mandate.propose"
	if m.RevocationEpoch > 0 {
		action = "mandate." + mandateVerb(m.State)
	}
	reasons := []string{"state." + string(m.State), "kind." + string(m.Kind)}
	if cascade != "" {
		reasons = append(reasons, cascade)
	}
	event := securityaudit.SecurityEvent{
		Type:             securityaudit.TypeMandateChanged,
		Time:             m.UpdatedAt,
		TenantProjectID:  m.TenantProjectID,
		Action:           action,
		Reasons:          reasons,
		PrincipalRef:     auditUserRef(m.LastTransitionBy),
		AgentBindingID:   m.AgentBindingID,
		MandateRef:       m.ID,
		ResourceType:     "mandate",
		ResourceID:       m.ID,
		PolicyRevisionID: m.PolicyRevisionID,
		RevocationEpoch:  m.RevocationEpoch,
	}
	if m.DelegationID != nil {
		event.DelegationRef = *m.DelegationID
	}
	return event
}

func mandateVerb(state mandate.State) string {
	switch state {
	case mandate.StateActive:
		return string(mandate.ActionApprove)
	case mandate.StateSuspended:
		return string(mandate.ActionSuspend)
	case mandate.StateRevoked:
		return string(mandate.ActionRevoke)
	}
	return "change"
}
