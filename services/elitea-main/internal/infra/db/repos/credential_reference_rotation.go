package repos

// Secret rotation of an active credential reference (ADR-0039, #1505).
//
// Two steps, two operators, as for a new reference:
//
//  1. StageRotation records the new secret version a ledgered write created
//     (POST …/{id}:rotate) as PENDING on the active reference. The reference
//     keeps its locator and generation, so the broker still releases the
//     current value.
//  2. :activate by another operator (Transition, activateRotation) moves the
//     reference to the pending locator and generation and clears the pending
//     columns, with its audit event, in one transaction.
//
// No value is read or written here: the secret store write happens before
// StageRotation, through the secret write ledger.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/epoch"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
)

// Rotation refusals.
var (
	// ErrCredentialReferenceNotRotatable is a reference that is not an
	// active static secret.
	ErrCredentialReferenceNotRotatable = errors.New("credential reference: only an active static_secret reference can be rotated")
	// ErrCredentialReferenceSelfRotation is an activation by the operator
	// who staged the rotation.
	ErrCredentialReferenceSelfRotation = errors.New("credential reference: the rotation must be activated by another operator than the one who staged it")
)

// PendingRotation is a secret version written for an active reference and
// not yet activated. It never holds a value.
type PendingRotation struct {
	Locator    string
	Generation int64
	StagedBy   string
	StagedAt   time.Time
}

type scannedRotation struct {
	locator    *string
	generation *int64
	stagedBy   *string
	stagedAt   *time.Time
}

func (r scannedRotation) pending() *PendingRotation {
	if r.locator == nil || r.generation == nil || r.stagedBy == nil || r.stagedAt == nil {
		return nil
	}
	return &PendingRotation{Locator: *r.locator, Generation: *r.generation, StagedBy: *r.stagedBy, StagedAt: r.stagedAt.UTC()}
}

// RotationStage is one written secret version to record as pending.
type RotationStage struct {
	ReferenceID string
	Locator     string
	Generation  int64
	// DecisionID is the StoreContext.DecisionID of the write.
	DecisionID string
	// StagedBy is the operator's auth_core user id.
	StagedBy string
}

// StageRotation records stage as the reference's pending rotation, with its
// audit event. The reference must be an active static secret. A pending
// rotation already there is replaced; it is answered so the caller can
// retire its secret version.
func (s *CredentialReferences) StageRotation(ctx context.Context, stage RotationStage) (CredentialReferenceRecord, *PendingRotation, error) {
	if err := s.writeReady(); err != nil {
		return CredentialReferenceRecord{}, nil, err
	}
	stagerID, stager, err := CanonicalUserID(stage.StagedBy)
	if err != nil {
		return CredentialReferenceRecord{}, nil, err
	}
	if !agentIdentityIsUUID(stage.ReferenceID) {
		return CredentialReferenceRecord{}, nil, credentialgrant.ErrReferenceNotFound
	}
	if stage.Locator == "" || len(stage.Locator) > 512 || stage.Generation <= 0 {
		return CredentialReferenceRecord{}, nil, errors.New("credential references: a rotation needs a locator and a generation")
	}
	var written CredentialReferenceRecord
	var replaced *PendingRotation
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		current, err := scanCredentialReference(tx.QueryRow(ctx,
			`SELECT `+credentialReferenceColumns+` FROM `+credentialReferenceFrom+` WHERE r.id = $1::uuid FOR UPDATE OF r`, stage.ReferenceID))
		if err != nil {
			return err
		}
		if current.State != credentialgrant.ReferenceActive || current.Class != credentialgrant.ClassStaticSecret {
			return ErrCredentialReferenceNotRotatable
		}
		replaced = current.Rotation
		if _, err := tx.Exec(ctx, `
UPDATE elitea_identity.credential_references
SET rotation_locator = $2, rotation_generation = $3, rotation_staged_by = $4, rotation_staged_at = $5, updated_at = $5
WHERE id = $1::uuid`, stage.ReferenceID, stage.Locator, stage.Generation, stagerID, s.now().UTC()); err != nil {
			return fmt.Errorf("stage the rotation: %w", err)
		}
		if written, err = scanCredentialReference(tx.QueryRow(ctx,
			`SELECT `+credentialReferenceColumns+` FROM `+credentialReferenceFrom+` WHERE r.id = $1::uuid`, stage.ReferenceID)); err != nil {
			return err
		}
		reasons := []string{"state.rotation_pending", "backend." + written.Backend,
			"generation." + strconv.FormatInt(stage.Generation, 10)}
		if replaced != nil {
			reasons = append(reasons, "replaces.generation."+strconv.FormatInt(replaced.Generation, 10))
		}
		event := credentialReferenceEvent(written, "credential_reference.rotation_stage", stager, reasons...)
		event.DecisionID = stage.DecisionID
		if err := s.audit.Append(ctx, tx, event); err != nil {
			return fmt.Errorf("audit the rotation stage: %w", err)
		}
		return nil
	})
	if err != nil {
		return CredentialReferenceRecord{}, nil, err
	}
	return written, replaced, nil
}

// activateRotation moves an active reference to its pending rotation. The
// caller's transaction holds the row; this commits it. Two-person rule: the
// activator must not be the operator who staged the rotation.
func (s *CredentialReferences) activateRotation(ctx context.Context, tx pgx.Tx, current CredentialReferenceRecord,
	actorID int64, actor string) (CredentialReferenceRecord, error) {
	pending := current.Rotation
	if pending.StagedBy == actor {
		return CredentialReferenceRecord{}, ErrCredentialReferenceSelfRotation
	}
	tag, err := tx.Exec(ctx, `
UPDATE elitea_identity.credential_references
SET locator = rotation_locator, generation = rotation_generation,
    rotation_locator = NULL, rotation_generation = NULL, rotation_staged_by = NULL, rotation_staged_at = NULL,
    last_transition_by = $2, updated_at = $3
WHERE id = $1::uuid AND state = 'active' AND rotation_locator = $4 AND rotation_generation = $5`,
		current.ID, actorID, s.now().UTC(), pending.Locator, pending.Generation)
	if err != nil {
		return CredentialReferenceRecord{}, fmt.Errorf("activate the rotation: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return CredentialReferenceRecord{}, ErrCredentialReferenceTransition
	}
	written, err := scanCredentialReference(tx.QueryRow(ctx,
		`SELECT `+credentialReferenceColumns+` FROM `+credentialReferenceFrom+` WHERE r.id = $1::uuid`, current.ID))
	if err != nil {
		return CredentialReferenceRecord{}, err
	}
	event := credentialReferenceEvent(written, "credential_reference.rotate", actor,
		"state.active", "generation."+strconv.FormatInt(written.Generation, 10),
		"from.generation."+strconv.FormatInt(current.Generation, 10), "staged_by."+pending.StagedBy)
	if err := s.audit.Append(ctx, tx, event); err != nil {
		return CredentialReferenceRecord{}, fmt.Errorf("audit the rotation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CredentialReferenceRecord{}, fmt.Errorf("commit the rotation: %w", err)
	}
	if s.notifier != nil {
		// Replicas drop what they cached for the old secret.
		s.notifier.Notify(ctx, epoch.Change{
			Epoch:     epoch.SecurityEpoch{Kind: epoch.KindCredentialRef, ID: written.ID, Value: written.Generation},
			ChangedAt: written.UpdatedAt,
		})
	}
	return written, nil
}
