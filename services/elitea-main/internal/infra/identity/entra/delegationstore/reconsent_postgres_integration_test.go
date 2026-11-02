package delegationstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// #1477: a re-consent delegates only from a sign-in stored after the run
// paused. The assertion that already failed is never used again, and the
// new delegation resolves the pause.
func TestReconsentDelegatesOnlyFromANewSignIn(t *testing.T) {
	pool := newCustodyPool(t)
	store := newTestStore(t, pool)
	ctx := context.Background()

	_, err := store.RetainSignIn(ctx, signInFor("session-1"))
	require.NoError(t, err)
	paused, err := store.Delegate(ctx, delegateFor("session-1"))
	require.NoError(t, err)
	recorded, err := store.RecordRequirement(ctx, Requirement{
		TenantProjectID: 7, ActorUserID: 42,
		ConversationUUID: "0192f5e2-7c3a-7b1e-8a9d-0000000000c1", ResponseMessageID: "0192f5e2-7c3a-7b1e-8a9d-0000000000c2",
		ExecutionID: "exec-1", ExecutionGeneration: "3", DelegationID: paused.ID,
		Scopes: []string{"https://graph.microsoft.com/Sites.Selected"}, Reason: ReasonInteractionRequired,
	})
	require.NoError(t, err)
	requirement, err := store.RequirementFor(ctx, recorded.Reference, 7, 42)
	require.NoError(t, err)
	_, err = store.RequirementFor(ctx, recorded.Reference, 8, 42)
	require.ErrorIs(t, err, ErrRequirementNotFound, "another tenant project cannot read it")

	reconsent := delegateFor("session-1")
	reconsent.SignedInAfter = requirement.CreatedAt
	_, err = store.Delegate(ctx, reconsent)
	require.ErrorIs(t, err, ErrNoSignInAssertion, "the sign-in from before the pause is not used")

	// The user signs in again: the OIDC callback stores a new assertion.
	time.Sleep(2 * time.Millisecond)
	_, err = store.RetainSignIn(ctx, signInFor("session-1"))
	require.NoError(t, err)
	answer, err := store.Delegate(ctx, reconsent)
	require.NoError(t, err)
	require.NotEqual(t, paused.ID, answer.ID)
	resolved, err := store.ResolveRequirement(ctx, recorded.Reference, 7, 42, answer.ID)
	require.NoError(t, err)
	require.True(t, resolved.Resolved)
	require.Equal(t, answer.ID, resolved.ResolvedDelegationID)
}
