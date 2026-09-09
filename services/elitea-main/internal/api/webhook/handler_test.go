package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type mockWebhookRepo struct {
	webhooks []Webhook
}

func (m *mockWebhookRepo) List(_ context.Context, _ string) ([]Webhook, error) {
	return m.webhooks, nil
}

func (m *mockWebhookRepo) Create(_ context.Context, projectID string, wh Webhook) (Webhook, error) {
	wh.ID = "wh-new"
	wh.ProjectID = projectID
	wh.CreatedAt = time.Now()
	wh.UpdatedAt = time.Now()
	m.webhooks = append(m.webhooks, wh)
	return wh, nil
}

func (m *mockWebhookRepo) Get(_ context.Context, _ string, id string) (Webhook, error) {
	for _, wh := range m.webhooks {
		if wh.ID == id {
			return wh, nil
		}
	}
	return Webhook{}, nil
}

func (m *mockWebhookRepo) Update(_ context.Context, _ string, id string, wh Webhook) (Webhook, error) {
	wh.ID = id
	wh.UpdatedAt = time.Now()
	return wh, nil
}

func (m *mockWebhookRepo) Delete(_ context.Context, _ string, _ string) error {
	return nil
}

func (m *mockWebhookRepo) ListByEvent(_ context.Context, _ string, _ string) ([]Webhook, error) {
	return m.webhooks, nil
}

// permissionResolverFunc adapts a function to auth.PermissionResolver.
type permissionResolverFunc func(
	context.Context,
	auth.User,
	string,
	string,
) (auth.PermissionResolution, error)

func (f permissionResolverFunc) ResolvePermissions(
	ctx context.Context,
	principal auth.User,
	mode string,
	projectID string,
) (auth.PermissionResolution, error) {
	return f(ctx, principal, mode, projectID)
}

// webhookPermissions is the ledger: every string Routes() gates on, paired with
// the route that names it. A route added without a row makes
// TestEveryWebhookRouteIsGated fail rather than ship ungated.
var webhookPermissions = []struct {
	method     string
	path       string
	permission string
}{
	{http.MethodGet, "/api/v2/projects/proj-1/webhooks/", listPermission},
	{http.MethodPost, "/api/v2/projects/proj-1/webhooks/", createPermission},
	{http.MethodGet, "/api/v2/projects/proj-1/webhooks/wh-1", detailsPermission},
	{http.MethodPut, "/api/v2/projects/proj-1/webhooks/wh-1", updatePermission},
	{http.MethodDelete, "/api/v2/projects/proj-1/webhooks/wh-1", deletePermission},
	{http.MethodGet, "/api/v2/projects/proj-1/webhooks/wh-1/deliveries", detailsPermission},
	{http.MethodPost, "/api/v2/projects/proj-1/webhooks/wh-1/deliveries/del-1/redeliver", updatePermission},
}

// allWebhookPermissions is the distinct set the five routes draw from.
var allWebhookPermissions = []string{
	listPermission,
	detailsPermission,
	createPermission,
	updatePermission,
	deletePermission,
}

// entitledResolver admits every webhook permission for any project.
func entitledResolver() permissionResolverFunc {
	return func(
		_ context.Context,
		_ auth.User,
		_ string,
		_ string,
	) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 1, Permissions: allWebhookPermissions}, nil
	}
}

// withTestUser stands in for apimw.Auth, which router.go wraps this mount in.
func withTestUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), auth.User{ID: "1"})))
	})
}

// webhookRouter reproduces router.go's shape: an authenticated caller, the
// {projectID} in the MOUNT pattern, and the subrouter under it.
func webhookRouter(repo Repository, resolver auth.PermissionResolver) *chi.Mux {
	h := NewHandler(repo, WithPermissionResolver(resolver))
	r := chi.NewRouter()
	r.Use(withTestUser)
	r.Route("/api/v2/projects/{projectID}/webhooks", func(r chi.Router) {
		r.Mount("/", h.Routes())
	})
	return r
}

/* ── the refused direction ─────────────────────────────────────────────── */

// A caller who resolves no permission is refused at every route (#496).
//
// Until this change the five routes carried no gate of any kind, so any
// authenticated caller could name any project id and read that project's
// webhook `secret`, repoint its `url`, or rotate the secret.
func TestEveryWebhookRouteIsGated(t *testing.T) {
	resolver := permissionResolverFunc(func(
		_ context.Context,
		_ auth.User,
		mode string,
		projectID string,
	) (auth.PermissionResolution, error) {
		if mode != auth.PermissionModeDefault {
			t.Errorf("resolver called in mode %q, want %q", mode, auth.PermissionModeDefault)
		}
		if projectID != "proj-1" {
			t.Errorf("resolver called for project %q, want the {projectID} of the mount", projectID)
		}
		return auth.PermissionResolution{UserID: 1, Permissions: []string{}}, nil
	})
	r := webhookRouter(&mockWebhookRepo{}, resolver)

	for _, route := range webhookPermissions {
		req := httptest.NewRequest(route.method, route.path, bytes.NewBufferString(`{}`))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s status = %d, want %d. This route is reachable without %s.",
				route.method, route.path, w.Code, http.StatusForbidden, route.permission)
		}
	}

	// The ledger must cover the registrations. chi.Walk reports what is really
	// mounted, so a sixth route cannot be added without a row here.
	registered := 0
	if err := chi.Walk(r, func(string, string, http.Handler, ...func(http.Handler) http.Handler) error {
		registered++
		return nil
	}); err != nil {
		t.Fatalf("walk the mounted webhook routes: %v", err)
	}
	if registered != len(webhookPermissions) {
		t.Fatalf("Routes() registers %d routes and webhookPermissions lists %d",
			registered, len(webhookPermissions))
	}
}

