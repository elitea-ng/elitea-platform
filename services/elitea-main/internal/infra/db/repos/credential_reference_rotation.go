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
// One rotation can be pending at a time. A second StageRotation is
// ErrCredentialReferenceRotationPending: each Vault rotation adds a KV v2
// version, and destroyed versions still count toward max_versions, so
// repeated rotations could prune the version the broker reads.
// :cancel_rotation (cancelRotation) and :retire clear a pending rotation.
//
// A transition that takes a secret version out of use (the replaced value
// after :activate, a cancelled rotation) answers it as
// CredentialReferenceRecord.Superseded. The caller retires that version
// through the secret write ledger after the commit and records the outcome
// with RecordSecretRetire.
//
// No value is read or written here: the secret store writes happen outside
// these transactions, through the secret write ledger.

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
	// ErrCredentialReferenceRotationPending is a :rotate while a rotation
	// is pending.
	ErrCredentialReferenceRotationPending = errors.New("credential reference: a rotation is already pending; activate it (:activate) or cancel it (:cancel_rotation) first")
	// ErrCredentialReferenceNoRotation is a :cancel_rotation with no
	// pending rotation.
	ErrCredentialReferenceNoRotation = errors.New("credential reference: no rotation is pending")
)

// SupersededSecret is a secret version that a transition took out of use.
// It never holds a value.
type SupersededSecret struct {
	Locator    string
	Generation int64
	// Reason is "rotation_activated" (the replaced value),
	// "rotation_cancelled" or "reference_retired" (the pending value).
	Reason string
}

// Secret retire outcomes for RecordSecretRetire.
const (
	SecretRetireDone       = "retired"
	SecretRetireNotOwned   = "not_owned"
	SecretRetireFailed     = "failed"
	SecretRetireNoBackend  = "backend_not_configured"
	secretRetireAuditEvent = "credential_reference.secret_retire"
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
// audit event. The reference must be an active static secret with no
// pending rotation (ErrCredentialReferenceRotationPending).
func (s *CredentialReferences) StageRotation(ctx context.Context, stage RotationStage) (CredentialReferenceRecord, error) {
	if err := s.writeReady(); err != nil {
		return CredentialReferenceRecord{}, err
	}
	stagerID, stager, err := CanonicalUserID(stage.StagedBy)
	if err != nil {
		return CredentialReferenceRecord{}, err
	}
	if !agentIdentityIsUUID(stage.ReferenceID) {
		return CredentialReferenceRecord{}, credentialgrant.ErrReferenceNotFound
	}
	if stage.Locator == "" || len(stage.Locator) > 512 || stage.Generation <= 0 {
		return CredentialReferenceRecord{}, errors.New("credential references: a rotation needs a locator and a generation")
	}
	var written CredentialReferenceRecord
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		current, err := scanCredentialReference(tx.QueryRow(ctx,
			`SELECT `+credentialReferenceColumns+` FROM `+credentialReferenceFrom+` WHERE r.id = $1::uuid FOR UPDATE OF r`, stage.ReferenceID))
		if err != nil {
			return err
		}
		if current.State != credentialgrant.ReferenceActive || current.Class != credentialgrant.ClassStaticSecret {
			return ErrCredentialReferenceNotRotatable
		}
		if current.Rotation != nil {
			return ErrCredentialReferenceRotationPending
		}
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
		event := credentialReferenceEvent(written, "credential_reference.rotation_stage", stager, reasons...)
		event.DecisionID = stage.DecisionID
		if err := s.audit.Append(ctx, tx, event); err != nil {
			return fmt.Errorf("audit the rotation stage: %w", err)
		}
		return nil
	})
	if err != nil {
		return CredentialReferenceRecord{}, err
	}
	return written, nil
}

// activateRotation moves an active reference to its pending rotation. The
// caller's transaction holds the row; this commits it. Two-person rule: the
// activator must not be the operator who staged the rotation. The answer
// carries the replaced value as Superseded, for the caller to retire after
// the replicas drop their caches.
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
	written.Superseded = &SupersededSecret{Locator: current.Locator, Generation: current.Generation, Reason: "rotation_activated"}
	return written, nil
}

// cancelRotation clears the pending rotation of a reference, with its audit
// event. The caller's transaction holds the row; this commits it. Any
// operator with the manage permission may cancel, the stager too. The
// answer carries the cancelled value as Superseded, for the caller to
// retire.
func (s *CredentialReferences) cancelRotation(ctx context.Context, tx pgx.Tx, current CredentialReferenceRecord,
	actorID int64, actor string) (CredentialReferenceRecord, error) {
	pending := current.Rotation
	if pending == nil {
		return CredentialReferenceRecord{}, ErrCredentialReferenceNoRotation
	}
	tag, err := tx.Exec(ctx, `
UPDATE elitea_identity.credential_references
SET rotation_locator = NULL, rotation_generation = NULL, rotation_staged_by = NULL, rotation_staged_at = NULL,
    last_transition_by = $2, updated_at = $3
WHERE id = $1::uuid AND rotation_locator = $4 AND rotation_generation = $5`,
		current.ID, actorID, s.now().UTC(), pending.Locator, pending.Generation)
	if err != nil {
		return CredentialReferenceRecord{}, fmt.Errorf("cancel the rotation: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return CredentialReferenceRecord{}, ErrCredentialReferenceTransition
	}
	written, err := scanCredentialReference(tx.QueryRow(ctx,
		`SELECT `+credentialReferenceColumns+` FROM `+credentialReferenceFrom+` WHERE r.id = $1::uuid`, current.ID))
	if err != nil {
		return CredentialReferenceRecord{}, err
	}
	event := credentialReferenceEvent(written, "credential_reference.rotation_cancel", actor,
		"state."+string(written.State), "cancels.generation."+strconv.FormatInt(pending.Generation, 10),
		"staged_by."+pending.StagedBy)
	if err := s.audit.Append(ctx, tx, event); err != nil {
		return CredentialReferenceRecord{}, fmt.Errorf("audit the rotation cancel: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CredentialReferenceRecord{}, fmt.Errorf("commit the rotation cancel: %w", err)
	}
	written.Superseded = &SupersededSecret{Locator: pending.Locator, Generation: pending.Generation, Reason: "rotation_cancelled"}
	return written, nil
}

// SecretRetire is the outcome of one retire of a superseded secret version.
type SecretRetire struct {
	Secret SupersededSecret
	// Outcome is one of the SecretRetire* constants.
	Outcome string
	// DecisionID is the StoreContext.DecisionID of the retire.
	DecisionID string
	// Actor is the operator whose transition superseded the version.
	Actor string
}

// RecordSecretRetire appends the credential_reference.secret_retire audit
// decision for one retire of a superseded secret version. It writes no
// reference column. The event names the reason, the backend and the
// generation; a locator is a path, not a reason code, so the caller logs it
// when the outcome is not "retired".
func (s *CredentialReferences) RecordSecretRetire(ctx context.Context, rec CredentialReferenceRecord, retire SecretRetire) error {
	if err := s.writeReady(); err != nil {
		return err
	}
	_, actor, err := CanonicalUserID(retire.Actor)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		event := credentialReferenceEvent(rec, secretRetireAuditEvent, actor,
			"outcome."+retire.Outcome, "reason."+retire.Secret.Reason, "backend."+rec.Backend,
			"generation."+strconv.FormatInt(retire.Secret.Generation, 10))
		event.Time = s.now().UTC()
		event.DecisionID = retire.DecisionID
		if err := s.audit.Append(ctx, tx, event); err != nil {
			return fmt.Errorf("audit the secret retire: %w", err)
		}
		return nil
	})
}
