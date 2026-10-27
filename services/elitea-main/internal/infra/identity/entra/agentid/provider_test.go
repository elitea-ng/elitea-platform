package agentid_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/agentid"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/delegationstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/entratest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/obo"
)

const (
	tenant       = "aaaaaaaa-0000-4000-8000-000000000001"
	otherTenant  = "bbbbbbbb-0000-4000-8000-000000000002"
	userOID      = "cccccccc-0000-4000-8000-000000000003"
	sponsorGroup = "dddddddd-0000-4000-8000-000000000004"
	graphAud     = "https://graph.microsoft.com"
	graphDefault = "https://graph.microsoft.com/.default"
	graphSites   = "https://graph.microsoft.com/Sites.Selected"
	delegationID = "eeeeeeee-0000-4000-8000-000000000005"
	bindingID    = "ffffffff-0000-4000-8000-000000000006"
	execution    = "exec-1152"
	generation   = int64(3)
)

// sessionStore is the SessionReader fake: one session per id.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]agentidentity.AgentSession
}

func (s *sessionStore) SessionByID(_ context.Context, id string) (agentidentity.AgentSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		return agentidentity.AgentSession{}, agentidentity.ErrSessionNotFound
	}
	return session, nil
}

// custody is the DelegationCustody fake.
type custody struct {
	mu        sync.Mutex
	assertion obo.Assertion
	err       error
	calls     int
}

func (c *custody) Load(_ context.Context, r delegationstore.LoadRequest) (obo.Assertion, delegationstore.Delegation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.err != nil {
		return obo.Assertion{}, delegationstore.Delegation{}, c.err
	}
	if r.DelegationID != delegationID || r.AgentBindingID != bindingID || r.TenantProjectID != 7 {
		return obo.Assertion{}, delegationstore.Delegation{}, delegationstore.ErrDelegationNotFound
	}
	return c.assertion, delegationstore.Delegation{ID: delegationID}, nil
}

type harness struct {
	mock      *entratest.Server
	provider  *agentid.Provider
	sessions  *sessionStore
	custody   *custody
	logs      *bytes.Buffer
	tokenFile string
	cfg       agentid.Config
}

func newHarness(t *testing.T, mutate ...func(*agentid.Config)) *harness {
	t.Helper()
	mock := entratest.New(t)
	h := &harness{mock: mock, logs: &bytes.Buffer{}, custody: &custody{},
		sessions: &sessionStore{sessions: map[string]agentidentity.AgentSession{}}}
	h.tokenFile = filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(h.tokenFile, []byte(mock.BlueprintAssertion()+"\n"), 0o600))
	cfg := agentid.Config{
		TenantID: tenant, BlueprintAppID: mock.BlueprintAppID, FederatedTokenFile: h.tokenFile,
		Authority: mock.URL, GraphBaseURL: mock.URL,
		AllowedResourceScopes: []string{graphDefault, graphSites},
		DefaultSponsorGroupID: sponsorGroup,
		AssertionAudiences:    []string{mock.BlueprintAudience},
		Sessions:              h.sessions, Delegations: h.custody, HTTPClient: mock.Client(),
		Logger: slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	for _, m := range mutate {
		m(&cfg)
	}
	provider, err := agentid.New(cfg)
	require.NoError(t, err)
	h.provider, h.cfg = provider, cfg
	return h
}

// replica builds another Provider on the same mock: a second elitea-main
// replica with its own caches.
func (h *harness) replica(t *testing.T) *agentid.Provider {
	t.Helper()
	provider, err := agentid.New(h.cfg)
	require.NoError(t, err)
	return provider
}

// session registers a session of the run and returns its id.
func (h *harness) session(delegation string) string {
	id := uuid.NewString()
	h.sessions.mu.Lock()
	defer h.sessions.mu.Unlock()
	h.sessions.sessions[id] = agentidentity.AgentSession{ID: id, TenantProjectID: 7, ExecutionID: execution,
		Generation: generation, AgentBindingID: bindingID, DelegationID: delegation}
	return id
}

