package repos

// External agent identities (elitea_identity.external_agent_identities,
// shared 0186, issue #1151). Every transition runs in one transaction that
// locks the row (SELECT ... FOR UPDATE), counts the active bindings that
// reference it (FOR SHARE), applies the domain transition and writes it with
// the epoch it read as a guard. Every write appends a
// securityaudit.TypeExternalIdentityChanged event in the same transaction;
// an append error rolls the change back.
//
// Lock order is external identity, then bindings, then the audit partition
// head. The binding store never locks an external identity, so the two
// never deadlock.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const (
	externalIdentityUniqueIndex      = "external_agent_identities_one_live_per_agent"
	externalIdentityObjectIndex      = "external_agent_identities_one_live_per_object"
	externalIdentitySelectColumnList = `id::text, tenant_project_id, application_id, mode, provider, provider_tenant,
       kind, external_id, generation, provider_config_key, agent_user_ref, owner_ref, state,
       revocation_epoch, created_by, last_transition_by, created_at, updated_at`
)

// ExternalAgentIdentities is the PostgreSQL store for external agent
// identities. It implements agentidentity.ExternalIdentityStore and
// domain.ExternalIdentityStates.
type ExternalAgentIdentities struct {
	pool  *pgxpool.Pool
	audit securityaudit.Appender
}

// NewExternalAgentIdentities builds the store. A nil audit makes every call
// fail rather than write unaudited.
func NewExternalAgentIdentities(pool *pgxpool.Pool, audit securityaudit.Appender) *ExternalAgentIdentities {
	return &ExternalAgentIdentities{pool: pool, audit: audit}
}

var (
	_ agentidentity.ExternalIdentityStore = (*ExternalAgentIdentities)(nil)
	_ domain.ExternalIdentityStates       = (*ExternalAgentIdentities)(nil)
)

func (s *ExternalAgentIdentities) ready() error {
	if s == nil || s.pool == nil {
		return errors.New("external agent identities: no database pool")
	}
	if s.audit == nil {
		return errors.New("external agent identities: no security audit appender")
	}
	return nil
}

func scanExternalIdentity(row pgx.Row) (domain.ExternalAgentIdentity, error) {
	var (
		e     domain.ExternalAgentIdentity
		mode  string
		state string
	)
	err := row.Scan(&e.ID, &e.TenantProjectID, &e.ApplicationID, &mode, &e.Ref.Provider, &e.Ref.ProviderTenant,
		&e.Ref.Kind, &e.Ref.ID, &e.Ref.Generation, &e.ProviderConfigKey, &e.AgentUserRef, &e.OwnerRef, &state,
		&e.RevocationEpoch, &e.CreatedBy, &e.LastTransitionBy, &e.CreatedAt, &e.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ExternalAgentIdentity{}, domain.ErrExternalIdentityNotFound
	}
	if err != nil {
		return domain.ExternalAgentIdentity{}, fmt.Errorf("scan external agent identity: %w", err)
	}
	e.Mode, e.State = domain.IdentityMode(mode), domain.ExternalIdentityState(state)
	e.CreatedAt, e.UpdatedAt = e.CreatedAt.UTC(), e.UpdatedAt.UTC()
	return e, nil
}

func mapExternalIdentityWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		(pgErr.ConstraintName == externalIdentityUniqueIndex || pgErr.ConstraintName == externalIdentityObjectIndex) {
		return domain.ErrDuplicateExternalIdentity
	}
	return err
}

