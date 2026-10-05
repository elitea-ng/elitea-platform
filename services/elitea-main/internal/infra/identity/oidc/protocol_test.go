package oidc

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"

	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
)

func TestProtocolRunsSignedAuthorizationCodeFlow(t *testing.T) {
	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	privateKey := mustRSAKey(t)
	exchanger := &exchangeStub{}
	config := validProtocolConfig()
	protocol, err := newProtocol(
		config,
		exchanger,
		&coreoidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&privateKey.PublicKey}},
		bytes.NewReader(sequentialBytes(64)),
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	// The protocol owns immutable copies of slice-valued configuration.
	config.Scopes[0] = "attacker"
	config.SupportedSigningAlgorithms[0] = "PS256"

	authorization, err := protocol.NewAuthorization(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if authorization.PKCEChallengeMethod != browserapp.OIDCPKCEChallengeS256 ||
		len(authorization.Correlation.Nonce) != 43 ||
		len(authorization.ProviderState.PKCEVerifier) != browserflow.MinPKCEVerifierBytes {
		t.Fatalf("authorization = %+v", authorization)
	}
	state := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, browserflow.TransactionIDRandomBytes))
	authorizationRequest, err := protocol.AuthorizationRequest(state, authorization)
	if err != nil {
		t.Fatal(err)
	}
	expectedChallengeBytes := sha256.Sum256([]byte(authorization.ProviderState.PKCEVerifier))
	expectedChallenge := base64.RawURLEncoding.EncodeToString(expectedChallengeBytes[:])
	if authorizationRequest.Transport != browserapp.OIDCAuthorizationPOST ||
		authorizationRequest.Endpoint != "https://issuer.example/authorize" ||
		authorizationRequest.ResponseType != "code" || authorizationRequest.ClientID != "elitea-client" ||
		authorizationRequest.RedirectURI != "https://elitea.example/auth/oidc/login_callback" ||
		authorizationRequest.Scope != "openid profile email" || authorizationRequest.State != state ||
		authorizationRequest.Nonce != authorization.Correlation.Nonce ||
		authorizationRequest.CodeChallengeMethod != browserapp.OIDCPKCEChallengeS256 ||
		authorizationRequest.CodeChallenge != expectedChallenge {
		t.Fatalf("authorization request = %+v", authorizationRequest)
	}

	emailVerified := true
	exchanger.result.RawIDToken = signIDToken(t, privateKey, jwt.MapClaims{
		"iss":                "https://issuer.example",
		"sub":                "subject-42",
		"aud":                "elitea-client",
		"exp":                now.Add(time.Hour).Unix(),
		"iat":                now.Add(-time.Minute).Unix(),
		"nbf":                now.Add(-time.Minute).Unix(),
		"nonce":              authorization.Correlation.Nonce,
		"preferred_username": "admin",
		"email":              "Admin@Example.Test",
		"email_verified":     emailVerified,
		"given_name":         "Ada",
		"family_name":        "Lovelace",
		"name":               "Ada Lovelace",
		"picture":            "https://images.example/avatar.png",
	})
	assertion, err := protocol.NewVerifier("authorization-code").Verify(
		context.Background(),
		browserflow.VerificationContext{
			Provider:      ProviderName,
			Correlation:   authorization.Correlation,
			ProviderState: authorization.ProviderState,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if exchanger.request != (ExchangeRequest{
		AuthorizationCode: "authorization-code",
		PKCEVerifier:      authorization.ProviderState.PKCEVerifier,
	}) {
		t.Fatalf("exchange request = %+v", exchanger.request)
	}
	if assertion.Provider != ProviderName || assertion.ProviderReference != "admin" ||
		assertion.Email != "Admin@Example.Test" || assertion.GivenName != "Ada" ||
		assertion.FamilyName != "Lovelace" || assertion.Name != "Ada Lovelace" ||
		!assertion.ProtocolCorrelation.Equal(authorization.Correlation) ||
		assertion.Expiration == nil || !assertion.Expiration.Equal(now.Add(time.Hour)) {
		t.Fatalf("assertion = %+v", assertion)
	}
	var attributes struct {
		NameID       string         `json:"nameid"`
		Attributes   map[string]any `json:"attributes"`
		SessionIndex string         `json:"sessionindex"`
	}
	if err := json.Unmarshal(assertion.ProviderAttributes, &attributes); err != nil {
		t.Fatal(err)
	}
	if attributes.NameID != "admin" || attributes.SessionIndex != "" ||
		attributes.Attributes["picture"] != "https://images.example/avatar.png" ||
		strings.Contains(string(assertion.ProviderAttributes), exchanger.result.RawIDToken) {
		t.Fatalf("provider attributes = %s", assertion.ProviderAttributes)
	}
}

func TestProtocolRejectsInvalidTokensAndClassifiesProviderOutages(t *testing.T) {
	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	key := mustRSAKey(t)
	baseClaims := jwt.MapClaims{
		"iss": "https://issuer.example", "sub": "subject-42", "aud": "elitea-client",
		"exp": now.Add(time.Hour).Unix(), "iat": now.Add(-time.Minute).Unix(),
		"nonce": "expected-nonce", "email_verified": true,
	}
	tests := []struct {
		name         string
		mutate       func(jwt.MapClaims)
		mutateConfig func(*Config)
		exchange     error
		keySet       coreoidc.KeySet
		want         error
		wantNot      error
	}{
		{name: "wrong issuer", mutate: func(claims jwt.MapClaims) { claims["iss"] = "https://other.example" }, want: ErrAssertionRejected, wantNot: browserapp.ErrAssertionVerifierUnavailable},
		{name: "wrong audience", mutate: func(claims jwt.MapClaims) { claims["aud"] = "another-client" }, want: ErrAssertionRejected, wantNot: browserapp.ErrAssertionVerifierUnavailable},
		{name: "multiple audiences missing authorized party", mutate: func(claims jwt.MapClaims) { claims["aud"] = []string{"elitea-client", "api"} }, want: ErrAssertionRejected, wantNot: browserapp.ErrAssertionVerifierUnavailable},
		{name: "multiple audiences wrong authorized party", mutate: func(claims jwt.MapClaims) { claims["aud"] = []string{"elitea-client", "api"}; claims["azp"] = "api" }, want: ErrAssertionRejected, wantNot: browserapp.ErrAssertionVerifierUnavailable},
		{name: "single audience wrong authorized party", mutate: func(claims jwt.MapClaims) { claims["azp"] = "other-client" }, want: ErrAssertionRejected, wantNot: browserapp.ErrAssertionVerifierUnavailable},
		{name: "expired", mutate: func(claims jwt.MapClaims) { claims["exp"] = now.Add(-time.Second).Unix() }, want: ErrAssertionRejected, wantNot: browserapp.ErrAssertionVerifierUnavailable},
		{name: "not before in future", mutate: func(claims jwt.MapClaims) { claims["nbf"] = now.Add(10 * time.Minute).Unix() }, want: ErrAssertionRejected, wantNot: browserapp.ErrAssertionVerifierUnavailable},
		{name: "missing issued at", mutate: func(claims jwt.MapClaims) { delete(claims, "iat") }, want: ErrAssertionRejected, wantNot: browserapp.ErrAssertionVerifierUnavailable},
		{name: "issued at in future", mutate: func(claims jwt.MapClaims) { claims["iat"] = now.Add(maxFutureIssuedAtSkew + time.Second).Unix() }, want: ErrAssertionRejected, wantNot: browserapp.ErrAssertionVerifierUnavailable},
		{name: "wrong nonce", mutate: func(claims jwt.MapClaims) { claims["nonce"] = "wrong-nonce" }, want: ErrAssertionRejected, wantNot: browserapp.ErrAssertionVerifierUnavailable},
		{name: "missing subject", mutate: func(claims jwt.MapClaims) { delete(claims, "sub") }, want: ErrAssertionRejected, wantNot: browserapp.ErrAssertionVerifierUnavailable},
		{name: "unverified email", mutate: func(claims jwt.MapClaims) { claims["email_verified"] = false }, want: ErrAssertionRejected, wantNot: browserapp.ErrAssertionVerifierUnavailable},
		{name: "unsupported configured algorithm", mutateConfig: func(config *Config) { config.SupportedSigningAlgorithms = []string{"PS256"} }, want: ErrAssertionRejected, wantNot: browserapp.ErrAssertionVerifierUnavailable},
		{name: "code rejected", exchange: errors.New("invalid_grant detail must not escape"), want: ErrAssertionRejected, wantNot: browserapp.ErrAssertionVerifierUnavailable},
		{name: "token endpoint outage", exchange: fmt.Errorf("%w: secret detail", ErrProviderUnavailable), want: browserapp.ErrAssertionVerifierUnavailable, wantNot: ErrAssertionRejected},
		{name: "JWKS outage", keySet: failingKeySet{err: fmt.Errorf("%w: secret detail", ErrProviderUnavailable)}, want: browserapp.ErrAssertionVerifierUnavailable, wantNot: ErrAssertionRejected},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims := cloneClaims(baseClaims)
			if test.mutate != nil {
				test.mutate(claims)
			}
			exchanger := &exchangeStub{
				result: ExchangeResult{RawIDToken: signIDToken(t, key, claims)},
				err:    test.exchange,
			}
			keySet := test.keySet
			if keySet == nil {
				keySet = &coreoidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}}
			}
			config := validProtocolConfig()
			if test.mutateConfig != nil {
				test.mutateConfig(&config)
			}
			protocol, err := newProtocol(config, exchanger, keySet, bytes.NewReader(sequentialBytes(64)), func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			_, err = protocol.NewVerifier("authorization-code").Verify(context.Background(), browserflow.VerificationContext{
				Provider:      ProviderName,
				Correlation:   browserflow.ProtocolCorrelation{Nonce: "expected-nonce"},
				ProviderState: browserflow.ProviderState{PKCEVerifier: strings.Repeat("v", browserflow.MinPKCEVerifierBytes)},
			})
			if !errors.Is(err, test.want) || (test.wantNot != nil && errors.Is(err, test.wantNot)) ||
				strings.Contains(err.Error(), "secret detail") {
				t.Fatalf("error = %v, want %v and not %v", err, test.want, test.wantNot)
			}
		})
	}
}