func provisionSpec(record string, application int64) agentidentity.ProvisionSpec {
	return agentidentity.ProvisionSpec{ExternalAgentIdentityID: record, TenantProjectID: 7, ApplicationID: application,
		Mode: identity.IdentityModeDedicatedAgent, ProviderTenant: tenant}
}

// activeAgent provisions and activates one agent identity.
func (h *harness) activeAgent(t *testing.T, application int64) identity.ExternalRef {
	t.Helper()
	result, err := h.provider.Provision(context.Background(), provisionSpec(uuid.NewString(), application))
	require.NoError(t, err)
	require.NoError(t, h.provider.Activate(context.Background(), result.Ref))
	return result.Ref
}

func (h *harness) appSpec(ref identity.ExternalRef, scopes ...string) agentidentity.AssumeSpec {
	if len(scopes) == 0 {
		scopes = []string{graphDefault}
	}
	return agentidentity.AssumeSpec{Ref: ref, SessionID: h.session(""), Audience: graphAud, Scopes: scopes, TTL: 30 * time.Minute}
}

func (h *harness) oboSpec(ref identity.ExternalRef, audience string) agentidentity.OBOSpec {
	h.custody.assertion = obo.Assertion{
		Token:    h.mock.AccessToken(tenant, userOID, audience, "access_agent", time.Hour),
		TenantID: tenant, ObjectID: userOID, Audience: audience, ExpiresAt: time.Now().Add(time.Hour),
	}
	return agentidentity.OBOSpec{
		AssumeSpec: agentidentity.AssumeSpec{Ref: ref, SessionID: h.session(delegationID), Audience: graphAud,
			Scopes: []string{graphSites}, TTL: 30 * time.Minute},
		Evidence: agentidentity.VerifiedUserEvidence{Issuer: h.mock.Issuer(tenant), TenantID: tenant, ObjectID: userOID,
			Audience: audience, DelegationID: delegationID},
	}
}

func claimsOf(t *testing.T, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var claims map[string]any
	require.NoError(t, json.Unmarshal(raw, &claims))
	return claims
}

// Acceptance: Provision creates a disabled agent identity; a repeat call
// returns the same object.
func TestProvisionCreatesADisabledIdentityAndIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	record := uuid.NewString()
	first, err := h.provider.Provision(ctx, provisionSpec(record, 41))
	require.NoError(t, err)
	require.Equal(t, agentid.ProviderName, first.Ref.Provider)
	require.Equal(t, agentid.RefKind, first.Ref.Kind)
	require.Equal(t, tenant, first.Ref.ProviderTenant)
	require.NoError(t, first.Ref.Validate(true))

	object, ok := h.mock.Agent(first.Ref.ID)
	require.True(t, ok)
	require.False(t, object.AccountEnabled, "created disabled")
	require.Equal(t, h.mock.BlueprintAppID, object.BlueprintID)
	require.Equal(t, "Elitea agent 41", object.DisplayName)
	require.Contains(t, object.Tags, "elitea:record:"+record)
	require.Contains(t, object.Tags, "elitea:managed")
	require.Equal(t, []string{h.mock.URL + "/v1.0/groups/" + sponsorGroup}, object.Sponsors)

	again, err := h.provider.Provision(ctx, provisionSpec(record, 41))
	require.NoError(t, err)
	require.Equal(t, first.Ref, again.Ref)
	require.Len(t, h.mock.Agents(), 1)

	// Delegated creation: an identity an administrator created with the
	// record's tag is adopted, and disabled until Activate.
	adoptedRecord := uuid.NewString()
	pre := h.mock.AddAgent(entratest.AgentIdentity{DisplayName: "pre-created", AccountEnabled: true,
		Tags: []string{"elitea:managed", "elitea:record:" + adoptedRecord}})
	adopted, err := h.provider.Provision(ctx, provisionSpec(adoptedRecord, 42))
	require.NoError(t, err)
	require.Equal(t, pre.ID, adopted.Ref.ID)
	object, _ = h.mock.Agent(pre.ID)
	require.False(t, object.AccountEnabled)
}

