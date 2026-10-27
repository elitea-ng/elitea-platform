package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
)

// pinExecutor is the one method the admission pin needs. Both pgx.Tx and the
// shared store's sqlExecutor satisfy it, so every admission path pins inside
// its own transaction.
type pinExecutor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// admissionPin is what one admission asks the pin to record.
type admissionPin struct {
	executionID string
	// continuationScope names the logical run (an agent conversation); empty
	// for admissions that have none.
	continuationScope string
	// continuation is a resume of an earlier run in the same scope.
	continuation bool
}

// pinExecutionAdmission writes the execution's profile pin in the caller's
// admission transaction (ADR-0039, #1111).
//
//   - Governed: a fresh admission pins the active revision. A continuation
//     copies the newest governed pin of its scope, so it keeps the revision
//     its run started under.
//   - Legacy with no guard (a deployment that never ran governed): nothing.
//     No statement is sent, so a flag-off admission is unchanged.
//   - Legacy with the guard (governed pins exist from before a flag-off
//     boot): only a continuation runs the statement, and it writes a row only
//     when its scope has a governed pin. A paused governed run therefore never
//     resumes as legacy (profile spec item 5.2).
//
// An existing pin for the execution id stands: a re-admission of the same
// execution never changes the profile it was first admitted under.
func pinExecutionAdmission(ctx context.Context, tx pinExecutor, admission profile.Admission, pin admissionPin) error {
	if strings.TrimSpace(pin.executionID) == "" {
		return errors.New("admission pin requires an execution id")
	}
	governed := admission.Governed()
	lookup := pin.continuation && pin.continuationScope != "" && admission.ContinuationGuard()
	if !governed && !lookup {
		return nil
	}
	var revisionID, digest any
	if governed {
		d := admission.Digest()
		revisionID, digest = admission.RevisionID(), d[:]
	}
	var scope any
	if pin.continuationScope != "" {
		scope = pin.continuationScope
	}
	if _, err := tx.Exec(ctx, `
WITH predecessor AS (
    SELECT profile, policy_revision_id, policy_digest
    FROM elitea_runtime.execution_admission_profiles
    WHERE $5::boolean
      AND continuation_scope = $4::text
      AND profile = 'elitea.credential-profile.governed.v1'
    ORDER BY pinned_at DESC
    LIMIT 1
), chosen AS (
    SELECT profile, policy_revision_id, policy_digest FROM predecessor
    UNION ALL
    SELECT 'elitea.credential-profile.governed.v1', $2::text, $3::bytea
    WHERE $2::text IS NOT NULL AND NOT EXISTS (SELECT 1 FROM predecessor)
)
INSERT INTO elitea_runtime.execution_admission_profiles (
    execution_id, profile, policy_revision_id, policy_digest, continuation_scope
)
SELECT $1::text, profile, policy_revision_id, policy_digest, $4::text
FROM chosen
LIMIT 1
ON CONFLICT (execution_id) DO NOTHING`,
		pin.executionID, revisionID, digest, scope, lookup,
	); err != nil {
		return fmt.Errorf("pin execution admission profile: %w", err)
	}
	return nil
}

// CredentialGovernanceStore is the PostgreSQL profile.RevisionStore,
// profile.PinReader and profile.GovernedPinProbe.
type CredentialGovernanceStore struct {
	pool *pgxpool.Pool
}

// NewCredentialGovernanceStore builds the store.
func NewCredentialGovernanceStore(pool *pgxpool.Pool) (*CredentialGovernanceStore, error) {
	if pool == nil {
		return nil, errors.New("credential governance database is required")
	}
	return &CredentialGovernanceStore{pool: pool}, nil
}

var (
	_ profile.RevisionStore    = (*CredentialGovernanceStore)(nil)
	_ profile.PinReader        = (*CredentialGovernanceStore)(nil)
	_ profile.GovernedPinProbe = (*CredentialGovernanceStore)(nil)
)

