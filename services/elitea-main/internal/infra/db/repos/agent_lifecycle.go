package repos

// Agent lifecycle governance (ADR-0040 decisions 1, 3, 5 and 6, issue
// #1497, shared 0204).
//
// The registry row of an agent is created with the application (the
// application save) and, for an agent that has none, by the first read.
// A publish of an agent whose tier needs approval goes through a change
// request: draft, submit (the publish checks and the release gate start),
// approvals by people other than the proposer, and the final approval
// publishes the candidate through the publish path in the approval's own
// transaction (CandidatePublisher). Every transition appends a security
// audit event (ADR-0039 outbox) in the same transaction, so a transition
// without its evidence does not happen.
//
// Lock order: the registry row, then the change request row. A direct
// publish locks the registry row only. Approvals of one request serialise
// on its row.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// CandidatePublisher is the publish path a change request publishes its
// candidate through (eliteacore.Handler).
type CandidatePublisher interface {
	// PrecheckCandidate runs every publish check at submit, the validation
	// gate included. A failed check is a *agentlifecycle.PublishRefusal.
	PrecheckCandidate(ctx context.Context, candidate agentlifecycle.PublishCandidate) error
	// PublishCandidateTx publishes the candidate in tx and records its
	// release.
	PublishCandidateTx(ctx context.Context, tx pgx.Tx, candidate agentlifecycle.PublishCandidate) (agentlifecycle.PublishedCandidate, error)
	// AnnouncePublished emits the publish event after the commit.
	AnnouncePublished(ctx context.Context, projectID int64, published agentlifecycle.PublishedCandidate)
}

// PolicySource reads the platform's tier table (the agent_lifecycle
// Configuration section).
type PolicySource func(ctx context.Context) (agentlifecycle.Policy, error)

// AgentLifecycle stores the registry, change requests and attestations.
type AgentLifecycle struct {
	pool      *pgxpool.Pool
	audit     securityaudit.Appender
	policy    PolicySource
	gate      agentlifecycle.ReleaseGate
	publisher CandidatePublisher
	now       func() time.Time
	// suspensionResumed is told the id of every certification suspension
	// an approval resumed, after the commit (the kill switch gate's
	// invalidation).
	suspensionResumed func(id int64)
}

// AgentLifecycleOption configures the store.
type AgentLifecycleOption func(*AgentLifecycle)

// WithLifecycleGate sets the release gate. Without it NoGate applies.
func WithLifecycleGate(gate agentlifecycle.ReleaseGate) AgentLifecycleOption {
	return func(s *AgentLifecycle) {
		if gate != nil {
			s.gate = gate
		}
	}
}

// WithLifecyclePublisher sets the publish path of change requests. Without
// it a release change request cannot be approved (503).
func WithLifecyclePublisher(publisher CandidatePublisher) AgentLifecycleOption {
	return func(s *AgentLifecycle) { s.publisher = publisher }
}

// ComposePublisher sets the publish path after construction: the publish
// handler takes this store as its governor, so one of the two is built
// first. It is a composition-time call, before the store serves.
func (s *AgentLifecycle) ComposePublisher(publisher CandidatePublisher) {
	if s != nil {
		s.publisher = publisher
	}
}

// WithLifecyclePolicy sets the tier table source. Without it the store
// reads the agent_lifecycle section on every call.
func WithLifecyclePolicy(policy PolicySource) AgentLifecycleOption {
	return func(s *AgentLifecycle) {
		if policy != nil {
			s.policy = policy
		}
	}
}

// WithSuspensionResumed is told every certification suspension an
// approval resumed.
func WithSuspensionResumed(notify func(id int64)) AgentLifecycleOption {
	return func(s *AgentLifecycle) { s.suspensionResumed = notify }
}

// NewAgentLifecycle builds the store. audit is required: a governed
// transition without its evidence is refused.
func NewAgentLifecycle(pool *pgxpool.Pool, audit securityaudit.Appender, policy PolicySource, options ...AgentLifecycleOption) (*AgentLifecycle, error) {
	if pool == nil {
		return nil, errors.New("agent lifecycle: the database pool is required")
	}
	if audit == nil {
		return nil, errors.New("agent lifecycle: the security audit appender is required")
	}
	if policy == nil {
		return nil, errors.New("agent lifecycle: the tier table source is required")
	}
	s := &AgentLifecycle{pool: pool, audit: audit, policy: policy, gate: agentlifecycle.NoGate{}, now: time.Now}
	for _, option := range options {
		option(s)
	}
	return s, nil
}

// lifecycleQuerier is a pgx.Tx or the pool.
type lifecycleQuerier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const registryColumns = `tenant_project_id, application_id, COALESCE(business_owner_id, 0),
       COALESCE(technical_owner_id, 0), risk_tier, intended_use, out_of_scope_use, data_classes,
       certification_state, COALESCE(certified_release_id, 0), certified_at, recertify_by,
       state_reason, state_changed_at, created_at, updated_at`

func scanRegistry(row pgx.Row) (agentlifecycle.Registry, error) {
	var (
		r           agentlifecycle.Registry
		tier, state string
		dataClasses []string
	)
	if err := row.Scan(&r.ProjectID, &r.ApplicationID, &r.BusinessOwnerID, &r.TechnicalOwnerID, &tier,
		&r.IntendedUse, &r.OutOfScopeUse, &dataClasses, &state, &r.CertifiedReleaseID, &r.CertifiedAt,
		&r.RecertifyBy, &r.StateReason, &r.StateChangedAt, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return agentlifecycle.Registry{}, err
	}
	if dataClasses == nil {
		dataClasses = []string{}
	}
	r.Tier, r.State, r.DataClasses = agentlifecycle.Tier(tier), agentlifecycle.CertificationState(state), dataClasses
	return r, nil
}

// ensureRegistry creates the registry row of an application that has none:
// tier low (a new agent), uncertified, the author of its first version as
// the technical owner. It creates nothing for an application that does not
// exist.
func ensureRegistry(ctx context.Context, q lifecycleQuerier, projectID, applicationID int64) error {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return agentlifecycle.Refuse(agentlifecycle.InvalidCode, "The project id is invalid.")
	}
	if _, err := q.Exec(ctx, fmt.Sprintf(`
INSERT INTO elitea_lifecycle.agent_registry (tenant_project_id, application_id, technical_owner_id)
SELECT $1, application.id,
       (SELECT version.author_id FROM %[1]s.application_versions AS version
        WHERE version.application_id = application.id AND version.author_id > 0
        ORDER BY version.id LIMIT 1)
FROM %[1]s.applications AS application
WHERE application.id = $2
ON CONFLICT (tenant_project_id, application_id) DO NOTHING`, schema), projectID, applicationID); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return agentlifecycle.Refuse(agentlifecycle.NotFoundCode, "The agent does not exist.")
		}
		return fmt.Errorf("agent lifecycle: create the registry row: %w", err)
	}
	return nil
}

// readRegistry reads the row, creating it first when it is missing; lock
// takes the row lock.
func readRegistry(ctx context.Context, q lifecycleQuerier, projectID, applicationID int64, lock bool) (agentlifecycle.Registry, error) {
	if err := ensureRegistry(ctx, q, projectID, applicationID); err != nil {
		return agentlifecycle.Registry{}, err
	}
	statement := `SELECT ` + registryColumns + ` FROM elitea_lifecycle.agent_registry
WHERE tenant_project_id = $1 AND application_id = $2`
	if lock {
		statement += ` FOR UPDATE`
	}
	registry, err := scanRegistry(q.QueryRow(ctx, statement, projectID, applicationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return agentlifecycle.Registry{}, agentlifecycle.Refuse(agentlifecycle.NotFoundCode, "The agent does not exist.")
	}
	if err != nil {
		return agentlifecycle.Registry{}, fmt.Errorf("agent lifecycle: read the registry row: %w", err)
	}
	return registry, nil
}

// EnsureRegistryTx creates the registry row of a new application in the
// application save's transaction (tier low, the default of a new agent).
// A database without the registry table (a schema older than shared 0204)
// skips it: the first read creates the row.
func EnsureRegistryTx(ctx context.Context, tx pgx.Tx, projectID, applicationID, ownerID int64) error {
	savepoint, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("agent lifecycle: create the registry row: %w", err)
	}
	_, err = savepoint.Exec(ctx, `
INSERT INTO elitea_lifecycle.agent_registry (tenant_project_id, application_id, technical_owner_id, business_owner_id)
VALUES ($1, $2, $3, $3)
ON CONFLICT (tenant_project_id, application_id) DO NOTHING`, projectID, applicationID, nullablePositive(ownerID))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "42P01" || pgErr.Code == "3F000") {
		return savepoint.Rollback(ctx)
	}
	if err != nil {
		_ = savepoint.Rollback(ctx)
		return fmt.Errorf("agent lifecycle: create the registry row: %w", err)
	}
	return savepoint.Commit(ctx)
}

