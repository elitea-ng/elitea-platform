package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

// The seam between RouterConfig.PATSigner and the /api/v2/auth/token route.
//
// THE DEFECT. The route signed every personal access token with
// APPLICATION_SECRET_KEY. A deployment with an authentication configuration
// file reads a token back with a DIFFERENT key: the bytes of
// credentials.pat_signing_key_file. Every token that deployment issued failed
// on first use, and the route shows the plaintext one time only.
//
// WHY THIS TEST EXISTS BESIDE THE HANDLER TEST.
// internal/api/v2/auth/tokens_signing_test.go proves the HANDLER prefers the
// signer, and cmd/elitea-main proves the composition root BUILDS one. Neither
// touches the line that joins them: NewRouter reads cfg.PATSigner and turns it
// into v2auth.WithTokenSigner. That line is where the defect lived. A refactor
// that drops it leaves both other suites green.
//
// It runs against PostgreSQL because the route signs a token only after it
// writes the row. ci-go.yml supplies ELITEA_TEST_DATABASE_URL.
func TestRouterSignsPersonalAccessTokensWithTheConfiguredKey(t *testing.T) {
	pool := newPATSigningPool(t)

	patKey := []byte("0123456789abcdef0123456789abcdef")
	const sessionSecret = "changeme-standalone"

	t.Run("a configured signer signs the token", func(t *testing.T) {
		signer := &routerPATSignerStub{key: patKey}
		router := NewRouter(RouterConfig{
			Pool:               pool,
			AuthValidator:      apimw.TokenValidator(testTokenValidator{user: authenticatedTestUser()}),
			PrincipalValidator: testPrincipalValidator{},
			SessionSecret:      sessionSecret,
			PATSigner:          signer,
		})

		token := createPATThroughRouter(t, router, "router-signer-token")

		if signer.calls != 1 {
			t.Fatalf("deployment signer calls = %d, want 1: NewRouter did not pass "+
				"cfg.PATSigner to the auth handler, so the route signed with "+
				"APPLICATION_SECRET_KEY and no validator can read the token back", signer.calls)
		}
		if !patTokenVerifies(t, token, patKey) {
			t.Fatal("the issued token does not verify with the deployment PAT signing key")
		}
		if patTokenVerifies(t, token, []byte(sessionSecret)) {
			t.Fatal("the issued token verifies with APPLICATION_SECRET_KEY, which this deployment does not read it back with")
		}
	})

	t.Run("no signer falls back to the session secret", func(t *testing.T) {
		router := NewRouter(RouterConfig{
			Pool:               pool,
			AuthValidator:      apimw.TokenValidator(testTokenValidator{user: authenticatedTestUser()}),
			PrincipalValidator: testPrincipalValidator{},
			SessionSecret:      sessionSecret,
		})

		token := createPATThroughRouter(t, router, "router-fallback-token")

		// The OIDC-only shape. APPLICATION_SECRET_KEY both signs the token and
		// reads it back, so the fallback must stay.
		if !patTokenVerifies(t, token, []byte(sessionSecret)) {
			t.Fatal("the issued token does not verify with APPLICATION_SECRET_KEY, " +
				"which the OIDC-only validator reads it back with")
		}
		if patTokenVerifies(t, token, patKey) {
			t.Fatal("the issued token verifies with a key this deployment never configured")
		}
	})
}

// routerPATSignerStub stands in for the deployment's authentication graph. It
// holds the key that graph validates a token with.
type routerPATSignerStub struct {
	key   []byte
	calls int
}

func (s *routerPATSignerStub) SignPAT(tokenUUID *string, expiresAt *time.Time) (string, error) {
	s.calls++
	var expires *string
	if expiresAt != nil {
		value := expiresAt.Format("2006-01-02T15:04")
		expires = &value
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS512, jwt.MapClaims{
		"uuid":    tokenUUID,
		"expires": expires,
	}).SignedString(s.key)
}

func createPATThroughRouter(t *testing.T, router http.Handler, name string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"name": name})
	if err != nil {
		t.Fatalf("encode the request body: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v2/auth/token/", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, testAuthHeader(request))
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST /api/v2/auth/token/ = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var created struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
	if created.Token == "" {
		t.Fatalf("the route returned no token: %s", recorder.Body.String())
	}
	return created.Token
}

func patTokenVerifies(t *testing.T, token string, key []byte) bool {
	t.Helper()
	_, err := jwt.Parse(token,
		func(*jwt.Token) (any, error) { return key, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS512.Alg()}),
	)
	return err == nil
}

// newPATSigningPool builds a throwaway database that holds the two tables the
// token route writes. The route signs a token only after the INSERT commits,
// so a stub repository cannot reach the signing seam.
func newPATSigningPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("ELITEA_TEST_DATABASE_URL") == "" {
		t.Skipf("set ELITEA_TEST_DATABASE_URL to run the personal access token signing seam test")
	}
	pool := newStatusOKIntegrationPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	source := filepath.Join("..", "infra", "db", "migrations", "001_initial.sql")
	initial, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read %s: %v", source, err)
	}
	if _, err := pool.Exec(ctx, string(initial)); err != nil {
		t.Fatalf("apply %s: %v", source, err)
	}
	// The create transaction also stamps elitea_identity.token_lifecycle
	// (tokens.go Create → repos.RecordPATIssued), so the schema this pool holds
	// has to include the migration that creates it or every POST is a 500.
	lifecycle, err := platformmigrations.Files.ReadFile("shared/0125_token_lifecycle.sql")
	if err != nil {
		t.Fatalf("read the token lifecycle migration: %v", err)
	}
	if _, err := pool.Exec(ctx, string(lifecycle)); err != nil {
		t.Fatalf("apply shared/0125_token_lifecycle.sql: %v", err)
	}
	// The token owner. authenticatedTestUser presents user 1.
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user (id, email, name)
VALUES (1, 'member@test.local', 'Member')
ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed the token owner: %v", err)
	}
	return pool
}