// CreateExternalIdentity inserts a record and its audit event. A second live
// record for the same tenant project, application, provider and provider
// tenant (per-agent modes) is ErrDuplicateExternalIdentity.
func (s *ExternalAgentIdentities) CreateExternalIdentity(ctx context.Context, e domain.ExternalAgentIdentity) (domain.ExternalAgentIdentity, error) {
	if err := s.ready(); err != nil {
		return domain.ExternalAgentIdentity{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.ExternalAgentIdentity{}, fmt.Errorf("begin external identity create: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	created, err := scanExternalIdentity(tx.QueryRow(ctx, `
INSERT INTO elitea_identity.external_agent_identities (
    id, tenant_project_id, application_id, mode, provider, provider_tenant, kind, external_id, generation,
    provider_config_key, agent_user_ref, owner_ref, state, revocation_epoch, created_by, last_transition_by,
    created_at, updated_at)
VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
RETURNING `+externalIdentitySelectColumnList,
		e.ID, e.TenantProjectID, e.ApplicationID, string(e.Mode), e.Ref.Provider, e.Ref.ProviderTenant, e.Ref.Kind,
		e.Ref.ID, e.Ref.Generation, e.ProviderConfigKey, e.AgentUserRef, e.OwnerRef, string(e.State), e.RevocationEpoch,
		e.CreatedBy, e.LastTransitionBy, e.CreatedAt, e.UpdatedAt))
	if err != nil {
		return domain.ExternalAgentIdentity{}, mapExternalIdentityWriteError(err)
	}
	if err := s.audit.Append(ctx, tx, externalIdentityAuditEvent(created, nil)); err != nil {
		return domain.ExternalAgentIdentity{}, fmt.Errorf("audit external identity create: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.ExternalAgentIdentity{}, mapExternalIdentityWriteError(fmt.Errorf("commit external identity create: %w", err))
	}
	return created, nil
}

// GetExternalIdentity reads one record.
func (s *ExternalAgentIdentities) GetExternalIdentity(ctx context.Context, id string) (domain.ExternalAgentIdentity, error) {
	if err := s.ready(); err != nil {
		return domain.ExternalAgentIdentity{}, err
	}
	if !agentIdentityIsUUID(id) {
		return domain.ExternalAgentIdentity{}, domain.ErrExternalIdentityNotFound
	}
	return scanExternalIdentity(s.pool.QueryRow(ctx, `
SELECT `+externalIdentitySelectColumnList+`
FROM elitea_identity.external_agent_identities WHERE id = $1::uuid`, id))
}

// ExternalIdentityState answers the state of the record in tenantProjectID.
// A record of another tenant project is not found.
func (s *ExternalAgentIdentities) ExternalIdentityState(ctx context.Context, tenantProjectID int64, id string) (domain.ExternalIdentityState, error) {
	e, err := s.GetExternalIdentity(ctx, id)
	if err != nil {
		return "", err
	}
	if e.TenantProjectID != tenantProjectID {
		return "", domain.ErrExternalIdentityNotFound
	}
	return e.State, nil
}

// ListExternalIdentities lists records, newest first.
func (s *ExternalAgentIdentities) ListExternalIdentities(ctx context.Context, filter domain.ExternalListFilter) ([]domain.ExternalAgentIdentity, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	limit, offset := agentIdentityListWindow(filter.Limit, filter.Offset)
	rows, err := s.pool.Query(ctx, `
SELECT `+externalIdentitySelectColumnList+`
FROM elitea_identity.external_agent_identities
WHERE ($1::bigint = 0 OR tenant_project_id = $1)
  AND ($2::bigint = 0 OR application_id = $2)
  AND ($3::text = '' OR state = $3)
ORDER BY created_at DESC, id DESC
LIMIT $4 OFFSET $5`, filter.TenantProjectID, filter.ApplicationID, string(filter.State), limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list external agent identities: %w", err)
	}
	defer rows.Close()
	out := []domain.ExternalAgentIdentity{}
	for rows.Next() {
		e, err := scanExternalIdentity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list external agent identities: %w", err)
	}
	return out, nil
}

const activeBindingCountSQL = `
SELECT count(*) FROM (
    SELECT 1 FROM elitea_identity.agent_identity_bindings
    WHERE external_agent_identity_id = $1::uuid AND tenant_project_id = $2 AND state = 'active'
    %s
) AS live`

// ActiveBindingCount counts the active bindings that reference the record.
func (s *ExternalAgentIdentities) ActiveBindingCount(ctx context.Context, tenantProjectID int64, id string) (int, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	if !agentIdentityIsUUID(id) {
		return 0, domain.ErrExternalIdentityNotFound
	}
	var n int
	if err := s.pool.QueryRow(ctx, fmt.Sprintf(activeBindingCountSQL, ""), id, tenantProjectID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count active bindings: %w", err)
	}
	return n, nil
}

// TransitionExternalIdentity locks the row and its active bindings, passes
// them to apply and writes the result. apply must bump the epoch by exactly
// one and keep the id, tenant project, application, mode and provider.
func (s *ExternalAgentIdentities) TransitionExternalIdentity(
	ctx context.Context,
	id string,
	apply func(current domain.ExternalAgentIdentity, activeBindings int) (domain.ExternalAgentIdentity, error),
) (domain.ExternalAgentIdentity, error) {
	if err := s.ready(); err != nil {
		return domain.ExternalAgentIdentity{}, err
	}
	if !agentIdentityIsUUID(id) {
		return domain.ExternalAgentIdentity{}, domain.ErrExternalIdentityNotFound
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.ExternalAgentIdentity{}, fmt.Errorf("begin external identity transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	current, err := scanExternalIdentity(tx.QueryRow(ctx, `
SELECT `+externalIdentitySelectColumnList+`
FROM elitea_identity.external_agent_identities WHERE id = $1::uuid FOR UPDATE`, id))
	if err != nil {
		return domain.ExternalAgentIdentity{}, err
	}
	var activeBindings int
	if err := tx.QueryRow(ctx, fmt.Sprintf(activeBindingCountSQL, "FOR SHARE"), id, current.TenantProjectID).Scan(&activeBindings); err != nil {
		return domain.ExternalAgentIdentity{}, fmt.Errorf("lock active bindings: %w", err)
	}
	next, err := apply(current, activeBindings)
	if err != nil {
		return domain.ExternalAgentIdentity{}, err
	}
	if next.ID != current.ID || next.RevocationEpoch != current.RevocationEpoch+1 ||
		next.TenantProjectID != current.TenantProjectID || next.ApplicationID != current.ApplicationID ||
		next.Mode != current.Mode || next.Ref.Provider != current.Ref.Provider ||
		next.Ref.ProviderTenant != current.Ref.ProviderTenant {
		return domain.ExternalAgentIdentity{}, errors.New("external agent identities: a transition must keep the record's keys and bump the epoch by one")
	}
	if strings.TrimSpace(next.LastTransitionBy) == "" {
		return domain.ExternalAgentIdentity{}, fmt.Errorf("%w: the actor is unknown", domain.ErrInvalidExternalIdentity)
	}
	written, err := scanExternalIdentity(tx.QueryRow(ctx, `
UPDATE elitea_identity.external_agent_identities
SET state = $2, revocation_epoch = $3, kind = $4, external_id = $5, generation = $6, agent_user_ref = $7,
    last_transition_by = $8, updated_at = $9
WHERE id = $1::uuid AND revocation_epoch = $10
RETURNING `+externalIdentitySelectColumnList,
		id, string(next.State), next.RevocationEpoch, next.Ref.Kind, next.Ref.ID, next.Ref.Generation, next.AgentUserRef,
		next.LastTransitionBy, next.UpdatedAt, current.RevocationEpoch))
	if errors.Is(err, domain.ErrExternalIdentityNotFound) {
		return domain.ExternalAgentIdentity{}, domain.ErrExternalTransition
	}
	if err != nil {
		return domain.ExternalAgentIdentity{}, mapExternalIdentityWriteError(err)
	}
	if err := s.audit.Append(ctx, tx, externalIdentityAuditEvent(written, &current)); err != nil {
		return domain.ExternalAgentIdentity{}, fmt.Errorf("audit external identity transition: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.ExternalAgentIdentity{}, mapExternalIdentityWriteError(fmt.Errorf("commit external identity transition: %w", err))
	}
	return written, nil
}

// externalIdentityAuditEvent is the TypeExternalIdentityChanged event of one
// write. previous is nil for the creation. Ids and epochs only.
func externalIdentityAuditEvent(e domain.ExternalAgentIdentity, previous *domain.ExternalAgentIdentity) securityaudit.SecurityEvent {
	action := "external_identity.create"
	reasons := []string{"state." + string(e.State), "mode." + string(e.Mode), "provider." + e.Ref.Provider}
	if previous != nil {
		action = "external_identity." + string(e.State)
		reasons = append(reasons, "from."+string(previous.State))
	}
	return securityaudit.SecurityEvent{
		Type:            securityaudit.TypeExternalIdentityChanged,
		Time:            e.UpdatedAt,
		TenantProjectID: e.TenantProjectID,
		Action:          action,
		Reasons:         reasons,
		PrincipalRef:    externalIdentityActorRef(e.LastTransitionBy),
		AuthorityID:     "external_agent_identity:" + e.ID,
		ResourceType:    "external_agent_identity",
		ResourceID:      e.ID,
		RevocationEpoch: e.RevocationEpoch,
	}
}

// externalIdentityActorRef keeps a system actor ("system:...") as it is and
// prefixes a user id.
func externalIdentityActorRef(actor string) string {
	if strings.HasPrefix(actor, "system:") {
		return actor
	}
	return auditUserRef(actor)
}
