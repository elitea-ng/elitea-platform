package repos

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
)

// Shared 0181 (issue #1112): the approver CHECK, the one-live-binding index,
// the FOR UPDATE transition under concurrent approvals, and the mandate
// round trip. CI-only: skipped without the integration database.

const agentIdentityTestRevision = "rev-1112"

type agentIdentityAllowAll struct{}

func (agentIdentityAllowAll) AllowsIdentityMode(agentidentity.IdentityMode) bool { return true }

type agentIdentityTTL time.Duration

func (d agentIdentityTTL) MaxMandateTTL() time.Duration { return time.Duration(d) }

func seedAgentIdentityPolicyRevision(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	canonical := []byte(`{"schema":"elitea.credential-policy.v1"}`)
	digest := sha256.Sum256(canonical)
	_, err := pool.Exec(context.Background(), `
INSERT INTO elitea_identity.credential_policy_revisions (id, digest, canonical, document, created_by)
VALUES ($1, $2, $3, $4::jsonb, 'integration-test') ON CONFLICT (id) DO NOTHING`,
		agentIdentityTestRevision, digest[:], canonical, string(canonical))
	require.NoError(t, err)
}

func proposedTestBinding(t *testing.T, id string, version int64, digestByte byte) agentidentity.AgentIdentityBinding {
	t.Helper()
	b, err := agentidentity.NewBinding(id, agentidentity.Proposal{
		TenantProjectID: 7, ApplicationID: 3, ApplicationVersionID: version,
		DefinitionDigest: [32]byte{digestByte}, Mode: agentidentity.IdentityModeDedicatedAgent,
		ExternalAgentIdentityID: "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0f22",
	}, agentIdentityAllowAll{}, agentIdentityTestRevision, "100", time.Now().UTC().Truncate(time.Microsecond))
	require.NoError(t, err)
	return b
}

func approveBy(approver string) func(agentidentity.AgentIdentityBinding) (agentidentity.AgentIdentityBinding, error) {
	return func(current agentidentity.AgentIdentityBinding) (agentidentity.AgentIdentityBinding, error) {
		return current.Transition(agentidentity.ActionApprove, approver, agentIdentityTestRevision, time.Now().UTC())
	}
}

func TestAgentIdentityBindingApproverCheckIsEnforcedByTheDatabase(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentIdentityPolicyRevision(t, pool)
	store := NewAgentIdentityBindings(pool)
	ctx := context.Background()

	created, err := store.CreateBinding(ctx, proposedTestBinding(t, "0192f5e2-7c3a-7b1e-8a9d-000000000001", 1, 1))
	require.NoError(t, err)

	// Bypass the domain: the CHECK alone must refuse the proposer as approver.
	_, err = pool.Exec(ctx, `
UPDATE elitea_identity.agent_identity_bindings SET approved_by = proposed_by, state = 'active' WHERE id = $1::uuid`, created.ID)
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "want a constraint violation, got %v", err)
	require.Equal(t, "23514", pgErr.Code)
	require.Equal(t, agentIdentityBindingApproverCheck, pgErr.ConstraintName)
	require.ErrorIs(t, mapAgentIdentityWriteError(err), agentidentity.ErrSelfApproval)

	// And a transition whose apply skipped the domain check maps to the typed error.
	_, err = store.TransitionBinding(ctx, created.ID, func(current agentidentity.AgentIdentityBinding) (agentidentity.AgentIdentityBinding, error) {
		next := current
		next.State, next.ApprovedBy, next.RevocationEpoch = agentidentity.BindingActive, current.ProposedBy, current.RevocationEpoch+1
		return next, nil
	})
	require.ErrorIs(t, err, agentidentity.ErrSelfApproval)

	// Active without an approver is refused as well.
	_, err = pool.Exec(ctx, `UPDATE elitea_identity.agent_identity_bindings SET state = 'active' WHERE id = $1::uuid`, created.ID)
	require.Error(t, err)
}

