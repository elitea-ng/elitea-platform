package oidc

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	browserapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/browserauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/browserflow"
)

func TestRemoteKeySetUsesBoundedHTTPSNoRedirectTransport(t *testing.T) {
	key := mustRSAKey(t)
	jwks := marshalJWKS(t, &key.PublicKey)
	transport := &roundTripperStub{roundTrip: func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.String() != "https://issuer.example/keys" {
			t.Fatalf("request = %s %s", request.Method, request.URL)
		}
		return tokenResponse(http.StatusOK, jwks), nil
	}}
	keySet, err := NewRemoteKeySet(RemoteKeySetConfig{
		JWKSURL:          "https://issuer.example/keys",
		RequestTimeout:   5 * time.Second,
		MaxResponseBytes: 4096,
	}, transport)
	if err != nil {
		t.Fatal(err)
	}
	rawToken := signIDToken(t, key, jwt.MapClaims{"sub": "subject"})
	payload, err := keySet.VerifySignature(context.Background(), rawToken)
	if err != nil || !strings.Contains(string(payload), `"sub":"subject"`) || transport.calls != 1 {
		t.Fatalf("payload=%s error=%v calls=%d", payload, err, transport.calls)
	}
}

func TestRemoteKeySetClassifiesHTTPAndSizeFailures(t *testing.T) {
	key := mustRSAKey(t)
	rawToken := signIDToken(t, key, jwt.MapClaims{"sub": "subject"})
	tests := []struct {
		name       string
		response   *http.Response
		algorithms []string
	}{
		{name: "redirect", response: tokenResponse(http.StatusFound, "redirect")},
		{name: "not found", response: tokenResponse(http.StatusNotFound, "missing")},
		{name: "outage", response: tokenResponse(http.StatusServiceUnavailable, "secret detail")},
		{name: "oversized", response: tokenResponse(http.StatusOK, strings.Repeat("x", 4097))},
		{name: "malformed", response: tokenResponse(http.StatusOK, `{"keys":[`)},
		{name: "duplicate keys member", response: tokenResponse(http.StatusOK, `{"keys":[],"keys":[]}`)},
		{name: "too many keys", response: tokenResponse(http.StatusOK, tooManyJWKSKeys(t))},
		{name: "RSA signing key missing modulus and exponent", response: tokenResponse(http.StatusOK, `{"keys":[{"kty":"RSA","use":"sig","alg":"RS256","kid":"broken"}]}`)},
		{name: "no configured algorithm key", response: tokenResponse(http.StatusOK, strings.Replace(marshalJWKS(t, &key.PublicKey), `"alg":"RS256"`, `"alg":"RS512"`, 1)), algorithms: []string{"RS256"}},
		{name: "wrong content type", response: func() *http.Response {
			response := tokenResponse(http.StatusOK, marshalJWKS(t, &key.PublicKey))
			response.Header.Set("Content-Type", "text/html")
			return response
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			keySet, err := NewRemoteKeySet(RemoteKeySetConfig{
				JWKSURL: "https://issuer.example/keys", SupportedSigningAlgorithms: test.algorithms,
				RequestTimeout: 5 * time.Second, MaxResponseBytes: 4096,
			}, &roundTripperStub{roundTrip: func(*http.Request) (*http.Response, error) {
				return test.response, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = keySet.VerifySignature(context.Background(), rawToken)
			if !errors.Is(err, ErrProviderUnavailable) || strings.Contains(err.Error(), "secret detail") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestRemoteKeySetUnknownKIDRemainsAssertionRejection(t *testing.T) {
	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	key := mustRSAKey(t)
	keySet, err := NewRemoteKeySet(RemoteKeySetConfig{
		JWKSURL: "https://issuer.example/keys", SupportedSigningAlgorithms: []string{"RS256"},
		RequestTimeout: 5 * time.Second, MaxResponseBytes: 4096,
	}, &roundTripperStub{roundTrip: func(*http.Request) (*http.Response, error) {
		return tokenResponse(http.StatusOK, marshalJWKS(t, &key.PublicKey)), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	exchanger := &exchangeStub{result: ExchangeResult{RawIDToken: signIDTokenWithKID(t, key, "unknown-key", jwt.MapClaims{
		"iss": "https://issuer.example", "sub": "subject-42", "aud": "elitea-client",
		"exp": now.Add(time.Hour).Unix(), "iat": now.Add(-time.Minute).Unix(),
		"nonce": "expected-nonce", "email_verified": true,
	})}}
	protocol, err := newProtocol(
		validProtocolConfig(), exchanger, keySet,
		strings.NewReader(strings.Repeat("r", 128)), func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = protocol.NewVerifier("authorization-code").Verify(context.Background(), browserflow.VerificationContext{
		Provider:      ProviderName,
		Correlation:   browserflow.ProtocolCorrelation{Nonce: "expected-nonce"},
		ProviderState: browserflow.ProviderState{PKCEVerifier: strings.Repeat("v", browserflow.MinPKCEVerifierBytes)},
	})
	if !errors.Is(err, ErrAssertionRejected) || errors.Is(err, browserapp.ErrAssertionVerifierUnavailable) ||
		errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("error = %v", err)
	}
}

func tooManyJWKSKeys(t *testing.T) string {
	t.Helper()
	keys := make([]map[string]any, maxJWKSKeys+1)
	for index := range keys {
		keys[index] = map[string]any{"kty": "RSA"}
	}
	encoded, err := json.Marshal(map[string]any{"keys": keys})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestRemoteKeySetRejectsUnboundedOrInsecureConfiguration(t *testing.T) {
	for _, config := range []RemoteKeySetConfig{
		{JWKSURL: "http://issuer.example/keys", RequestTimeout: 5 * time.Second, MaxResponseBytes: 4096},
		{JWKSURL: "https://issuer.example/keys", RequestTimeout: 31 * time.Second, MaxResponseBytes: 4096},
		{JWKSURL: "https://issuer.example/keys", RequestTimeout: 5 * time.Second, MaxResponseBytes: maxJWKSResponseBytes + 1},
	} {
		if _, err := NewRemoteKeySet(config, &roundTripperStub{}); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("config=%+v error=%v", config, err)
		}
	}
}

func marshalJWKS(t *testing.T, publicKey *rsa.PublicKey) string {
	t.Helper()
	exponent := big.NewInt(int64(publicKey.E)).Bytes()
	document := map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "test-key",
		"n": base64.RawURLEncoding.EncodeToString(publicKey.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(exponent),
	}}}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
