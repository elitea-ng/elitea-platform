package configurations_test

// The token rule of the action table on the global provider update (ADR-0059
// R1.2b, issue 2993): the middleware leaves the decision to the handler
// (adminaccess.TokenRuleInHandler), which classifies the update against the
// stored row and refuses an inherit token the `route` outcome only.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// providerTokenRouter is providerEventRouter for an inherit personal access
// token, with the token caller the middleware records.
func providerTokenRouter(pool *pgxpool.Pool) chi.Router {
	handler := configurations.NewHandler(pool, configurations.WithPublicProjectID(globalScopeProject))
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := auth.ContextWithUser(req.Context(),
				auth.User{ID: "7", UserID: "7", CredentialKind: auth.CredentialKindToken, TokenID: "5"})
			ctx = adminaccess.ContextWithTokenCaller(ctx,
				adminaccess.TokenCaller{Access: adminaccess.TokenAccessInherit, StandingGlobalAdmin: true})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Mount("/gateway/providers", handler.GlobalProviderRoutes(openCatalogueGatesExternal))
	return r
}

func storedGlobalProviderAPIBase(t *testing.T, pool *pgxpool.Pool, id int) string {
	t.Helper()
	var apiBase string
	if err := pool.QueryRow(t.Context(), fmt.Sprintf(
		`SELECT data->>'api_base' FROM p_%d.configuration WHERE id = $1`, globalScopeProject), id).Scan(&apiBase); err != nil {
		t.Fatal(err)
	}
	return apiBase
}

func TestAnInheritTokenIsRefusedAGlobalProviderRouteChangeOnly(t *testing.T) {
	pool := newCreateEchoPool(t)
	created := platformSurfacePost(t, providerEventRouter(pool), "/gateway/providers", map[string]any{
		"elitea_title": "autotest_provider_token",
		"type":         "open_ai",
		"data":         map[string]any{"api_base": "http://autotest.invalid/v1"},
	})
	if created.Code != http.StatusCreated && created.Code != http.StatusOK {
		t.Fatalf("POST = %d: %s", created.Code, created.Body.String())
	}
	var row struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &row); err != nil || row.ID == 0 {
		t.Fatalf("decode the created provider %s: %v", created.Body.String(), err)
	}
	target := "/gateway/providers/" + strconv.Itoa(row.ID)
	token := providerTokenRouter(pool)

	// The settings outcome: a label, and a body that restates the stored
	// endpoint unchanged.
	for _, body := range []map[string]any{
		{"label": "Renamed"},
		{"label": "Renamed again", "type": "open_ai", "data": map[string]any{"api_base": "http://autotest.invalid/v1"}},
		// A restated type with no data arms the stored-row guard too.
		{"label": "Typed", "type": "open_ai"},
	} {
		if recorder := adminProviderDo(t, token, http.MethodPut, target, body); recorder.Code != http.StatusOK {
			t.Fatalf("token settings PUT %v = %d: %s", body, recorder.Code, recorder.Body.String())
		}
	}

	// The route outcome: a new endpoint, and a new key.
	for _, body := range []map[string]any{
		{"data": map[string]any{"api_base": "http://moved.autotest.invalid/v1"}},
		{"data": map[string]any{"api_key": "a-new-key"}},
	} {
		recorder := adminProviderDo(t, token, http.MethodPut, target, body)
		var refusal struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(recorder.Body.Bytes(), &refusal)
		if recorder.Code != http.StatusForbidden || refusal.Code != adminaccess.CodeTokenCriticalAction {
			t.Fatalf("token route PUT %v = %d: %s, want 403 %s", body, recorder.Code, recorder.Body.String(),
				adminaccess.CodeTokenCriticalAction)
		}
	}
	if got := storedGlobalProviderAPIBase(t, pool, row.ID); got != "http://autotest.invalid/v1" {
		t.Fatalf("after the refusals data.api_base = %q, want it unchanged", got)
	}

	// A session makes the same route change.
	if recorder := adminProviderDo(t, providerEventRouter(pool), http.MethodPut, target,
		map[string]any{"data": map[string]any{"api_base": "http://moved.autotest.invalid/v1"}}); recorder.Code != http.StatusOK {
		t.Fatalf("session route PUT = %d: %s", recorder.Code, recorder.Body.String())
	}
}