func TestProvisionRefusesAtTheIdentityLimitAndWithoutASponsor(t *testing.T) {
	h := newHarness(t, func(c *agentid.Config) { c.IdentityLimit = 2 })
	ctx := context.Background()
	record := uuid.NewString()
	_, err := h.provider.Provision(ctx, provisionSpec(record, 1))
	require.NoError(t, err)
	_, err = h.provider.Provision(ctx, provisionSpec(uuid.NewString(), 2))
	require.NoError(t, err)
	_, err = h.provider.Provision(ctx, provisionSpec(uuid.NewString(), 3))
	require.ErrorIs(t, err, agentid.ErrIdentityLimit)
	require.Len(t, h.mock.Agents(), 2)
	// A repeat of an existing record is not a creation.
	_, err = h.provider.Provision(ctx, provisionSpec(record, 1))
	require.NoError(t, err)
	require.Equal(t, 2, h.provider.Capabilities().IdentityLimit)

	unsponsored := newHarness(t, func(c *agentid.Config) {
		c.DefaultSponsorGroupID = ""
		c.SponsorGroups = map[int64]string{9: sponsorGroup}
	})
	_, err = unsponsored.provider.Provision(ctx, provisionSpec(uuid.NewString(), 1))
	require.ErrorIs(t, err, agentid.ErrNoSponsor)
	require.Zero(t, unsponsored.mock.Calls())
}

// Acceptance: the app-only token has idtyp=app, azp = the agent and
// xms_par_app_azp = the blueprint.
func TestAppOnlyTokenIsIssuedToTheAgent(t *testing.T) {
	h := newHarness(t)
	ref := h.activeAgent(t, 1)
	object, _ := h.mock.Agent(ref.ID)

	credential, err := h.provider.AssumeApp(context.Background(), h.appSpec(ref))
	require.NoError(t, err)
	claims := claimsOf(t, credential.Token)
	require.Equal(t, "app", claims["idtyp"])
	require.Equal(t, object.AppID, claims["azp"])
	require.Equal(t, h.mock.BlueprintAppID, claims["xms_par_app_azp"])
	require.Equal(t, tenant, claims["tid"])
	require.Equal(t, agentidentity.FidelityConfiguredAct, credential.Actor.Fidelity)
	require.Equal(t, agentidentity.Principal{Kind: "agent", Ref: ref.ID}, credential.Actor.Current)
	require.Nil(t, credential.Actor.Subject)
	require.WithinDuration(t, time.Now().Add(30*time.Minute), credential.ExpiresAt, time.Minute)

	// T1 was minted with fmi_path = the agent, and every token request of
	// the run carried the run's client-request-id.
	want := obo.CorrelationID(execution, generation)
	var sawT1, sawAgent bool
	for _, r := range h.mock.TokenRequests() {
		if r.FMIPath == object.AppID {
			sawT1 = true
			require.Equal(t, want, r.ClientRequestID)
		}
		if r.ClientID == object.AppID {
			sawAgent = true
			require.Equal(t, want, r.ClientRequestID)
		}
	}
	require.True(t, sawT1 && sawAgent)
	require.Contains(t, h.logs.String(), `"uti"`)
}

func TestTheFederatedTokenIsReReadOnEveryRequest(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, os.WriteFile(h.tokenFile, []byte("stale"), 0o600))
	_, err := h.provider.Provision(context.Background(), provisionSpec(uuid.NewString(), 1))
	require.ErrorIs(t, err, agentid.ErrRejected)
	h.mock.SetBlueprintAssertion("rotated-projected-token")
	require.NoError(t, os.WriteFile(h.tokenFile, []byte("rotated-projected-token"), 0o600))
	_, err = h.provider.Provision(context.Background(), provisionSpec(uuid.NewString(), 1))
	require.NoError(t, err)
}

