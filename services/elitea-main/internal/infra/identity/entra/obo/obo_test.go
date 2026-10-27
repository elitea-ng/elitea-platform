package obo_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/entratest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/obo"
)

const (
	tenantA = "aaaaaaaa-0000-4000-8000-000000000001"
	tenantB = "bbbbbbbb-0000-4000-8000-000000000002"
	userOID = "cccccccc-0000-4000-8000-000000000003"
	graph   = "https://graph.microsoft.com/Sites.Selected"
)

type fakeAgentSource struct{ value string }

func (f fakeAgentSource) AgentAssertion(context.Context, string, string) (string, error) {
	return f.value, nil
}

func classic(t *testing.T, mock *entratest.Server) *obo.ClassicOBO {
	t.Helper()
	exchanger, err := obo.NewClassicOBO(obo.ClassicConfig{
		Authority:      mock.URL,
		ClientID:       mock.MiddleTierClientID,
		AllowedTenants: []string{tenantA},
		Credential: obo.FederatedCredential(func(context.Context) (string, error) {
			return "federated-workload-token", nil
		}),
		HTTPClient: mock.Client(),
	})
	require.NoError(t, err)
	return exchanger
}

func agent(t *testing.T, mock *entratest.Server, source obo.AgentAssertionSource) *obo.AgentOBO {
	t.Helper()
	exchanger, err := obo.NewAgentOBO(obo.AgentConfig{
		Authority:          mock.URL,
		BlueprintAppID:     "22222222-2222-2222-2222-222222222222",
		AssertionAudiences: []string{mock.BlueprintAudience},
		AllowedTenants:     []string{tenantA},
		Assertions:         source,
		HTTPClient:         mock.Client(),
	})
	require.NoError(t, err)
	return exchanger
}

func assertionFor(mock *entratest.Server, aud string) obo.Assertion {
	return obo.Assertion{
		Token:     mock.AccessToken(tenantA, userOID, aud, "access_as_user", time.Hour),
		TenantID:  tenantA,
		ObjectID:  userOID,
		Audience:  aud,
		ExpiresAt: time.Now().Add(time.Hour),
	}
}

func request() obo.ExchangeRequest {
	return obo.ExchangeRequest{DelegationID: "d-1", Scopes: []string{graph}, ExecutionID: "exec-1", Generation: 2}
}

func TestClassicOBOAgainstTheMockReturnsAToken(t *testing.T) {
	mock := entratest.New(t)
	token, err := classic(t, mock).Exchange(context.Background(), assertionFor(mock, mock.MiddleTierClientID), request())
	require.NoError(t, err)
	require.NotEmpty(t, token.Token)
	require.Equal(t, []string{graph}, token.Scopes)
	require.True(t, token.ExpiresAt.After(time.Now().Add(50*time.Minute)))

	sent := mock.Requests()
	require.Len(t, sent, 1)
	require.Equal(t, mock.MiddleTierClientID, sent[0].ClientID)
	require.True(t, sent[0].HasAssertion, "a federated client assertion authenticates the middle tier")
	require.False(t, sent[0].HasSecret)
	require.Equal(t, obo.CorrelationID("exec-1", 2), sent[0].ClientRequestID)
	require.JSONEq(t, `{"access_token":{"xms_cc":{"values":["cp1"]}}}`, sent[0].Claims)
}

func TestClassicOBORefusesAWrongAudienceLocallyAndAtTheProvider(t *testing.T) {
	mock := entratest.New(t)
	exchanger := classic(t, mock)

	// Recorded audience not this exchanger's.
	_, err := exchanger.Exchange(context.Background(), assertionFor(mock, entratest.GraphAudience), request())
	require.ErrorIs(t, err, obo.ErrAudienceMismatch)
	// Recorded audience right, token payload says otherwise.
	lying := assertionFor(mock, mock.MiddleTierClientID)
	lying.Token = mock.AccessToken(tenantA, userOID, entratest.GraphAudience, "x", time.Hour)
	_, err = exchanger.Exchange(context.Background(), lying, request())
	require.ErrorIs(t, err, obo.ErrAudienceMismatch)
	require.Empty(t, mock.Requests(), "a mismatched assertion never leaves the process")

	// The provider's own AADSTS50013 maps to the same error.
	mock.ScriptOBO(entratest.Response{Status: http.StatusBadRequest, Body: map[string]any{
		"error": "invalid_grant", "error_codes": []int{50013},
	}})
	_, err = exchanger.Exchange(context.Background(), assertionFor(mock, mock.MiddleTierClientID), request())
	require.ErrorIs(t, err, obo.ErrAudienceMismatch)
}

func TestAgentOBORequiresTheBlueprintAudience(t *testing.T) {
	mock := entratest.New(t)
	exchanger := agent(t, mock, fakeAgentSource{value: mock.AgentAssertion})
	r := request()
	r.AgentID = mock.AgentID

	token, err := exchanger.Exchange(context.Background(), assertionFor(mock, mock.BlueprintAudience), r)
	require.NoError(t, err)
	require.NotEmpty(t, token.Token)

	// A classic (middle-tier) delegation is refused by Agent OBO ...
	_, err = exchanger.Exchange(context.Background(), assertionFor(mock, mock.MiddleTierClientID), r)
	require.ErrorIs(t, err, obo.ErrAudienceMismatch)
	// ... and a blueprint delegation by classic OBO.
	_, err = classic(t, mock).Exchange(context.Background(), assertionFor(mock, mock.BlueprintAudience), request())
	require.ErrorIs(t, err, obo.ErrAudienceMismatch)
}