// Revision reads one revision. Its digest is verified by the caller
// (profile.ValidateForActivation), so a tampered row refuses boot.
func (s *CredentialGovernanceStore) Revision(ctx context.Context, id string) (profile.PolicyRevision, error) {
	if !profile.ValidRevisionID(id) {
		return profile.PolicyRevision{}, profile.ErrRevisionNotFound
	}
	var (
		revision  profile.PolicyRevision
		digest    []byte
		canonical []byte
		createdAt time.Time
	)
	err := s.pool.QueryRow(ctx, `
SELECT id, digest, canonical, created_by, created_at
FROM elitea_identity.credential_policy_revisions
WHERE id = $1`, id).Scan(&revision.ID, &digest, &canonical, &revision.CreatedBy, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return profile.PolicyRevision{}, profile.ErrRevisionNotFound
	}
	if err != nil {
		return profile.PolicyRevision{}, fmt.Errorf("read credential policy revision: %w", err)
	}
	if len(digest) != len(revision.Digest) {
		return profile.PolicyRevision{}, profile.ErrDigestMismatch
	}
	copy(revision.Digest[:], digest)
	// Decode without re-validating: Validate runs at activation, and a
	// document this release cannot read must refuse boot there, not here.
	if err := json.Unmarshal(canonical, &revision.Document); err != nil {
		return profile.PolicyRevision{}, fmt.Errorf("%w: stored document does not decode", profile.ErrInvalidPolicy)
	}
	revision.Canonical = canonical
	revision.CreatedAt = createdAt
	return revision, nil
}

// PutRevision appends a revision. The digest must match the canonical bytes.
func (s *CredentialGovernanceStore) PutRevision(ctx context.Context, revision profile.PolicyRevision) error {
	if !profile.ValidRevisionID(revision.ID) {
		return fmt.Errorf("%w: revision id", profile.ErrInvalidPolicy)
	}
	if err := revision.VerifyDigest(); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
INSERT INTO elitea_identity.credential_policy_revisions (id, digest, canonical, document, created_by)
VALUES ($1, $2, $3, $4::jsonb, $5)
ON CONFLICT (id) DO NOTHING`,
		revision.ID, revision.Digest[:], revision.Canonical, string(revision.Canonical), revision.CreatedBy)
	if err != nil {
		return fmt.Errorf("write credential policy revision: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return profile.ErrRevisionExists
	}
	return nil
}

// ReadPin returns the execution's pin. No row is the legacy profile.
func (s *CredentialGovernanceStore) ReadPin(ctx context.Context, executionID string) (profile.Pin, error) {
	pin := profile.Pin{ExecutionID: executionID}
	var (
		revisionID *string
		digest     []byte
		name       string
	)
	err := s.pool.QueryRow(ctx, `
SELECT profile, policy_revision_id, policy_digest, pinned_at
FROM elitea_runtime.execution_admission_profiles
WHERE execution_id = $1`, executionID).Scan(&name, &revisionID, &digest, &pin.PinnedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		pin.Profile = profile.ProfileLegacy
		return pin, nil
	}
	if err != nil {
		return profile.Pin{}, fmt.Errorf("read execution admission profile: %w", err)
	}
	pin.Profile = profile.Profile(name)
	if revisionID != nil {
		pin.PolicyRevisionID = *revisionID
	}
	if digest != nil {
		if len(digest) != len(pin.PolicyDigest) {
			return profile.Pin{}, profile.ErrDigestMismatch
		}
		copy(pin.PolicyDigest[:], digest)
	}
	return pin, nil
}

// AnyGovernedPin is the boot-time probe for the continuation guard. A
// database without migration 0180 (a developer bootstrap, or pods that start
// before elitea-migrate) has no pins at all, so the probe answers false rather
// than refusing a flag-off boot.
func (s *CredentialGovernanceStore) AnyGovernedPin(ctx context.Context) (bool, error) {
	var present bool
	if err := s.pool.QueryRow(ctx,
		`SELECT to_regclass('elitea_runtime.execution_admission_profiles') IS NOT NULL`,
	).Scan(&present); err != nil {
		return false, fmt.Errorf("probe governed admission pins: %w", err)
	}
	if !present {
		return false, nil
	}
	var found bool
	if err := s.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM elitea_runtime.execution_admission_profiles
    WHERE profile = 'elitea.credential-profile.governed.v1'
)`).Scan(&found); err != nil {
		return false, fmt.Errorf("probe governed admission pins: %w", err)
	}
	return found, nil
}