// Acceptance: Agent OBO through #1117 succeeds with aud = the blueprint; a
// middle-tier aud is refused before the exchange.
func TestAgentOBOThroughTheExchanger(t *testing.T) {
	h := newHarness(t)
	ref := h.activeAgent(t, 1)
	object, _ := h.mock.Agent(ref.ID)

	credential, err := h.provider.AssumeOnBehalfOf(context.Background(), h.oboSpec(ref, h.mock.BlueprintAudience))
	require.NoError(t, err)
	claims := claimsOf(t, credential.Token)
	require.Equal(t, "user", claims["idtyp"])
	require.Equal(t, object.AppID, claims["azp"])
	require.Equal(t, h.mock.BlueprintAppID, claims["xms_par_app_azp"])
	require.Equal(t, userOID, claims["oid"])
	require.Equal(t, &agentidentity.Principal{Kind: "user", Ref: userOID}, credential.Actor.Subject)
	require.Equal(t, agentidentity.FidelityConfiguredAct, credential.Actor.Fidelity)
	requests := h.mock.Requests()
	require.Len(t, requests, 1)
	require.Equal(t, object.AppID, requests[0].ClientID)
	require.Equal(t, obo.CorrelationID(execution, generation), requests[0].ClientRequestID)

	calls, loads := h.mock.Calls(), h.custody.calls
	_, err = h.provider.AssumeOnBehalfOf(context.Background(), h.oboSpec(ref, h.mock.MiddleTierClientID))
	require.ErrorIs(t, err, agentid.ErrAudienceMismatch)
	require.Equal(t, calls, h.mock.Calls(), "refused before any Entra call")
	require.Equal(t, loads, h.custody.calls, "refused before the assertion left custody")

	// Evidence that names the blueprint over a middle-tier assertion is
	// refused before the exchange as well.
	spec := h.oboSpec(ref, h.mock.MiddleTierClientID)
	spec.Evidence = agentidentity.VerifiedUserEvidence{TenantID: tenant, ObjectID: userOID,
		Audience: h.mock.BlueprintAudience, DelegationID: delegationID}
	_, err = h.provider.AssumeOnBehalfOf(context.Background(), spec)
	require.ErrorIs(t, err, agentid.ErrAudienceMismatch)
	require.Len(t, h.mock.Requests(), 1)
}

func TestAgentOBOMapsCustodyAndEntraErrors(t *testing.T) {
	h := newHarness(t)
	ref := h.activeAgent(t, 1)
	ctx := context.Background()

	h.custody.err = delegationstore.ErrDelegationNotUsable
	_, err := h.provider.AssumeOnBehalfOf(ctx, h.oboSpec(ref, h.mock.BlueprintAudience))
	var interaction *agentidentity.InteractionRequired
	require.ErrorAs(t, err, &interaction)
	h.custody.err = nil

	for _, tc := range []struct {
		response entratest.Response
		want     error
	}{
		{entratest.InteractionRequired(), agentid.ErrInteractionRequired},
		{entratest.ClaimsChallenge(`{"access_token":{"nbf":{"essential":true}}}`), agentid.ErrClaimsChallenge},
		{entratest.Response{Status: http.StatusBadRequest, Body: map[string]any{"error": "invalid_grant", "error_codes": []int{82014}}}, agentid.ErrConsentNotGranted},
		{entratest.Response{Status: http.StatusServiceUnavailable, Body: map[string]any{"error": "x"}}, agentid.ErrUnavailable},
	} {
		h.mock.ScriptOBO(tc.response)
		_, err := h.provider.AssumeOnBehalfOf(ctx, h.oboSpec(ref, h.mock.BlueprintAudience))
		require.ErrorIs(t, err, tc.want)
	}
	_, err = h.provider.AssumeOnBehalfOf(ctx, h.oboSpec(ref, h.mock.BlueprintAudience))
	require.NoError(t, err)

	noCustody := newHarness(t, func(c *agentid.Config) { c.Delegations = nil })
	_, err = noCustody.provider.AssumeOnBehalfOf(ctx, noCustody.oboSpec(ref, noCustody.mock.BlueprintAudience))
	require.ErrorIs(t, err, agentid.ErrDelegationUnavailable)
}

