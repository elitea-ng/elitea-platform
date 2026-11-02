package provideroauth

// Shared 0219 (issue #1464): the consent stores the Atlassian gateway base
// of the target site, the token source releases it, and a consent that does
// not cover the target site stores no grant. CI-only: skipped without the
// integration database.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/refresh"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
)

func TestConsentStoresTheGatewayBaseAndTheTokenSourceReleasesIt(t *testing.T) {
	config := newOAuthDatabase(t)
	pool := openPool(t, config)
	provider, srv := newRotatingProvider(t, tRedirect)
	client := jiraClient(t, srv.URL, tRedirect)
	consent := newTestConsent(t, pool, client)
	ctx := context.Background()

	started, err := consent.Start(ctx, tStart)
	require.NoError(t, err)
	state := provider.authorize(t, started.AuthorizationURL, "code-base")
	_, err = consent.Callback(ctx, CallbackRequest{Provider: "jira", State: state, Code: "code-base"}, asUser(tUser))
	require.NoError(t, err)

	leases, err := refresh.NewLeases(pool)
	require.NoError(t, err)
	coordinator, err := refresh.NewCoordinator(leases, refresh.Config{Holder: "base"})
	require.NoError(t, err)
	source, err := NewTokenSource(TokenSourceConfig{
		Grants: newTestGrants(t, pool), Leases: leases, Coordinator: coordinator, Clients: []*Client{client},
	})
	require.NoError(t, err)
	token, err := source.AccessToken(ctx, tKey)
	require.NoError(t, err)
	require.Equal(t, AtlassianAPIURL+"/ex/jira/"+tCloudID, token.APIBase)

	// A stored base this client could not have written is never released.
	_, err = pool.Exec(ctx, `UPDATE elitea_identity.provider_oauth_grants SET api_base_url = 'https://evil.example/ex/jira/x'`)
	require.NoError(t, err)
	token, err = source.AccessToken(ctx, tKey)
	require.NoError(t, err)
	require.Empty(t, token.APIBase)
}

func TestConsentForASiteTheTokenCannotReachStoresNoGrant(t *testing.T) {
	config := newOAuthDatabase(t)
	pool := openPool(t, config)
	provider, srv := newRotatingProvider(t, tRedirect)
	client := jiraClient(t, srv.URL, tRedirect)
	consent, err := NewConsent(ConsentConfig{
		Grants: newTestGrants(t, pool), Clients: []*Client{client},
		Targets:  fixedTargets{tTarget: {ID: tTarget, TenantProjectID: tTenant, ProviderType: "jira", TargetIdentity: "https://other.atlassian.net"}},
		Bindings: fixedBindings{tBinding: {ID: tBinding, TenantProjectID: tTenant, State: agentidentity.BindingActive}},
	})
	require.NoError(t, err)
	ctx := context.Background()
	started, err := consent.Start(ctx, tStart)
	require.NoError(t, err)
	state := provider.authorize(t, started.AuthorizationURL, "code-other")
	_, err = consent.Callback(ctx, CallbackRequest{Provider: "jira", State: state, Code: "code-other"}, asUser(tUser))
	require.ErrorIs(t, err, ErrSiteNotAccessible)
	_, err = consent.grants.load(ctx, tKey)
	require.ErrorIs(t, err, ErrConsentRequired)
}
