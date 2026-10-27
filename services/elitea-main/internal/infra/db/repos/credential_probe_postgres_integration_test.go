package repos

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// Issue #1120: the probe reads only active, unexpired grants of the
// reference that list the capability, and its audit event lands in the
// outbox. CI-only: skipped without the integration database.
func TestCredentialProbeGrantsAndAudit(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedGrantWorld(t, pool)
	store := NewCredentialGrants(pool, agentIdentityAudit(t))
	ctx := context.Background()

	// A use-only grant, approved: not a probe grant.
	created, err := store.CreateGrant(ctx, proposedTestGrant(t, "0192f5e2-7c3a-7b1e-8a9d-000000001201", grantTestTargetA))
	require.NoError(t, err)
	_, err = store.TransitionGrant(ctx, 7, created.ID, grantTransitionBy(credentialgrant.ActionApprove, "200"))
	require.NoError(t, err)
	rows, err := store.ActiveGrantsWithCapability(ctx, 7, grantTestCredential, credentialgrant.CapProbe)
	require.NoError(t, err)
	require.Empty(t, rows)
	rows, err = store.ActiveGrantsWithCapability(ctx, 7, grantTestCredential, credentialgrant.CapUse)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	// Revoke it and propose a probe grant: proposed is not active.
	_, err = store.TransitionGrant(ctx, 7, created.ID, grantTransitionBy(credentialgrant.ActionRevoke, "100"))
	require.NoError(t, err)
	probeGrant := proposedTestGrant(t, "0192f5e2-7c3a-7b1e-8a9d-000000001202", grantTestTargetA)
	probeGrant.Capabilities = []credentialgrant.Capability{credentialgrant.CapProbe}
	probeGrant, err = store.CreateGrant(ctx, probeGrant)
	require.NoError(t, err)
	rows, err = store.ActiveGrantsWithCapability(ctx, 7, grantTestCredential, credentialgrant.CapProbe)
	require.NoError(t, err)
	require.Empty(t, rows)
	_, err = store.TransitionGrant(ctx, 7, probeGrant.ID, grantTransitionBy(credentialgrant.ActionApprove, "200"))
	require.NoError(t, err)
	rows, err = store.ActiveGrantsWithCapability(ctx, 7, grantTestCredential, credentialgrant.CapProbe)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, probeGrant.ID, rows[0].ID)

	// Another tenant and a malformed id read nothing.
	rows, err = store.ActiveGrantsWithCapability(ctx, 8, grantTestCredential, credentialgrant.CapProbe)
	require.NoError(t, err)
	require.Empty(t, rows)
	rows, err = store.ActiveGrantsWithCapability(ctx, 7, "nope", credentialgrant.CapProbe)
	require.NoError(t, err)
	require.Empty(t, rows)

	// Expired is not active.
	_, err = pool.Exec(ctx, `UPDATE elitea_identity.credential_use_grants SET expires_at = now() - interval '1 minute' WHERE id = $1::uuid`, probeGrant.ID)
	require.NoError(t, err)
	rows, err = store.ActiveGrantsWithCapability(ctx, 7, grantTestCredential, credentialgrant.CapProbe)
	require.NoError(t, err)
	require.Empty(t, rows)

	require.NoError(t, store.RecordProbe(ctx, securityaudit.SecurityEvent{
		Type: securityaudit.TypeProbe, Time: time.Now().UTC(), TenantProjectID: 7, Action: "credential.probe",
		Effect: securityaudit.EffectAllow, Reasons: []string{"probe.unavailable"}, PrincipalRef: "user:200",
		GrantRef: probeGrant.ID, ResourceType: "credential_reference", ResourceID: grantTestCredential,
	}))
	events := auditEvents(t, pool, securityaudit.TypeProbe, grantTestCredential)
	require.Len(t, events, 1)
	require.Equal(t, "credential.probe", events[0].Action)
	require.Error(t, store.RecordProbe(ctx, securityaudit.SecurityEvent{Type: securityaudit.TypeDecision}))
}
