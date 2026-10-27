package agentid_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/agentid"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/entratest"
)

// Delegated creation: an object tagged with the record only is adopted,
// given elitea:managed, and from then on resolves, lists and gets tokens.
// A record-tagged object of another blueprint is never adopted.
func TestAdoptionTagsTheObjectManaged(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	record := uuid.NewString()
	foreign := h.mock.AddAgent(entratest.AgentIdentity{DisplayName: "foreign", BlueprintID: uuid.NewString(),
		AccountEnabled: true, Tags: []string{"elitea:record:" + record}})
	pre := h.mock.AddAgent(entratest.AgentIdentity{DisplayName: "admin made", AccountEnabled: true,
		Tags: []string{"elitea:record:" + record, "team:x"}})

	result, err := h.provider.Provision(ctx, provisionSpec(record, 1))
	require.NoError(t, err)
	require.Equal(t, pre.ID, result.Ref.ID)
	object, _ := h.mock.Agent(pre.ID)
	require.False(t, object.AccountEnabled)
	require.ElementsMatch(t, []string{"elitea:record:" + record, "team:x", "elitea:managed"}, object.Tags)
	untouched, _ := h.mock.Agent(foreign.ID)
	require.True(t, untouched.AccountEnabled)
	require.Equal(t, []string{"elitea:record:" + record}, untouched.Tags)

	require.NoError(t, h.provider.Activate(ctx, result.Ref))
	resolved, err := h.provider.Resolve(ctx, result.Ref)
	require.NoError(t, err)
	require.True(t, resolved.Enabled)
	page, err := h.provider.List(ctx, agentidentity.ListSpec{})
	require.NoError(t, err)
	require.Len(t, page.Objects, 1)
	require.Equal(t, pre.ID, page.Objects[0].Ref.ID)
	_, err = h.provider.AssumeApp(ctx, h.appSpec(result.Ref))
	require.NoError(t, err)

	// Only a foreign-blueprint copy exists: a new identity is created.
	other := uuid.NewString()
	h.mock.AddAgent(entratest.AgentIdentity{DisplayName: "foreign2", BlueprintID: uuid.NewString(),
		Tags: []string{"elitea:managed", "elitea:record:" + other}})
	created, err := h.provider.Provision(ctx, provisionSpec(other, 2))
	require.NoError(t, err)
	mine, _ := h.mock.Agent(created.Ref.ID)
	require.Equal(t, h.mock.BlueprintAppID, mine.BlueprintID)
}

func tokenRequestCount(mock *entratest.Server) int {
	return len(mock.TokenRequests()) + len(mock.Requests())
}

// A disabled identity is refused locally, before any token request, however
// this process came to know it: just provisioned, or resolved disabled.
func TestDisabledIdentitiesAreRefusedBeforeAnyTokenRequest(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	provisioned, err := h.provider.Provision(ctx, provisionSpec(uuid.NewString(), 1))
	require.NoError(t, err)
	before := tokenRequestCount(h.mock)
	_, err = h.provider.AssumeApp(ctx, h.appSpec(provisioned.Ref))
	require.ErrorIs(t, err, agentid.ErrIdentityDisabled)
	_, err = h.provider.AssumeOnBehalfOf(ctx, h.oboSpec(provisioned.Ref, h.mock.BlueprintAudience))
	require.ErrorIs(t, err, agentid.ErrIdentityDisabled)
	object, _ := h.mock.Agent(provisioned.Ref.ID)
	_, err = h.provider.AssertionSource().AgentAssertion(ctx, tenant, object.AppID)
	require.ErrorIs(t, err, agentid.ErrIdentityDisabled)
	require.Equal(t, before, tokenRequestCount(h.mock), "no token request for a provisioned, disabled identity")

	// Cached while enabled, then read disabled by Resolve: evicted.
	ref := h.activeAgent(t, 2)
	_, err = h.provider.AssumeApp(ctx, h.appSpec(ref))
	require.NoError(t, err)
	require.True(t, h.mock.SetAccountEnabled(ref.ID, false))
	resolved, err := h.provider.Resolve(ctx, ref)
	require.NoError(t, err)
	require.False(t, resolved.Enabled)
	before = tokenRequestCount(h.mock)
	_, err = h.provider.AssumeApp(ctx, h.appSpec(ref))
	require.ErrorIs(t, err, agentid.ErrIdentityDisabled)
	require.Equal(t, before, tokenRequestCount(h.mock))

	// Cached while enabled, then its tags removed out of band: Resolve
	// reports replaced and evicts, and the token path refuses it locally.
	owned := h.activeAgent(t, 3)
	_, err = h.provider.AssumeApp(ctx, h.appSpec(owned))
	require.NoError(t, err)
	require.True(t, h.mock.DeleteAgent(owned.ID))
	stripped := h.mock.AddAgent(entratest.AgentIdentity{ID: owned.ID, DisplayName: "stripped", AccountEnabled: true})
	_, err = h.provider.Resolve(ctx, owned)
	require.ErrorIs(t, err, agentidentity.ErrIdentityReplaced)
	before = tokenRequestCount(h.mock)
	_, err = h.provider.AssumeApp(ctx, h.appSpec(identity.ExternalRef{Provider: agentid.ProviderName,
		ProviderTenant: tenant, Kind: agentid.RefKind, ID: stripped.ID}))
	require.ErrorIs(t, err, agentidentity.ErrIdentityReplaced)
	require.Equal(t, before, tokenRequestCount(h.mock))
}

