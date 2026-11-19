package microsoftlogin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftapps"
)

func unitService(t *testing.T, apps Apps) *Service {
	t.Helper()
	s, err := newSealer(bytes.Repeat([]byte{1}, 32))
	require.NoError(t, err)
	return &Service{apps: apps, sealer: s}
}

func refusalCode(t *testing.T, err error) string {
	t.Helper()
	var refusal *Refusal
	require.ErrorAs(t, err, &refusal)
	return refusal.Code
}

func TestResolveClientAppliesEveryRuleOfTheDecision(t *testing.T) {
	ctx := context.Background()
	t.Run("a custom credential signs in with its own client at an Entra authority", func(t *testing.T) {
		c, err := unitService(t, &fakeApps{customAllow: true}).resolveClient(ctx, 7, customOutlook())
		require.NoError(t, err)
		require.Equal(t, AppCustom, c.appRef)
		require.Equal(t, fixtureClient, c.clientID)
		require.Equal(t, "https://login.microsoftonline.com/"+fixtureTenant, c.authority)
		require.ElementsMatch(t, []string{"offline_access", "Mail.Read", "openid", "profile"}, c.scopes)
	})
	t.Run("a registry-less deployment still signs a custom credential in", func(t *testing.T) {
		_, err := unitService(t, nil).resolveClient(ctx, 7, customOutlook())
		require.NoError(t, err)
	})
	t.Run("custom app switched off refuses a custom credential", func(t *testing.T) {
		_, err := unitService(t, &fakeApps{customAllow: false}).resolveClient(ctx, 7, customOutlook())
		require.Equal(t, CodeCustomAppsDisabled, refusalCode(t, err))
	})
	t.Run("a default app reference with no registry is refused, never replaced by a client", func(t *testing.T) {
		credential := defaultAppOutlook()
		credential.Data["client_id"] = fixtureClient
		credential.Data["oauth_discovery_endpoint"] = "https://login.microsoftonline.com/" + fixtureTenant
		_, err := unitService(t, nil).resolveClient(ctx, 7, credential)
		require.Equal(t, CodeAppsUnavailable, refusalCode(t, err))
	})
	t.Run("a registry refusal keeps its stable code", func(t *testing.T) {
		apps := &fakeApps{customAllow: true, err: &microsoftapps.ResolveError{Code: microsoftapps.CodeProjectNotServed, Message: "no"}}
		_, err := unitService(t, apps).resolveClient(ctx, 7, defaultAppOutlook())
		require.Equal(t, microsoftapps.CodeProjectNotServed, refusalCode(t, err))
	})
	t.Run("a registry read failure is not a refusal", func(t *testing.T) {
		apps := &fakeApps{customAllow: true, err: errors.New("db down")}
		_, err := unitService(t, apps).resolveClient(ctx, 7, defaultAppOutlook())
		require.Error(t, err)
		var refusal *Refusal
		require.False(t, errors.As(err, &refusal))
	})
	t.Run("the dataverse tenant reaches the registry", func(t *testing.T) {
		apps := &fakeApps{customAllow: true, resolved: microsoftapps.Resolved{
			AppID: "bbbbbbbb-0000-4000-8000-000000000002", ClientID: fixtureClient,
			Authority: "https://login.microsoftonline.com/" + fixtureTenant,
		}}
		credential := Credential{ID: credentialID, Type: "dataverse", Data: map[string]any{
			"oauth_app_id": "bbbbbbbb-0000-4000-8000-000000000002", "tenant_id": fixtureTenant, "url": "https://org.crm.dynamics.com",
		}}
		c, err := unitService(t, apps).resolveClient(ctx, 7, credential)
		require.NoError(t, err)
		require.Equal(t, fixtureTenant, apps.requests[0].TenantID)
		require.Contains(t, c.scopes, "https://org.crm.dynamics.com/user_impersonation")
	})
	t.Run("a custom client id must be a GUID", func(t *testing.T) {
		credential := customOutlook()
		credential.Data["client_id"] = "not-a-guid"
		_, err := unitService(t, &fakeApps{customAllow: true}).resolveClient(ctx, 7, credential)
		require.Equal(t, CodeClientInvalid, refusalCode(t, err))
	})
	t.Run("a credential that is not delegated or not Microsoft", func(t *testing.T) {
		sharePointAppOnly := Credential{ID: credentialID, Type: "sharepoint", Data: map[string]any{"client_id": fixtureClient}}
		_, err := unitService(t, nil).resolveClient(ctx, 7, sharePointAppOnly)
		require.Equal(t, CodeNotDelegated, refusalCode(t, err))
		_, err = unitService(t, nil).resolveClient(ctx, 7, Credential{ID: credentialID, Type: "github"})
		require.Equal(t, CodeNotMicrosoft, refusalCode(t, err))
	})
	t.Run("a sharepoint delegated credential asks for its own scopes", func(t *testing.T) {
		credential := Credential{ID: credentialID, Type: "sharepoint", Data: map[string]any{
			"client_id": fixtureClient, "oauth_discovery_endpoint": "https://login.microsoftonline.com/" + fixtureTenant,
			"scopes": []any{"Sites.Read.All"},
		}}
		c, err := unitService(t, &fakeApps{customAllow: true}).resolveClient(ctx, 7, credential)
		require.NoError(t, err)
		require.Equal(t, []string{"Sites.Read.All", "openid", "profile"}, c.scopes)
	})
	t.Run("a sharepoint credential on a non-Entra authority is refused by the new flow", func(t *testing.T) {
		credential := Credential{ID: credentialID, Type: "sharepoint", Data: map[string]any{
			"client_id": fixtureClient, "oauth_discovery_endpoint": "https://adfs.example.com/adfs",
		}}
		_, err := unitService(t, &fakeApps{customAllow: true}).resolveClient(ctx, 7, credential)
		require.Equal(t, CodeAuthorityInvalid, refusalCode(t, err))
	})
}

