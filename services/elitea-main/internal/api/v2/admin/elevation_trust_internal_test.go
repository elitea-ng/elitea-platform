package admin

// Identity trust under an elevation (ADR-0059 decision 5; story R2.8a, issue
// 2522). The identity provider save classifies in its handler, so the
// restriction of the action table decides there: internal/api's middleware
// puts the elevated caller on the context when the save's outcomes disagree
// (an approved elevation, or a sealed one at the admin level), and leaves a
// sealed super_admin elevation to pass, flagged, to the trust gate, which
// reads the sealed kind's full resolution.
//
//   - an admin-level elevation that changes the issuer: 403 elevation_restricted;
//   - an approved super_admin-level one: the same;
//   - a sealed super_admin-level one: changes it, and the row is flagged;
//   - a sealed admin-level one: 403 elevation_restricted;
//   - an elevated person who changes the display name: the row is flagged.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// elevatedResolver answers a resolution with no standing set and the level's
// role set given by one active elevation on path.
type elevatedResolver struct{ level, path string }

func (e elevatedResolver) resolution() auth.PermissionResolution {
	role, _ := adminaccess.LookupBuiltInRole(e.level)
	const id = "0b7c2d4e-0000-4000-8000-0000000e1e71"
	set := role.Set()
	return auth.PermissionResolution{
		UserID: 5, Permissions: set, ElevatedPermissions: set, ElevationSessionRef: 9,
		ElevationID: id, ElevationIDs: []string{id},
		Elevations: []auth.ElevationGrant{{ID: id, Level: e.level, Path: e.path}},
	}
}

func (e elevatedResolver) ResolvePermissions(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
	return e.resolution(), nil
}

// elevatedSave sends the save as a browser session of kind under the
// elevation, with the middleware's half done: the caller on the context when
// deferred is true.
func elevatedSave(t *testing.T, kind auth.CredentialKind, elevation elevatedResolver, deferred bool, body string) (
	*recordingProviderStore, int, map[string]any, bool,
) {
	t.Helper()
	store := storeWithCorporate()
	handler := NewHandler(nil, WithIdentityProviders(store, newRecordingVault()), WithPermissionResolver(elevation))
	request := providerRequest(http.MethodPut, "corporate", body)
	principal := auth.User{ID: "5", UserID: "5", CredentialKind: kind, BrowserSessionRef: 9}
	ctx := auth.ContextWithUser(request.Context(), principal)
	if deferred {
		ctx = adminaccess.ContextWithElevationCaller(ctx,
			adminaccess.ElevationCallerOf(principal.Credential(), elevation.resolution()))
	}
	recorder, annotation := audited(request.WithContext(ctx), handler.IdentityProviderSave)
	var decoded map[string]any
	if recorder.Code != http.StatusOK {
		decoded = decodedBody(t, recorder)
	}
	return store, recorder.Code, decoded, annotation.ElevationFlagged
}

func TestAnApprovedElevationCannotChangeAProvidersIssuer(t *testing.T) {
	for _, level := range []string{adminaccess.ElevationLevelAdmin, adminaccess.ElevationLevelSuperAdmin} {
		store, status, body, _ := elevatedSave(t, auth.CredentialKindSession,
			elevatedResolver{level: level, path: adminaccess.ElevationPathApproved}, true, attackerIssuerBody)
		require.Equal(t, http.StatusForbidden, status, "%s: %v", level, body)
		require.Equal(t, adminaccess.CodeElevationRestricted, body["code"], level)
		require.Empty(t, store.upserted, level)
		require.Equal(t, "https://idp.example.com", store.rows["corporate"].OIDC.Issuer, level)
	}
}

func TestASealedAdminElevationCannotChangeAProvidersIssuer(t *testing.T) {
	store, status, body, _ := elevatedSave(t, auth.CredentialKindSealed,
		elevatedResolver{level: adminaccess.ElevationLevelAdmin, path: adminaccess.ElevationPathSealed}, true, attackerIssuerBody)
	require.Equal(t, http.StatusForbidden, status, "%v", body)
	require.Equal(t, adminaccess.CodeElevationRestricted, body["code"])
	require.Empty(t, store.upserted)
}

// The sealed super_admin elevation: every outcome agrees (admitted, flagged),
// so the middleware flags and defers nothing; the trust gate reads the
// sealed kind's full resolution and admits it.
func TestASealedSuperAdminElevationChangesAProvidersIssuerFlagged(t *testing.T) {
	store, status, body, flagged := elevatedSave(t, auth.CredentialKindSealed,
		elevatedResolver{level: adminaccess.ElevationLevelSuperAdmin, path: adminaccess.ElevationPathSealed}, false, attackerIssuerBody)
	require.Equal(t, http.StatusOK, status, "%v", body)
	require.Equal(t, "https://attacker.example", store.rows["corporate"].OIDC.Issuer)
	require.True(t, flagged, "the trust change under the sealed elevation is not flagged")
}

// The trust gate opens to the sealed kind only: the same resolution on an
// ordinary browser session (an approved elevation that somehow reached the
// handler with no deferred caller) is refused by the trust gate.
func TestTheTrustGateOpensToTheSealedKindOnly(t *testing.T) {
	store, status, body, _ := elevatedSave(t, auth.CredentialKindSession,
		elevatedResolver{level: adminaccess.ElevationLevelSuperAdmin, path: adminaccess.ElevationPathSealed}, false, attackerIssuerBody)
	require.Equal(t, http.StatusForbidden, status, "%v", body)
	require.Equal(t, CodeIdentityTrustSuperAdminRequired, body["code"])
	require.Empty(t, store.upserted)
}

func TestAnElevatedDisplayNameChangeIsFlagged(t *testing.T) {
	renamed := strings.Replace(validOIDCBody, `"Corporate SSO"`, `"Sign in with Corp"`, 1)
	store, status, body, flagged := elevatedSave(t, auth.CredentialKindSession,
		elevatedResolver{level: adminaccess.ElevationLevelAdmin, path: adminaccess.ElevationPathApproved}, true, renamed)
	require.Equal(t, http.StatusOK, status, "%v", body)
	require.Equal(t, "Sign in with Corp", store.rows["corporate"].DisplayName)
	require.True(t, flagged, "a settings change under an elevation is not flagged")
}