// Acceptance: AssertedUserEvidence is refused before any Entra call.
func TestAssertedEvidenceIsRefusedBeforeAnyCall(t *testing.T) {
	h := newHarness(t)
	require.False(t, h.provider.Capabilities().AcceptsAssertedUser)
	ref := identity.ExternalRef{Provider: agentid.ProviderName, ProviderTenant: tenant, Kind: agentid.RefKind, ID: uuid.NewString()}
	for _, evidence := range []agentidentity.UserEvidence{
		agentidentity.AssertedUserEvidence{Issuer: "elitea", Subject: "user:1", MandateID: "m-1"},
		&agentidentity.AssertedUserEvidence{Issuer: "elitea", Subject: "user:1", MandateID: "m-1"},
	} {
		_, err := h.provider.AssumeOnBehalfOf(context.Background(), agentidentity.OBOSpec{
			AssumeSpec: h.appSpec(ref, graphSites), Evidence: evidence})
		require.ErrorIs(t, err, agentid.ErrAssertedUserRefused)
	}
	require.Zero(t, h.mock.Calls())
	require.Zero(t, h.custody.calls)
}

// Acceptance: a scope outside the session ceiling or allowed_resource_scopes
// is refused before any Entra call.
func TestScopesOutsideTheCeilingOrTheAllowListAreRefusedBeforeAnyCall(t *testing.T) {
	h := newHarness(t)
	ref := h.activeAgent(t, 1)
	before := h.mock.Calls()
	for name, spec := range map[string]agentidentity.AssumeSpec{
		"not allowed":         h.appSpec(ref, "https://graph.microsoft.com/Mail.Read"),
		"outside the ceiling": {Ref: ref, SessionID: h.session(""), Audience: "https://contoso.sharepoint.com", Scopes: []string{graphDefault}},
		"no scope":            {Ref: ref, SessionID: h.session(""), Audience: graphAud},
		"one of two bad":      h.appSpec(ref, graphDefault, "https://management.azure.com/.default"),
	} {
		_, err := h.provider.AssumeApp(context.Background(), spec)
		require.ErrorIs(t, err, agentid.ErrScopeNotAllowed, name)
		_, err = h.provider.AssumeOnBehalfOf(context.Background(), agentidentity.OBOSpec{AssumeSpec: spec,
			Evidence: agentidentity.VerifiedUserEvidence{TenantID: tenant, ObjectID: userOID,
				Audience: h.mock.BlueprintAudience, DelegationID: delegationID}})
		require.ErrorIs(t, err, agentid.ErrScopeNotAllowed, name)
	}
	require.Equal(t, before, h.mock.Calls())
	require.Zero(t, h.custody.calls)
}

// Acceptance: a suspended identity is accountEnabled=false at Entra and
// gets no new token; agent B is unaffected.
func TestSuspendedIdentityGetsNoTokensAndAgentBIsUnaffected(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := h.activeAgent(t, 1), h.activeAgent(t, 2)
	_, err := h.provider.AssumeApp(ctx, h.appSpec(a))
	require.NoError(t, err)
	_, err = h.provider.AssumeApp(ctx, h.appSpec(b))
	require.NoError(t, err)

	require.NoError(t, h.provider.Suspend(ctx, a))
	object, _ := h.mock.Agent(a.ID)
	require.False(t, object.AccountEnabled)
	_, err = h.provider.AssumeApp(ctx, h.appSpec(a))
	require.ErrorIs(t, err, agentid.ErrIdentityDisabled)
	_, err = h.provider.AssumeOnBehalfOf(ctx, h.oboSpec(a, h.mock.BlueprintAudience))
	require.ErrorIs(t, err, agentid.ErrIdentityDisabled)
	_, err = h.provider.AssumeApp(ctx, h.appSpec(b))
	require.NoError(t, err, "agent B is unaffected")
	_, err = h.provider.AssumeOnBehalfOf(ctx, h.oboSpec(b, h.mock.BlueprintAudience))
	require.NoError(t, err)

	// Disabled out of band while this replica holds B's T1: Entra refuses
	// the agent token and the error stays typed.
	require.True(t, h.mock.SetAccountEnabled(b.ID, false))
	_, err = h.provider.AssumeApp(ctx, h.appSpec(b))
	require.ErrorIs(t, err, agentid.ErrIdentityDisabled)
	resolved, err := h.provider.Resolve(ctx, b)
	require.NoError(t, err)
	require.False(t, resolved.Enabled)

	require.NoError(t, h.provider.Resume(ctx, a))
	_, err = h.provider.AssumeApp(ctx, h.appSpec(a))
	require.NoError(t, err)
}