func TestTheOutlookAndTeamsListsHaveOneSource(t *testing.T) {
	// The Default app registry's offered lists and defaults are the credential's own.
	require.Equal(t, []string{"Mail.Read"}, microsoftapps.Defaults("outlook"))
	require.Equal(t, []string{"User.Read"}, microsoftapps.Defaults("ms_graph"))
	require.Len(t, microsoftapps.Offered("outlook"), 6)
	require.Len(t, microsoftapps.Offered("teams"), 8)
}

func TestSealedValuesAreBoundToTheirRow(t *testing.T) {
	s, err := newSealer(bytes.Repeat([]byte{1}, 32))
	require.NoError(t, err)
	base := binding{userID: 1, projectID: 2, credentialID: credentialID, credType: "outlook", appRef: "custom",
		clientKind: "web", fingerprint: fingerprint(fixtureClient), authority: "https://login.microsoftonline.com/t", scope: "Mail.Read"}
	expires := time.Now().UTC().Truncate(time.Microsecond)
	aad := handoffAAD{handoffDigest: digestOf("handoff", "h"), stateDigest: digestOf("state", "s"), tid: "t", oid: "o", expiresAt: expires, binding: base}
	sealed := s.seal("token", aad)
	opened, err := s.open(sealed, aad)
	require.NoError(t, err)
	require.Equal(t, "token", opened)

	for name, mutate := range map[string]func(*handoffAAD){
		"user":        func(a *handoffAAD) { a.userID = 9 },
		"project":     func(a *handoffAAD) { a.projectID = 9 },
		"credential":  func(a *handoffAAD) { a.credentialID = "other" },
		"app":         func(a *handoffAAD) { a.appRef = "bbbbbbbb-0000-4000-8000-000000000002" },
		"client":      func(a *handoffAAD) { a.fingerprint = fingerprint("other") },
		"authority":   func(a *handoffAAD) { a.authority = "https://login.microsoftonline.com/other" },
		"scope":       func(a *handoffAAD) { a.scope = "Mail.ReadWrite" },
		"identity":    func(a *handoffAAD) { a.oid = "someone-else" },
		"handoff row": func(a *handoffAAD) { a.handoffDigest = digestOf("handoff", "other") },
		"expiry":      func(a *handoffAAD) { a.expiresAt = expires.Add(time.Second) },
	} {
		moved := aad
		mutate(&moved)
		_, err := s.open(sealed, moved)
		require.ErrorIs(t, err, ErrCustodyIntegrity, name)
	}
	// A verifier sealed for a state does not open as an access token.
	_, err = s.open(sealed, stateAAD{digest: digestOf("state", "s"), redirectURI: "r", expiresAt: expires, binding: base})
	require.ErrorIs(t, err, ErrCustodyIntegrity)
}

