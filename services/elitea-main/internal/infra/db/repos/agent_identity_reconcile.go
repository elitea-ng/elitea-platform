package repos

// Agent identity reconciliation (shared 0191, issue #1154): the per-provider
// pass lease and outage health, and the drift findings. A drift transition
// runs through ExternalAgentIdentities' locked transition (its invariants
// and its TypeExternalIdentityChanged event) and writes the finding and the
// drift event in the same transaction.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/reconcile"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const driftFindingColumnList = `id::text, kind, provider, provider_tenant, external_id,
       coalesce(external_agent_identity_id::text, ''), tenant_project_id, application_id, revocation_epoch,
       detected_at, last_seen_at`

const providerHealthColumnList = `provider, consecutive_failures, admission_denied, last_pass_at, last_success_at`

// AgentIdentityReconcile is the PostgreSQL reconcile.Store and the
// agentidentity.ProviderAdmission the broker reads.
type AgentIdentityReconcile struct {
	pool       *pgxpool.Pool
	audit      securityaudit.Appender
	identities *ExternalAgentIdentities
}

// NewAgentIdentityReconcile builds the store. A nil audit makes every write
// fail rather than write unaudited.
func NewAgentIdentityReconcile(pool *pgxpool.Pool, audit securityaudit.Appender) *AgentIdentityReconcile {
	return &AgentIdentityReconcile{pool: pool, audit: audit, identities: NewExternalAgentIdentities(pool, audit)}
}

var (
	_ reconcile.Store                 = (*AgentIdentityReconcile)(nil)
	_ agentidentity.ProviderAdmission = (*AgentIdentityReconcile)(nil)
)

func (s *AgentIdentityReconcile) ready() error {
	if s == nil || s.pool == nil {
		return errors.New("agent identity reconcile: no database pool")
	}
	if s.audit == nil {
		return errors.New("agent identity reconcile: no security audit appender")
	}
	return nil
}

// ClaimPass takes the provider's pass lease when it expired. The
// conditional upsert is atomic: of two replicas at most one gets a row back.
// The database clock decides, so replica clock skew does not matter.
func (s *AgentIdentityReconcile) ClaimPass(ctx context.Context, provider, holder string, lease time.Duration) (bool, error) {
	if err := s.ready(); err != nil {
		return false, err
	}
	var claimed bool
	err := s.pool.QueryRow(ctx, `
INSERT INTO elitea_identity.agent_identity_provider_health AS h (provider, lease_holder, lease_until)
VALUES ($1, $2, clock_timestamp() + make_interval(secs => $3))
ON CONFLICT (provider) DO UPDATE
SET lease_holder = EXCLUDED.lease_holder, lease_until = EXCLUDED.lease_until, updated_at = clock_timestamp()
WHERE h.lease_until <= clock_timestamp()
RETURNING true`, provider, holder, lease.Seconds()).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim reconcile pass: %w", err)
	}
	return claimed, nil
}

// Records answers every record of the provider.
func (s *AgentIdentityReconcile) Records(ctx context.Context, provider string) ([]domain.ExternalAgentIdentity, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
SELECT `+externalIdentitySelectColumnList+`
FROM elitea_identity.external_agent_identities WHERE provider = $1
ORDER BY created_at, id`, provider)
	if err != nil {
		return nil, fmt.Errorf("reconcile records: %w", err)
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
		return nil, fmt.Errorf("reconcile records: %w", err)
	}
	return out, nil
}

