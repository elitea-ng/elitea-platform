package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// DEFECT: the /api/v2/auth/token route signed every personal access token with
// the raw key of WithTokenSigningKey, which the composition root filled from
// APPLICATION_SECRET_KEY. A deployment that authenticates through
// ELITEA_AUTH_CONFIG_FILE validates a personal access token with a DIFFERENT
// key: the bytes of credentials.pat_signing_key_file, which
// deploy/scripts/gen-runtime-certs.sh mints at random.
//
// Evidence of the failure: deploy/docker-compose.standalone-full.yml sets both
// APPLICATION_SECRET_KEY and ELITEA_AUTH_CONFIG_FILE on the same service. The
// route returned a token signed with "changeme-standalone", the FormGraph
// validator parsed it against the random file bytes, and the request answered
// 401 "token validation failed". The route shows the plaintext one time only,
// so the user kept a permanently dead credential.
//
// The handler must sign with the key the SAME deployment validates with.

// patSignerStub is the FormGraph's role: it holds the deployment's real PAT
// signing key.
type patSignerStub struct {
	key      []byte
	uuid     *string
	expires  *time.Time
	calls    int
	signFunc func([]byte, *string, *time.Time) (string, error)
}

func (s *patSignerStub) SignPAT(tokenUUID *string, expiresAt *time.Time) (string, error) {
	s.calls++
	s.uuid, s.expires = tokenUUID, expiresAt
	return s.signFunc(s.key, tokenUUID, expiresAt)
}

func signWithKey(key []byte, tokenUUID *string, expiresAt *time.Time) (string, error) {
	var expires *string
	if expiresAt != nil {
		value := expiresAt.Format("2006-01-02T15:04")
		expires = &value
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS512, baselineTokenClaims{
		UUID:    tokenUUID,
		Expires: expires,
	}).SignedString(key)
}

func verifies(t *testing.T, token string, key []byte) bool {
	t.Helper()
	_, err := jwt.ParseWithClaims(token, &baselineTokenClaims{},
		func(*jwt.Token) (any, error) { return key, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS512.Alg()}),
	)
	return err == nil
}

func createdTokenFrom(t *testing.T, handler *Handler) string {
	t.Helper()
	req := browserSessionTokenRequest("/token/", `{"name":"ci-token"}`)
	rec := httptest.NewRecorder()
	handler.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created Token
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response %s: %v", rec.Body.String(), err)
	}
	return created.Token
}

func signingTestHandler(signer TokenSigner, sessionSecret []byte) *Handler {
	const tokenUUID = "8ce4be49-0d10-4f05-a63f-d6d46f99a3f0"
	return &Handler{
		tokenSigningKey: sessionSecret,
		tokenSigner:     signer,
		tokens: tokenRepositoryStub{
			createFunc: func(_ context.Context, input tokenCreateInput) (tokenRecord, error) {
				return tokenRecord{
					ID:     11,
					UUID:   stringAddress(tokenUUID),
					UserID: input.OwnerID,
					Name:   input.Name,
				}, nil
			},
		},
	}
}

func TestTokenCreateSignsWithTheDeploymentPATKeyNotTheSessionSecret(t *testing.T) {
	patKey := []byte("0123456789abcdef0123456789abcdef")
	sessionSecret := []byte("changeme-standalone")
	signer := &patSignerStub{key: patKey, signFunc: signWithKey}

	token := createdTokenFrom(t, signingTestHandler(signer, sessionSecret))

	if signer.calls != 1 {
		t.Fatalf("signer calls = %d, want 1 — the route must sign through the deployment signer", signer.calls)
	}
	if !verifies(t, token, patKey) {
		t.Fatal("the returned token does not verify with the deployment PAT signing key")
	}
	if verifies(t, token, sessionSecret) {
		t.Fatal("the returned token verifies with APPLICATION_SECRET_KEY, the key no validator reads it back with")
	}
	if signer.uuid == nil || *signer.uuid == "" {
		t.Fatal("the signer received no token uuid")
	}
}

// With no signer the raw key still signs, which is the OIDC-only shape:
// APPLICATION_SECRET_KEY signs the token and authsvc.NewLocalValidator reads
// it back with the same value.
func TestTokenCreateFallsBackToTheSessionSecretWhenNoSignerIsWired(t *testing.T) {
	sessionSecret := []byte("changeme-standalone")

	token := createdTokenFrom(t, signingTestHandler(nil, sessionSecret))

	if !verifies(t, token, sessionSecret) {
		t.Fatal("the returned token does not verify with the configured signing key")
	}
}

// A deployment that supplies only a signer must serve the route. The
// availability gate read the raw key alone, so a form deployment with no
// APPLICATION_SECRET_KEY answered 503 on every token route.
func TestTokenRoutesAreAvailableWithASignerAndNoRawKey(t *testing.T) {
	patKey := []byte("0123456789abcdef0123456789abcdef")
	signer := &patSignerStub{key: patKey, signFunc: signWithKey}

	token := createdTokenFrom(t, signingTestHandler(signer, nil))

	if !verifies(t, token, patKey) {
		t.Fatal("the returned token does not verify with the deployment PAT signing key")
	}
}

// With neither a signer nor a key the route must stay 503. It must not return
// an unsigned or empty bearer.
func TestTokenRoutesReportNotConfiguredWithNoSignerAndNoKey(t *testing.T) {
	handler := signingTestHandler(nil, nil)
	req := browserSessionTokenRequest("/token/", `{"name":"ci-token"}`)
	rec := httptest.NewRecorder()
	handler.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