// Acceptance: a replaced object (same display name, new object id) is
// ErrIdentityReplaced.
func TestReplacedObjectYieldsErrIdentityReplaced(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ref := h.activeAgent(t, 1)
	resolved, err := h.provider.Resolve(ctx, ref)
	require.NoError(t, err)
	require.True(t, resolved.Enabled)
	require.Equal(t, ref, resolved.Ref)

	recreated, ok := h.mock.RecreateAgent(ref.ID)
	require.True(t, ok)
	require.NotEqual(t, ref.ID, recreated.ID)
	_, err = h.provider.Resolve(ctx, ref)
	require.ErrorIs(t, err, agentidentity.ErrIdentityReplaced)
	_, err = h.provider.AssumeApp(ctx, h.appSpec(ref))
	require.Error(t, err)

	// Recreated by hand without Elitea's tags, under the same name.
	untagged := h.activeAgent(t, 2)
	object, _ := h.mock.Agent(untagged.ID)
	require.True(t, h.mock.DeleteAgent(untagged.ID))
	h.mock.AddAgent(entratest.AgentIdentity{DisplayName: object.DisplayName})
	_, err = h.provider.Resolve(ctx, untagged)
	require.ErrorIs(t, err, agentidentity.ErrIdentityReplaced)

	// Deleted with no successor: not found.
	gone := h.activeAgent(t, 3)
	require.True(t, h.mock.DeleteAgent(gone.ID))
	_, err = h.provider.Resolve(ctx, gone)
	require.ErrorIs(t, err, agentidentity.ErrIdentityNotFound)

	// An object id that now belongs to another blueprint, or lost the
	// back-reference tag, is not the recorded one.
	foreign := h.mock.AddAgent(entratest.AgentIdentity{DisplayName: "x", BlueprintID: uuid.NewString(),
		Tags: []string{"elitea:managed", "elitea:record:" + uuid.NewString()}})
	_, err = h.provider.Resolve(ctx, identity.ExternalRef{Provider: agentid.ProviderName, ProviderTenant: tenant,
		Kind: agentid.RefKind, ID: foreign.ID})
	require.ErrorIs(t, err, agentidentity.ErrIdentityReplaced)

	// Revoke soft-deletes, and is idempotent.
	revoked := h.activeAgent(t, 4)
	require.NoError(t, h.provider.Revoke(ctx, agentidentity.RevokeSpec{Ref: revoked, Reason: "retired"}))
	_, ok = h.mock.Agent(revoked.ID)
	require.False(t, ok)
	require.NoError(t, h.provider.Revoke(ctx, agentidentity.RevokeSpec{Ref: revoked, Reason: "retired"}))
}

func TestListPagesTheBlueprintsTaggedIdentities(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var want []string
	for i := range 5 {
		want = append(want, h.activeAgent(t, int64(i+1)).ID)
	}
	h.mock.AddAgent(entratest.AgentIdentity{DisplayName: "not elitea"})
	h.mock.AddAgent(entratest.AgentIdentity{DisplayName: "other", BlueprintID: uuid.NewString(), Tags: []string{"elitea:managed"}})

	var got []string
	token, pages := "", 0
	for {
		page, err := h.provider.List(ctx, agentidentity.ListSpec{PageToken: token, PageSize: 2})
		require.NoError(t, err)
		for _, object := range page.Objects {
			require.Equal(t, agentid.RefKind, object.Ref.Kind)
			require.True(t, object.Enabled)
			got = append(got, object.Ref.ID)
		}
		pages++
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
	}
	slices.Sort(got)
	slices.Sort(want)
	require.Equal(t, want, got)
	require.Equal(t, 3, pages)

	_, err := h.provider.List(ctx, agentidentity.ListSpec{PageToken: "https://evil.example/v1.0/servicePrincipals"})
	require.ErrorIs(t, err, agentid.ErrInvalidRequest)
	other, err := h.provider.List(ctx, agentidentity.ListSpec{ProviderTenant: otherTenant})
	require.NoError(t, err)
	require.Empty(t, other.Objects)
}