// upsertFinding opens a finding, or refreshes the open one of the same
// object and kind. It answers whether it opened one.
func upsertFinding(ctx context.Context, tx pgx.Tx, f reconcile.Finding) (bool, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return false, err
	}
	var record any
	if f.ExternalAgentIdentityID != "" {
		record = f.ExternalAgentIdentityID
	}
	var inserted bool
	err = tx.QueryRow(ctx, `
INSERT INTO elitea_identity.external_identity_drift_findings AS f (
    id, kind, provider, provider_tenant, external_id, external_agent_identity_id, tenant_project_id,
    application_id, revocation_epoch, detected_at, last_seen_at)
VALUES ($1::uuid, $2, $3, $4, $5, $6::uuid, $7, $8, $9, $10, $10)
ON CONFLICT (provider, provider_tenant, external_id, kind) WHERE resolved_at IS NULL
DO UPDATE SET last_seen_at = EXCLUDED.last_seen_at,
    revocation_epoch = greatest(f.revocation_epoch, EXCLUDED.revocation_epoch),
    external_agent_identity_id = coalesce(EXCLUDED.external_agent_identity_id, f.external_agent_identity_id)
RETURNING (xmax = 0)`,
		id.String(), string(f.Kind), f.Provider, f.ProviderTenant, f.ExternalID, record, f.TenantProjectID,
		f.ApplicationID, f.RevocationEpoch, f.DetectedAt.UTC()).Scan(&inserted)
	if err != nil {
		return false, fmt.Errorf("record drift finding: %w", err)
	}
	return inserted, nil
}

// ApplyDrift runs the drift transition and writes its finding and drift
// event in the transition's transaction.
func (s *AgentIdentityReconcile) ApplyDrift(ctx context.Context, id string, change reconcile.DriftChange) (domain.ExternalAgentIdentity, error) {
	if err := s.ready(); err != nil {
		return domain.ExternalAgentIdentity{}, err
	}
	if change.Apply == nil || change.Event == nil || !change.Finding.Kind.Valid() || change.Finding.Kind == reconcile.KindOrphan {
		return domain.ExternalAgentIdentity{}, errors.New("agent identity reconcile: a drift change needs a record kind, a transition and an event")
	}
	return s.identities.transitionExternalIdentity(ctx, id, change.Apply,
		func(ctx context.Context, tx pgx.Tx, written domain.ExternalAgentIdentity) error {
			finding := change.Finding
			finding.ExternalAgentIdentityID, finding.RevocationEpoch = written.ID, written.RevocationEpoch
			if _, err := upsertFinding(ctx, tx, finding); err != nil {
				return err
			}
			if err := s.audit.Append(ctx, tx, change.Event(written)); err != nil {
				return fmt.Errorf("audit drift: %w", err)
			}
			return nil
		})
}