func (s *AgentLifecycle) currentPolicy(ctx context.Context) (agentlifecycle.Policy, error) {
	policy, err := s.policy(ctx)
	if err != nil {
		return agentlifecycle.Policy{}, fmt.Errorf("agent lifecycle: read the tier table: %w", err)
	}
	return policy, nil
}

// event builds one audit event of this package.
func lifecycleEvent(projectID int64, action string, actorID int64, resourceType, resourceID string, reasons ...string) securityaudit.SecurityEvent {
	principal := "system:agent_lifecycle"
	if actorID > 0 {
		principal = "user:" + strconv.FormatInt(actorID, 10)
	}
	if reasons == nil {
		reasons = []string{}
	}
	return securityaudit.SecurityEvent{
		Type: securityaudit.TypeAgentLifecycle, TenantProjectID: projectID, Action: action,
		Reasons: reasons, PrincipalRef: principal, ResourceType: resourceType, ResourceID: resourceID,
	}
}

func agentRef(applicationID int64) string { return strconv.FormatInt(applicationID, 10) }

func changeRef(applicationID, changeID int64) string {
	return strconv.FormatInt(applicationID, 10) + ":" + strconv.FormatInt(changeID, 10)
}

func (s *AgentLifecycle) append(ctx context.Context, tx pgx.Tx, event securityaudit.SecurityEvent) error {
	if err := s.audit.Append(ctx, tx, event); err != nil {
		return fmt.Errorf("agent lifecycle: append the audit event %s: %w", event.Action, err)
	}
	return nil
}

func (s *AgentLifecycle) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("agent lifecycle: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("agent lifecycle: commit: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Registry (decision 1)

// Registry reads an agent's registry record, creating it when missing, with
// the agent's name and its active agent suspension.
func (s *AgentLifecycle) Registry(ctx context.Context, projectID, applicationID int64) (agentlifecycle.Registry, error) {
	registry, err := readRegistry(ctx, s.pool, projectID, applicationID, false)
	if err != nil {
		return agentlifecycle.Registry{}, err
	}
	s.decorate(ctx, []*agentlifecycle.Registry{&registry})
	return registry, nil
}

// decorate adds the application names and active agent suspensions. A
// failure leaves the fields empty: they are display fields.
func (s *AgentLifecycle) decorate(ctx context.Context, rows []*agentlifecycle.Registry) {
	byProject := map[int64][]int64{}
	for _, row := range rows {
		byProject[row.ProjectID] = append(byProject[row.ProjectID], row.ApplicationID)
	}
	names := map[[2]int64]string{}
	for projectID, ids := range byProject {
		schema, err := tenantschema.QuoteInt(projectID)
		if err != nil {
			continue
		}
		result, err := s.pool.Query(ctx, fmt.Sprintf(`SELECT id, name FROM %s.applications WHERE id = ANY($1::bigint[])`, schema), ids)
		if err != nil {
			continue
		}
		for result.Next() {
			var (
				id   int64
				name string
			)
			if result.Scan(&id, &name) == nil {
				names[[2]int64{projectID, id}] = name
			}
		}
		result.Close()
	}
	suspensions := map[[2]int64]int64{}
	if result, err := s.pool.Query(ctx, `
SELECT id, project_id::bigint, scope_key::bigint FROM elitea_runtime.execution_suspensions
WHERE scope = 'agent' AND resumed_at IS NULL AND scope_key ~ '^[0-9]{1,18}$'`); err == nil {
		for result.Next() {
			var id, projectID, applicationID int64
			if result.Scan(&id, &projectID, &applicationID) == nil {
				suspensions[[2]int64{projectID, applicationID}] = id
			}
		}
		result.Close()
	}
	for _, row := range rows {
		key := [2]int64{row.ProjectID, row.ApplicationID}
		row.ApplicationName = names[key]
		row.SuspensionID = suspensions[key]
	}
}

// RegistryActor is the caller of a registry edit.
type RegistryActor struct {
	UserID     int64
	Governance bool
}

// UpdateRegistry edits the registry record. Any edit needs an owner or the
// governance role (an agent with no owner yet accepts any caller the route
// admitted); a tier move follows agentlifecycle.CheckTierChange and is
// audited as agent.registry.tier_changed.
func (s *AgentLifecycle) UpdateRegistry(ctx context.Context, projectID, applicationID int64, actor RegistryActor, update agentlifecycle.RegistryUpdate) (agentlifecycle.Registry, error) {
	if update.DataClasses != nil {
		classes, err := agentlifecycle.NormalizeDataClasses(*update.DataClasses)
		if err != nil {
			return agentlifecycle.Registry{}, agentlifecycle.Refuse(agentlifecycle.InvalidCode, err.Error())
		}
		update.DataClasses = &classes
	}
	for _, text := range []*string{update.IntendedUse, update.OutOfScopeUse} {
		if text != nil && !agentlifecycle.TextWithin(*text, agentlifecycle.MaxUseText) {
			return agentlifecycle.Registry{}, agentlifecycle.Refuse(agentlifecycle.InvalidCode,
				fmt.Sprintf("intended_use and out_of_scope_use are at most %d characters.", agentlifecycle.MaxUseText))
		}
	}
	for _, owner := range []*int64{update.BusinessOwnerID, update.TechnicalOwnerID} {
		if owner != nil && *owner < 0 {
			return agentlifecycle.Registry{}, agentlifecycle.Refuse(agentlifecycle.InvalidCode, "An owner id is a user id, or 0 to clear it.")
		}
	}
	var updated agentlifecycle.Registry
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		current, err := readRegistry(ctx, tx, projectID, applicationID, true)
		if err != nil {
			return err
		}
		ownerless := current.BusinessOwnerID == 0 && current.TechnicalOwnerID == 0
		isOwner := ownerless || current.IsOwner(actor.UserID)
		if !isOwner && !actor.Governance {
			return agentlifecycle.Refuse(agentlifecycle.OwnerRequiredCode,
				"Only an owner of the agent or a governance role holder can edit its registry record.")
		}
		tier := current.Tier
		if update.Tier != nil {
			if err := agentlifecycle.CheckTierChange(agentlifecycle.TierChange{
				From: current.Tier, To: *update.Tier,
				CallerIsOwner: current.IsOwner(actor.UserID) || ownerless, CallerGovernance: actor.Governance,
			}); err != nil {
				return err
			}
			tier = *update.Tier
		}
		if err := ownersExist(ctx, tx, update.BusinessOwnerID, update.TechnicalOwnerID); err != nil {
			return err
		}
		pick := func(value *int64, fallback int64) any {
			if value == nil {
				return nullablePositive(fallback)
			}
			return nullablePositive(*value)
		}
		text := func(value *string, fallback string) string {
			if value == nil {
				return fallback
			}
			return strings.TrimSpace(*value)
		}
		classes := current.DataClasses
		if update.DataClasses != nil {
			classes = *update.DataClasses
		}
		updated, err = scanRegistry(tx.QueryRow(ctx, `
UPDATE elitea_lifecycle.agent_registry
SET business_owner_id = $3, technical_owner_id = $4, risk_tier = $5, intended_use = $6,
    out_of_scope_use = $7, data_classes = $8::text[], updated_at = clock_timestamp()
WHERE tenant_project_id = $1 AND application_id = $2
RETURNING `+registryColumns,
			projectID, applicationID, pick(update.BusinessOwnerID, current.BusinessOwnerID),
			pick(update.TechnicalOwnerID, current.TechnicalOwnerID), string(tier),
			text(update.IntendedUse, current.IntendedUse), text(update.OutOfScopeUse, current.OutOfScopeUse), classes))
		if err != nil {
			return fmt.Errorf("agent lifecycle: update the registry row: %w", err)
		}
		if tier != current.Tier {
			return s.append(ctx, tx, lifecycleEvent(projectID, agentlifecycle.ActionRegistryTierChanged, actor.UserID,
				"agent", agentRef(applicationID), "from:"+string(current.Tier), "to:"+string(tier)))
		}
		return nil
	})
	if err != nil {
		return agentlifecycle.Registry{}, err
	}
	s.decorate(ctx, []*agentlifecycle.Registry{&updated})
	return updated, nil
}