func TestIssuedTokensAreValidatedBeforeReturn(t *testing.T) {
	h := newHarness(t)
	ref := h.activeAgent(t, 1)
	object, _ := h.mock.Agent(ref.ID)
	_, err := h.provider.AssumeApp(context.Background(), h.appSpec(ref)) // caches T1
	require.NoError(t, err)
	for name, claims := range map[string]jwt.MapClaims{
		"another blueprint": {"tid": tenant, "azp": object.AppID, "xms_par_app_azp": uuid.NewString(), "idtyp": "app"},
		"another agent":     {"tid": tenant, "azp": uuid.NewString(), "xms_par_app_azp": h.mock.BlueprintAppID, "idtyp": "app"},
		"a user token":      {"tid": tenant, "azp": object.AppID, "xms_par_app_azp": h.mock.BlueprintAppID, "idtyp": "user"},
		"another tenant":    {"tid": otherTenant, "azp": object.AppID, "xms_par_app_azp": h.mock.BlueprintAppID, "idtyp": "app"},
	} {
		h.mock.ScriptToken(entratest.Response{Status: http.StatusOK, Body: map[string]any{
			"token_type": "Bearer", "expires_in": 3599, "access_token": h.mock.MintToken(claims)}})
		_, err := h.provider.AssumeApp(context.Background(), h.appSpec(ref))
		require.ErrorIs(t, err, agentid.ErrTokenValidation, name)
	}
	h.mock.ScriptToken(entratest.Response{Status: http.StatusBadRequest, Body: map[string]any{
		"error": "invalid_grant", "error_codes": []int{82014}}})
	_, err = h.provider.AssumeApp(context.Background(), h.appSpec(ref))
	require.ErrorIs(t, err, agentid.ErrConsentNotGranted)
}

func TestTheAssertionSourceServesOnlyThisBlueprintsAgents(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	source := h.provider.AssertionSource()
	ref := h.activeAgent(t, 1)
	object, _ := h.mock.Agent(ref.ID)
	untagged := h.mock.AddAgent(entratest.AgentIdentity{DisplayName: "hand made", AccountEnabled: true})

	for name, call := range map[string][2]string{
		"another tenant": {otherTenant, object.AppID},
		"not a guid":     {tenant, "agent"},
		"unknown agent":  {tenant, uuid.NewString()},
		"not elitea's":   {tenant, untagged.AppID},
	} {
		_, err := source.AgentAssertion(ctx, call[0], call[1])
		require.Error(t, err, name)
	}
	require.Empty(t, h.mock.IssuedAgentAssertions())
	t1, err := source.AgentAssertion(ctx, tenant, object.AppID)
	require.NoError(t, err)
	require.Equal(t, h.mock.IssuedAgentAssertions(), []string{t1})
}

