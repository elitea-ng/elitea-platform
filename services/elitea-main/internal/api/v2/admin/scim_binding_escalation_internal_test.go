package admin

// The own-binding lock of the no-escalation rule (issue 2488) on
// PUT /admin/scim_group_bindings/administration/{id}.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/rolepolicy"
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
		WithPermissionResolver(fixedResolver{rolepolicy.SuperAdminPermission})), nil)
	require.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String())

	// A caller who is not super_admin, with no database to read the binding's
	// members from: refused, with the fail-closed code.
	recorder = saveBindingAs(t, NewHandler(nil, WithSCIMGroupBindings(store),
		WithPermissionResolver(fixedResolver{"configuration.roles.user_project_permissions.edit"})), principal)
	require.Equal(t, http.StatusForbidden, recorder.Code, recorder.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, rolepolicy.CodeCallerUnresolved, body["code"])
}

func TestASuperAdminEditsABindingAsBefore(t *testing.T) {
	store := &stubBindingStore{}
	recorder := saveBindingAs(t, NewHandler(nil, WithSCIMGroupBindings(store),
		WithPermissionResolver(fixedResolver{rolepolicy.SuperAdminPermission})), &auth.User{ID: "5", UserID: "5"})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
}