func TestAgentIdentityBindingUniqueIndexCoversOnlyNonRevokedBindings(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentIdentityPolicyRevision(t, pool)
	store := NewAgentIdentityBindings(pool)
	ctx := context.Background()

	first, err := store.CreateBinding(ctx, proposedTestBinding(t, "0192f5e2-7c3a-7b1e-8a9d-000000000011", 5, 9))
	require.NoError(t, err)
	_, err = store.CreateBinding(ctx, proposedTestBinding(t, "0192f5e2-7c3a-7b1e-8a9d-000000000012", 5, 9))
	require.ErrorIs(t, err, agentidentity.ErrDuplicateBinding)

	// Another digest of the same version, and the same digest in another version, are fine.
	_, err = store.CreateBinding(ctx, proposedTestBinding(t, "0192f5e2-7c3a-7b1e-8a9d-000000000013", 5, 10))
	require.NoError(t, err)
	_, err = store.CreateBinding(ctx, proposedTestBinding(t, "0192f5e2-7c3a-7b1e-8a9d-000000000014", 6, 9))
	require.NoError(t, err)

	revoked, err := store.TransitionBinding(ctx, first.ID, func(current agentidentity.AgentIdentityBinding) (agentidentity.AgentIdentityBinding, error) {
		return current.Transition(agentidentity.ActionRevoke, "100", "", time.Now().UTC())
	})
	require.NoError(t, err)
	require.Equal(t, agentidentity.BindingRevoked, revoked.State)
	require.Equal(t, int64(1), revoked.RevocationEpoch)

	again, err := store.CreateBinding(ctx, proposedTestBinding(t, "0192f5e2-7c3a-7b1e-8a9d-000000000015", 5, 9))
	require.NoError(t, err, "a revoked binding must not block a new proposal")
	require.Equal(t, agentidentity.BindingProposed, again.State)
}

func TestAgentIdentityBindingConcurrentApprovalsGiveOneSuccess(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentIdentityPolicyRevision(t, pool)
	store := NewAgentIdentityBindings(pool)
	ctx := context.Background()

	created, err := store.CreateBinding(ctx, proposedTestBinding(t, "0192f5e2-7c3a-7b1e-8a9d-000000000021", 1, 2))
	require.NoError(t, err)

	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, approver := range []string{"201", "202"} {
		wg.Add(1)
		go func(i int, approver string) {
			defer wg.Done()
			<-start
			_, errs[i] = store.TransitionBinding(ctx, created.ID, approveBy(approver))
		}(i, approver)
	}
	close(start)
	wg.Wait()

	successes, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, agentidentity.ErrInvalidTransition):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	require.Equal(t, 1, successes)
	require.Equal(t, 1, conflicts)

	stored, err := store.Get(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, agentidentity.BindingActive, stored.State)
	require.Equal(t, int64(1), stored.RevocationEpoch, "exactly one transition was written")

	active, err := store.ActiveForDefinition(ctx, 7, 1, [32]byte{2})
	require.NoError(t, err)
	require.Equal(t, created.ID, active.ID)
	require.Equal(t, [32]byte{2}, active.DefinitionDigest)
}

func TestMandatesRoundTripAndApproverCheck(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedAgentIdentityPolicyRevision(t, pool)
	bindings := NewAgentIdentityBindings(pool)
	mandates := NewMandates(pool)
	ctx := context.Background()

	binding, err := bindings.CreateBinding(ctx, proposedTestBinding(t, "0192f5e2-7c3a-7b1e-8a9d-000000000031", 1, 3))
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Microsecond)
	m, err := mandate.New("0192f5e2-7c3a-7b1e-8a9d-000000000032", mandate.Request{
		TenantProjectID: 7, Kind: mandate.KindAutonomous, AgentBindingID: binding.ID,
		OperationScopes: []string{"github.issues.read"}, ExpiresAt: now.Add(time.Hour),
	}, agentIdentityTTL(24*time.Hour), agentIdentityTestRevision, "100", now)
	require.NoError(t, err)

	created, err := mandates.CreateMandate(ctx, m)
	require.NoError(t, err)
	require.Equal(t, []string{"github.issues.read"}, created.OperationScopes)
	require.Empty(t, created.ResourceScopes)
	require.Nil(t, created.SubjectUserID)
	require.True(t, created.ExpiresAt.Equal(m.ExpiresAt))

	_, err = pool.Exec(ctx, `UPDATE elitea_identity.mandates SET approved_by = issued_by WHERE id = $1::uuid`, created.ID)
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr))
	require.Equal(t, mandateApproverCheck, pgErr.ConstraintName)

	approved, err := mandates.TransitionMandate(ctx, created.ID, func(current mandate.Mandate) (mandate.Mandate, error) {
		return current.Transition(mandate.ActionApprove, "200", agentIdentityTestRevision, time.Now().UTC())
	})
	require.NoError(t, err)
	require.Equal(t, mandate.StateActive, approved.State)
	require.Equal(t, int64(1), approved.RevocationEpoch)

	listed, err := mandates.ListMandates(ctx, mandate.ListFilter{TenantProjectID: 7, State: mandate.StateActive})
	require.NoError(t, err)
	require.Len(t, listed, 1)

	// An autonomous mandate naming a user is refused by the table as well.
	_, err = pool.Exec(ctx, `UPDATE elitea_identity.mandates SET subject_user_id = 5 WHERE id = $1::uuid`, created.ID)
	require.Error(t, err)
}