func TestAgentOBOWithoutASourceIsDenied(t *testing.T) {
	mock := entratest.New(t)
	r := request()
	r.AgentID = mock.AgentID
	_, err := agent(t, mock, nil).Exchange(context.Background(), assertionFor(mock, mock.BlueprintAudience), r)
	require.ErrorIs(t, err, obo.ErrAgentAssertionUnavailable)
	_, err = agent(t, mock, fakeAgentSource{}).Exchange(context.Background(), assertionFor(mock, mock.BlueprintAudience), r)
	require.ErrorIs(t, err, obo.ErrAgentAssertionUnavailable)
	require.Empty(t, mock.Requests())
}

func TestTokenEndpointErrorsMapByCodeNotByText(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want error
	}{
		{"interaction_required", map[string]any{"error": "interaction_required"}, obo.ErrInteractionRequired},
		{"consent_required", map[string]any{"error": "consent_required"}, obo.ErrInteractionRequired},
		{"AADSTS50076", map[string]any{"error": "invalid_grant", "error_codes": []int{50076}}, obo.ErrInteractionRequired},
		{"AADSTS50079", map[string]any{"error": "invalid_grant", "error_codes": []int{50079}}, obo.ErrInteractionRequired},
		{"AADSTS65001", map[string]any{"error": "invalid_grant", "error_codes": []int{65001}}, obo.ErrInteractionRequired},
		{"suberror basic_action", map[string]any{"error": "invalid_grant", "suberror": "basic_action"}, obo.ErrInteractionRequired},
		{"AADSTS50013", map[string]any{"error": "invalid_grant", "error_codes": []int{50013}}, obo.ErrAudienceMismatch},
		{"AADSTS82014", map[string]any{"error": "invalid_grant", "error_codes": []int{82014, 65001}}, obo.ErrAgentConsentNotGranted},
		{"plain invalid_grant", map[string]any{"error": "invalid_grant", "suberror": "bad_token"}, obo.ErrRejected},
		// Description text that names an interaction code is not read.
		{"text only", map[string]any{"error": "invalid_grant", "error_description": "AADSTS50076 interaction_required"}, obo.ErrRejected},
		{"server_error", map[string]any{"error": "temporarily_unavailable"}, obo.ErrUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := entratest.New(t)
			mock.ScriptOBO(entratest.Response{Status: http.StatusBadRequest, Body: tc.body})
			_, err := classic(t, mock).Exchange(context.Background(), assertionFor(mock, mock.MiddleTierClientID), request())
			require.ErrorIs(t, err, tc.want)
		})
	}

	t.Run("claims challenge", func(t *testing.T) {
		mock := entratest.New(t)
		challenge := `{"access_token":{"acrs":{"essential":true,"value":"c1"}}}`
		mock.ScriptOBO(entratest.ClaimsChallenge(challenge))
		_, err := classic(t, mock).Exchange(context.Background(), assertionFor(mock, mock.MiddleTierClientID), request())
		var typed *obo.ErrClaimsChallenge
		require.ErrorAs(t, err, &typed)
		require.Equal(t, challenge, typed.Claims)

		// Answering the challenge sends it back, merged with cp1.
		r := request()
		r.Claims = typed.Claims
		_, err = classic(t, mock).Exchange(context.Background(), assertionFor(mock, mock.MiddleTierClientID), r)
		require.NoError(t, err)
		sent := mock.Requests()
		var merged map[string]map[string]any
		require.NoError(t, json.Unmarshal([]byte(sent[len(sent)-1].Claims), &merged))
		require.Contains(t, merged["access_token"], "acrs")
		require.Contains(t, merged["access_token"], "xms_cc")
	})

	t.Run("5xx and throttling", func(t *testing.T) {
		for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
			mock := entratest.New(t)
			mock.ScriptOBO(entratest.Response{Status: status, Body: map[string]any{"error": "x"}})
			_, err := classic(t, mock).Exchange(context.Background(), assertionFor(mock, mock.MiddleTierClientID), request())
			require.ErrorIs(t, err, obo.ErrUnavailable)
		}
	})
}