// Acceptance: T1 never appears outside the package. API: the only value
// that carries it is the obo.AgentAssertionSource the obo package calls;
// no exported method answers it. Canary: a full life cycle at debug level
// logs no T1, and no error, credential or formatted value carries one.
func TestT1NeverLeavesThePackage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var observed []string
	record := func(values ...any) {
		for _, v := range values {
			observed = append(observed, fmt.Sprintf("%v %+v %#v", v, v, v))
			if raw, err := json.Marshal(v); err == nil {
				observed = append(observed, string(raw))
			}
		}
	}

	a, b := h.activeAgent(t, 1), h.activeAgent(t, 2)
	app, err := h.provider.AssumeApp(ctx, h.appSpec(a))
	require.NoError(t, err)
	delegated, err := h.provider.AssumeOnBehalfOf(ctx, h.oboSpec(a, h.mock.BlueprintAudience))
	require.NoError(t, err)
	observed = append(observed, app.Token, delegated.Token)
	record(app, delegated, h.provider)
	require.NoError(t, h.provider.Suspend(ctx, a))
	_, err = h.provider.AssumeApp(ctx, h.appSpec(a))
	record(err)
	h.mock.ScriptToken(entratest.Response{Status: http.StatusBadRequest, Body: map[string]any{
		"error": "invalid_client", "error_description": "echo " + strings.Join(h.mock.IssuedAgentAssertions(), " ")}})
	_, err = h.provider.AssumeApp(ctx, h.appSpec(b))
	record(err)
	h.mock.ScriptOBO(entratest.InteractionRequired())
	_, err = h.provider.AssumeOnBehalfOf(ctx, h.oboSpec(b, h.mock.BlueprintAudience))
	record(err)
	resolved, err := h.provider.Resolve(ctx, b)
	require.NoError(t, err)
	page, err := h.provider.List(ctx, agentidentity.ListSpec{})
	require.NoError(t, err)
	record(resolved, page, h.provider.Capabilities())
	observed = append(observed, h.logs.String())

	issued := h.mock.IssuedAgentAssertions()
	require.NotEmpty(t, issued)
	for _, t1 := range issued {
		for _, value := range observed {
			require.NotContains(t, value, t1)
		}
		// Not even a fragment: the signature segment is unique to it.
		signature := t1[strings.LastIndex(t1, ".")+1:]
		require.NotContains(t, h.logs.String(), signature)
	}

	// API: every exported method that answers a bare string answers
	// configuration, never a token; the assertion source is an unexported
	// type with no exported field.
	value := reflectProvider(h.provider)
	for _, method := range value.stringMethods {
		for _, t1 := range issued {
			require.NotEqual(t, t1, method.result, method.name)
		}
	}
	require.Equal(t, []string{"BlueprintAppID", "GoString", "Name", "String", "TenantID"}, value.names())
	require.False(t, value.sourceExported)
}

func TestConfigurationRefusesMultiTenantAliasesAndSecrets(t *testing.T) {
	base := agentid.Config{TenantID: tenant, BlueprintAppID: "22222222-2222-2222-2222-222222222222",
		FederatedTokenFile:    "/var/run/secrets/azure/tokens/azure-identity-token",
		AllowedResourceScopes: []string{graphDefault}, Sessions: &sessionStore{}}
	require.NoError(t, base.Validate())
	for name, mutate := range map[string]func(*agentid.Config){
		"common":          func(c *agentid.Config) { c.TenantID = "common" },
		"organizations":   func(c *agentid.Config) { c.TenantID = "organizations" },
		"consumers":       func(c *agentid.Config) { c.TenantID = "Consumers" },
		"upper case":      func(c *agentid.Config) { c.TenantID = strings.ToUpper(tenant) },
		"client secret":   func(c *agentid.Config) { c.ClientSecret = "s3cr3t" },
		"certificate":     func(c *agentid.Config) { c.CertificateFile = "/etc/cert.pem" },
		"no token file":   func(c *agentid.Config) { c.FederatedTokenFile = "" },
		"no scopes":       func(c *agentid.Config) { c.AllowedResourceScopes = nil },
		"bare scope":      func(c *agentid.Config) { c.AllowedResourceScopes = []string{"User.Read"} },
		"http graph":      func(c *agentid.Config) { c.GraphBaseURL = "http://graph.microsoft.com" },
		"authority path":  func(c *agentid.Config) { c.Authority = "https://login.microsoftonline.com/common" },
		"bad sponsor":     func(c *agentid.Config) { c.SponsorGroups = map[int64]string{1: "group"} },
		"bad blueprint":   func(c *agentid.Config) { c.BlueprintAppID = "blueprint" },
		"negative limit":  func(c *agentid.Config) { c.IdentityLimit = -1 },
		"no session read": func(c *agentid.Config) { c.Sessions = nil },
	} {
		cfg := base
		mutate(&cfg)
		err := cfg.Validate()
		require.ErrorIs(t, err, agentid.ErrInvalidConfiguration, name)
		_, err = agentid.New(cfg)
		require.True(t, errors.Is(err, agentid.ErrInvalidConfiguration), name)
	}
}
