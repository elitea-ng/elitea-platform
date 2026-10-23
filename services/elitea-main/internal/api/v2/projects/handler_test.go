package projects_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	handler "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/projects"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

type projectListerFunc func(context.Context, sqlcgen.ListCurrentUserProjectsParams) ([]sqlcgen.ListCurrentUserProjectsRow, error)

func (f projectListerFunc) ListCurrentUserProjects(
	ctx context.Context,
	params sqlcgen.ListCurrentUserProjectsParams,
) ([]sqlcgen.ListCurrentUserProjectsRow, error) {
	return f(ctx, params)
}

func setupProjectRouter(store handler.CurrentProjectLister) *chi.Mux {
	h := handler.NewCurrentProjectListHandler(store)
	router := chi.NewRouter()
	router.Mount("/projects", h.Routes())
	return router
}

func withUser(request *http.Request, user auth.User) *http.Request {
	return request.WithContext(auth.ContextWithUser(request.Context(), user))
}

func TestGetProjectRequiresAuthenticatedOwningUser(t *testing.T) {
	store := projectListerFunc(func(context.Context, sqlcgen.ListCurrentUserProjectsParams) ([]sqlcgen.ListCurrentUserProjectsRow, error) {
		t.Fatal("project query must not run")
		return nil, nil
	})
	router := setupProjectRouter(store)

	for name, user := range map[string]*auth.User{
		"missing principal":         nil,
		"non-numeric principal":     {ID: "not-a-user"},
		"token without owning user": {ID: "17", TokenID: "17", AuthType: "token"},
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/projects/project/default/1", nil)
			if user != nil {
				request = withUser(request, *user)
			}
			recorder := httptest.NewRecorder()

			router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusUnauthorized, recorder.Body.String())
			}
		})
	}
}

func TestGetProjectReportsMissingRepositoryAsUnavailable(t *testing.T) {
	router := setupProjectRouter(nil)
	request := withUser(
		httptest.NewRequest(http.MethodGet, "/projects/project/default/1", nil),
		auth.User{ID: "7", UserID: "7"},
	)
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
	}
}

func TestGetProjectReturnsExactCurrentPlainArray(t *testing.T) {
	groupID := int32(8)
	groupName := "delivery"
	store := projectListerFunc(func(_ context.Context, params sqlcgen.ListCurrentUserProjectsParams) ([]sqlcgen.ListCurrentUserProjectsRow, error) {
		want := sqlcgen.ListCurrentUserProjectsParams{
			CheckPublicRole: true,
			PublicProjectID: 1,
			UserID:          7,
		}
		if !reflect.DeepEqual(params, want) {
			t.Fatalf("params = %+v, want %+v", params, want)
		}
		return []sqlcgen.ListCurrentUserProjectsRow{
			{
				ID:             1,
				Name:           "promptlib_public",
				OwnerID:        1,
				Plugins:        []string{"configuration", "models"},
				KeycloakGroups: []byte(`{"platform":"public"}`),
				CreateSuccess:  true,
				GroupID:        &groupID,
				GroupName:      &groupName,
			},
			{
				ID:             1,
				Name:           "promptlib_public",
				OwnerID:        1,
				Plugins:        []string{"configuration", "models"},
				KeycloakGroups: []byte(`{"platform":"public"}`),
				CreateSuccess:  true,
				GroupID:        &groupID,
				GroupName:      &groupName,
			},
			{
				ID:             2,
				Name:           "project_user_7",
				OwnerID:        7,
				Plugins:        nil,
				KeycloakGroups: []byte(`{}`),
				CreateSuccess:  true,
				Suspended:      true,
			},
		}, nil
	})
	router := setupProjectRouter(store)
	request := withUser(
		httptest.NewRequest(http.MethodGet, "/projects/project/default/1?check_public_role=true", nil),
		auth.User{ID: "7", UserID: "7", AuthType: "user"},
	)
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}
	want := `[{"id":1,"name":"promptlib_public","owner_id":1,"plugins":["configuration","models"],"keycloak_groups":{"platform":"public"},"create_success":true,"suspended":false,"groups":[{"id":8,"name":"delivery"}]},{"id":2,"name":"project_user_7","owner_id":7,"plugins":null,"keycloak_groups":{},"create_success":true,"suspended":true,"groups":[]}]`
	if got := strings.TrimSpace(recorder.Body.String()); got != want {
		t.Fatalf("body = %s\nwant = %s", got, want)
	}
	for _, fabricated := range []string{`"items"`, `"total"`, `"status"`, `"role"`, `"description"`} {
		if strings.Contains(recorder.Body.String(), fabricated) {
			t.Fatalf("body contains non-baseline field %s: %s", fabricated, recorder.Body.String())
		}
	}
}

func TestGetProjectPreservesCurrentQuerySemantics(t *testing.T) {
	store := projectListerFunc(func(_ context.Context, params sqlcgen.ListCurrentUserProjectsParams) ([]sqlcgen.ListCurrentUserProjectsRow, error) {
		if !params.CheckPublicRole || params.PublicProjectID != 1 || params.UserID != 7 ||
			params.Search == nil || *params.Search != "Private" ||
			params.Offset == nil || *params.Offset != 2 ||
			params.Limit == nil || *params.Limit != 5 {
			t.Fatalf("unexpected params: %+v", params)
		}
		return []sqlcgen.ListCurrentUserProjectsRow{}, nil
	})
	router := setupProjectRouter(store)
	request := withUser(
		httptest.NewRequest(http.MethodGet, "/projects/project/default/1?check_public_role=false&search=Private&offset=2&limit=5", nil),
		auth.User{ID: "19", UserID: "7", TokenID: "19", AuthType: "token"},
	)
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != `[]` {
		t.Fatalf("status/body = %d %q, want 200 []", recorder.Code, recorder.Body.String())
	}
}