func TestTheAssertionContractIsCheckedBeforeTheNetwork(t *testing.T) {
	mock := entratest.New(t)
	exchanger := classic(t, mock)
	good := assertionFor(mock, mock.MiddleTierClientID)

	otherTenant := good
	otherTenant.TenantID = tenantB
	_, err := exchanger.Exchange(context.Background(), otherTenant, request())
	require.ErrorIs(t, err, obo.ErrInvalidRequest)

	expired := good
	expired.ExpiresAt = time.Now().Add(-time.Second)
	_, err = exchanger.Exchange(context.Background(), expired, request())
	require.ErrorIs(t, err, obo.ErrInteractionRequired)

	for _, scopes := range [][]string{nil, {""}, {"a b"}, {graph, graph}} {
		r := request()
		r.Scopes = scopes
		_, err = exchanger.Exchange(context.Background(), good, r)
		require.ErrorIs(t, err, obo.ErrInvalidRequest)
	}
	r := request()
	r.AgentID = mock.AgentID
	_, err = exchanger.Exchange(context.Background(), good, r)
	require.ErrorIs(t, err, obo.ErrInvalidRequest, "classic OBO refuses an agent id")
	require.Empty(t, mock.Requests())
}

func TestAClientSecretNeedsThePolicyToAllowIt(t *testing.T) {
	mock := entratest.New(t)
	config := obo.ClassicConfig{
		Authority: mock.URL, ClientID: mock.MiddleTierClientID, AllowedTenants: []string{tenantA},
		Credential: obo.ClientSecretCredential("s3cr3t-value"), HTTPClient: mock.Client(),
	}
	_, err := obo.NewClassicOBO(config)
	require.ErrorIs(t, err, obo.ErrInvalidConfiguration)

	config.AllowClientSecret = true
	exchanger, err := obo.NewClassicOBO(config)
	require.NoError(t, err)
	_, err = exchanger.Exchange(context.Background(), assertionFor(mock, mock.MiddleTierClientID), request())
	require.NoError(t, err)
	require.True(t, mock.Requests()[0].HasSecret)
	require.NotContains(t, fmt.Sprintf("%v %#v", config.Credential, config.Credential), "s3cr3t-value")
}

func TestACertificateCredentialSignsAClientAssertion(t *testing.T) {
	mock := entratest.New(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "middle-tier"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	certificate, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	exchanger, err := obo.NewClassicOBO(obo.ClassicConfig{
		Authority: mock.URL, ClientID: mock.MiddleTierClientID, AllowedTenants: []string{tenantA},
		Credential: obo.CertificateCredential(certificate, key), HTTPClient: mock.Client(),
	})
	require.NoError(t, err)
	_, err = exchanger.Exchange(context.Background(), assertionFor(mock, mock.MiddleTierClientID), request())
	require.NoError(t, err)
	require.True(t, mock.Requests()[0].HasAssertion)
}

func TestConfigurationIsValidated(t *testing.T) {
	credential := obo.FederatedCredential(func(context.Context) (string, error) { return "x", nil })
	for _, config := range []obo.ClassicConfig{
		{Authority: "http://login.example", ClientID: "c", AllowedTenants: []string{tenantA}, Credential: credential},
		{Authority: "https://login.example/tenant", ClientID: "c", AllowedTenants: []string{tenantA}, Credential: credential},
		{Authority: "https://login.example", ClientID: "c", AllowedTenants: []string{"organizations"}, Credential: credential},
		{Authority: "https://login.example", ClientID: "c", AllowedTenants: nil, Credential: credential},
		{Authority: "https://login.example", ClientID: "", AllowedTenants: []string{tenantA}, Credential: credential},
		{Authority: "https://login.example", ClientID: "c", AllowedTenants: []string{tenantA}},
	} {
		_, err := obo.NewClassicOBO(config)
		require.ErrorIs(t, err, obo.ErrInvalidConfiguration, "%+v", config.Authority)
	}
}

func TestCorrelationIDIsStablePerExecutionGeneration(t *testing.T) {
	require.Equal(t, obo.CorrelationID("e", 1), obo.CorrelationID("e", 1))
	require.NotEqual(t, obo.CorrelationID("e", 1), obo.CorrelationID("e", 2))
	require.NotEqual(t, obo.CorrelationID("e", 1), obo.CorrelationID("f", 1))
}

// Logs and errors contain no token text: every value that can be logged or
// printed is captured and searched for the assertion, the issued token and
// the provider's description.
func TestLogsAndErrorsContainNoTokenText(t *testing.T) {
	mock := entratest.New(t)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	exchanger := classic(t, mock)
	assertion := assertionFor(mock, mock.MiddleTierClientID)

	token, err := exchanger.Exchange(context.Background(), assertion, request())
	require.NoError(t, err)
	logger.Info("exchanged", "assertion", assertion, "token", token)
	_, _ = fmt.Fprintf(&logs, "%v %+v %#v %s %v %#v\n", assertion, assertion, assertion, token, token, token)

	mock.ScriptOBO(entratest.InteractionRequired(), entratest.ClaimsChallenge(`{"access_token":{}}`))
	for range 2 {
		_, err = exchanger.Exchange(context.Background(), assertion, request())
		require.Error(t, err)
		logger.Error("exchange failed", "err", err)
		_, _ = fmt.Fprintf(&logs, "%v %+v\n", err, err)
	}

	text := logs.String()
	require.NotContains(t, text, assertion.Token)
	require.NotContains(t, text, token.Token)
	require.NotContains(t, text, "federated-workload-token")
	require.NotContains(t, text, "eyJhbGciOi")
	for _, part := range strings.Split(assertion.Token, ".") {
		require.NotContains(t, text, part)
	}
}
