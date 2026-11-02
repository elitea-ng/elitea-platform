package repos

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/epoch"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
)

// #1505: a rotation is staged on an active reference, the broker keeps the
// current secret until another operator activates it, and :activate moves
// the reference to the new locator and generation with its audit event.
// CI-only: skipped without the integration database.
func TestCredentialReferenceRotationIsTwoPerson(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	notifier := &recordingNotifier{}
	store := NewCredentialReferences(pool, agentIdentityAudit(t)).WithNotifier(notifier)
	staged := stageRefTest(t, store, "configuration:55555555-5555-4555-8555-555555555555:pgvault")
	id := staged.Record.ID

	// A staged reference is not rotatable.
	_, err := store.StageRotation(ctx, RotationStage{ReferenceID: id, Locator: "governed_new", Generation: 1, DecisionID: newRefTestID(), StagedBy: "300"})
	require.ErrorIs(t, err, ErrCredentialReferenceNotRotatable)
	_, err = store.Transition(ctx, id, CredentialReferenceActivate, "200")
	require.NoError(t, err)
	notifier.changes = nil

	first, err := store.StageRotation(ctx, RotationStage{ReferenceID: id, Locator: "governed_first", Generation: 1, DecisionID: newRefTestID(), StagedBy: "300"})
	require.NoError(t, err)
	require.NotNil(t, first.Rotation)
	require.Equal(t, staged.Record.Locator, first.Locator, "the broker still reads the current secret")
	require.Equal(t, "governed_first", first.Rotation.Locator)

	// One rotation at a time: a second stage is refused and changes nothing.
	_, err = store.StageRotation(ctx, RotationStage{ReferenceID: id, Locator: "governed_other", Generation: 1, DecisionID: newRefTestID(), StagedBy: "300"})
	require.ErrorIs(t, err, ErrCredentialReferenceRotationPending)

	// :cancel_rotation clears it and answers the cancelled version.
	cancelled, err := store.Transition(ctx, id, CredentialReferenceCancelRotation, "300")
	require.NoError(t, err)
	require.Nil(t, cancelled.Rotation)
	require.Equal(t, credentialgrant.ReferenceActive, cancelled.State)
	require.Equal(t, &SupersededSecret{Locator: "governed_first", Generation: 1, Reason: "rotation_cancelled"}, cancelled.Superseded)
	_, err = store.Transition(ctx, id, CredentialReferenceCancelRotation, "300")
	require.ErrorIs(t, err, ErrCredentialReferenceNoRotation)

	decision := newRefTestID()
	second, err := store.StageRotation(ctx, RotationStage{ReferenceID: id, Locator: "governed_second", Generation: 1, DecisionID: decision, StagedBy: "300"})
	require.NoError(t, err)
	require.Equal(t, "governed_second", second.Rotation.Locator)
	require.Equal(t, "300", second.Rotation.StagedBy)
	require.Empty(t, notifier.changes, "a staged rotation changes nothing the broker reads")

	// A half-written pending rotation is refused by the database.
	_, err = pool.Exec(ctx, `UPDATE elitea_identity.credential_references SET rotation_generation = NULL WHERE id = $1::uuid`, id)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr)
	require.Equal(t, "credential_references_rotation_complete", pgErr.ConstraintName)

	// The operator who rotated cannot activate it.
	_, err = store.Transition(ctx, id, CredentialReferenceActivate, "300")
	require.ErrorIs(t, err, ErrCredentialReferenceSelfRotation)

	rotated, err := store.Transition(ctx, id, CredentialReferenceActivate, "200")
	require.NoError(t, err)
	require.Equal(t, credentialgrant.ReferenceActive, rotated.State)
	require.Equal(t, "governed_second", rotated.Locator)
	require.Equal(t, int64(1), rotated.Generation)
	require.Nil(t, rotated.Rotation)
	require.Equal(t, &SupersededSecret{Locator: staged.Record.Locator, Generation: staged.Record.Generation, Reason: "rotation_activated"},
		rotated.Superseded, "the replaced value is answered for its retire")
	require.Len(t, notifier.changes, 1)
	require.Equal(t, epoch.KindCredentialRef, notifier.changes[0].Epoch.Kind)

	// Nothing is pending now: :activate on an active reference is refused.
	_, err = store.Transition(ctx, id, CredentialReferenceActivate, "400")
	require.ErrorIs(t, err, ErrCredentialReferenceTransition)

	require.NoError(t, store.RecordSecretRetire(ctx, rotated, SecretRetire{Secret: *rotated.Superseded, Outcome: SecretRetireDone,
		DecisionID: newRefTestID(), Actor: "200"}))

	for action, want := range map[string]int{"credential_reference.rotation_stage": 2, "credential_reference.rotate": 1,
		"credential_reference.rotation_cancel": 1, "credential_reference.secret_retire": 1} {
		var events int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.security_audit_outbox
WHERE event::text LIKE '%' || $1 || '%' AND event::text LIKE '%' || $2 || '"%'`, id, action).Scan(&events))
		require.Equal(t, want, events, action)
	}
	var stagedEvents int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.security_audit_outbox
WHERE event::text LIKE '%' || $1 || '%'`, decision).Scan(&stagedEvents))
	require.Equal(t, 1, stagedEvents, "the stage event carries the write's decision id")

	// The governed resolver reads the new locator.
	ref, err := NewCredentialGrants(pool, nil).CredentialReference(ctx, 7, id)
	require.NoError(t, err)
	require.Equal(t, "governed_second", ref.Locator)
}

// #1505 review: :retire cancels a pending rotation. The retired reference
// keeps no rotation columns, and the pending version is answered for its
// retire.
func TestCredentialReferenceRetireCancelsThePendingRotation(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	store := NewCredentialReferences(pool, agentIdentityAudit(t))
	staged := stageRefTest(t, store, "configuration:66666666-6666-4666-8666-666666666666:pgvault")
	id := staged.Record.ID
	_, err := store.Transition(ctx, id, CredentialReferenceActivate, "200")
	require.NoError(t, err)
	_, err = store.StageRotation(ctx, RotationStage{ReferenceID: id, Locator: "governed_pending", Generation: 1, DecisionID: newRefTestID(), StagedBy: "300"})
	require.NoError(t, err)

	retired, err := store.Transition(ctx, id, CredentialReferenceRetire, "300")
	require.NoError(t, err)
	require.Equal(t, credentialgrant.ReferenceRetired, retired.State)
	require.Nil(t, retired.Rotation)
	require.Equal(t, &SupersededSecret{Locator: "governed_pending", Generation: 1, Reason: "reference_retired"}, retired.Superseded)

	read, err := store.Get(ctx, id)
	require.NoError(t, err)
	require.Nil(t, read.Rotation, "GET shows no pending rotation on a retired reference")
	require.Nil(t, read.Superseded, "Superseded is not stored")
	var pending int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM elitea_identity.credential_references
WHERE id = $1::uuid AND rotation_locator IS NOT NULL`, id).Scan(&pending))
	require.Zero(t, pending)

	// A retire with nothing pending supersedes nothing.
	other := stageRefTest(t, store, "configuration:77777777-7777-4777-8777-777777777777:pgvault")
	plain, err := store.Transition(ctx, other.Record.ID, CredentialReferenceRetire, "300")
	require.NoError(t, err)
	require.Nil(t, plain.Superseded)
}
