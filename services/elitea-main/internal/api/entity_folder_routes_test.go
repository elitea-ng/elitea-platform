package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/entityfolders"
)

type entityFolderStub struct{ calls int }

func (s *entityFolderStub) List(context.Context, string, entityfolders.Kind) ([]entityfolders.Folder, error) {
	s.calls++
	return []entityfolders.Folder{}, nil
}

func (s *entityFolderStub) Create(_ context.Context, _ string, k entityfolders.Kind, name string, _ *int) (entityfolders.Folder, error) {
	s.calls++
	return entityfolders.Folder{ID: "1", Kind: k, Name: name, ItemIDs: []int{}}, nil
}

func (s *entityFolderStub) Update(_ context.Context, _ string, k entityfolders.Kind, id string, _ *string, _ *int) (entityfolders.Folder, error) {
	s.calls++
	return entityfolders.Folder{ID: id, Kind: k, ItemIDs: []int{}}, nil
}

func (s *entityFolderStub) Delete(context.Context, string, entityfolders.Kind, string) error {
	s.calls++
	return nil
}

func (s *entityFolderStub) Rebalance(context.Context, string, entityfolders.Kind) ([]entityfolders.Folder, error) {
	s.calls++
	return nil, nil
}

func (s *entityFolderStub) Move(context.Context, string, entityfolders.Kind, int, *string) error {
	s.calls++
	return nil
}

func entityFolderRouter(gate func(string) func(http.Handler) http.Handler) (chi.Router, *entityFolderStub) {
	stub := &entityFolderStub{}
	r := chi.NewRouter()
	registerEntityFolderRoutes(r, entityfolders.NewHandler(stub), gate)
	return r, stub
}

func serveEntityFolder(r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(method, path, bytes.NewBufferString(body)))
	return rec
}

// Every kind is gated by the permissions of ITS list, and a write is never gated
// by the list permission: create makes a folder, update does the rest.
func TestEntityFolderRoutesAskEachKindForItsOwnPermission(t *testing.T) {
	var asked []string
	gate := func(permission string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked = append(asked, permission)
				next.ServeHTTP(w, r)
			})
		}
	}
	router, _ := entityFolderRouter(gate)
	for kind, perms := range entityFolderPermissions {
		base := "/entity_folders/prompt_lib/7/" + string(kind)
		for _, tc := range []struct{ method, path, body, want string }{
			{http.MethodGet, base, "", perms.list},
			{http.MethodPost, base, `{"name":"A"}`, perms.create},
			{http.MethodPut, base + "/3", `{"name":"B"}`, perms.update},
			{http.MethodDelete, base + "/3", "", perms.update},
			{http.MethodPut, base + "/items/5", `{"folder_id":"3"}`, perms.update},
		} {
			asked = nil
			rec := serveEntityFolder(router, tc.method, tc.path, tc.body)
			if rec.Code >= 400 {
				t.Fatalf("%s %s: status %d %s", tc.method, tc.path, rec.Code, rec.Body.String())
			}
			if len(asked) != 1 || asked[0] != tc.want {
				t.Fatalf("%s %s asked %v, want exactly [%s]", tc.method, tc.path, asked, tc.want)
			}
		}
	}
}

func TestEntityFolderRoutesPermissionsAreTheListsOwn(t *testing.T) {
	want := map[entityfolders.Kind][3]string{
		entityfolders.KindAgent:    {"models.applications.applications.list", "models.applications.applications.create", "models.applications.application.update"},
		entityfolders.KindPipeline: {"models.applications.applications.list", "models.applications.applications.create", "models.applications.application.update"},
		entityfolders.KindSkill:    {"models.applications.skills.list", "models.applications.skills.create", "models.applications.skills.update"},
		entityfolders.KindToolkit:  {"models.applications.tools.list", "models.applications.tools.create", "models.applications.tool.update"},
		entityfolders.KindMCP:      {"models.applications.tools.list", "models.applications.tools.create", "models.applications.tool.update"},
	}
	if len(entityFolderPermissions) != len(entityfolders.Kinds) {
		t.Fatalf("a kind has no permissions: %d of %d", len(entityFolderPermissions), len(entityfolders.Kinds))
	}
	for kind, perms := range want {
		got := entityFolderPermissions[kind]
		if got.list != perms[0] || got.create != perms[1] || got.update != perms[2] {
			t.Fatalf("%s: %+v, want %v", kind, got, perms)
		}
	}
}

// A denied caller never reaches the handler, and an unknown kind is a 404 that
// does not even ask for a permission.
func TestEntityFolderRoutesRefuseBeforeTheHandlerRuns(t *testing.T) {
	deny := func(string) func(http.Handler) http.Handler {
		return func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
		}
	}
	router, stub := entityFolderRouter(deny)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/entity_folders/prompt_lib/7/agent", ""},
		{http.MethodPost, "/entity_folders/prompt_lib/7/skill", `{"name":"A"}`},
		{http.MethodPut, "/entity_folders/prompt_lib/7/mcp/3", `{"name":"B"}`},
		{http.MethodDelete, "/entity_folders/prompt_lib/7/toolkit/3", ""},
		{http.MethodPut, "/entity_folders/prompt_lib/7/pipeline/items/5", `{}`},
	} {
		if rec := serveEntityFolder(router, tc.method, tc.path, tc.body); rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s: status %d, want 403", tc.method, tc.path, rec.Code)
		}
	}
	if stub.calls != 0 {
		t.Fatalf("the repository was reached %d times by denied callers", stub.calls)
	}

	asked := 0
	counting := func(string) func(http.Handler) http.Handler {
		asked++
		return func(next http.Handler) http.Handler { return next }
	}
	router, stub = entityFolderRouter(counting)
	asked = 0
	for _, path := range []string{"/entity_folders/prompt_lib/7/widget", "/entity_folders/prompt_lib/7/AGENT"} {
		if rec := serveEntityFolder(router, http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s: status %d, want 404", path, rec.Code)
		}
	}
	if asked != 0 || stub.calls != 0 {
		t.Fatalf("an unknown kind reached a gate (%d) or the repository (%d)", asked, stub.calls)
	}
}
