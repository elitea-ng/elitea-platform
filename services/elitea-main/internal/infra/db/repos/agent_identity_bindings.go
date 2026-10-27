package repos

// Agent identity bindings (elitea_identity.agent_identity_bindings, shared
// 0181, issue #1112). Every transition runs in one transaction that locks the
// row (SELECT ... FOR UPDATE), applies the domain transition and writes it
// with the epoch it read as a guard, so two concurrent approvals give one
// success and one agentidentity.ErrInvalidTransition.
//
// Every write appends a securityaudit.TypeBindingChanged event in the same
// transaction; an append error rolls the change back (ADR-0039, #1114).
//
// A revoke revokes every non-revoked mandate on the binding and a suspend
// suspends every active one (mandate.FollowBinding), in the same
// transaction, each with its epoch bumped and its own TypeMandateChanged
// event. Lock order is binding, then mandates, then the audit partition
// head; the mandate store takes the binding first as well (FOR SHARE), so
// the two never deadlock.

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
	agentIdentityBindingUniqueIndex      = "agent_identity_bindings_one_live_per_definition"
	agentIdentityBindingApproverCheck    = "agent_identity_bindings_approver_not_proposer"
	agentIdentityBindingExternalFK       = "agent_identity_bindings_external_same_application"
	agentIdentityListDefaultLimit        = 50
	agentIdentityListMaxLimit            = 200
	agentIdentityBindingSelectColumnList = `id::text, tenant_project_id, application_id, application_version_id,
       definition_digest, mode, COALESCE(external_agent_identity_id::text, ''), workload_identity,
       state, revocation_epoch, policy_revision_id, proposed_by, COALESCE(approved_by, ''),
       last_transition_by, created_at, updated_at`
)

// AgentIdentityBindings is the PostgreSQL store for bindings. It implements
// agentidentity.BindingReader and governance.BindingStore.
type AgentIdentityBindings struct {
	pool  *pgxpool.Pool
	audit securityaudit.Appender
}

// NewAgentIdentityBindings builds the store over pool. audit records every
// change; a nil audit makes every call fail rather than write unaudited.
func NewAgentIdentityBindings(pool *pgxpool.Pool, audit securityaudit.Appender) *AgentIdentityBindings {
	return &AgentIdentityBindings{pool: pool, audit: audit}
}

var _ agentidentity.BindingReader = (*AgentIdentityBindings)(nil)

func (s *AgentIdentityBindings) ready() error {
	if s == nil || s.pool == nil {
		return errors.New("agent identity bindings: no database pool")
	}
	if s.audit == nil {
		return errors.New("agent identity bindings: no security audit appender")
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
		&b.LastTransitionBy, &b.CreatedAt, &b.UpdatedAt)
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
		case pgErr.Code == "23503" && pgErr.ConstraintName == agentIdentityBindingExternalFK:
			// A fork or an import gets its own external identity (ADR-0039
			// decision 1): the reference must be of the same application.
			return fmt.Errorf("%w: external_agent_identity_id must name an external identity of the same tenant project and application", agentidentity.ErrInvalidBinding)
		}
	}
	return err
}

