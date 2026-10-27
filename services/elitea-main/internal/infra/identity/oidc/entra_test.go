package oidc

import (
	"context"
	"testing"
	"time"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/entratest"
)

const (
	entraTenantA = "aaaaaaaa-0000-4000-8000-000000000001"
	entraTenantB = "bbbbbbbb-0000-4000-8000-000000000002"
	entraUser    = "cccccccc-0000-4000-8000-000000000003"
)

func entraFixture(t *testing.T) (*entratest.Server, EntraPolicy, *EntraAssertionVerifier) {
	t.Helper()
	mock := entratest.New(t)
	policy, err := NewEntraPolicy(mock.URL+"/organizations/v2.0", mock.MiddleTierClientID, []string{entraTenantA}, "")
	require.NoError(t, err)
	ctx := coreoidc.ClientContext(context.Background(), mock.Client())
	keys := coreoidc.NewRemoteKeySet(ctx, mock.URL+"/discovery/v2.0/keys")
	verifier, err := NewEntraAssertionVerifier(policy, keys, nil)
	require.NoError(t, err)
	return mock, policy, verifier
}

func TestEntraPolicyDefaultsTheMiddleTierScope(t *testing.T) {
	_, policy, _ := entraFixture(t)
	require.Equal(t, "api://"+policy.ClientID()+"/.default", policy.APIScope())
	for _, tenants := range [][]string{nil, {"common"}, {"organizations"}, {"AAAAAAAA-0000-4000-8000-000000000001"}} {
		_, err := NewEntraPolicy("https://login.microsoftonline.com/organizations/v2.0", "client", tenants, "")
		require.ErrorIs(t, err, ErrInvalidConfiguration, "%v", tenants)
	}
}

func TestEntraIssuerOutsideTheAllowListIsRejected(t *testing.T) {
	_, policy, _ := entraFixture(t)
	good := EntraClaims{Issuer: policy.IssuerFor(entraTenantA), TenantID: entraTenantA, ObjectID: entraUser}
	identity, err := policy.VerifyIdentity(good)
	require.NoError(t, err)
	require.Equal(t, EntraIdentity{TenantID: entraTenantA, ObjectID: entraUser}, identity)

	for name, claims := range map[string]EntraClaims{
		"tenant not allowed":   {Issuer: policy.IssuerFor(entraTenantB), TenantID: entraTenantB, ObjectID: entraUser},
		"iss names another":    {Issuer: policy.IssuerFor(entraTenantB), TenantID: entraTenantA, ObjectID: entraUser},
		"organizations issuer": {Issuer: policy.Authority() + "/organizations/v2.0", TenantID: entraTenantA, ObjectID: entraUser},
		"common issuer":        {Issuer: policy.Authority() + "/common/v2.0", TenantID: entraTenantA, ObjectID: entraUser},
		"template issuer":      {Issuer: policy.IssuerTemplate(), TenantID: entraTenantA, ObjectID: entraUser},
		"v1 issuer":            {Issuer: "https://sts.windows.net/" + entraTenantA + "/", TenantID: entraTenantA, ObjectID: entraUser},
	} {
		_, err := policy.VerifyIdentity(claims)
		require.ErrorIs(t, err, ErrEntraTenantNotAllowed, name)
		require.ErrorIs(t, err, ErrAssertionRejected, name)
	}
}

func TestEntraTokenWithoutTidOrOidIsRejected(t *testing.T) {
	_, policy, _ := entraFixture(t)
	for name, claims := range map[string]EntraClaims{
		"no tid":          {Issuer: policy.IssuerFor(entraTenantA), ObjectID: entraUser},
		"no oid":          {Issuer: policy.IssuerFor(entraTenantA), TenantID: entraTenantA},
		"tid is an alias": {Issuer: policy.IssuerFor("organizations"), TenantID: "organizations", ObjectID: entraUser},
	} {
		_, err := policy.VerifyIdentity(claims)
		require.ErrorIs(t, err, ErrEntraIdentityClaims, name)
	}
}

func TestEntraAssertionMustBeAMiddleTierDelegatedAccessToken(t *testing.T) {
	mock, policy, verifier := entraFixture(t)
	ctx := context.Background()

	raw := mock.AccessToken(entraTenantA, entraUser, mock.MiddleTierClientID, "access_as_user", time.Hour)
	verified, err := verifier.Verify(ctx, raw)
	require.NoError(t, err)
	require.Equal(t, EntraIdentity{TenantID: entraTenantA, ObjectID: entraUser}, verified.Identity)
	require.Equal(t, mock.MiddleTierClientID, verified.Audience)
	require.Equal(t, []string{"access_as_user"}, verified.Scopes)
	require.NotContains(t, verified.String(), raw)

	now := time.Now()
	idToken := mock.MintToken(jwt.MapClaims{
		"iss": policy.IssuerFor(entraTenantA), "aud": mock.MiddleTierClientID, "tid": entraTenantA, "oid": entraUser,
		"nonce": "n", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	appOnly := mock.MintToken(jwt.MapClaims{
		"iss": policy.IssuerFor(entraTenantA), "aud": mock.MiddleTierClientID, "tid": entraTenantA, "oid": entraUser,
		"idtyp": "app", "roles": []string{"Sites.Selected"}, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	refused := map[string]string{
		"ID token":          idToken,
		"Graph token":       mock.AccessToken(entraTenantA, entraUser, entratest.GraphAudience, "User.Read", time.Hour),
		"Graph URL token":   mock.AccessToken(entraTenantA, entraUser, "https://graph.microsoft.com", "User.Read", time.Hour),
		"app-only token":    appOnly,
		"no scp":            mock.AccessToken(entraTenantA, entraUser, mock.MiddleTierClientID, "-", time.Hour),
		"blueprint token":   mock.AccessToken(entraTenantA, entraUser, mock.BlueprintAudience, "access_agent", time.Hour),
		"disallowed tenant": mock.AccessToken(entraTenantB, entraUser, mock.MiddleTierClientID, "access_as_user", time.Hour),
	}
	for name, token := range refused {
		_, err := verifier.Verify(ctx, token)
		require.ErrorIs(t, err, ErrAssertionRejected, name)
	}

	// Signature, expiry.
	_, err = verifier.Verify(ctx, raw[:len(raw)-4]+"AAAA")
	require.ErrorIs(t, err, ErrAssertionRejected)
	_, err = verifier.Verify(ctx, mock.AccessToken(entraTenantA, entraUser, mock.MiddleTierClientID, "access_as_user", -time.Minute))
	require.ErrorIs(t, err, ErrAssertionRejected)
}
