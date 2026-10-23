package secrets_test

// A SECRET COULD BE STORED UNDER A NAME NOTHING CAN RESOLVE.
//
// `Create` accepted any non-empty name and answered 201 with
// `secret_name: "{{secret.<name>}}"`. The placeholder resolver matches
// `[A-Za-z0-9_]+` (infra/storage/expansion_unsecreter.go), and a name it does
// not match is left in place verbatim. So a secret named `openai-api-key` was
// stored, listed, and ADVERTISED with a reference token that no expansion can
// resolve: the toolkit sent the literal `{{secret.openai-api-key}}` to the
// provider as the API key and failed with an opaque upstream 401.
//
// The administration-mode routes already refused the same name (admin.go).
// The shipped SPA invited it — its NAME_PATTERN allows a hyphen.
//
// The rename case is the trap in the fix. A vault written before this rule can
// hold such a name, and the exact-name SDK route reads it. So an edit that does
// not CHANGE the name must keep working.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/secrets"
)

// unreachableSecretPool is an already-closed pool. A name the handler ACCEPTS
// then reaches the vault read and fails there with 500, which is what the
// control cases below measure: 500 means the name passed, 400 means it did not.
func unreachableSecretPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://unused:unused@127.0.0.1:1/unused")
	if err != nil {
		t.Fatalf("build the closed pool: %v", err)
	}
	pool.Close()
	return pool
}

// secretNameRouter routes the two write methods directly. The project gate is
// measured in project_authorization_postgres_integration_test.go; these cases
// are about the body.
func secretNameRouter(t *testing.T) *chi.Mux {
	t.Helper()
	h := secrets.NewHandler(unreachableSecretPool(t))
	router := chi.NewRouter()
	router.Post("/secrets/{projectID}", h.Create)
	router.Put("/secret/{projectID}/{name}", h.Update)
	return router
}

const wantSecretNameRefusal = "secret name must contain only letters, digits and underscores"

func TestCreateRefusesANameThePlaceholderCannotResolve(t *testing.T) {
	for _, name := range []string{
		"openai-api-key",
		"openai.api.key",
		"openai api key",
		"{{secret.nested}}",
		strings.Repeat("a", 129),
	} {
		t.Run(name, func(t *testing.T) {
			body := strings.NewReader(`{"name":"` + name + `","value":"sk-live"}`)
			req := httptest.NewRequest(http.MethodPost, "/secrets/5", body)
			rec := httptest.NewRecorder()
			secretNameRouter(t).ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d. This name is stored and can never be resolved.",
					rec.Code, http.StatusBadRequest)
			}
			if !strings.Contains(rec.Body.String(), wantSecretNameRefusal) {
				t.Fatalf("body = %s, want the administration-mode message", rec.Body.String())
			}
		})
	}
}

// The control. A resolvable name must pass the check and reach the vault. A
// guard that refuses every name would fail here.
func TestCreateAcceptsAResolvableName(t *testing.T) {
	body := strings.NewReader(`{"name":"openai_api_key","value":"sk-live"}`)
	req := httptest.NewRequest(http.MethodPost, "/secrets/5", body)
	rec := httptest.NewRecorder()
	secretNameRouter(t).ServeHTTP(rec, req)

	if rec.Code == http.StatusBadRequest {
		t.Fatalf("a resolvable name was refused: %s", rec.Body.String())
	}
}

// A RENAME onto an unresolvable name is refused.
func TestUpdateRefusesARenameToAnUnresolvableName(t *testing.T) {
	body := strings.NewReader(`{"name":"openai-api-key","value":"sk-live"}`)
	req := httptest.NewRequest(http.MethodPut, "/secret/5/openai_api_key", body)
	rec := httptest.NewRecorder()
	secretNameRouter(t).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), wantSecretNameRefusal) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// A VALUE-ONLY edit of a secret already stored under an unresolvable name must
// keep working. The name does not change, so the rule does not apply to it.
// The two cases below are the omitted-name form and the same-name form.
func TestUpdateKeepsAValueOnlyEditOfAStoredUnresolvableName(t *testing.T) {
	for _, body := range []string{
		`{"value":"sk-live"}`,
		`{"name":"openai-api-key","value":"sk-live"}`,
	} {
		t.Run(body, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/secret/5/openai-api-key", strings.NewReader(body))
			rec := httptest.NewRecorder()
			secretNameRouter(t).ServeHTTP(rec, req)

			if rec.Code == http.StatusBadRequest {
				t.Fatalf("a value-only edit of a stored hyphenated secret was refused: %s. "+
					"The SDK route still reads that secret by its exact name.", rec.Body.String())
			}
		})
	}
}
