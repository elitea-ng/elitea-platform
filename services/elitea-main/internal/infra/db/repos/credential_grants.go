package repos

// Credential grants (elitea_identity.credential_use_grants and the tables
// beside it, shared 0184, issue #1116). Every grant transition runs in one
// transaction that locks the row (SELECT ... FOR UPDATE), applies the domain
// transition and writes it with the epoch it read as a guard, so two
// concurrent approvals give one success and one
// credentialgrant.ErrInvalidTransition.
//
// Every write appends a securityaudit.TypeGrantChanged event in the same
// transaction; an append error rolls the change back (ADR-0039, #1114). The
// event names ids and the epoch only.
//
// The same store is the reader the decision gates read (grant by exact key,
// credential reference, agent toolkit binding). With the governance flag
// off nothing builds it.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/foldervisibility"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/epoch"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/projectaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const (
	credentialGrantUniqueIndex   = "credential_use_grants_one_live_per_key"
	credentialGrantApproverCheck = "credential_use_grants_approver_not_proposer"
	credentialGrantColumnList    = `id::text, tenant_project_id, agent_binding_id::text, toolkit_id,
       credential_reference_id::text, target_binding_id::text, capabilities, operation_scopes,
       resource_scopes, obligations, state, epoch, expires_at, proposed_by, COALESCE(approved_by, ''),
       last_transition_by, created_at, updated_at`
)

// CredentialGrants is the PostgreSQL store for grants and the reader of the
// grant, credential reference and toolkit binding records.
type CredentialGrants struct {
	pool  *pgxpool.Pool
	audit securityaudit.Appender
	// notifier signals every replica after an epoch change commits
	// (#1121); nil signals nothing.
	notifier epoch.Notifier
}

// WithNotifier signals n after every committed epoch change.
func (s *CredentialGrants) WithNotifier(n epoch.Notifier) *CredentialGrants {
	s.notifier = n
	return s
}

// NewCredentialGrants builds the store over pool. audit records every grant
// change; a nil audit makes every write fail rather than write unaudited.
func NewCredentialGrants(pool *pgxpool.Pool, audit securityaudit.Appender) *CredentialGrants {
	return &CredentialGrants{pool: pool, audit: audit}
}

var (
	_ credentialgrant.Store                = (*CredentialGrants)(nil)
	_ credentialgrant.GrantReader          = (*CredentialGrants)(nil)
	_ credentialgrant.ReferenceReader      = (*CredentialGrants)(nil)
	_ credentialgrant.ToolkitBindingReader = (*CredentialGrants)(nil)
	_ credentialgrant.TargetReader         = (*CredentialGrants)(nil)
)

func (s *CredentialGrants) readReady() error {
	if s == nil || s.pool == nil {
		return errors.New("credential grants: no database pool")
	}
	return nil
}

func (s *CredentialGrants) writeReady() error {
	if err := s.readReady(); err != nil {
		return err
	}
	if s.audit == nil {
		return errors.New("credential grants: no security audit appender")
	}
	return nil
}

func scanCredentialGrant(row pgx.Row) (credentialgrant.CredentialUseGrant, error) {
	var (
		g            credentialgrant.CredentialUseGrant
		capabilities []string
		state        string
	)
	err := row.Scan(&g.ID, &g.TenantProjectID, &g.AgentBindingID, &g.ToolkitID,
		&g.CredentialReferenceID, &g.TargetBindingID, &capabilities, &g.OperationScopes,
		&g.ResourceScopes, &g.Obligations, &state, &g.Epoch, &g.ExpiresAt, &g.ProposedBy, &g.ApprovedBy,
		&g.LastTransitionBy, &g.CreatedAt, &g.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return credentialgrant.CredentialUseGrant{}, credentialgrant.ErrGrantNotFound
	}
	if err != nil {
		return credentialgrant.CredentialUseGrant{}, fmt.Errorf("scan credential grant: %w", err)
	}
	g.Capabilities = make([]credentialgrant.Capability, len(capabilities))
	for i, c := range capabilities {
		g.Capabilities[i] = credentialgrant.Capability(c)
	}
	g.State = credentialgrant.GrantState(state)
	if g.ExpiresAt != nil {
		utc := g.ExpiresAt.UTC()
		g.ExpiresAt = &utc
	}
	g.CreatedAt, g.UpdatedAt = g.CreatedAt.UTC(), g.UpdatedAt.UTC()
	return g, nil
}