func idToken(claims map[string]any) string {
	raw, _ := json.Marshal(claims)
	return "e30." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

func TestReadIdentity(t *testing.T) {
	good := map[string]any{"aud": fixtureClient, "tid": fixtureTenant, "oid": fixtureOID, "name": "Alex\x00 Example\n"}
	who, err := readIdentity(idToken(good), fixtureClient)
	require.NoError(t, err)
	require.Equal(t, "Alex Example", who.name, "control characters are dropped")

	list := map[string]any{"aud": []any{"x", fixtureClient}, "tid": fixtureTenant, "oid": fixtureOID, "preferred_username": "a@example.com"}
	who, err = readIdentity(idToken(list), fixtureClient)
	require.NoError(t, err)
	require.Equal(t, "a@example.com", who.name)

	for name, token := range map[string]string{
		"another client's token": idToken(map[string]any{"aud": "other", "tid": fixtureTenant, "oid": fixtureOID}),
		"no tenant":              idToken(map[string]any{"aud": fixtureClient, "oid": fixtureOID}),
		"no person":              idToken(map[string]any{"aud": fixtureClient, "tid": fixtureTenant}),
		"not a jwt":              "abc",
		"empty":                  "",
	} {
		_, err := readIdentity(token, fixtureClient)
		require.Error(t, err, name)
	}
	require.Len(t, cleanName(string(bytes.Repeat([]byte("é"), 500))), 256, "bounded to 128 runes")
}

func TestTenantMatches(t *testing.T) {
	authority := "https://login.microsoftonline.com/"
	require.True(t, tenantMatches(authority+fixtureTenant, fixtureTenant))
	require.True(t, tenantMatches(authority+"11111111-1111-1111-1111-111111111111", "11111111-1111-1111-1111-111111111111"))
	require.False(t, tenantMatches(authority+fixtureTenant, "99999999-9999-9999-9999-999999999999"))
	require.True(t, tenantMatches(authority+"organizations", "99999999-9999-9999-9999-999999999999"))
	require.True(t, tenantMatches(authority+"contoso.onmicrosoft.com", "99999999-9999-9999-9999-999999999999"))
}

func TestLifetime(t *testing.T) {
	require.Equal(t, 3599*time.Second, lifetime(float64(3599)))
	require.Equal(t, 3599*time.Second, lifetime("3599"))
	require.Equal(t, time.Hour, lifetime(nil))
	require.Equal(t, time.Hour, lifetime(float64(-5)))
	require.Equal(t, 24*time.Hour, lifetime(float64(10_000_000)))
}

func TestChallengeMatches(t *testing.T) {
	verifier, challenge := pkce(t)
	require.True(t, challengeMatches(challenge, verifier))
	require.False(t, challengeMatches(challenge, ""))
	require.False(t, challengeMatches(challenge, verifier+"x"))
	require.True(t, challengeMatches("", ""))
	require.False(t, challengeMatches("", verifier))
}

func TestOutcomeReturnURLs(t *testing.T) {
	desktop := Outcome{Known: true, ClientKind: ClientDesktop, ReturnPort: 50000, State: "st", Handoff: "ho"}
	require.Equal(t, "http://127.0.0.1:50000/oauth/microsoft?handoff=ho&state=st", desktop.ReturnURL())
	mobile := Outcome{Known: true, ClientKind: ClientMobile, ReturnScheme: "elitea", State: "st", ErrorCode: ErrDenied}
	require.Equal(t, "elitea://oauth/microsoft?state=st&error=denied", mobile.ReturnURL())
	require.Empty(t, Outcome{ClientKind: ClientWeb}.ReturnURL())
	require.NotContains(t, fmt.Sprint(Outcome{Handoff: "ho"}), "secret")
}
