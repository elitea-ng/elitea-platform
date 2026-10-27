package delegationstore

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// Requirement reasons.
const (
	ReasonInteractionRequired = "interaction_required"
	ReasonClaimsChallenge     = "claims_challenge"
)

const maxClaimsChallengeBytes = 8 << 10

var (
	// ErrRequirementNotFound is an unknown reference, or one that belongs
	// to another user or tenant project.
	ErrRequirementNotFound = errors.New("delegationstore: authorization requirement not found")
	// ErrRequirementResolved refuses a second resolution.
	ErrRequirementResolved = errors.New("delegationstore: authorization requirement already resolved")
)

// Requirement is a governed run paused for credential authorization.
type Requirement struct {
	ID string
	// Reference is the opaque authorization_reference the API returns.
	Reference           string
	TenantProjectID     int64
	ActorUserID         int64
	ConversationUUID    string
	ResponseMessageID   string
	ExecutionID         string
	ExecutionGeneration string
	DelegationID        string
	Scopes              []string
	Reason              string
	// Claims is the claims challenge to answer, if any. It names required
	// claims and holds no credential.
	Claims               string
	Resolved             bool
	ResolvedDelegationID string
	CreatedAt            time.Time
}

func (r Requirement) valid() bool {
	return r.TenantProjectID > 0 && r.ActorUserID > 0 && isUUID(r.ConversationUUID) &&
		isUUID(r.ResponseMessageID) && validText(r.ExecutionID) && len(r.ExecutionID) <= 128 &&
		validText(r.ExecutionGeneration) && len(r.ExecutionGeneration) <= 128 && isUUID(r.DelegationID) &&
		len(r.Scopes) > 0 && validScopes(r.Scopes) && len(r.Claims) <= maxClaimsChallengeBytes &&
		(r.Reason == ReasonInteractionRequired || (r.Reason == ReasonClaimsChallenge && r.Claims != ""))
}

