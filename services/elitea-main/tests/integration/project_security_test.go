package integration

import (
	"context"
	"encoding/json"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	v2toolkits "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
)

func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testpg.DSN(os.Getenv("INTEGRATION_DATABASE_URL"))
	if dsn == "" {
		t.Skip("INTEGRATION_DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("open integration database: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(t.Context(), "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		t.Fatalf("enable pgvector: %v", err)
	}
	if err := db.RunMigrations(t.Context(), pool); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return pool
}

func seedProjectMember(t *testing.T, pool *pgxpool.Pool, projectID, userID int) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO auth_core__user (id, email, name) VALUES ($1, $2, $3)
		ON CONFLICT (id) DO NOTHING`, userID, "integration@example.test", "Integration User"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO centry.project (id, name, owner_id, create_success)
		VALUES ($1, $2, $3, true) ON CONFLICT (id) DO NOTHING`, projectID, "integration-project", userID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := pool.Exec(ctx, "SELECT create_tenant_schema($1)", "p_2"); err != nil {
		t.Fatalf("create tenant schema: %v", err)
	}
	var roleID int
	if err := pool.QueryRow(ctx, `
		INSERT INTO auth_core__project_role (project_id, name) VALUES ($1, 'editor')
		ON CONFLICT (project_id, name) DO UPDATE SET name = EXCLUDED.name
		RETURNING id`, projectID).Scan(&roleID); err != nil {
		t.Fatalf("seed project role: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO auth_core__project_user_role (project_id, user_id, role_id)
		VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, projectID, userID, roleID); err != nil {
		t.Fatalf("seed project membership: %v", err)
	}
}

func requestWithUser(method, path, userID string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	return req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: userID}))
}

func TestProjectAuthorizationAndToolkitRedaction(t *testing.T) {
	pool := integrationPool(t)
	seedProjectMember(t, pool, 2, 2)

	r := chi.NewRouter()
	r.With(apimw.RequireProjectAccess(pool)).Get("/projects/{projectID}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	allowed := httptest.NewRecorder()
	r.ServeHTTP(allowed, requestWithUser(http.MethodGet, "/projects/2", "2"))
	if allowed.Code != http.StatusNoContent {
		t.Fatalf("member status = %d, want %d", allowed.Code, http.StatusNoContent)
	}

	denied := httptest.NewRecorder()
	r.ServeHTTP(denied, requestWithUser(http.MethodGet, "/projects/1", "2"))
	if denied.Code != http.StatusForbidden {
		t.Fatalf("cross-project status = %d, want %d", denied.Code, http.StatusForbidden)
	}

	if _, err := pool.Exec(t.Context(), `
		INSERT INTO p_2.elitea_tools (name, type, description, owner_id, author_id, settings)
		VALUES ('private-toolkit', 'github', '', 2, 2, '{"repository":"EliteaAI/elitea-platform","access_token":"must-not-leak"}')`); err != nil {
		t.Fatalf("seed toolkit: %v", err)
	}
	h := v2toolkits.NewHandler(pool)
	toolkitRouter := chi.NewRouter()
	toolkitRouter.Get("/tools/{projectID}", h.List)
	result := httptest.NewRecorder()
	toolkitRouter.ServeHTTP(result, httptest.NewRequest(http.MethodGet, "/tools/2", nil))
	if result.Code != http.StatusOK {
		t.Fatalf("toolkit listing status = %d, want %d", result.Code, http.StatusOK)
	}
	var body struct {
		Rows []struct {
			Settings map[string]any `json:"settings"`
		} `json:"rows"`
	}
	if err := json.NewDecoder(result.Body).Decode(&body); err != nil {
		t.Fatalf("decode toolkit listing: %v", err)
	}
	if len(body.Rows) != 1 || body.Rows[0].Settings["repository"] != "EliteaAI/elitea-platform" {
		t.Fatalf("unexpected toolkit listing: %#v", body)
	}
	if _, leaked := body.Rows[0].Settings["access_token"]; leaked {
		t.Fatal("toolkit access token leaked in listing")
	}
}