// mapCredentialGrantWriteError turns the constraints the domain also
// enforces into its typed errors.
func mapCredentialGrantWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "23505" && pgErr.ConstraintName == credentialGrantUniqueIndex:
			return credentialgrant.ErrDuplicateGrant
		case pgErr.Code == "23514" && pgErr.ConstraintName == credentialGrantApproverCheck:
			return credentialgrant.ErrSelfApproval
		case pgErr.Code == "23503":
			return credentialgrant.ErrReferenceNotUsable
		}
	}
	return err
}

func capabilityStrings(capabilities []credentialgrant.Capability) []string {
	out := make([]string, len(capabilities))
	for i, c := range capabilities {
		out[i] = string(c)
	}
	return out
}

// CreateGrant inserts a proposed grant and its audit event.
func (s *CredentialGrants) CreateGrant(ctx context.Context, g credentialgrant.CredentialUseGrant) (credentialgrant.CredentialUseGrant, error) {
	if err := s.writeReady(); err != nil {
		return credentialgrant.CredentialUseGrant{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return credentialgrant.CredentialUseGrant{}, fmt.Errorf("begin grant create: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	created, err := scanCredentialGrant(tx.QueryRow(ctx, `
INSERT INTO elitea_identity.credential_use_grants (
    id, tenant_project_id, agent_binding_id, toolkit_id, credential_reference_id, target_binding_id,
    capabilities, operation_scopes, resource_scopes, obligations, state, epoch, expires_at,
    proposed_by, approved_by, last_transition_by, created_at, updated_at)
VALUES ($1::uuid, $2, $3::uuid, $4, $5::uuid, $6::uuid, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
RETURNING `+credentialGrantColumnList,
		g.ID, g.TenantProjectID, g.AgentBindingID, g.ToolkitID, g.CredentialReferenceID, g.TargetBindingID,
		capabilityStrings(g.Capabilities), nonNilStrings(g.OperationScopes), nonNilStrings(g.ResourceScopes),
		nonNilStrings(g.Obligations), string(g.State), g.Epoch, g.ExpiresAt,
		g.ProposedBy, agentIdentityNullableText(g.ApprovedBy), g.LastTransitionBy, g.CreatedAt, g.UpdatedAt))
	if err != nil {
		return credentialgrant.CredentialUseGrant{}, mapCredentialGrantWriteError(err)
	}
	if err := s.audit.Append(ctx, tx, grantAuditEvent(created, nil)); err != nil {
		return credentialgrant.CredentialUseGrant{}, fmt.Errorf("audit grant create: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return credentialgrant.CredentialUseGrant{}, mapCredentialGrantWriteError(fmt.Errorf("commit grant create: %w", err))
	}
	return created, nil
}

// ListGrants lists one tenant project's grants, newest first.
func (s *CredentialGrants) ListGrants(ctx context.Context, filter credentialgrant.ListFilter) ([]credentialgrant.CredentialUseGrant, error) {
	if err := s.readReady(); err != nil {
		return nil, err
	}
	if filter.TenantProjectID <= 0 {
		return nil, fmt.Errorf("%w: tenant_project_id must be positive", credentialgrant.ErrInvalidGrant)
	}
	if filter.AgentBindingID != "" && !agentIdentityIsUUID(filter.AgentBindingID) {
		return []credentialgrant.CredentialUseGrant{}, nil
	}
	limit, offset := agentIdentityListWindow(filter.Limit, filter.Offset)
	rows, err := s.pool.Query(ctx, `
SELECT `+credentialGrantColumnList+`
FROM elitea_identity.credential_use_grants
WHERE tenant_project_id = $1
  AND ($2::text = '' OR agent_binding_id = NULLIF($2, '')::uuid)
  AND ($3::text = '' OR state = $3)
ORDER BY created_at DESC, id DESC
LIMIT $4 OFFSET $5`, filter.TenantProjectID, filter.AgentBindingID, string(filter.State), limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list credential grants: %w", err)
	}
	defer rows.Close()
	out := []credentialgrant.CredentialUseGrant{}
	for rows.Next() {
		g, err := scanCredentialGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list credential grants: %w", err)
	}
	return out, nil
}

// TransitionGrant locks the row of id in tenantProjectID, passes it to apply
// and writes the result. apply must keep the id and bump the epoch by exactly
// one. The UPDATE is guarded by the epoch it read as well as by the lock.
func (s *CredentialGrants) TransitionGrant(
	ctx context.Context,
	tenantProjectID int64,
	id string,
	apply func(credentialgrant.CredentialUseGrant) (credentialgrant.CredentialUseGrant, error),
) (credentialgrant.CredentialUseGrant, error) {
	if err := s.writeReady(); err != nil {
		return credentialgrant.CredentialUseGrant{}, err
	}
	if !agentIdentityIsUUID(id) || tenantProjectID <= 0 {
		return credentialgrant.CredentialUseGrant{}, credentialgrant.ErrGrantNotFound
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return credentialgrant.CredentialUseGrant{}, fmt.Errorf("begin grant transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	current, err := scanCredentialGrant(tx.QueryRow(ctx, `
SELECT `+credentialGrantColumnList+`
FROM elitea_identity.credential_use_grants
WHERE id = $1::uuid AND tenant_project_id = $2 FOR UPDATE`, id, tenantProjectID))
	if err != nil {
		return credentialgrant.CredentialUseGrant{}, err
	}
	next, err := apply(current)
	if err != nil {
		return credentialgrant.CredentialUseGrant{}, err
	}
	if next.ID != current.ID || next.Epoch != current.Epoch+1 {
		return credentialgrant.CredentialUseGrant{}, errors.New("credential grants: a transition must keep the id and bump the epoch by one")
	}
	if strings.TrimSpace(next.LastTransitionBy) == "" {
		return credentialgrant.CredentialUseGrant{}, fmt.Errorf("%w: the actor is unknown", credentialgrant.ErrInvalidGrant)
	}
	written, err := scanCredentialGrant(tx.QueryRow(ctx, `
UPDATE elitea_identity.credential_use_grants
SET state = $3, epoch = $4, approved_by = $5, last_transition_by = $6, updated_at = $7
WHERE id = $1::uuid AND tenant_project_id = $2 AND epoch = $8
RETURNING `+credentialGrantColumnList,
		id, tenantProjectID, string(next.State), next.Epoch, agentIdentityNullableText(next.ApprovedBy),
		next.LastTransitionBy, next.UpdatedAt, current.Epoch))
	if errors.Is(err, credentialgrant.ErrGrantNotFound) {
		return credentialgrant.CredentialUseGrant{}, credentialgrant.ErrInvalidTransition
	}
	if err != nil {
		return credentialgrant.CredentialUseGrant{}, mapCredentialGrantWriteError(err)
	}
	if err := s.audit.Append(ctx, tx, grantAuditEvent(written, &current)); err != nil {
		return credentialgrant.CredentialUseGrant{}, fmt.Errorf("audit grant transition: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return credentialgrant.CredentialUseGrant{}, mapCredentialGrantWriteError(fmt.Errorf("commit grant transition: %w", err))
	}
	if s.notifier != nil {
		s.notifier.Notify(ctx, epoch.Change{Epoch: epoch.SecurityEpoch{Kind: epoch.KindGrant, ID: written.ID, Value: written.Epoch}})
	}
	return written, nil
}

// GrantForKey reads the one non-revoked grant for an exact key.
func (s *CredentialGrants) GrantForKey(ctx context.Context, key credentialgrant.GrantKey) (credentialgrant.CredentialUseGrant, error) {
	if err := s.readReady(); err != nil {
		return credentialgrant.CredentialUseGrant{}, err
	}
	if !agentIdentityIsUUID(key.AgentBindingID) || !agentIdentityIsUUID(key.CredentialReferenceID) ||
		!agentIdentityIsUUID(key.TargetBindingID) {
		return credentialgrant.CredentialUseGrant{}, credentialgrant.ErrGrantNotFound
	}
	return scanCredentialGrant(s.pool.QueryRow(ctx, `
SELECT `+credentialGrantColumnList+`
FROM elitea_identity.credential_use_grants
WHERE tenant_project_id = $1 AND agent_binding_id = $2::uuid AND toolkit_id = $3
  AND credential_reference_id = $4::uuid AND target_binding_id = $5::uuid
  AND state IN ('proposed', 'active', 'suspended')`,
		key.TenantProjectID, key.AgentBindingID, key.ToolkitID, key.CredentialReferenceID, key.TargetBindingID))
}

// CredentialReference reads one credential reference of a tenant project.
func (s *CredentialGrants) CredentialReference(ctx context.Context, tenantProjectID int64, id string) (credentialgrant.CredentialReference, error) {
	if err := s.readReady(); err != nil {
		return credentialgrant.CredentialReference{}, err
	}
	if !agentIdentityIsUUID(id) {
		return credentialgrant.CredentialReference{}, credentialgrant.ErrReferenceNotFound
	}
	var (
		ref          credentialgrant.CredentialReference
		class, state string
	)
	err := s.pool.QueryRow(ctx, `
SELECT id::text, tenant_project_id, class, backend, locator, provider_type, target_binding_id::text, generation, state
FROM elitea_identity.credential_references
WHERE id = $1::uuid AND tenant_project_id = $2`, id, tenantProjectID).Scan(
		&ref.ID, &ref.TenantProjectID, &class, &ref.Backend, &ref.Locator, &ref.ProviderType,
		&ref.TargetBindingID, &ref.Generation, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return credentialgrant.CredentialReference{}, credentialgrant.ErrReferenceNotFound
	}
	if err != nil {
		return credentialgrant.CredentialReference{}, fmt.Errorf("read credential reference: %w", err)
	}
	ref.Class, ref.State = credentialgrant.CredentialClass(class), credentialgrant.ReferenceState(state)
	return ref, nil
}

// TargetBinding reads one target binding of a tenant project.
func (s *CredentialGrants) TargetBinding(ctx context.Context, tenantProjectID int64, id string) (credentialgrant.TargetBinding, error) {
	if err := s.readReady(); err != nil {
		return credentialgrant.TargetBinding{}, err
	}
	if !agentIdentityIsUUID(id) {
		return credentialgrant.TargetBinding{}, credentialgrant.ErrTargetNotFound
	}
	var t credentialgrant.TargetBinding
	err := s.pool.QueryRow(ctx, `
SELECT id::text, tenant_project_id, provider_type, target_identity, audience
FROM elitea_identity.target_bindings
WHERE id = $1::uuid AND tenant_project_id = $2`, id, tenantProjectID).Scan(
		&t.ID, &t.TenantProjectID, &t.ProviderType, &t.TargetIdentity, &t.Audience)
	if errors.Is(err, pgx.ErrNoRows) {
		return credentialgrant.TargetBinding{}, credentialgrant.ErrTargetNotFound
	}
	if err != nil {
		return credentialgrant.TargetBinding{}, fmt.Errorf("read target binding: %w", err)
	}
	return t, nil
}

// AgentToolkitBinding reads the toolkit binding of an agent binding.
func (s *CredentialGrants) AgentToolkitBinding(ctx context.Context, agentBindingID string, toolkitID int64) (credentialgrant.AgentToolkitBinding, error) {
	if err := s.readReady(); err != nil {
		return credentialgrant.AgentToolkitBinding{}, err
	}
	if !agentIdentityIsUUID(agentBindingID) {
		return credentialgrant.AgentToolkitBinding{}, credentialgrant.ErrToolkitNotBound
	}
	var (
		b      credentialgrant.AgentToolkitBinding
		digest []byte
	)
	err := s.pool.QueryRow(ctx, `
SELECT agent_binding_id::text, tenant_project_id, toolkit_id, toolkit_digest
FROM elitea_identity.agent_toolkit_bindings
WHERE agent_binding_id = $1::uuid AND toolkit_id = $2`, agentBindingID, toolkitID).Scan(
		&b.AgentBindingID, &b.TenantProjectID, &b.ToolkitID, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return credentialgrant.AgentToolkitBinding{}, credentialgrant.ErrToolkitNotBound
	}
	if err != nil {
		return credentialgrant.AgentToolkitBinding{}, fmt.Errorf("read agent toolkit binding: %w", err)
	}
	if len(digest) != len(b.ToolkitDigest) {
		return credentialgrant.AgentToolkitBinding{}, errors.New("read agent toolkit binding: stored digest is not 32 bytes")
	}
	copy(b.ToolkitDigest[:], digest)
	return b, nil
}

// grantAuditEvent is the TypeGrantChanged event of one write. previous is
// nil for the proposal. It names references and the epoch only.
func grantAuditEvent(g credentialgrant.CredentialUseGrant, previous *credentialgrant.CredentialUseGrant) securityaudit.SecurityEvent {
	action := "grant.propose"
	reasons := []string{"state." + string(g.State)}
	if previous != nil {
		action = "grant." + grantVerb(g.State)
		reasons = append(reasons, "from."+string(previous.State))
	}
	return securityaudit.SecurityEvent{
		Type:            securityaudit.TypeGrantChanged,
		Time:            g.UpdatedAt,
		TenantProjectID: g.TenantProjectID,
		Action:          action,
		Reasons:         reasons,
		PrincipalRef:    auditUserRef(g.LastTransitionBy),
		AgentBindingID:  g.AgentBindingID,
		GrantRef:        g.ID,
		ResourceType:    "credential_use_grant",
		ResourceID:      g.ID,
		RevocationEpoch: g.Epoch,
	}
}

func grantVerb(state credentialgrant.GrantState) string {
	switch state {
	case credentialgrant.GrantActive:
		return string(credentialgrant.ActionApprove)
	case credentialgrant.GrantSuspended:
		return string(credentialgrant.ActionSuspend)
	case credentialgrant.GrantRevoked:
		return string(credentialgrant.ActionRevoke)
	}
	return "change"
}

// CredentialGrantEntityAccess answers the entity access gate: the user is a
// member of the tenant project (projectaccess.Membership, the predicate the
// project routes use) and the application exists there and is not hidden
// from the user by a folder no_access override.
type CredentialGrantEntityAccess struct{ pool *pgxpool.Pool }

// NewCredentialGrantEntityAccess builds the reader over pool.
func NewCredentialGrantEntityAccess(pool *pgxpool.Pool) *CredentialGrantEntityAccess {
	return &CredentialGrantEntityAccess{pool: pool}
}

// AgentAccessible implements authzcompose.EntityAccess.
func (a *CredentialGrantEntityAccess) AgentAccessible(ctx context.Context, userID, tenantProjectID, applicationID int64) (bool, error) {
	if a == nil || a.pool == nil {
		return false, errors.New("credential grant entity access: no database pool")
	}
	if userID <= 0 || tenantProjectID <= 0 || applicationID <= 0 {
		return false, nil
	}
	schema, err := tenantSchema(strconv.FormatInt(tenantProjectID, 10))
	if err != nil {
		return false, nil
	}
	var member bool
	var applications, folders int32
	err = a.pool.QueryRow(ctx, `
SELECT `+projectaccess.Membership(1, 2)+`,
       CASE WHEN to_regclass($3) IS NULL THEN 0 ELSE 1 END::integer,
       CASE
         WHEN to_regclass($4) IS NOT NULL AND to_regclass($5) IS NOT NULL AND to_regclass($6) IS NOT NULL THEN 1
         WHEN to_regclass($6) IS NULL THEN 0 ELSE -1 END::integer`,
		tenantProjectID, userID, schema+".applications",
		schema+".entity_folders", schema+".social_folder_items", schema+".folder_access_overrides",
	).Scan(&member, &applications, &folders)
	if err != nil {
		return false, fmt.Errorf("credential grant entity access: %w", err)
	}
	if folders < 0 {
		return false, foldervisibility.ErrPartialProjection
	}
	if !member || applications == 0 {
		return false, nil
	}
	query := fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s.applications a WHERE a.id = $1`, schema)
	args := []any{applicationID}
	if folders == 1 {
		query += " AND " + foldervisibility.ExclusionSQL(schema, "a.id", "$2", "$3")
		args = append(args, []string{"agent", "pipeline"}, userID)
	}
	var visible bool
	if err := a.pool.QueryRow(ctx, query+")", args...).Scan(&visible); err != nil {
		return false, fmt.Errorf("credential grant entity access: %w", err)
	}
	return visible, nil
}
