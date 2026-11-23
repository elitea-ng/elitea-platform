package admin

// The no-escalation rule (issue 2488) on the SCIM group binding writes: the
// order of the refusals and the fail-closed answer. The rule itself, against a
// database, is in role_escalation_postgres_integration_test.go.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

type fixedResolver []string

func (f fixedResolver) ResolvePermissions(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{UserID: 5, Permissions: f}, nil
}

func saveBindingAs(t *testing.T, handler *Handler, principal *auth.User) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Put("/bindings/{id}", handler.SCIMGroupBindingSave)
	request := httptest.NewRequest(http.MethodPut, "/bindings/7",
		strings.NewReader(`{"display_name":"Team","project_id":1,"role_name":"admin"}`))
	if principal != nil {
		request = request.WithContext(auth.ContextWithUser(request.Context(), *principal))
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestABindingEditFailsClosedWhenTheCallerCannotBeRead(t *testing.T) {
	store := &stubBindingStore{}
	principal := &auth.User{ID: "5", UserID: "5"}

	// No resolver, and no principal: refused, never "allowed".
	recorder := saveBindingAs(t, NewHandler(nil, WithSCIMGroupBindings(store)), principal)
	require.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String())
	recorder = saveBindingAs(t, NewHandler(nil, WithSCIMGroupBindings(store),
		WithPermissionResolver(fixedResolver{adminaccess.SuperAdminPermission})), nil)
	require.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String())

	// A caller who is not super_admin, with no database to read the binding's
	// members from: refused, with the fail-closed code.
	recorder = saveBindingAs(t, NewHandler(nil, WithSCIMGroupBindings(store),
		WithPermissionResolver(fixedResolver{"configuration.roles.user_project_permissions.edit"})), principal)
	require.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, adminaccess.CodeCallerUnresolved, body["code"])
}

func TestEveryBindingWriteFailsClosedForACallerWhoIsNotASuperAdmin(t *testing.T) {
	handler := NewHandler(nil, WithSCIMGroupBindings(&stubBindingStore{}),
		WithPermissionResolver(fixedResolver{"configuration.roles.user_project_permissions.edit"}))
	router := chi.NewRouter()
	router.Post("/bindings", handler.SCIMGroupBindingCreate)
	router.Put("/bindings/{id}", handler.SCIMGroupBindingSave)
	router.Delete("/bindings/{id}", handler.SCIMGroupBindingDelete)
	const body = `{"display_name":"Team","project_id":1,"role_name":"admin"}`
	for _, call := range []struct{ method, target, body string }{
		{http.MethodPost, "/bindings", body},
		{http.MethodPut, "/bindings/7", body},
		{http.MethodDelete, "/bindings/7", ""},
	} {
		request := httptest.NewRequest(call.method, call.target, strings.NewReader(call.body))
		request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: "5", UserID: "5"}))
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusForbidden, recorder.Code, "%s %s: %s", call.method, call.target, recorder.Body.String())
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &decoded))
		require.Equal(t, adminaccess.CodeCallerUnresolved, decoded["code"])
	}
}

func TestASuperAdminEditsABindingAsBefore(t *testing.T) {
	store := &stubBindingStore{}
	recorder := saveBindingAs(t, NewHandler(nil, WithSCIMGroupBindings(store),
		WithPermissionResolver(fixedResolver{adminaccess.SuperAdminPermission})), &auth.User{ID: "5", UserID: "5"})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
}