// RecordRequirement stores a pending requirement and returns it with its
// new reference.
func (s *Store) RecordRequirement(ctx context.Context, r Requirement) (Requirement, error) {
	if !r.valid() {
		return Requirement{}, ErrInvalid
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Requirement{}, err
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return Requirement{}, err
	}
	r.ID = id.String()
	r.Reference = "car_" + base64.RawURLEncoding.EncodeToString(random[:])
	r.Resolved, r.ResolvedDelegationID = false, ""
	now := s.now().UTC()
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
INSERT INTO elitea_identity.credential_authorization_requirements
    (id, reference, tenant_project_id, actor_user_id, conversation_uuid, response_message_id,
     execution_id, execution_generation, delegation_id, scopes, reason, claims_challenge)
VALUES ($1::uuid, $2, $3, $4, $5::uuid, $6::uuid, $7, $8, $9::uuid, $10, $11, $12)
RETURNING created_at`,
			id.String(), r.Reference, r.TenantProjectID, r.ActorUserID, r.ConversationUUID, r.ResponseMessageID,
			r.ExecutionID, r.ExecutionGeneration, r.DelegationID, r.Scopes, r.Reason, r.Claims).Scan(&r.CreatedAt); err != nil {
			return fmt.Errorf("delegationstore: record the authorization requirement: %w", err)
		}
		return s.audit.Append(ctx, tx, securityaudit.SecurityEvent{
			Type: securityaudit.TypeDelegationChanged, Time: now, TenantProjectID: r.TenantProjectID,
			Action: "delegation.authorization_required", Reasons: []string{"reason." + r.Reason},
			PrincipalRef: userRef(r.ActorUserID), DelegationRef: r.DelegationID, ExecutionID: r.ExecutionID,
			ResourceType: "credential_authorization_requirement", ResourceID: id.String(),
		})
	})
	if err != nil {
		return Requirement{}, err
	}
	return r, nil
}

// RequirementFor reads a requirement by reference for its own user and
// tenant project.
func (s *Store) RequirementFor(ctx context.Context, reference string, tenantProjectID, actorUserID int64) (Requirement, error) {
	return s.readRequirement(ctx, s.pool, reference, tenantProjectID, actorUserID, false)
}

// ResolveRequirement marks a pending requirement resolved by a new
// delegation (the user re-consented and signed in again).
func (s *Store) ResolveRequirement(ctx context.Context, reference string, tenantProjectID, actorUserID int64, delegationID string) (Requirement, error) {
	if !isUUID(delegationID) {
		return Requirement{}, ErrInvalid
	}
	var resolved Requirement
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := s.readRequirement(ctx, tx, reference, tenantProjectID, actorUserID, true)
		if err != nil {
			return err
		}
		if r.Resolved {
			// The same answer again is a retry of a resume whose
			// continuation failed after this commit: hand back the
			// resolution so the run can still continue. Another delegation
			// is a second answer, refused.
			if r.ResolvedDelegationID == delegationID {
				resolved = r
				return nil
			}
			return ErrRequirementResolved
		}
		// The answer must be a live re-consent of the same user for the same
		// agent binding and project as the delegation that paused the run.
		var matches bool
		if err := tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM elitea_identity.delegations AS answer
    JOIN elitea_identity.delegations AS paused ON paused.id = $2::uuid
    WHERE answer.id = $1::uuid
      AND answer.state = 'active'
      AND answer.expires_at > clock_timestamp()
      AND answer.tenant_project_id = paused.tenant_project_id
      AND answer.agent_binding_id = paused.agent_binding_id
      AND answer.tenant_id = paused.tenant_id
      AND answer.subject_object_id = paused.subject_object_id
      AND answer.tenant_project_id = $3
)`, delegationID, r.DelegationID, r.TenantProjectID).Scan(&matches); err != nil {
			return fmt.Errorf("delegationstore: check the resolving delegation: %w", err)
		}
		if !matches {
			return ErrInvalid
		}
		if _, err := tx.Exec(ctx, `
UPDATE elitea_identity.credential_authorization_requirements
SET state = 'resolved', resolved_delegation_id = $2::uuid, resolved_at = clock_timestamp()
WHERE reference = $1`, reference, delegationID); err != nil {
			return fmt.Errorf("delegationstore: resolve the authorization requirement: %w", err)
		}
		r.Resolved, r.ResolvedDelegationID = true, delegationID
		resolved = r
		return s.audit.Append(ctx, tx, securityaudit.SecurityEvent{
			Type: securityaudit.TypeDelegationChanged, Time: s.now().UTC(), TenantProjectID: r.TenantProjectID,
			Action: "delegation.authorization_resolved", Reasons: []string{"reason." + r.Reason},
			PrincipalRef: userRef(actorUserID), DelegationRef: delegationID, ExecutionID: r.ExecutionID,
			ResourceType: "credential_authorization_requirement", ResourceID: r.ID,
		})
	})
	if err != nil {
		return Requirement{}, err
	}
	return resolved, nil
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Store) readRequirement(ctx context.Context, q queryRower, reference string, tenantProjectID, actorUserID int64, lock bool) (Requirement, error) {
	if reference == "" || len(reference) > 128 || tenantProjectID <= 0 || actorUserID <= 0 {
		return Requirement{}, ErrRequirementNotFound
	}
	sql := `
SELECT id::text, reference, tenant_project_id, actor_user_id, conversation_uuid::text, response_message_id::text,
       execution_id, execution_generation, delegation_id::text, scopes, reason, claims_challenge,
       state = 'resolved', COALESCE(resolved_delegation_id::text, ''), created_at
FROM elitea_identity.credential_authorization_requirements
WHERE reference = $1 AND tenant_project_id = $2 AND actor_user_id = $3`
	if lock {
		sql += " FOR UPDATE"
	}
	var r Requirement
	err := q.QueryRow(ctx, sql, reference, tenantProjectID, actorUserID).Scan(
		&r.ID, &r.Reference, &r.TenantProjectID, &r.ActorUserID, &r.ConversationUUID, &r.ResponseMessageID,
		&r.ExecutionID, &r.ExecutionGeneration, &r.DelegationID, &r.Scopes, &r.Reason, &r.Claims,
		&r.Resolved, &r.ResolvedDelegationID, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Requirement{}, ErrRequirementNotFound
	}
	if err != nil {
		return Requirement{}, fmt.Errorf("delegationstore: read the authorization requirement: %w", err)
	}
	return r, nil
}