// ownersExist refuses an owner id that names no user.
func ownersExist(ctx context.Context, tx pgx.Tx, owners ...*int64) error {
	for _, owner := range owners {
		if owner == nil || *owner == 0 {
			continue
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM public.auth_core__user WHERE id = $1)`, *owner).Scan(&exists); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
				return nil // no user table to check against (a bare test schema)
			}
			return fmt.Errorf("agent lifecycle: read the owner: %w", err)
		}
		if !exists {
			return agentlifecycle.Refuse(agentlifecycle.InvalidCode, fmt.Sprintf("User %d does not exist.", *owner))
		}
	}
	return nil
}

// MarkDue sets a certified agent to due, for an external tool (ADR-0040
// "Delegated outside the platform"). A due, lapsed or suspended agent is
// left as it is; an uncertified agent is refused.
func (s *AgentLifecycle) MarkDue(ctx context.Context, projectID, applicationID, actorID int64, reason string) (agentlifecycle.Registry, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || !agentlifecycle.TextWithin(reason, agentlifecycle.MaxReason) {
		return agentlifecycle.Registry{}, agentlifecycle.Refuse(agentlifecycle.InvalidCode,
			fmt.Sprintf("A reason of 1 to %d characters is required.", agentlifecycle.MaxReason))
	}
	var updated agentlifecycle.Registry
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		current, err := readRegistry(ctx, tx, projectID, applicationID, true)
		if err != nil {
			return err
		}
		switch current.State {
		case agentlifecycle.StateUncertified:
			return agentlifecycle.Refuse(agentlifecycle.NotReleasedCode, "The agent is not certified, so it cannot become due.")
		case agentlifecycle.StateCertified:
		default:
			updated = current
			return nil
		}
		updated, err = scanRegistry(tx.QueryRow(ctx, `
UPDATE elitea_lifecycle.agent_registry
SET certification_state = 'due', state_reason = $3, state_changed_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE tenant_project_id = $1 AND application_id = $2
RETURNING `+registryColumns, projectID, applicationID, reason))
		if err != nil {
			return fmt.Errorf("agent lifecycle: mark the agent due: %w", err)
		}
		return s.append(ctx, tx, lifecycleEvent(projectID, agentlifecycle.ActionCertificationDue, actorID,
			"agent", agentRef(applicationID), agentlifecycle.ReasonExternal))
	})
	if err != nil {
		return agentlifecycle.Registry{}, err
	}
	s.decorate(ctx, []*agentlifecycle.Registry{&updated})
	return updated, nil
}

// ListRegistry is the cross-project registry view (admin console).
func (s *AgentLifecycle) ListRegistry(ctx context.Context, filter agentlifecycle.RegistryFilter) ([]agentlifecycle.Registry, int, error) {
	limit := filter.Limit
	if limit <= 0 || limit > agentlifecycle.MaxRegistryExportRows {
		limit = agentlifecycle.MaxRegistryListRows
	}
	offset := max(filter.Offset, 0)
	rows, err := s.pool.Query(ctx, `
SELECT `+registryColumns+`, count(*) OVER ()
FROM elitea_lifecycle.agent_registry
WHERE ($1 = '' OR risk_tier = $1)
  AND ($2 = '' OR certification_state = $2)
  AND ($3::bigint = 0 OR business_owner_id = $3 OR technical_owner_id = $3)
  AND ($4::bigint = 0 OR tenant_project_id = $4)
ORDER BY tenant_project_id, application_id
LIMIT $5 OFFSET $6`, string(filter.Tier), string(filter.State), filter.OwnerID, filter.ProjectID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("agent lifecycle: list the registry: %w", err)
	}
	defer rows.Close()
	var (
		out   []agentlifecycle.Registry
		total int
	)
	for rows.Next() {
		var (
			r           agentlifecycle.Registry
			tier, state string
			classes     []string
		)
		if err := rows.Scan(&r.ProjectID, &r.ApplicationID, &r.BusinessOwnerID, &r.TechnicalOwnerID, &tier,
			&r.IntendedUse, &r.OutOfScopeUse, &classes, &state, &r.CertifiedReleaseID, &r.CertifiedAt,
			&r.RecertifyBy, &r.StateReason, &r.StateChangedAt, &r.CreatedAt, &r.UpdatedAt, &total); err != nil {
			return nil, 0, fmt.Errorf("agent lifecycle: scan the registry: %w", err)
		}
		if classes == nil {
			classes = []string{}
		}
		r.Tier, r.State, r.DataClasses = agentlifecycle.Tier(tier), agentlifecycle.CertificationState(state), classes
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("agent lifecycle: list the registry: %w", err)
	}
	rows.Close()
	pointers := make([]*agentlifecycle.Registry, len(out))
	for i := range out {
		pointers[i] = &out[i]
	}
	s.decorate(ctx, pointers)
	if out == nil {
		out = []agentlifecycle.Registry{}
		if offset > 0 {
			// The window is past the end: count without it.
			if err := s.pool.QueryRow(ctx, `
SELECT count(*) FROM elitea_lifecycle.agent_registry
WHERE ($1 = '' OR risk_tier = $1) AND ($2 = '' OR certification_state = $2)
  AND ($3::bigint = 0 OR business_owner_id = $3 OR technical_owner_id = $3)
  AND ($4::bigint = 0 OR tenant_project_id = $4)`,
				string(filter.Tier), string(filter.State), filter.OwnerID, filter.ProjectID).Scan(&total); err != nil {
				return nil, 0, fmt.Errorf("agent lifecycle: count the registry: %w", err)
			}
		}
	}
	return out, total, nil
}

// ---------------------------------------------------------------------------
// Direct publish (decision 3, tiers that need no approval)

// CheckDirectPublish refuses a direct publish of an agent whose tier needs
// an approved change request.
func (s *AgentLifecycle) CheckDirectPublish(ctx context.Context, projectID, applicationID int64) error {
	registry, err := readRegistry(ctx, s.pool, projectID, applicationID, false)
	if err != nil {
		return err
	}
	return s.directPublishAllowed(ctx, registry)
}

func (s *AgentLifecycle) directPublishAllowed(ctx context.Context, registry agentlifecycle.Registry) error {
	policy, err := s.currentPolicy(ctx)
	if err != nil {
		return err
	}
	requirement := policy.Requirement(registry.Tier)
	if !requirement.RequiresApproval() {
		return nil
	}
	return agentlifecycle.RefuseWith(agentlifecycle.ChangeRequiredCode,
		fmt.Sprintf("This agent is %s risk. Its release needs a change request with %d approval(s).",
			registry.Tier, requirement.Approvals),
		map[string]any{"risk_tier": string(registry.Tier), "required_approvals": requirement.Approvals})
}

// RecordDirectPublishTx checks the tier under the registry row's lock and
// records the self-approved change request of a direct publish, in the
// publish transaction.
func (s *AgentLifecycle) RecordDirectPublishTx(ctx context.Context, tx pgx.Tx, publish agentlifecycle.DirectPublish) error {
	registry, err := readRegistry(ctx, tx, publish.ProjectID, publish.ApplicationID, true)
	if err != nil {
		return err
	}
	if err := s.directPublishAllowed(ctx, registry); err != nil {
		return err
	}
	if publish.ReleaseID <= 0 {
		// The deployment records no releases: there is nothing to link.
		return nil
	}
	var changeID int64
	if err := tx.QueryRow(ctx, `
INSERT INTO elitea_lifecycle.change_requests
    (tenant_project_id, application_id, kind, state, risk_tier, candidate_version_id, version_name,
     category, summary, proposed_by, self_approved, release_id, decided_by, decided_at, submitted_at, published_at)
VALUES ($1, $2, 'release', 'published', $3, $4, $5, $6, $7, $8, true, $9, $8, clock_timestamp(),
        clock_timestamp(), clock_timestamp())
RETURNING id`,
		publish.ProjectID, publish.ApplicationID, string(registry.Tier), publish.SourceVersionID,
		clampText(publish.VersionName, agentlifecycle.MaxVersionName), clampText(publish.Category, agentlifecycle.MaxCategory),
		"Direct publish of "+string(registry.Tier)+"-tier agent (no approval required).",
		nullablePositive(publish.ActorID), publish.ReleaseID).Scan(&changeID); err != nil {
		return fmt.Errorf("agent lifecycle: record the direct publish: %w", err)
	}
	return s.append(ctx, tx, lifecycleEvent(publish.ProjectID, agentlifecycle.ActionChangePublished, publish.ActorID,
		"agent_change_request", changeRef(publish.ApplicationID, changeID), "self_approved", "tier:"+string(registry.Tier)))
}

func clampText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return text
}

// ---------------------------------------------------------------------------
// Change requests (decision 3)

const changeRequestColumns = `id, tenant_project_id, application_id, kind, state, risk_tier,
       COALESCE(candidate_version_id, 0), COALESCE(candidate_revision_id, 0), COALESCE(version_name, ''),
       category, summary, COALESCE(proposed_by, 0), self_approved, required_approvals,
       required_governance_approvals, COALESCE(gate_id, 0), COALESCE(override_reason, ''),
       COALESCE(overridden_by, 0), overridden_at, COALESCE(current_release_id, 0), COALESCE(release_id, 0),
       COALESCE(decided_by, 0), decided_at, decision_reason, created_at, submitted_at, published_at`

func scanChangeRequest(row pgx.Row) (agentlifecycle.ChangeRequest, error) {
	var (
		c                 agentlifecycle.ChangeRequest
		kind, state, tier string
	)
	if err := row.Scan(&c.ID, &c.ProjectID, &c.ApplicationID, &kind, &state, &tier, &c.CandidateVersionID,
		&c.CandidateRevisionID, &c.VersionName, &c.Category, &c.Summary, &c.ProposedBy, &c.SelfApproved,
		&c.RequiredApprovals, &c.RequiredGovernanceApprovals, &c.GateID, &c.OverrideReason, &c.OverriddenBy,
		&c.OverriddenAt, &c.CurrentReleaseID, &c.ReleaseID, &c.DecidedBy, &c.DecidedAt, &c.DecisionReason,
		&c.CreatedAt, &c.SubmittedAt, &c.PublishedAt); err != nil {
		return agentlifecycle.ChangeRequest{}, err
	}
	c.Kind, c.State, c.Tier = agentlifecycle.ChangeKind(kind), agentlifecycle.ChangeState(state), agentlifecycle.Tier(tier)
	return c, nil
}

func readChangeRequest(ctx context.Context, q lifecycleQuerier, projectID, applicationID, changeID int64, lock bool) (agentlifecycle.ChangeRequest, error) {
	statement := `SELECT ` + changeRequestColumns + ` FROM elitea_lifecycle.change_requests
WHERE tenant_project_id = $1 AND application_id = $2 AND id = $3`
	if lock {
		statement += ` FOR UPDATE`
	}
	change, err := scanChangeRequest(q.QueryRow(ctx, statement, projectID, applicationID, changeID))
	if errors.Is(err, pgx.ErrNoRows) {
		return agentlifecycle.ChangeRequest{}, agentlifecycle.Refuse(agentlifecycle.NotFoundCode, "The change request is not a change request of this agent.")
	}
	if err != nil {
		return agentlifecycle.ChangeRequest{}, fmt.Errorf("agent lifecycle: read the change request: %w", err)
	}
	approvals, err := readApprovals(ctx, q, changeID)
	if err != nil {
		return agentlifecycle.ChangeRequest{}, err
	}
	change.Approvals = approvals
	return change, nil
}

func readApprovals(ctx context.Context, q lifecycleQuerier, changeID int64) ([]agentlifecycle.RecordedApproval, error) {
	rows, err := q.Query(ctx, `
SELECT approver_id, holds_governance_role, note, created_at
FROM elitea_lifecycle.change_request_approvals WHERE change_request_id = $1 ORDER BY id`, changeID)
	if err != nil {
		return nil, fmt.Errorf("agent lifecycle: read the approvals: %w", err)
	}
	defer rows.Close()
	approvals := []agentlifecycle.RecordedApproval{}
	for rows.Next() {
		var approval agentlifecycle.RecordedApproval
		if err := rows.Scan(&approval.ApproverID, &approval.Governance, &approval.Note, &approval.CreatedAt); err != nil {
			return nil, fmt.Errorf("agent lifecycle: scan an approval: %w", err)
		}
		approvals = append(approvals, approval)
	}
	return approvals, rows.Err()
}

// GetChangeRequest reads one change request with its approvals.
func (s *AgentLifecycle) GetChangeRequest(ctx context.Context, projectID, applicationID, changeID int64) (agentlifecycle.ChangeRequest, error) {
	return readChangeRequest(ctx, s.pool, projectID, applicationID, changeID, false)
}

// ListChangeRequests lists an agent's change requests, newest first.
func (s *AgentLifecycle) ListChangeRequests(ctx context.Context, projectID, applicationID int64) ([]agentlifecycle.ChangeRequest, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+changeRequestColumns+` FROM elitea_lifecycle.change_requests
WHERE tenant_project_id = $1 AND application_id = $2 ORDER BY id DESC LIMIT $3`,
		projectID, applicationID, agentlifecycle.MaxRegistryListRows)
	if err != nil {
		return nil, fmt.Errorf("agent lifecycle: list the change requests: %w", err)
	}
	defer rows.Close()
	out := []agentlifecycle.ChangeRequest{}
	for rows.Next() {
		change, err := scanChangeRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("agent lifecycle: scan a change request: %w", err)
		}
		out = append(out, change)
	}
	return out, rows.Err()
}

var versionNamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// headRelease is the agent's published release, 0 when none.
func headRelease(ctx context.Context, q lifecycleQuerier, projectID, applicationID int64) (int64, int64, error) {
	var releaseID, versionID int64
	err := q.QueryRow(ctx, `
SELECT h.release_id, r.application_version_id
FROM elitea_lifecycle.application_release_heads AS h
JOIN elitea_lifecycle.application_releases AS r ON r.id = h.release_id
WHERE h.tenant_project_id = $1 AND h.application_id = $2`, projectID, applicationID).Scan(&releaseID, &versionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("agent lifecycle: read the published release: %w", err)
	}
	return releaseID, versionID, nil
}

// candidateVersion reads the application and status of a version.
func candidateVersion(ctx context.Context, q lifecycleQuerier, projectID, versionID int64) (int64, string, error) {
	schema, err := tenantschema.QuoteInt(projectID)
	if err != nil {
		return 0, "", agentlifecycle.Refuse(agentlifecycle.InvalidCode, "The project id is invalid.")
	}
	var (
		applicationID int64
		status        string
	)
	err = q.QueryRow(ctx, fmt.Sprintf(`SELECT application_id, COALESCE(status, '') FROM %s.application_versions WHERE id = $1`, schema),
		versionID).Scan(&applicationID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", agentlifecycle.Refuse(agentlifecycle.NotFoundCode, "The version is not a version of this agent.")
	}
	if err != nil {
		return 0, "", fmt.Errorf("agent lifecycle: read the candidate version: %w", err)
	}
	return applicationID, status, nil
}

// latestRevision is the newest draft revision of a version, 0 when none.
func latestRevision(ctx context.Context, q lifecycleQuerier, projectID, versionID int64) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `
SELECT COALESCE(max(id), 0) FROM elitea_lifecycle.version_revisions
WHERE tenant_project_id = $1 AND application_version_id = $2`, projectID, versionID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("agent lifecycle: read the candidate revision: %w", err)
	}
	return id, nil
}

// CreateChangeRequest creates a draft. A release request names a draft
// version and the name it is published under; a recertification request
// certifies the agent's published release again.
func (s *AgentLifecycle) CreateChangeRequest(ctx context.Context, request agentlifecycle.NewChangeRequest) (agentlifecycle.ChangeRequest, error) {
	request.Summary = strings.TrimSpace(request.Summary)
	request.VersionName = strings.TrimSpace(request.VersionName)
	if request.ProposedBy <= 0 {
		return agentlifecycle.ChangeRequest{}, agentlifecycle.Refuse(agentlifecycle.InvalidCode, "A change request needs a signed-in user.")
	}
	if request.Summary == "" || !agentlifecycle.TextWithin(request.Summary, agentlifecycle.MaxSummary) {
		return agentlifecycle.ChangeRequest{}, agentlifecycle.Refuse(agentlifecycle.InvalidCode,
			fmt.Sprintf("A summary of 1 to %d characters is required.", agentlifecycle.MaxSummary))
	}
	switch request.Kind {
	case agentlifecycle.KindRelease:
		if request.CandidateVersionID <= 0 || request.VersionName == "" ||
			len(request.VersionName) > agentlifecycle.MaxVersionName || !versionNamePattern.MatchString(request.VersionName) {
			return agentlifecycle.ChangeRequest{}, agentlifecycle.Refuse(agentlifecycle.InvalidCode,
				"A release change request needs version_id and a version_name of letters, digits, '.', '_' and '-'.")
		}
		if !agentlifecycle.TextWithin(request.Category, agentlifecycle.MaxCategory) {
			return agentlifecycle.ChangeRequest{}, agentlifecycle.Refuse(agentlifecycle.InvalidCode, "The category is too long.")
		}
	case agentlifecycle.KindRecertification:
		request.CandidateVersionID, request.VersionName, request.Category = 0, "", ""
	default:
		return agentlifecycle.ChangeRequest{}, agentlifecycle.Refuse(agentlifecycle.InvalidCode, "kind must be release or recertification.")
	}
	var created agentlifecycle.ChangeRequest
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		registry, err := readRegistry(ctx, tx, request.ProjectID, request.ApplicationID, true)
		if err != nil {
			return err
		}
		policy, err := s.currentPolicy(ctx)
		if err != nil {
			return err
		}
		requirement := policy.Requirement(registry.Tier)
		if !requirement.RequiresApproval() {
			return agentlifecycle.RefuseWith(agentlifecycle.ChangeNotRequiredCode,
				fmt.Sprintf("A %s-tier agent needs no approval. Publish it directly.", registry.Tier),
				map[string]any{"risk_tier": string(registry.Tier)})
		}
		var candidateVersionID, currentReleaseID int64
		head, headVersion, err := headRelease(ctx, tx, request.ProjectID, request.ApplicationID)
		if err != nil {
			return err
		}
		if request.Kind == agentlifecycle.KindRelease {
			applicationID, status, err := candidateVersion(ctx, tx, request.ProjectID, request.CandidateVersionID)
			if err != nil {
				return err
			}
			if applicationID != request.ApplicationID {
				return agentlifecycle.Refuse(agentlifecycle.NotFoundCode, "The version is not a version of this agent.")
			}
			if status == "published" || status == "embedded" {
				return agentlifecycle.Refuse(agentlifecycle.InvalidCode, "The version is already published. Choose a draft version.")
			}
			candidateVersionID = request.CandidateVersionID
		} else {
			if head == 0 {
				return agentlifecycle.Refuse(agentlifecycle.NotReleasedCode,
					"The agent has no published release to recertify. Publish it through a release change request.")
			}
			candidateVersionID = headVersion
		}
		currentReleaseID = head
		created, err = scanChangeRequest(tx.QueryRow(ctx, `
INSERT INTO elitea_lifecycle.change_requests
    (tenant_project_id, application_id, kind, state, risk_tier, candidate_version_id, version_name,
     category, summary, proposed_by, required_approvals, required_governance_approvals, current_release_id)
VALUES ($1, $2, $3, 'draft', $4, $5, NULLIF($6, ''), $7, $8, $9, $10, $11, $12)
RETURNING `+changeRequestColumns,
			request.ProjectID, request.ApplicationID, string(request.Kind), string(registry.Tier),
			nullablePositive(candidateVersionID), request.VersionName, request.Category, request.Summary,
			request.ProposedBy, requirement.Approvals, requirement.GovernanceApprovals, nullablePositive(currentReleaseID)))
		if isUniqueViolation(err, "change_requests_one_open") {
			return agentlifecycle.Refuse(agentlifecycle.OpenChangeCode,
				"The agent already has an open change request. Withdraw it or finish it first.")
		}
		if err != nil {
			return fmt.Errorf("agent lifecycle: create the change request: %w", err)
		}
		created.Approvals = []agentlifecycle.RecordedApproval{}
		return nil
	})
	return created, err
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

// SubmitChangeRequest submits the proposer's draft: the publish checks run
// on a release candidate, the release gate starts, and the requirement of
// the agent's tier is recorded.
func (s *AgentLifecycle) SubmitChangeRequest(ctx context.Context, projectID, applicationID, changeID, actorID int64) (agentlifecycle.ChangeRequest, error) {
	draft, err := readChangeRequest(ctx, s.pool, projectID, applicationID, changeID, false)
	if err != nil {
		return agentlifecycle.ChangeRequest{}, err
	}
	if draft.State != agentlifecycle.ChangeDraft {
		return agentlifecycle.ChangeRequest{}, agentlifecycle.Refuse(agentlifecycle.StateConflictCode, "Only a draft change request can be submitted.")
	}
	if actorID != draft.ProposedBy {
		return agentlifecycle.ChangeRequest{}, agentlifecycle.Refuse(agentlifecycle.ProposerOnlyCode, "Only the author of a change request can submit it.")
	}
	if draft.Kind == agentlifecycle.KindRelease {
		if s.publisher == nil {
			return agentlifecycle.ChangeRequest{}, errors.New("agent lifecycle: no publish path is composed")
		}
		if err := s.publisher.PrecheckCandidate(ctx, agentlifecycle.PublishCandidate{
			ProjectID: projectID, ApplicationID: applicationID, VersionID: draft.CandidateVersionID,
			VersionName: draft.VersionName, Category: draft.Category, PublisherID: draft.ProposedBy,
		}); err != nil {
			return agentlifecycle.ChangeRequest{}, err
		}
	}
	head, _, err := headRelease(ctx, s.pool, projectID, applicationID)
	if err != nil {
		return agentlifecycle.ChangeRequest{}, err
	}
	// The gate's runs make their model calls for the user of ctx: the
	// proposer, who submits.
	gateID, err := s.gate.StartGate(ctx, projectID, applicationID, draft.CandidateVersionID, head)
	if err != nil {
		return agentlifecycle.ChangeRequest{}, fmt.Errorf("agent lifecycle: start the release gate: %w", err)
	}
	var submitted agentlifecycle.ChangeRequest
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		registry, err := readRegistry(ctx, tx, projectID, applicationID, true)
		if err != nil {
			return err
		}
		current, err := readChangeRequest(ctx, tx, projectID, applicationID, changeID, true)
		if err != nil {
			return err
		}
		if !agentlifecycle.CanMove(current.State, agentlifecycle.ChangeSubmitted) {
			return agentlifecycle.Refuse(agentlifecycle.StateConflictCode, "Only a draft change request can be submitted.")
		}
		policy, err := s.currentPolicy(ctx)
		if err != nil {
			return err
		}
		requirement := policy.Requirement(registry.Tier)
		if !requirement.RequiresApproval() {
			return agentlifecycle.RefuseWith(agentlifecycle.ChangeNotRequiredCode,
				fmt.Sprintf("A %s-tier agent needs no approval. Withdraw this request and publish directly.", registry.Tier),
				map[string]any{"risk_tier": string(registry.Tier)})
		}
		revision, err := latestRevision(ctx, tx, projectID, current.CandidateVersionID)
		if err != nil {
			return err
		}
		if current.Kind == agentlifecycle.KindRecertification && head == 0 {
			return agentlifecycle.Refuse(agentlifecycle.NotReleasedCode, "The agent has no published release to recertify.")
		}
		submitted, err = scanChangeRequest(tx.QueryRow(ctx, `
UPDATE elitea_lifecycle.change_requests
SET state = 'submitted', submitted_at = clock_timestamp(), updated_at = clock_timestamp(), risk_tier = $4,
    required_approvals = $5, required_governance_approvals = $6, gate_id = $7, candidate_revision_id = $8,
    current_release_id = $9
WHERE tenant_project_id = $1 AND application_id = $2 AND id = $3
RETURNING `+changeRequestColumns,
			projectID, applicationID, changeID, string(registry.Tier), requirement.Approvals,
			requirement.GovernanceApprovals, nullablePositive(gateID), nullablePositive(revision), nullablePositive(head)))
		if err != nil {
			return fmt.Errorf("agent lifecycle: submit the change request: %w", err)
		}
		submitted.Approvals = current.Approvals
		reasons := []string{"kind:" + string(current.Kind), "tier:" + string(registry.Tier)}
		if gateID > 0 {
			reasons = append(reasons, "gate:"+strconv.FormatInt(gateID, 10))
		}
		return s.append(ctx, tx, lifecycleEvent(projectID, agentlifecycle.ActionChangeSubmitted, actorID,
			"agent_change_request", changeRef(applicationID, changeID), reasons...))
	})
	return submitted, err
}

// Approval is one approval asked for.
type Approval struct {
	ProjectID     int64
	ApplicationID int64
	ChangeID      int64
	ApproverID    int64
	// Governance is whether the approver holds the governance role in the
	// project (the handler resolves it).
	Governance bool
	Note       string
}

// ApprovalOutcome is the change request after an approval, and what the
// final approval published.
type ApprovalOutcome struct {
	Change    agentlifecycle.ChangeRequest
	Complete  bool
	Published *agentlifecycle.PublishedCandidate
}

// ApproveChangeRequest records one approval. It refuses while the release
// gate is pending or failed (not overridden), while a mandatory attestation
// is missing, when the candidate was saved again since submission, and when
// the maker-checker rule refuses the approver. The final approval publishes
// a release candidate through the publish path, or certifies the published
// release again, in the same transaction.
func (s *AgentLifecycle) ApproveChangeRequest(ctx context.Context, approval Approval) (ApprovalOutcome, error) {
	if !agentlifecycle.TextWithin(approval.Note, agentlifecycle.MaxApprovalNote) {
		return ApprovalOutcome{}, agentlifecycle.Refuse(agentlifecycle.InvalidCode,
			fmt.Sprintf("The note is at most %d characters.", agentlifecycle.MaxApprovalNote))
	}
	snapshot, err := readChangeRequest(ctx, s.pool, approval.ProjectID, approval.ApplicationID, approval.ChangeID, false)
	if err != nil {
		return ApprovalOutcome{}, err
	}
	if snapshot.State != agentlifecycle.ChangeSubmitted {
		return ApprovalOutcome{}, agentlifecycle.Refuse(agentlifecycle.StateConflictCode, "Only a submitted change request can be approved.")
	}
	if approval.ApproverID == snapshot.ProposedBy {
		// Refused before the gate is read: the proposer learns the rule,
		// not the gate's state.
		return ApprovalOutcome{}, agentlifecycle.Refuse(agentlifecycle.SelfApprovalCode, "The author of a change request cannot approve it.")
	}
	// The gate runs outside this service: read it before the transaction.
	// A passed gate does not fail later, and an override only releases.
	if snapshot.GateID > 0 && snapshot.OverriddenBy == 0 {
		result, err := s.gate.GateResult(ctx, approval.ProjectID, snapshot.GateID)
		if err != nil {
			return ApprovalOutcome{}, fmt.Errorf("agent lifecycle: read the release gate: %w", err)
		}
		if err := agentlifecycle.CheckGate(snapshot.GateID, result, result.Overridden); err != nil {
			return ApprovalOutcome{}, err
		}
	}
	var (
		outcome ApprovalOutcome
		resumed []int64
	)
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		registry, err := readRegistry(ctx, tx, approval.ProjectID, approval.ApplicationID, true)
		if err != nil {
			return err
		}
		change, err := readChangeRequest(ctx, tx, approval.ProjectID, approval.ApplicationID, approval.ChangeID, true)
		if err != nil {
			return err
		}
		if change.State != agentlifecycle.ChangeSubmitted {
			return agentlifecycle.Refuse(agentlifecycle.StateConflictCode, "Only a submitted change request can be approved.")
		}
		if err := s.checkFresh(ctx, tx, change); err != nil {
			return err
		}
		policy, err := s.currentPolicy(ctx)
		if err != nil {
			return err
		}
		// The live tier and table: a tier raised after submission asks for
		// more, never less.
		requirement := policy.Requirement(registry.Tier)
		requirement.Approvals = max(requirement.Approvals, change.RequiredApprovals)
		requirement.GovernanceApprovals = max(requirement.GovernanceApprovals, change.RequiredGovernanceApprovals)
		if missing, err := s.missingAttestations(ctx, tx, change, requirement.Attestations); err != nil {
			return err
		} else if len(missing) > 0 {
			kinds := make([]string, len(missing))
			for i, kind := range missing {
				kinds[i] = string(kind)
			}
			return agentlifecycle.RefuseWith(agentlifecycle.AttestationMissingCode,
				"A mandatory attestation is missing, expired or revoked: "+strings.Join(kinds, ", ")+".",
				map[string]any{"missing": kinds})
		}
		existing := make([]agentlifecycle.Approval, len(change.Approvals))
		for i, recorded := range change.Approvals {
			existing[i] = agentlifecycle.Approval{ApproverID: recorded.ApproverID, Governance: recorded.Governance}
		}
		complete, err := agentlifecycle.EvaluateApproval(agentlifecycle.ApprovalRequest{
			Requirement: requirement, ProposerID: change.ProposedBy, ApproverID: approval.ApproverID,
			ApproverGovernance: approval.Governance, Existing: existing,
		})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO elitea_lifecycle.change_request_approvals
    (change_request_id, proposed_by, approver_id, holds_governance_role, note)
VALUES ($1, $2, $3, $4, $5)`, change.ID, change.ProposedBy, approval.ApproverID, approval.Governance,
			strings.TrimSpace(approval.Note)); err != nil {
			return fmt.Errorf("agent lifecycle: record the approval: %w", err)
		}
		reasons := []string{"tier:" + string(registry.Tier)}
		if approval.Governance {
			reasons = append(reasons, "governance_role")
		}
		if err := s.append(ctx, tx, lifecycleEvent(approval.ProjectID, agentlifecycle.ActionChangeApproved, approval.ApproverID,
			"agent_change_request", changeRef(approval.ApplicationID, change.ID), reasons...)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.change_requests
SET required_approvals = $2, required_governance_approvals = $3, updated_at = clock_timestamp()
WHERE id = $1`, change.ID, requirement.Approvals, requirement.GovernanceApprovals); err != nil {
			return fmt.Errorf("agent lifecycle: record the requirement: %w", err)
		}
		outcome.Complete = complete
		if complete {
			resumed, err = s.finalize(ctx, tx, change, registry, requirement, approval.ApproverID, &outcome)
			if err != nil {
				return err
			}
		}
		outcome.Change, err = readChangeRequest(ctx, tx, approval.ProjectID, approval.ApplicationID, change.ID, false)
		return err
	})
	if err != nil {
		return ApprovalOutcome{}, err
	}
	if outcome.Published != nil && s.publisher != nil {
		s.publisher.AnnouncePublished(ctx, approval.ProjectID, *outcome.Published)
	}
	if s.suspensionResumed != nil {
		for _, id := range resumed {
			s.suspensionResumed(id)
		}
	}
	return outcome, nil
}

// checkFresh refuses an approval of a candidate that changed since it was
// submitted: a release candidate saved again or published meanwhile, or a
// recertification whose published release moved.
func (s *AgentLifecycle) checkFresh(ctx context.Context, tx pgx.Tx, change agentlifecycle.ChangeRequest) error {
	stale := agentlifecycle.Refuse(agentlifecycle.StaleCode,
		"The agent changed after this change request was submitted. Withdraw it and submit a new one.")
	head, _, err := headRelease(ctx, tx, change.ProjectID, change.ApplicationID)
	if err != nil {
		return err
	}
	if change.Kind == agentlifecycle.KindRecertification {
		if head == 0 || head != change.CurrentReleaseID {
			return stale
		}
		return nil
	}
	revision, err := latestRevision(ctx, tx, change.ProjectID, change.CandidateVersionID)
	if err != nil {
		return err
	}
	if revision != change.CandidateRevisionID {
		return stale
	}
	_, status, err := candidateVersion(ctx, tx, change.ProjectID, change.CandidateVersionID)
	if err != nil {
		return err
	}
	if status == "published" || status == "embedded" {
		return stale
	}
	return nil
}

// missingAttestations lists the required kinds with no valid attestation on
// the agent, or, for a recertification, on the release it certifies.
func (s *AgentLifecycle) missingAttestations(ctx context.Context, tx pgx.Tx, change agentlifecycle.ChangeRequest, required []agentlifecycle.AttestationKind) ([]agentlifecycle.AttestationKind, error) {
	if len(required) == 0 {
		return nil, nil
	}
	releaseRef := ""
	if change.Kind == agentlifecycle.KindRecertification && change.CurrentReleaseID > 0 {
		releaseRef = strconv.FormatInt(change.CurrentReleaseID, 10)
	}
	rows, err := tx.Query(ctx, `
SELECT DISTINCT kind FROM elitea_lifecycle.attestations
WHERE tenant_project_id = $1 AND revoked_at IS NULL
  AND (valid_until IS NULL OR valid_until > $4)
  AND ((subject_type = 'agent' AND subject_id = $2) OR (subject_type = 'release' AND subject_id = $3 AND $3 <> ''))`,
		change.ProjectID, agentRef(change.ApplicationID), releaseRef, s.now())
	if err != nil {
		return nil, fmt.Errorf("agent lifecycle: read the attestations: %w", err)
	}
	defer rows.Close()
	var present []agentlifecycle.AttestationKind
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			return nil, fmt.Errorf("agent lifecycle: scan an attestation: %w", err)
		}
		present = append(present, agentlifecycle.AttestationKind(kind))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return agentlifecycle.MissingAttestations(required, present), nil
}

// finalize publishes or certifies after the final approval, in tx.
func (s *AgentLifecycle) finalize(ctx context.Context, tx pgx.Tx, change agentlifecycle.ChangeRequest, registry agentlifecycle.Registry, requirement agentlifecycle.Requirement, approverID int64, outcome *ApprovalOutcome) ([]int64, error) {
	releaseID := change.CurrentReleaseID
	state := agentlifecycle.ChangeApproved
	if change.Kind == agentlifecycle.KindRelease {
		if s.publisher == nil {
			return nil, errors.New("agent lifecycle: no publish path is composed")
		}
		published, err := s.publisher.PublishCandidateTx(ctx, tx, agentlifecycle.PublishCandidate{
			ProjectID: change.ProjectID, ApplicationID: change.ApplicationID, VersionID: change.CandidateVersionID,
			VersionName: change.VersionName, Category: change.Category, PublisherID: change.ProposedBy,
		})
		if err != nil {
			return nil, err
		}
		outcome.Published = &published
		releaseID, state = published.ReleaseID, agentlifecycle.ChangePublished
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.change_requests
SET state = $2, release_id = $3, decided_by = $4, decided_at = clock_timestamp(), updated_at = clock_timestamp(),
    published_at = CASE WHEN $2 = 'published' THEN clock_timestamp() ELSE published_at END
WHERE id = $1`, change.ID, string(state), releaseID, approverID); err != nil {
		return nil, fmt.Errorf("agent lifecycle: close the change request: %w", err)
	}
	if state == agentlifecycle.ChangePublished {
		if err := s.append(ctx, tx, lifecycleEvent(change.ProjectID, agentlifecycle.ActionChangePublished, approverID,
			"agent_change_request", changeRef(change.ApplicationID, change.ID),
			"release:"+strconv.FormatInt(releaseID, 10), "tier:"+string(registry.Tier))); err != nil {
			return nil, err
		}
	}
	now := s.now()
	if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.agent_registry
SET certification_state = 'certified', certified_release_id = $3, certified_at = $4, recertify_by = $5,
    state_reason = '', state_changed_at = clock_timestamp(), drift_checked_at = NULL, updated_at = clock_timestamp()
WHERE tenant_project_id = $1 AND application_id = $2`,
		change.ProjectID, change.ApplicationID, releaseID, now, requirement.RecertifyBy(now)); err != nil {
		return nil, fmt.Errorf("agent lifecycle: certify the agent: %w", err)
	}
	if err := s.append(ctx, tx, lifecycleEvent(change.ProjectID, agentlifecycle.ActionCertificationCert, approverID,
		"agent", agentRef(change.ApplicationID), "release:"+strconv.FormatInt(releaseID, 10),
		"change_request:"+strconv.FormatInt(change.ID, 10))); err != nil {
		return nil, err
	}
	// A recertified agent runs again: resume the suspension its lapse wrote.
	rows, err := tx.Query(ctx, `
UPDATE elitea_runtime.execution_suspensions
SET resumed_at = greatest(clock_timestamp(), created_at), resumed_by = $3, resume_reason = $4
WHERE scope = 'agent' AND project_id = $1 AND scope_key = $2 AND source = 'certification' AND resumed_at IS NULL
RETURNING id`, change.ProjectID, agentRef(change.ApplicationID), approverID,
		"Recertified by change request "+strconv.FormatInt(change.ID, 10)+".")
	if err != nil {
		return nil, fmt.Errorf("agent lifecycle: resume the certification suspension: %w", err)
	}
	defer rows.Close()
	var resumed []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		resumed = append(resumed, id)
	}
	return resumed, rows.Err()
}

// RejectChangeRequest rejects a submitted change request. The proposer
// withdraws instead.
func (s *AgentLifecycle) RejectChangeRequest(ctx context.Context, projectID, applicationID, changeID, actorID int64, reason string) (agentlifecycle.ChangeRequest, error) {
	return s.close(ctx, projectID, applicationID, changeID, actorID, reason, agentlifecycle.ChangeRejected, false)
}

// WithdrawChangeRequest withdraws a draft or submitted change request: the
// proposer, or a governance role holder.
func (s *AgentLifecycle) WithdrawChangeRequest(ctx context.Context, projectID, applicationID, changeID, actorID int64, governance bool, reason string) (agentlifecycle.ChangeRequest, error) {
	return s.close(ctx, projectID, applicationID, changeID, actorID, reason, agentlifecycle.ChangeWithdrawn, governance)
}

func (s *AgentLifecycle) close(ctx context.Context, projectID, applicationID, changeID, actorID int64, reason string, to agentlifecycle.ChangeState, governance bool) (agentlifecycle.ChangeRequest, error) {
	reason = strings.TrimSpace(reason)
	if !agentlifecycle.TextWithin(reason, agentlifecycle.MaxReason) {
		return agentlifecycle.ChangeRequest{}, agentlifecycle.Refuse(agentlifecycle.InvalidCode,
			fmt.Sprintf("The reason is at most %d characters.", agentlifecycle.MaxReason))
	}
	if to == agentlifecycle.ChangeRejected && reason == "" {
		return agentlifecycle.ChangeRequest{}, agentlifecycle.Refuse(agentlifecycle.InvalidCode, "A rejection needs a reason.")
	}
	var closed agentlifecycle.ChangeRequest
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		change, err := readChangeRequest(ctx, tx, projectID, applicationID, changeID, true)
		if err != nil {
			return err
		}
		if !agentlifecycle.CanMove(change.State, to) {
			return agentlifecycle.Refuse(agentlifecycle.StateConflictCode,
				fmt.Sprintf("A %s change request cannot be %s.", change.State, to))
		}
		action := agentlifecycle.ActionChangeWithdrawn
		if to == agentlifecycle.ChangeRejected {
			action = agentlifecycle.ActionChangeRejected
			if actorID == change.ProposedBy {
				return agentlifecycle.Refuse(agentlifecycle.SelfApprovalCode,
					"The author of a change request cannot reject it. Withdraw it instead.")
			}
		} else if actorID != change.ProposedBy && !governance {
			return agentlifecycle.Refuse(agentlifecycle.ProposerOnlyCode,
				"Only the author or a governance role holder can withdraw a change request.")
		}
		closed, err = scanChangeRequest(tx.QueryRow(ctx, `
UPDATE elitea_lifecycle.change_requests
SET state = $4, decided_by = $5, decided_at = clock_timestamp(), decision_reason = $6, updated_at = clock_timestamp()
WHERE tenant_project_id = $1 AND application_id = $2 AND id = $3
RETURNING `+changeRequestColumns, projectID, applicationID, changeID, string(to), actorID, reason))
		if err != nil {
			return fmt.Errorf("agent lifecycle: close the change request: %w", err)
		}
		closed.Approvals = change.Approvals
		return s.append(ctx, tx, lifecycleEvent(projectID, action, actorID,
			"agent_change_request", changeRef(applicationID, changeID), "from:"+string(change.State)))
	})
	return closed, err
}

// OverrideGate releases a failed or stuck release gate: a governance role
// holder who is not the proposer, with a reason. Approval still needs the
// tier's approvers.
func (s *AgentLifecycle) OverrideGate(ctx context.Context, projectID, applicationID, changeID, actorID int64, governance bool, reason string) (agentlifecycle.ChangeRequest, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || !agentlifecycle.TextWithin(reason, agentlifecycle.MaxOverrideReason) {
		return agentlifecycle.ChangeRequest{}, agentlifecycle.Refuse(agentlifecycle.InvalidCode,
			fmt.Sprintf("An override needs a reason of 1 to %d characters.", agentlifecycle.MaxOverrideReason))
	}
	if !governance {
		return agentlifecycle.ChangeRequest{}, agentlifecycle.Refuse(agentlifecycle.GovernanceRequiredCode,
			"Only a governance role holder ("+agentlifecycle.GovernancePermission+") can override a release gate.")
	}
	snapshot, err := readChangeRequest(ctx, s.pool, projectID, applicationID, changeID, false)
	if err != nil {
		return agentlifecycle.ChangeRequest{}, err
	}
	if snapshot.State != agentlifecycle.ChangeSubmitted {
		return agentlifecycle.ChangeRequest{}, agentlifecycle.Refuse(agentlifecycle.StateConflictCode, "Only a submitted change request has a gate to override.")
	}
	if actorID == snapshot.ProposedBy {
		return agentlifecycle.ChangeRequest{}, agentlifecycle.Refuse(agentlifecycle.SelfApprovalCode, "The author of a change request cannot override its gate.")
	}
	if snapshot.GateID == 0 || snapshot.OverriddenBy != 0 {
		return agentlifecycle.ChangeRequest{}, agentlifecycle.Refuse(agentlifecycle.GateNotOverridableCode, "The change request has no failed gate to override.")
	}
	result, err := s.gate.GateResult(ctx, projectID, snapshot.GateID)
	if err != nil {
		return agentlifecycle.ChangeRequest{}, fmt.Errorf("agent lifecycle: read the release gate: %w", err)
	}
	if result.State != agentlifecycle.GateFailed {
		return agentlifecycle.ChangeRequest{}, agentlifecycle.Refuse(agentlifecycle.GateNotOverridableCode,
			"Only a failed release gate can be overridden. This gate is "+string(result.State)+".")
	}
	// The gate may already hold an override (made through the gate's own
	// route); the change request records it all the same.
	if !result.Overridden {
		if err := s.gate.Override(ctx, projectID, snapshot.GateID, actorID, reason); err != nil {
			return agentlifecycle.ChangeRequest{}, fmt.Errorf("agent lifecycle: override the release gate: %w", err)
		}
	}
	var overridden agentlifecycle.ChangeRequest
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		change, err := readChangeRequest(ctx, tx, projectID, applicationID, changeID, true)
		if err != nil {
			return err
		}
		if change.State != agentlifecycle.ChangeSubmitted || change.OverriddenBy != 0 {
			return agentlifecycle.Refuse(agentlifecycle.GateNotOverridableCode, "The change request has no failed gate to override.")
		}
		overridden, err = scanChangeRequest(tx.QueryRow(ctx, `
UPDATE elitea_lifecycle.change_requests
SET override_reason = $4, overridden_by = $5, overridden_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE tenant_project_id = $1 AND application_id = $2 AND id = $3
RETURNING `+changeRequestColumns, projectID, applicationID, changeID, reason, actorID))
		if err != nil {
			return fmt.Errorf("agent lifecycle: record the gate override: %w", err)
		}
		overridden.Approvals = change.Approvals
		return s.append(ctx, tx, lifecycleEvent(projectID, agentlifecycle.ActionChangeGateOverride, actorID,
			"agent_change_request", changeRef(applicationID, changeID),
			"gate:"+strconv.FormatInt(change.GateID, 10), "gate_state:"+string(result.State)))
	})
	return overridden, err
}

// GateResult reads the release gate of a change request (display).
func (s *AgentLifecycle) GateResult(ctx context.Context, projectID, gateID int64) (agentlifecycle.GateResult, error) {
	if gateID <= 0 {
		return agentlifecycle.GateResult{State: agentlifecycle.GatePassed}, nil
	}
	return s.gate.GateResult(ctx, projectID, gateID)
}

// ---------------------------------------------------------------------------
// Attestations (decision 6)

const attestationColumns = `id, tenant_project_id, kind, custom_kind, subject_type, subject_id, attester,
       COALESCE(attester_user_id, 0), valid_until, evidence_url, note, recorded_by, created_at, revoked_at,
       COALESCE(revoked_by, 0), COALESCE(revoke_reason, '')`

func scanAttestation(row pgx.Row) (agentlifecycle.Attestation, error) {
	var (
		a                 agentlifecycle.Attestation
		kind, subjectType string
	)
	if err := row.Scan(&a.ID, &a.ProjectID, &kind, &a.CustomKind, &subjectType, &a.SubjectID, &a.Attester,
		&a.AttesterUserID, &a.ValidUntil, &a.EvidenceURL, &a.Note, &a.RecordedBy, &a.CreatedAt, &a.RevokedAt,
		&a.RevokedBy, &a.RevokeReason); err != nil {
		return agentlifecycle.Attestation{}, err
	}
	a.Kind, a.SubjectType = agentlifecycle.AttestationKind(kind), agentlifecycle.SubjectType(subjectType)
	return a, nil
}

// RecordAttestation records an attestation. The caller validated the
// evidence URL's scheme; the subject must exist in the project.
func (s *AgentLifecycle) RecordAttestation(ctx context.Context, a agentlifecycle.Attestation) (agentlifecycle.Attestation, error) {
	if !a.Kind.Valid() || !a.SubjectType.Valid() || !agentlifecycle.ValidSubjectID(a.SubjectID) {
		return agentlifecycle.Attestation{}, agentlifecycle.Refuse(agentlifecycle.InvalidCode, "kind, subject_type and subject_id are required and must be known values.")
	}
	if (a.Kind == agentlifecycle.AttestationCustom) != (a.CustomKind != "") ||
		(a.CustomKind != "" && !agentlifecycle.ValidCustomKind(a.CustomKind)) {
		return agentlifecycle.Attestation{}, agentlifecycle.Refuse(agentlifecycle.InvalidCode,
			"custom_kind names a custom attestation (a short identifier) and is empty for every other kind.")
	}
	a.Attester = strings.TrimSpace(a.Attester)
	if a.Attester == "" || !agentlifecycle.TextWithin(a.Attester, agentlifecycle.MaxAttester) ||
		!agentlifecycle.TextWithin(a.Note, agentlifecycle.MaxNote) || len(a.EvidenceURL) > agentlifecycle.MaxEvidenceURL {
		return agentlifecycle.Attestation{}, agentlifecycle.Refuse(agentlifecycle.InvalidCode, "attester is required; attester, note and evidence_url have length limits.")
	}
	if a.RecordedBy <= 0 {
		return agentlifecycle.Attestation{}, agentlifecycle.Refuse(agentlifecycle.InvalidCode, "An attestation needs a signed-in caller.")
	}
	var recorded agentlifecycle.Attestation
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := subjectExists(ctx, tx, a.ProjectID, a.SubjectType, a.SubjectID); err != nil {
			return err
		}
		var err error
		recorded, err = scanAttestation(tx.QueryRow(ctx, `
INSERT INTO elitea_lifecycle.attestations
    (tenant_project_id, kind, custom_kind, subject_type, subject_id, attester, attester_user_id, valid_until,
     evidence_url, note, recorded_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING `+attestationColumns, a.ProjectID, string(a.Kind), a.CustomKind, string(a.SubjectType), a.SubjectID,
			a.Attester, nullablePositive(a.AttesterUserID), a.ValidUntil, a.EvidenceURL, a.Note, a.RecordedBy))
		if err != nil {
			return fmt.Errorf("agent lifecycle: record the attestation: %w", err)
		}
		return s.append(ctx, tx, lifecycleEvent(a.ProjectID, agentlifecycle.ActionAttestationRecorded, a.RecordedBy,
			"agent_attestation", strconv.FormatInt(recorded.ID, 10), "kind:"+string(a.Kind),
			"subject:"+string(a.SubjectType)+":"+a.SubjectID))
	})
	return recorded, err
}

// subjectExists refuses an attestation about something the project does
// not have. A toolkit or an index is not checked: their stores differ by
// type and the id is a reference the GRC tool owns.
func subjectExists(ctx context.Context, tx pgx.Tx, projectID int64, subjectType agentlifecycle.SubjectType, subjectID string) error {
	id, err := strconv.ParseInt(subjectID, 10, 64)
	switch subjectType {
	case agentlifecycle.SubjectAgent:
		if err != nil || id <= 0 {
			return agentlifecycle.Refuse(agentlifecycle.InvalidCode, "An agent subject is the agent's numeric id.")
		}
		if _, err := readRegistry(ctx, tx, projectID, id, false); err != nil {
			return err
		}
	case agentlifecycle.SubjectRelease:
		if err != nil || id <= 0 {
			return agentlifecycle.Refuse(agentlifecycle.InvalidCode, "A release subject is the release's numeric id.")
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM elitea_lifecycle.application_releases
WHERE tenant_project_id = $1 AND id = $2)`, projectID, id).Scan(&exists); err != nil {
			return fmt.Errorf("agent lifecycle: read the release: %w", err)
		}
		if !exists {
			return agentlifecycle.Refuse(agentlifecycle.NotFoundCode, "The release is not a release of this project.")
		}
	}
	return nil
}