// Graph's tag index lags: a retry on the same replica finds its object by
// id; a copy another replica created in the lag is healed on the next
// Provision, keeping the lowest object id on every replica.
func TestProvisionSurvivesTheIndexLag(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	record := uuid.NewString()
	first, err := h.provider.Provision(ctx, provisionSpec(record, 1))
	require.NoError(t, err)
	h.mock.SetIndexed(first.Ref.ID, false)
	again, err := h.provider.Provision(ctx, provisionSpec(record, 1))
	require.NoError(t, err)
	require.Equal(t, first.Ref, again.Ref)
	require.Len(t, h.mock.Agents(), 1, "the retry created no copy")

	// Another replica, inside the lag, creates a second copy.
	other := h.replica(t)
	second, err := other.Provision(ctx, provisionSpec(record, 1))
	require.NoError(t, err)
	require.NotEqual(t, first.Ref.ID, second.Ref.ID)
	require.Len(t, h.mock.Agents(), 2)

	h.mock.SetIndexed(first.Ref.ID, true)
	winner := min(first.Ref.ID, second.Ref.ID)
	for _, p := range []*agentid.Provider{h.replica(t), other, h.provider} {
		healed, err := p.Provision(ctx, provisionSpec(record, 1))
		require.NoError(t, err)
		require.Equal(t, winner, healed.Ref.ID)
	}
	agents := h.mock.Agents()
	require.Len(t, agents, 1)
	require.Equal(t, winner, agents[0].ID)
}

func TestProvisionRefusesOtherModes(t *testing.T) {
	h := newHarness(t)
	for _, mode := range []identity.IdentityMode{identity.IdentityModeEliteaNative, identity.IdentityModeRuntimeDomainBinding, ""} {
		spec := provisionSpec(uuid.NewString(), 1)
		spec.Mode = mode
		_, err := h.provider.Provision(context.Background(), spec)
		require.ErrorIs(t, err, agentid.ErrModeNotSupported)
	}
	require.Zero(t, h.mock.Calls())
}

// App-only takes exactly one <resource>/.default; a delegated scope is for
// OBO only.
func TestAppOnlyTakesOneDefaultScope(t *testing.T) {
	h := newHarness(t)
	ref := h.activeAgent(t, 1)
	before := h.mock.Calls()
	for name, scopes := range map[string][]string{
		"delegated scope": {graphSites},
		"two scopes":      {graphDefault, graphSites},
	} {
		_, err := h.provider.AssumeApp(context.Background(), h.appSpec(ref, scopes...))
		require.ErrorIs(t, err, agentid.ErrScopeNotAllowed, name)
	}
	require.Equal(t, before, h.mock.Calls())
	_, err := h.provider.AssumeOnBehalfOf(context.Background(), h.oboSpec(ref, h.mock.BlueprintAudience))
	require.NoError(t, err, "OBO keeps the delegated scope")

	for name, scopes := range map[string][]string{
		"malformed default": {"https://graph.microsoft.com.default"},
		"duplicate":         {graphDefault, graphDefault},
	} {
		cfg := h.cfg
		cfg.AllowedResourceScopes = scopes
		require.ErrorIs(t, cfg.Validate(), agentid.ErrInvalidConfiguration, name)
	}
}

// Concurrent callers share one Graph token refresh.
func TestTheGraphTokenIsRefreshedOnce(t *testing.T) {
	h := newHarness(t)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.provider.List(context.Background(), agentidentity.ListSpec{})
			require.NoError(t, err)
		}()
	}
	wg.Wait()
	graphTokens := slices.DeleteFunc(h.mock.TokenRequests(), func(r entratest.TokenRequest) bool {
		return !strings.HasPrefix(r.Scope, entratest.GraphResource)
	})
	require.Len(t, graphTokens, 1)
}