// RecordOrphan opens an orphan finding with its event, or refreshes the open
// one without an event.
func (s *AgentIdentityReconcile) RecordOrphan(ctx context.Context, f reconcile.Finding, event securityaudit.SecurityEvent) (bool, error) {
	if err := s.ready(); err != nil {
		return false, err
	}
	if f.Kind != reconcile.KindOrphan || f.ExternalAgentIdentityID != "" {
		return false, errors.New("agent identity reconcile: an orphan finding names no record")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin orphan finding: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	created, err := upsertFinding(ctx, tx, f)
	if err != nil {
		return false, err
	}
	if created {
		if err := s.audit.Append(ctx, tx, event); err != nil {
			return false, fmt.Errorf("audit orphan: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit orphan finding: %w", err)
	}
	return created, nil
}

// OpenFindings answers open findings, newest first.
func (s *AgentIdentityReconcile) OpenFindings(ctx context.Context, provider string, limit, offset int) ([]reconcile.Finding, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	limit, offset = agentIdentityListWindow(limit, offset)
	rows, err := s.pool.Query(ctx, `
SELECT `+driftFindingColumnList+`
FROM elitea_identity.external_identity_drift_findings
WHERE resolved_at IS NULL AND ($1::text = '' OR provider = $1)
ORDER BY detected_at DESC, id DESC
LIMIT $2 OFFSET $3`, provider, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list drift findings: %w", err)
	}
	defer rows.Close()
	out := []reconcile.Finding{}
	for rows.Next() {
		var (
			f    reconcile.Finding
			kind string
		)
		if err := rows.Scan(&f.ID, &kind, &f.Provider, &f.ProviderTenant, &f.ExternalID, &f.ExternalAgentIdentityID,
			&f.TenantProjectID, &f.ApplicationID, &f.RevocationEpoch, &f.DetectedAt, &f.LastSeenAt); err != nil {
			return nil, fmt.Errorf("scan drift finding: %w", err)
		}
		f.Kind, f.DetectedAt, f.LastSeenAt = reconcile.FindingKind(kind), f.DetectedAt.UTC(), f.LastSeenAt.UTC()
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list drift findings: %w", err)
	}
	return out, nil
}

// ResolveFindings closes findings.
func (s *AgentIdentityReconcile) ResolveFindings(ctx context.Context, ids []string) error {
	if err := s.ready(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	if _, err := s.pool.Exec(ctx, `
UPDATE elitea_identity.external_identity_drift_findings SET resolved_at = clock_timestamp()
WHERE id = ANY($1::uuid[]) AND resolved_at IS NULL`, ids); err != nil {
		return fmt.Errorf("resolve drift findings: %w", err)
	}
	return nil
}

func scanProviderHealth(row pgx.Row) (reconcile.ProviderHealth, error) {
	var h reconcile.ProviderHealth
	var lastPass, lastSuccess *time.Time
	if err := row.Scan(&h.Provider, &h.ConsecutiveFailures, &h.AdmissionDenied, &lastPass, &lastSuccess); err != nil {
		return reconcile.ProviderHealth{}, err
	}
	if lastPass != nil {
		h.LastPassAt = lastPass.UTC()
	}
	if lastSuccess != nil {
		h.LastSuccessAt = lastSuccess.UTC()
	}
	return h, nil
}

// RecordPass records one pass result.
func (s *AgentIdentityReconcile) RecordPass(ctx context.Context, provider string, ok bool, threshold int) (reconcile.ProviderHealth, error) {
	if err := s.ready(); err != nil {
		return reconcile.ProviderHealth{}, err
	}
	if threshold <= 0 {
		return reconcile.ProviderHealth{}, errors.New("agent identity reconcile: the outage threshold must be positive")
	}
	h, err := scanProviderHealth(s.pool.QueryRow(ctx, `
INSERT INTO elitea_identity.agent_identity_provider_health AS h (
    provider, consecutive_failures, admission_denied, last_pass_at, last_success_at)
VALUES ($1, CASE WHEN $2 THEN 0 ELSE 1 END, NOT $2 AND 1 >= $3, clock_timestamp(),
        CASE WHEN $2 THEN clock_timestamp() END)
ON CONFLICT (provider) DO UPDATE SET
    consecutive_failures = CASE WHEN $2 THEN 0 ELSE h.consecutive_failures + 1 END,
    admission_denied = CASE WHEN $2 THEN false ELSE h.consecutive_failures + 1 >= $3 END,
    last_pass_at = clock_timestamp(),
    last_success_at = CASE WHEN $2 THEN clock_timestamp() ELSE h.last_success_at END,
    updated_at = clock_timestamp()
RETURNING `+providerHealthColumnList, provider, ok, threshold))
	if err != nil {
		return reconcile.ProviderHealth{}, fmt.Errorf("record reconcile pass: %w", err)
	}
	return h, nil
}

// Health answers every provider's health.
func (s *AgentIdentityReconcile) Health(ctx context.Context) ([]reconcile.ProviderHealth, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
SELECT `+providerHealthColumnList+`
FROM elitea_identity.agent_identity_provider_health ORDER BY provider`)
	if err != nil {
		return nil, fmt.Errorf("read reconcile health: %w", err)
	}
	defer rows.Close()
	out := []reconcile.ProviderHealth{}
	for rows.Next() {
		h, err := scanProviderHealth(rows)
		if err != nil {
			return nil, fmt.Errorf("scan reconcile health: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read reconcile health: %w", err)
	}
	return out, nil
}

// ProviderAdmissionDenied answers whether new agent sessions for provider
// are denied. A provider never reconciled is not denied.
func (s *AgentIdentityReconcile) ProviderAdmissionDenied(ctx context.Context, provider string) (bool, error) {
	if s == nil || s.pool == nil {
		return true, errors.New("agent identity reconcile: no database pool")
	}
	var denied bool
	err := s.pool.QueryRow(ctx, `
SELECT admission_denied FROM elitea_identity.agent_identity_provider_health WHERE provider = $1`, provider).Scan(&denied)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return true, fmt.Errorf("read provider admission: %w", err)
	}
	return denied, nil
}