// ListAttestations lists the attestations of the project, newest first,
// optionally of one subject.
func (s *AgentLifecycle) ListAttestations(ctx context.Context, projectID int64, subjectType agentlifecycle.SubjectType, subjectID string) ([]agentlifecycle.Attestation, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+attestationColumns+` FROM elitea_lifecycle.attestations
WHERE tenant_project_id = $1 AND ($2 = '' OR subject_type = $2) AND ($3 = '' OR subject_id = $3)
ORDER BY id DESC LIMIT $4`, projectID, string(subjectType), subjectID, agentlifecycle.MaxRegistryListRows)
	if err != nil {
		return nil, fmt.Errorf("agent lifecycle: list the attestations: %w", err)
	}
	defer rows.Close()
	out := []agentlifecycle.Attestation{}
	for rows.Next() {
		a, err := scanAttestation(rows)
		if err != nil {
			return nil, fmt.Errorf("agent lifecycle: scan an attestation: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RevokeAttestation stamps a revocation. A revoked attestation no longer
// satisfies a mandatory kind.
func (s *AgentLifecycle) RevokeAttestation(ctx context.Context, projectID, attestationID, actorID int64, reason string) (agentlifecycle.Attestation, error) {
	reason = strings.TrimSpace(reason)
	if !agentlifecycle.TextWithin(reason, agentlifecycle.MaxReason) {
		return agentlifecycle.Attestation{}, agentlifecycle.Refuse(agentlifecycle.InvalidCode,
			fmt.Sprintf("The reason is at most %d characters.", agentlifecycle.MaxReason))
	}
	var revoked agentlifecycle.Attestation
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		revoked, err = scanAttestation(tx.QueryRow(ctx, `
UPDATE elitea_lifecycle.attestations
SET revoked_at = clock_timestamp(), revoked_by = $3, revoke_reason = $4
WHERE tenant_project_id = $1 AND id = $2 AND revoked_at IS NULL
RETURNING `+attestationColumns, projectID, attestationID, actorID, reason))
		if errors.Is(err, pgx.ErrNoRows) {
			return agentlifecycle.Refuse(agentlifecycle.NotFoundCode, "No active attestation has this id in the project.")
		}
		if err != nil {
			return fmt.Errorf("agent lifecycle: revoke the attestation: %w", err)
		}
		return s.append(ctx, tx, lifecycleEvent(projectID, agentlifecycle.ActionAttestationRevoked, actorID,
			"agent_attestation", strconv.FormatInt(attestationID, 10), "kind:"+string(revoked.Kind)))
	})
	return revoked, err
}