// CreateBinding inserts a proposed binding and its audit event. A second
// non-revoked binding for the same tenant project, version and digest is
// ErrDuplicateBinding.
func (s *AgentIdentityBindings) CreateBinding(ctx context.Context, b agentidentity.AgentIdentityBinding) (agentidentity.AgentIdentityBinding, error) {
	if err := s.ready(); err != nil {
		return agentidentity.AgentIdentityBinding{}, err
	}
	if b.LastTransitionBy == "" {
		b.LastTransitionBy = b.ProposedBy
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return agentidentity.AgentIdentityBinding{}, fmt.Errorf("begin binding create: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row := tx.QueryRow(ctx, `
INSERT INTO elitea_identity.agent_identity_bindings (
    id, tenant_project_id, application_id, application_version_id, definition_digest, mode,
    external_agent_identity_id, workload_identity, state, revocation_epoch, policy_revision_id,
    proposed_by, approved_by, last_transition_by, created_at, updated_at)
VALUES ($1::uuid, $2, $3, $4, $5, $6, $7::uuid, $8, $9, $10, $11, $12, $13, $14, $15, $16)
RETURNING `+agentIdentityBindingSelectColumnList,
		b.ID, b.TenantProjectID, b.ApplicationID, b.ApplicationVersionID, b.DefinitionDigest[:], string(b.Mode),
		agentIdentityNullableText(b.ExternalAgentIdentityID), b.WorkloadIdentity, string(b.State), b.RevocationEpoch, b.PolicyRevisionID,
		b.ProposedBy, agentIdentityNullableText(b.ApprovedBy), b.LastTransitionBy, b.CreatedAt, b.UpdatedAt)
	created, err := scanAgentIdentityBinding(row)
	if err != nil {
		return agentidentity.AgentIdentityBinding{}, mapAgentIdentityWriteError(err)
	}
	if err := s.audit.Append(ctx, tx, bindingAuditEvent(created, nil)); err != nil {
		return agentidentity.AgentIdentityBinding{}, fmt.Errorf("audit binding create: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return agentidentity.AgentIdentityBinding{}, mapAgentIdentityWriteError(fmt.Errorf("commit binding create: %w", err))
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

// ActiveForDefinition reads the active binding of one agent version. A
// binding whose external identity is missing or not active answers with
// State BindingSuspended (agentidentity.BindingReader, #1151).
func (s *AgentIdentityBindings) ActiveForDefinition(ctx context.Context, tenantProjectID, versionID int64, digest [32]byte) (agentidentity.AgentIdentityBinding, error) {
	if err := s.ready(); err != nil {
		return agentidentity.AgentIdentityBinding{}, err
	}
	var externalState string
	row := s.pool.QueryRow(ctx, `
SELECT `+agentIdentityBindingSelectColumnList+`,
       COALESCE((SELECT e.state FROM elitea_identity.external_agent_identities AS e
                 WHERE e.id = b.external_agent_identity_id AND e.tenant_project_id = b.tenant_project_id), '')
FROM elitea_identity.agent_identity_bindings AS b
WHERE tenant_project_id = $1 AND application_version_id = $2 AND definition_digest = $3
  AND state = 'active'`, tenantProjectID, versionID, digest[:])
	b, err := scanAgentIdentityBinding(scanTail{row: row, tail: &externalState})
	if err != nil {
		return agentidentity.AgentIdentityBinding{}, err
	}
	if !agentidentity.UsableBinding(b, agentidentity.ExternalIdentityState(externalState)) {
		b.State = agentidentity.BindingSuspended
	}
	return b, nil
}

// scanTail lets scanAgentIdentityBinding read a row with extra trailing
// columns: it appends tail to the destinations.
type scanTail struct {
	row  pgx.Row
	tail *string
}

func (s scanTail) Scan(dest ...any) error { return s.row.Scan(append(dest, s.tail)...) }

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
// UPDATE is guarded by the epoch it read as well as by the lock. A revoke or
// suspend cascades to the binding's mandates (see the file comment). Every
// row changed appends its audit event; any error rolls all of it back.
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
	if strings.TrimSpace(next.LastTransitionBy) == "" {
		return agentidentity.AgentIdentityBinding{}, fmt.Errorf("%w: the actor is unknown", agentidentity.ErrInvalidBinding)
	}
	written, err := scanAgentIdentityBinding(tx.QueryRow(ctx, `
UPDATE elitea_identity.agent_identity_bindings
SET state = $2, revocation_epoch = $3, approved_by = $4, policy_revision_id = $5, updated_at = $6,
    last_transition_by = $8
WHERE id = $1::uuid AND revocation_epoch = $7
RETURNING `+agentIdentityBindingSelectColumnList,
		id, string(next.State), next.RevocationEpoch, agentIdentityNullableText(next.ApprovedBy), next.PolicyRevisionID,
		next.UpdatedAt, current.RevocationEpoch, next.LastTransitionBy))
	if errors.Is(err, agentidentity.ErrBindingNotFound) {
		return agentidentity.AgentIdentityBinding{}, agentidentity.ErrInvalidTransition
	}
	if err != nil {
		return agentidentity.AgentIdentityBinding{}, mapAgentIdentityWriteError(err)
	}
	cascaded, err := cascadeBindingToMandates(ctx, tx, written)
	if err != nil {
		return agentidentity.AgentIdentityBinding{}, err
	}
	// Events last, after every row lock, so the partition head is the last
	// lock this transaction takes.
	if err := s.audit.Append(ctx, tx, bindingAuditEvent(written, &current)); err != nil {
		return agentidentity.AgentIdentityBinding{}, fmt.Errorf("audit binding transition: %w", err)
	}
	for _, m := range cascaded {
		if err := s.audit.Append(ctx, tx, mandateAuditEvent(m, bindingCascadeReason(written.State))); err != nil {
			return agentidentity.AgentIdentityBinding{}, fmt.Errorf("audit mandate cascade: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return agentidentity.AgentIdentityBinding{}, mapAgentIdentityWriteError(fmt.Errorf("commit binding transition: %w", err))
	}
	return written, nil
}

// cascadeBindingToMandates applies mandate.FollowBinding to every
// non-revoked mandate of binding when binding is now revoked or suspended,
// and returns the mandates it changed. It locks them FOR UPDATE in id order.
func cascadeBindingToMandates(ctx context.Context, tx pgx.Tx, binding agentidentity.AgentIdentityBinding) ([]mandate.Mandate, error) {
	var action mandate.Action
	switch binding.State {
	case agentidentity.BindingRevoked:
		action = mandate.ActionRevoke
	case agentidentity.BindingSuspended:
		action = mandate.ActionSuspend
	default:
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
SELECT `+mandateSelectColumnList+`
FROM elitea_identity.mandates
WHERE agent_binding_id = $1::uuid AND state <> 'revoked'
ORDER BY id
FOR UPDATE`, binding.ID)
	if err != nil {
		return nil, fmt.Errorf("lock mandates of binding: %w", err)
	}
	var live []mandate.Mandate
	for rows.Next() {
		m, err := scanMandate(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		live = append(live, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lock mandates of binding: %w", err)
	}
	changed := make([]mandate.Mandate, 0, len(live))
	for _, current := range live {
		next, ok, err := current.FollowBinding(action, binding.LastTransitionBy, binding.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("cascade binding %s to mandate: %w", binding.State, err)
		}
		if !ok {
			continue
		}
		written, err := writeMandateTransition(ctx, tx, current, next)
		if err != nil {
			return nil, err
		}
		changed = append(changed, written)
	}
	return changed, nil
}

func bindingCascadeReason(state agentidentity.BindingState) string {
	return "cascade.binding_" + string(state)
}

// bindingAuditEvent is the TypeBindingChanged event of one write. previous
// is nil for the proposal. It names references only.
func bindingAuditEvent(b agentidentity.AgentIdentityBinding, previous *agentidentity.AgentIdentityBinding) securityaudit.SecurityEvent {
	action := "binding.propose"
	reasons := []string{"state." + string(b.State)}
	if previous != nil {
		action = "binding." + bindingVerb(b.State)
		reasons = append(reasons, "from."+string(previous.State))
	}
	return securityaudit.SecurityEvent{
		Type:             securityaudit.TypeBindingChanged,
		Time:             b.UpdatedAt,
		TenantProjectID:  b.TenantProjectID,
		Action:           action,
		Reasons:          reasons,
		PrincipalRef:     auditUserRef(b.LastTransitionBy),
		AgentBindingID:   b.ID,
		ResourceType:     "agent_identity_binding",
		ResourceID:       b.ID,
		PolicyRevisionID: b.PolicyRevisionID,
		RevocationEpoch:  b.RevocationEpoch,
	}
}

func bindingVerb(state agentidentity.BindingState) string {
	switch state {
	case agentidentity.BindingActive:
		return string(agentidentity.ActionApprove)
	case agentidentity.BindingSuspended:
		return string(agentidentity.ActionSuspend)
	case agentidentity.BindingRevoked:
		return string(agentidentity.ActionRevoke)
	}
	return "change"
}

func auditUserRef(userID string) string { return "user:" + userID }

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