/* ── the entitled direction ────────────────────────────────────────────── */

// Each route passes with the exact string it names, and with no other.
//
// The negative half of this case is what pins the ledger: without it a router
// that gated all five on `list` would still pass every other test in this file
// and would let a viewer delete a webhook.
func TestEachWebhookRouteNeedsItsOwnPermission(t *testing.T) {
	for _, route := range webhookPermissions {
		for _, held := range allWebhookPermissions {
			resolver := permissionResolverFunc(func(
				_ context.Context,
				_ auth.User,
				_ string,
				_ string,
			) (auth.PermissionResolution, error) {
				return auth.PermissionResolution{UserID: 1, Permissions: []string{held}}, nil
			})
			r := webhookRouter(&mockWebhookRepo{}, resolver)

			req := httptest.NewRequest(route.method, route.path, bytes.NewBufferString(`{}`))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			refused := w.Code == http.StatusForbidden
			switch {
			case held == route.permission && refused:
				t.Errorf("%s %s was refused while holding its own permission %q",
					route.method, route.path, held)
			case held != route.permission && !refused:
				t.Errorf("%s %s answered %d while holding only %q; it must need %q",
					route.method, route.path, w.Code, held, route.permission)
			}
		}
	}
}

// A caller with no identity is refused before the resolver is asked.
func TestWebhookRoutesRefuseAnUnauthenticatedCaller(t *testing.T) {
	h := NewHandler(&mockWebhookRepo{}, WithPermissionResolver(entitledResolver()))
	r := chi.NewRouter()
	r.Route("/api/v2/projects/{projectID}/webhooks", func(r chi.Router) {
		r.Mount("/", h.Routes())
	})

	for _, route := range webhookPermissions {
		req := httptest.NewRequest(route.method, route.path, bytes.NewBufferString(`{}`))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s status = %d without a caller, want %d",
				route.method, route.path, w.Code, http.StatusUnauthorized)
		}
	}
}

// A Handler built without WithPermissionResolver serves nothing.
func TestWebhookRoutesFailClosedWithoutAResolver(t *testing.T) {
	r := chi.NewRouter()
	r.Use(withTestUser)
	r.Route("/api/v2/projects/{projectID}/webhooks", func(r chi.Router) {
		r.Mount("/", NewHandler(&mockWebhookRepo{}).Routes())
	})

	for _, route := range webhookPermissions {
		req := httptest.NewRequest(route.method, route.path, bytes.NewBufferString(`{}`))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s status = %d with no resolver composed, want %d",
				route.method, route.path, w.Code, http.StatusForbidden)
		}
	}
}

func TestHandler_List(t *testing.T) {
	repo := &mockWebhookRepo{
		webhooks: []Webhook{
			{ID: "wh-1", ProjectID: "proj-1", URL: "https://example.com/hook", Events: []string{"application.created"}, Active: true},
		},
	}
	r := webhookRouter(repo, entitledResolver())

	req := httptest.NewRequest(http.MethodGet, "/api/v2/projects/proj-1/webhooks/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var body map[string]any
	_ = json.NewDecoder(w.Body).Decode(&body)
	items := body["items"].([]any)
	if len(items) != 1 {
		t.Errorf("expected 1 webhook, got %d", len(items))
	}
}

func TestHandler_Create(t *testing.T) {
	repo := &mockWebhookRepo{}
	r := webhookRouter(repo, entitledResolver())

	payload, _ := json.Marshal(Webhook{URL: "https://example.com/hook", Events: []string{"skill.created"}, Active: true})
	req := httptest.NewRequest(http.MethodPost, "/api/v2/projects/proj-1/webhooks/", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var wh Webhook
	_ = json.NewDecoder(w.Body).Decode(&wh)
	if wh.ID != "wh-new" {
		t.Errorf("expected ID wh-new, got %s", wh.ID)
	}
}

func TestHandler_Delete(t *testing.T) {
	repo := &mockWebhookRepo{}
	r := webhookRouter(repo, entitledResolver())

	req := httptest.NewRequest(http.MethodDelete, "/api/v2/projects/proj-1/webhooks/wh-1", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
}