func TestGetProjectDoesNotFabricateFallbackOnQueryFailure(t *testing.T) {
	store := projectListerFunc(func(context.Context, sqlcgen.ListCurrentUserProjectsParams) ([]sqlcgen.ListCurrentUserProjectsRow, error) {
		return nil, errors.New("database unavailable")
	})
	router := setupProjectRouter(store)
	request := withUser(
		httptest.NewRequest(http.MethodGet, "/projects/project/default/1", nil),
		auth.User{ID: "7", UserID: "7"},
	)
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	if strings.Contains(recorder.Body.String(), "Project 1") {
		t.Fatalf("query failure fabricated a project: %s", recorder.Body.String())
	}
}

func TestGetProjectRejectsUnusableNumericParametersBeforeQuery(t *testing.T) {
	store := projectListerFunc(func(context.Context, sqlcgen.ListCurrentUserProjectsParams) ([]sqlcgen.ListCurrentUserProjectsRow, error) {
		t.Fatal("project query must not run")
		return nil, nil
	})
	router := setupProjectRouter(store)

	for _, path := range []string{
		"/projects/project/default/not-a-project",
		"/projects/project/default/1?limit=not-a-number",
		"/projects/project/default/1?offset=2147483648",
	} {
		t.Run(path, func(t *testing.T) {
			request := withUser(httptest.NewRequest(http.MethodGet, path, nil), auth.User{ID: "7", UserID: "7"})
			recorder := httptest.NewRecorder()

			router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
			}
		})
	}
}

// TestGroupWritesAreGated replaces TestPutProjectGroupsEchoesBody, which
// asserted the defect: the PUT decoded its body and echoed it back as the
// response without touching a table, and that test pinned the echo in place.
//
// The three group writes are gated on a permission resolver supplied through
// WithPermissionResolver. A Handler built without one — which is what
// setupProjectRouter builds — must REFUSE them rather than run ungated, and an
// unauthenticated caller must never reach the handler at all.
func TestGroupWritesAreGated(t *testing.T) {
	router := setupProjectRouter(nil)
	body := func() *bytes.Reader {
		payload, err := json.Marshal(map[string]any{"name": "alpha", "groups": []string{"alpha"}})
		if err != nil {
			t.Fatal(err)
		}
		return bytes.NewReader(payload)
	}

	writes := []struct {
		method string
		path   string
	}{
		{http.MethodPut, "/projects/groups/prompt_lib/1"},
		{http.MethodPost, "/projects/group/prompt_lib/1"},
		{http.MethodDelete, "/projects/group/prompt_lib/1/2"},
	}

	for _, write := range writes {
		t.Run("unauthenticated "+write.method+" "+write.path, func(t *testing.T) {
			request := httptest.NewRequest(write.method, write.path, body())
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()

			router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d; body=%s",
					recorder.Code, http.StatusUnauthorized, recorder.Body.String())
			}
		})

		t.Run("no resolver "+write.method+" "+write.path, func(t *testing.T) {
			request := withUser(
				httptest.NewRequest(write.method, write.path, body()),
				auth.User{ID: "7", UserID: "7"},
			)
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()

			router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d; body=%s",
					recorder.Code, http.StatusForbidden, recorder.Body.String())
			}
		})
	}
}

// TestQuotaRoutesAreGated is the same fail-closed argument for the quota and
// statistics routes (#246). The quota READ matters as much as the write: the
// row names a project's ceilings, and the statistics read reports what it has
// consumed.
//
// A Handler built without a resolver must REFUSE all three rather than run
// ungated — including the reads, which is the part an "it's only a GET"
// reading would get wrong.
func TestQuotaRoutesAreGated(t *testing.T) {
	router := setupProjectRouter(nil)
	body := func() *bytes.Reader {
		payload, err := json.Marshal(map[string]any{"vcu_hard_limit": 1})
		if err != nil {
			t.Fatal(err)
		}
		return bytes.NewReader(payload)
	}

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/projects/quota/1"},
		{http.MethodPut, "/projects/quota/1?usage_type=vcu"},
		{http.MethodGet, "/projects/statistics/1"},
	}

	for _, route := range routes {
		t.Run("unauthenticated "+route.method+" "+route.path, func(t *testing.T) {
			request := httptest.NewRequest(route.method, route.path, body())
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()

			router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d; body=%s",
					recorder.Code, http.StatusUnauthorized, recorder.Body.String())
			}
		})

		t.Run("no resolver "+route.method+" "+route.path, func(t *testing.T) {
			request := withUser(
				httptest.NewRequest(route.method, route.path, body()),
				auth.User{ID: "7", UserID: "7"},
			)
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()

			router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d; body=%s",
					recorder.Code, http.StatusForbidden, recorder.Body.String())
			}
		})
	}
}