func TestProtocolRequiresAuthorizedPartyForMultipleAudiences(t *testing.T) {
	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	key := mustRSAKey(t)
	exchanger := &exchangeStub{result: ExchangeResult{RawIDToken: signIDToken(t, key, jwt.MapClaims{
		"iss": "https://issuer.example", "sub": "subject-42",
		"aud": []string{"elitea-client", "api"}, "azp": "elitea-client",
		"exp": now.Add(time.Hour).Unix(), "iat": now.Add(-time.Minute).Unix(),
		"nonce": "expected-nonce", "email_verified": true,
	})}}
	protocol, err := newProtocol(validProtocolConfig(), exchanger, &coreoidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}}, bytes.NewReader(sequentialBytes(64)), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := protocol.NewVerifier("authorization-code").Verify(context.Background(), browserflow.VerificationContext{
		Provider:      ProviderName,
		Correlation:   browserflow.ProtocolCorrelation{Nonce: "expected-nonce"},
		ProviderState: browserflow.ProviderState{PKCEVerifier: strings.Repeat("v", browserflow.MinPKCEVerifierBytes)},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProtocolRejectsDuplicateIdentityClaims(t *testing.T) {
	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	key := mustRSAKey(t)
	rawClaims := fmt.Sprintf(
		`{"iss":"https://issuer.example","sub":"first","sub":"second","aud":"elitea-client","exp":%d,"iat":%d,"nonce":"expected-nonce","email_verified":true}`,
		now.Add(time.Hour).Unix(), now.Add(-time.Minute).Unix(),
	)
	exchanger := &exchangeStub{result: ExchangeResult{RawIDToken: signRawIDToken(t, key, []byte(rawClaims))}}
	protocol, err := newProtocol(validProtocolConfig(), exchanger, &coreoidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}}, bytes.NewReader(sequentialBytes(64)), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	_, err = protocol.NewVerifier("authorization-code").Verify(context.Background(), browserflow.VerificationContext{
		Provider:      ProviderName,
		Correlation:   browserflow.ProtocolCorrelation{Nonce: "expected-nonce"},
		ProviderState: browserflow.ProviderState{PKCEVerifier: strings.Repeat("v", browserflow.MinPKCEVerifierBytes)},
	})
	if !errors.Is(err, ErrAssertionRejected) {
		t.Fatalf("error = %v", err)
	}
}

func TestProtocolPreservesCurrentProviderReferenceFallbackAndExpirationOverride(t *testing.T) {
	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	key := mustRSAKey(t)
	exchanger := &exchangeStub{result: ExchangeResult{RawIDToken: signIDToken(t, key, jwt.MapClaims{
		"iss": "https://issuer.example", "sub": "subject-fallback", "aud": "elitea-client",
		"exp": now.Add(time.Hour).Unix(), "iat": now.Add(-time.Minute).Unix(), "nonce": "expected-nonce",
	})}}
	config := validProtocolConfig()
	config.ExpirationOverride = 15 * time.Minute
	config.RequireEmailVerified = false
	protocol, err := newProtocol(config, exchanger, &coreoidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}}, bytes.NewReader(sequentialBytes(64)), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	assertion, err := protocol.NewVerifier("authorization-code").Verify(context.Background(), browserflow.VerificationContext{
		Provider:      ProviderName,
		Correlation:   browserflow.ProtocolCorrelation{Nonce: "expected-nonce"},
		ProviderState: browserflow.ProviderState{PKCEVerifier: strings.Repeat("v", browserflow.MinPKCEVerifierBytes)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if assertion.ProviderReference != "subject-fallback" || assertion.Expiration == nil ||
		!assertion.Expiration.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("assertion = %+v", assertion)
	}

	config.ExpirationOverride = 2 * time.Hour
	protocol, err = newProtocol(config, exchanger, &coreoidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}}, bytes.NewReader(sequentialBytes(64)), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	assertion, err = protocol.NewVerifier("authorization-code").Verify(context.Background(), browserflow.VerificationContext{
		Provider:      ProviderName,
		Correlation:   browserflow.ProtocolCorrelation{Nonce: "expected-nonce"},
		ProviderState: browserflow.ProviderState{PKCEVerifier: strings.Repeat("v", browserflow.MinPKCEVerifierBytes)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if assertion.Expiration == nil || !assertion.Expiration.Equal(now.Add(time.Hour)) {
		t.Fatalf("extended expiration = %v, want signed exp", assertion.Expiration)
	}
}

func TestOAuth2CodeExchangerIsBoundedAndSendsPKCEVerifier(t *testing.T) {
	transport := &roundTripperStub{roundTrip: func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.String() != "https://issuer.example/token" {
			t.Fatalf("request = %s %s", request.Method, request.URL)
		}
		clientID, clientSecret, ok := request.BasicAuth()
		if !ok || clientID != "elitea-client" || clientSecret != "client-secret" {
			t.Fatalf("basic auth = %q %q %t", clientID, clientSecret, ok)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		values, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatal(err)
		}
		if values.Get("grant_type") != "authorization_code" || values.Get("code") != "authorization-code" ||
			values.Get("code_verifier") != strings.Repeat("v", browserflow.MinPKCEVerifierBytes) ||
			values.Get("redirect_uri") != "https://elitea.example/auth/oidc/login_callback" ||
			values.Get("client_secret") != "" {
			t.Fatalf("token form = %v", values)
		}
		return tokenResponse(http.StatusOK, `{"access_token":"ephemeral","token_type":"Bearer","id_token":"signed-id-token"}`), nil
	}}
	exchanger, err := NewOAuth2CodeExchanger(validExchangerConfig(), transport)
	if err != nil {
		t.Fatal(err)
	}
	result, err := exchanger.Exchange(context.Background(), ExchangeRequest{
		AuthorizationCode: "authorization-code",
		PKCEVerifier:      strings.Repeat("v", browserflow.MinPKCEVerifierBytes),
	})
	if err != nil || result.RawIDToken != "signed-id-token" || transport.calls != 1 {
		t.Fatalf("result = %+v error = %v calls = %d", result, err, transport.calls)
	}
}

func TestOAuth2CodeExchangerSupportsCurrentClientSecretPostMode(t *testing.T) {
	transport := &roundTripperStub{roundTrip: func(request *http.Request) (*http.Response, error) {
		if _, _, ok := request.BasicAuth(); ok {
			t.Fatal("client_secret_post request used Basic authentication")
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		values, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatal(err)
		}
		if values.Get("client_id") != "elitea-client" || values.Get("client_secret") != "client-secret" ||
			values.Get("code_verifier") == "" {
			t.Fatalf("token form = %v", values)
		}
		return tokenResponse(http.StatusOK, `{"access_token":"ephemeral","token_type":"Bearer","id_token":"signed-id-token"}`), nil
	}}
	config := validExchangerConfig()
	config.AuthStyle = TokenEndpointAuthPost
	exchanger, err := NewOAuth2CodeExchanger(config, transport)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exchanger.Exchange(context.Background(), ExchangeRequest{
		AuthorizationCode: "authorization-code",
		PKCEVerifier:      strings.Repeat("v", browserflow.MinPKCEVerifierBytes),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOAuth2CodeExchangerSeparatesRejectionFromDependencyFailure(t *testing.T) {
	tests := []struct {
		name     string
		response *http.Response
		err      error
		want     error
	}{
		{name: "invalid grant", response: tokenResponse(http.StatusBadRequest, `{"error":"invalid_grant"}`), want: ErrAssertionRejected},
		{name: "access denied", response: tokenResponse(http.StatusBadRequest, `{"error":"access_denied"}`), want: ErrAssertionRejected},
		{name: "invalid client is configuration failure", response: tokenResponse(http.StatusBadRequest, `{"error":"invalid_client"}`), want: ErrProviderUnavailable},
		{name: "unknown bad request", response: tokenResponse(http.StatusBadRequest, `{"error":"unknown"}`), want: ErrProviderUnavailable},
		{name: "unauthorized", response: tokenResponse(http.StatusUnauthorized, `{"error":"invalid_client"}`), want: ErrProviderUnavailable},
		{name: "forbidden", response: tokenResponse(http.StatusForbidden, `{"error":"access_denied"}`), want: ErrProviderUnavailable},
		{name: "not found", response: tokenResponse(http.StatusNotFound, "missing"), want: ErrProviderUnavailable},
		{name: "provider unavailable", response: tokenResponse(http.StatusServiceUnavailable, "do not leak"), want: ErrProviderUnavailable},
		{name: "redirect rejected", response: tokenResponse(http.StatusFound, "redirect"), want: ErrProviderUnavailable},
		{name: "transport outage", err: errors.New("dial secret endpoint detail"), want: ErrProviderUnavailable},
		{name: "malformed success", response: tokenResponse(http.StatusOK, "not-json"), want: ErrProviderUnavailable},
		{name: "missing id token", response: tokenResponse(http.StatusOK, `{"access_token":"ephemeral","token_type":"Bearer"}`), want: ErrProviderUnavailable},
		{name: "duplicate id token", response: tokenResponse(http.StatusOK, `{"access_token":"ephemeral","token_type":"Bearer","id_token":"first","id_token":"second"}`), want: ErrProviderUnavailable},
		{name: "duplicate error", response: tokenResponse(http.StatusBadRequest, `{"error":"invalid_grant","error":"access_denied"}`), want: ErrProviderUnavailable},
		{name: "nested duplicate member", response: tokenResponse(http.StatusOK, `{"access_token":"ephemeral","token_type":"Bearer","id_token":"signed","extra":{"value":1,"value":2}}`), want: ErrProviderUnavailable},
		{name: "missing content type", response: tokenResponseWithoutContentType(http.StatusOK, `{"access_token":"ephemeral","token_type":"Bearer","id_token":"signed"}`), want: ErrProviderUnavailable},
		{name: "wrong content type", response: tokenResponseWithContentType(http.StatusOK, `{"access_token":"ephemeral","token_type":"Bearer","id_token":"signed"}`, "text/plain"), want: ErrProviderUnavailable},
		{name: "duplicate content type", response: tokenResponseWithContentTypes(http.StatusOK, `{"access_token":"ephemeral","token_type":"Bearer","id_token":"signed"}`, "application/json", "text/plain"), want: ErrProviderUnavailable},
		{name: "oversized response", response: tokenResponse(http.StatusOK, strings.Repeat("x", 4097)), want: ErrProviderUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := &roundTripperStub{roundTrip: func(*http.Request) (*http.Response, error) {
				return test.response, test.err
			}}
			config := validExchangerConfig()
			config.MaxResponseBytes = 4096
			exchanger, err := NewOAuth2CodeExchanger(config, transport)
			if err != nil {
				t.Fatal(err)
			}
			_, err = exchanger.Exchange(context.Background(), ExchangeRequest{
				AuthorizationCode: "authorization-code",
				PKCEVerifier:      strings.Repeat("v", browserflow.MinPKCEVerifierBytes),
			})
			if !errors.Is(err, test.want) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestOIDCConstructorsRejectTLSBypassWeakAlgorithmsAndFalseBounds(t *testing.T) {
	keySet := failingKeySet{err: ErrProviderUnavailable}
	exchanger := &exchangeStub{}
	protocolTests := []func(*Config){
		func(config *Config) { config.Issuer = "http://issuer.example" },
		func(config *Config) { config.AuthorizationEndpoint = "http://issuer.example/authorize" },
		func(config *Config) { config.AuthorizationEndpoint += "?state=configured" },
		func(config *Config) { config.AuthorizationEndpoint = "https://issuer.exämple/authorize" },
		func(config *Config) { config.LoginMode = "GET" },
		func(config *Config) { config.LoginMode = " post" },
		func(config *Config) { config.RedirectURI = "http://elitea.example/callback" },
		func(config *Config) { config.SupportedSigningAlgorithms = []string{"none"} },
		func(config *Config) { config.SupportedSigningAlgorithms = []string{"HS256"} },
		func(config *Config) { config.Scopes = []string{"profile", "email"} },
	}
	for index, mutate := range protocolTests {
		config := validProtocolConfig()
		mutate(&config)
		if _, err := NewProtocol(config, exchanger, keySet); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("protocol case %d error = %v", index, err)
		}
	}

	exchangeTests := []func(*CodeExchangerConfig){
		func(config *CodeExchangerConfig) { config.TokenEndpoint = "http://issuer.example/token" },
		func(config *CodeExchangerConfig) { config.RedirectURI = "http://elitea.example/callback" },
		func(config *CodeExchangerConfig) { config.MaxResponseBytes = maxTokenResponseBytes + 1 },
	}
	for index, mutate := range exchangeTests {
		config := validExchangerConfig()
		mutate(&config)
		if _, err := NewOAuth2CodeExchanger(config, &roundTripperStub{}); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("exchanger case %d error = %v", index, err)
		}
	}
}

func TestProtocolPreservesEndpointQueryAndSelectsStrictInitiationTransport(t *testing.T) {
	tests := []struct {
		name string
		mode browserapp.OIDCAuthorizationTransport
		want browserapp.OIDCAuthorizationTransport
	}{
		{name: "default post", want: browserapp.OIDCAuthorizationPOST},
		{name: "explicit post", mode: browserapp.OIDCAuthorizationPOST, want: browserapp.OIDCAuthorizationPOST},
		{name: "explicit get", mode: browserapp.OIDCAuthorizationGET, want: browserapp.OIDCAuthorizationGET},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validProtocolConfig()
			config.LoginMode = test.mode
			config.AuthorizationEndpoint += "?prompt=login&tenant=elitea"
			protocol, err := NewProtocol(config, &exchangeStub{}, failingKeySet{err: ErrProviderUnavailable})
			if err != nil {
				t.Fatal(err)
			}
			authorization, err := protocol.NewAuthorization(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			state := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, browserflow.TransactionIDRandomBytes))
			request, err := protocol.AuthorizationRequest(state, authorization)
			if err != nil {
				t.Fatal(err)
			}
			if request.Transport != test.want ||
				request.Endpoint != "https://issuer.example/authorize?prompt=login&tenant=elitea" {
				t.Fatalf("authorization request = %+v", request)
			}
		})
	}
}

func TestProtocolRejectsAmbiguousOrSensitiveAuthorizationEndpointQuery(t *testing.T) {
	controlled := []string{
		"response_type", "client_id", "redirect_uri", "scope", "state", "nonce",
		"code_challenge", "code_challenge_method", "client_secret", "code_verifier",
		"access_token", "refresh_token", "id_token",
	}
	for _, key := range controlled {
		t.Run(key, func(t *testing.T) {
			config := validProtocolConfig()
			config.AuthorizationEndpoint += "?" + key + "=configured"
			if _, err := NewProtocol(config, &exchangeStub{}, failingKeySet{err: ErrProviderUnavailable}); !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	for name, query := range map[string]string{
		"duplicate extension": "prompt=login&prompt=consent",
		"malformed escape":    "prompt=%zz",
		"control value":       "prompt=%0Ainjected",
		"too many":            "a=1&b=1&c=1&d=1&e=1&f=1&g=1&h=1&i=1&j=1&k=1&l=1&m=1&n=1&o=1&p=1&q=1",
	} {
		t.Run(name, func(t *testing.T) {
			config := validProtocolConfig()
			config.AuthorizationEndpoint += "?" + query
			if _, err := NewProtocol(config, &exchangeStub{}, failingKeySet{err: ErrProviderUnavailable}); !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func validProtocolConfig() Config {
	return Config{
		Issuer:                     "https://issuer.example",
		AuthorizationEndpoint:      "https://issuer.example/authorize",
		ClientID:                   "elitea-client",
		RedirectURI:                "https://elitea.example/auth/oidc/login_callback",
		Scopes:                     []string{"openid", "profile", "email"},
		SupportedSigningAlgorithms: []string{"RS256"},
		RequireEmailVerified:       true,
	}
}

func validExchangerConfig() CodeExchangerConfig {
	return CodeExchangerConfig{
		TokenEndpoint:  "https://issuer.example/token",
		ClientID:       "elitea-client",
		ClientSecret:   "client-secret",
		RedirectURI:    "https://elitea.example/auth/oidc/login_callback",
		AuthStyle:      TokenEndpointAuthBasic,
		RequestTimeout: 5 * time.Second,
	}
}

type exchangeStub struct {
	request ExchangeRequest
	result  ExchangeResult
	err     error
}

func (stub *exchangeStub) Exchange(_ context.Context, request ExchangeRequest) (ExchangeResult, error) {
	stub.request = request
	return stub.result, stub.err
}

type failingKeySet struct{ err error }

func (keySet failingKeySet) VerifySignature(context.Context, string) ([]byte, error) {
	return nil, keySet.err
}

type roundTripperStub struct {
	roundTrip func(*http.Request) (*http.Response, error)
	calls     int
}

func (stub *roundTripperStub) RoundTrip(request *http.Request) (*http.Response, error) {
	stub.calls++
	return stub.roundTrip(request)
}

func tokenResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func tokenResponseWithoutContentType(status int, body string) *http.Response {
	response := tokenResponse(status, body)
	response.Header.Del("Content-Type")
	return response
}

func tokenResponseWithContentType(status int, body string, contentType string) *http.Response {
	response := tokenResponse(status, body)
	response.Header.Set("Content-Type", contentType)
	return response
}

func tokenResponseWithContentTypes(status int, body string, contentTypes ...string) *http.Response {
	response := tokenResponseWithoutContentType(status, body)
	for _, contentType := range contentTypes {
		response.Header.Add("Content-Type", contentType)
	}
	return response
}

func mustRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func signIDToken(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims) string {
	return signIDTokenWithKID(t, key, "test-key", claims)
}

func signIDTokenWithKID(t *testing.T, key *rsa.PrivateKey, keyID string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = keyID
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func signRawIDToken(t *testing.T, key *rsa.PrivateKey, claims []byte) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test-key","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString(claims)
	unsigned := header + "." + payload
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func sequentialBytes(length int) []byte {
	value := make([]byte, length)
	for index := range value {
		value[index] = byte(index)
	}
	return value
}

func cloneClaims(source jwt.MapClaims) jwt.MapClaims {
	clone := make(jwt.MapClaims, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
