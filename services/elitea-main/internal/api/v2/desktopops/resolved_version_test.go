package desktopops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

type fakeResolver struct {
	err                        error
	project, actor             int64
	application, versionCalled uint64
}

func (f *fakeResolver) Resolve(_ context.Context, project, actor int64, application, version uint64) (storage.ClientApplicationVersion, error) {
	f.project, f.actor, f.application, f.versionCalled = project, actor, application, version
	if f.err != nil {
		return storage.ClientApplicationVersion{}, f.err
	}
	return storage.ClientApplicationVersion{
		SchemaVersion: storage.ClientApplicationVersionSchemaVersion,
		ProjectID:     project, ApplicationID: int64(application), VersionID: int64(version),
		VersionDetails:   json.RawMessage(`{"tools":[]}`),
		DefinitionSHA256: strings.Repeat("a", 64),
	}, nil
}

// serveWith runs one handler with the principal on the context, as apimw.Auth
// leaves it, and the chi route params the router would set.
func serveWith(t *testing.T, method, pattern, path, body string, user *auth.User, h http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Method(method, pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user != nil {
			r = r.WithContext(auth.ContextWithUser(r.Context(), *user))
		}
		h(w, r)
	}))
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Idempotency-Key", "test-key")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

const resolvedURL = "/api/v2/elitea_core/resolved_version/prompt_lib/3/11/12"

func TestResolvedVersionServesTheCallersDocument(t *testing.T) {
	resolver := &fakeResolver{}
	h := &resolvedVersionHandler{useCase: resolver}
	// A browser session is served: the route is a safe read.
	session := &auth.User{ID: "7", UserID: "7", AuthType: "session"}
	response := serveWith(t, http.MethodGet, ResolvedVersionPath, resolvedURL, "", session, h.serve)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", response.Code, response.Body)
	}
	if resolver.project != 3 || resolver.actor != 7 || resolver.application != 11 || resolver.versionCalled != 12 {
		t.Fatalf("resolver called with %+v", resolver)
	}
	if response.Header().Get("ETag") != `"`+strings.Repeat("a", 64)+`"` || response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("headers = %v", response.Header())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body["schema_version"] != storage.ClientApplicationVersionSchemaVersion {
		t.Fatalf("body = %s", response.Body)
	}
}

func TestResolvedVersionErrorsMapToTypedAnswers(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{storage.ErrContentNotFound, http.StatusNotFound, "application_version_not_found"},
		{storage.ErrClientApplicationVersionUnresolvable, http.StatusUnprocessableEntity, "application_version_unresolvable"},
		{storage.ErrContentUnavailable, http.StatusServiceUnavailable, "resolved_version_unavailable"},
		{errors.New("boom"), http.StatusServiceUnavailable, "resolved_version_unavailable"},
	} {
		h := &resolvedVersionHandler{useCase: &fakeResolver{err: tc.err}}
		response := serveWith(t, http.MethodGet, ResolvedVersionPath, resolvedURL, "",
			&auth.User{ID: "7", UserID: "7", AuthType: "token", TokenID: "1"}, h.serve)
		if response.Code != tc.status || !strings.Contains(response.Body.String(), `"`+tc.code+`"`) {
			t.Errorf("%v: status = %d, body %s", tc.err, response.Code, response.Body)
		}
	}
}

func TestResolvedVersionWithoutAnAgentPlaneAnswers501(t *testing.T) {
	h := &resolvedVersionHandler{}
	response := serveWith(t, http.MethodGet, ResolvedVersionPath, resolvedURL, "",
		&auth.User{ID: "7", UserID: "7", AuthType: "token", TokenID: "1"}, h.serve)
	if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), "resolved_version_unavailable") {
		t.Fatalf("status = %d, body %s", response.Code, response.Body)
	}
}

func TestResolvedVersionRefusesABadIdentity(t *testing.T) {
	h := &resolvedVersionHandler{useCase: &fakeResolver{}}
	response := serveWith(t, http.MethodGet, ResolvedVersionPath,
		"/api/v2/elitea_core/resolved_version/prompt_lib/3/011/12", "",
		&auth.User{ID: "7", UserID: "7", AuthType: "token", TokenID: "1"}, h.serve)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestRouteConstructorsRequireTheCredentialPlane(t *testing.T) {
	if _, err := NewResolvedVersionRoute(&fakeResolver{}, apimwZero(), nil); !errors.Is(err, ErrInvalidRoute) {
		t.Fatalf("err = %v", err)
	}
}

func apimwZero() apimw.AuthConfig { return apimw.AuthConfig{} }

type projectContextResolver struct{ details string }

func (f projectContextResolver) Resolve(_ context.Context, project, _ int64, application, version uint64) (storage.ClientApplicationVersion, error) {
	return storage.ClientApplicationVersion{
		SchemaVersion: storage.ClientApplicationVersionSchemaVersion,
		ProjectID:     project, ApplicationID: int64(application), VersionID: int64(version),
		VersionDetails:   json.RawMessage(f.details),
		DefinitionSHA256: strings.Repeat("b", 64),
		WithheldSecrets:  []string{"/instructions", "/project_context/content"},
	}, nil
}

type fixedPermissions struct{ permissions []string }

func (f fixedPermissions) ResolvePermissions(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{UserID: 7, Permissions: f.permissions}, nil
}

// The frozen project context is project content behind its own read
// permission; the resolved version carries it only for a caller who holds it.
func TestResolvedVersionCarriesProjectContextOnlyForItsReaders(t *testing.T) {
	const details = `{"tools":[],"instructions":"x","project_context":{"content":"PROJECT-CONTEXT-CANARY"}}`
	token := &auth.User{ID: "7", UserID: "7", AuthType: "token", TokenID: "1"}
	decode := func(t *testing.T, body []byte) storage.ClientApplicationVersion {
		t.Helper()
		var document storage.ClientApplicationVersion
		if err := json.Unmarshal(body, &document); err != nil {
			t.Fatal(err)
		}
		return document
	}

	reader := &resolvedVersionHandler{useCase: projectContextResolver{details}, permissions: fixedPermissions{
		[]string{ResolvedVersionPermission, ProjectContextViewPermission}}}
	response := serveWith(t, http.MethodGet, ResolvedVersionPath, resolvedURL, "", token, reader.serve)
	document := decode(t, response.Body.Bytes())
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "PROJECT-CONTEXT-CANARY") ||
		document.ProjectContextWithheld {
		t.Fatalf("a reader: status = %d, body %s", response.Code, response.Body)
	}

	other := &resolvedVersionHandler{useCase: projectContextResolver{details}, permissions: fixedPermissions{
		[]string{ResolvedVersionPermission}}}
	response = serveWith(t, http.MethodGet, ResolvedVersionPath, resolvedURL, "", token, other.serve)
	document = decode(t, response.Body.Bytes())
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "PROJECT-CONTEXT-CANARY") ||
		!document.ProjectContextWithheld {
		t.Fatalf("a non-reader: status = %d, body %s", response.Code, response.Body)
	}
	if document.DefinitionSHA256 == strings.Repeat("b", 64) || response.Header().Get("ETag") != `"`+document.DefinitionSHA256+`"` {
		t.Fatalf("the digest must describe the document served: %s / %s", document.DefinitionSHA256, response.Header().Get("ETag"))
	}
	if len(document.WithheldSecrets) != 1 || document.WithheldSecrets[0] != "/instructions" {
		t.Fatalf("withheld_secrets = %v", document.WithheldSecrets)
	}
}

type countingPermissions struct {
	permissions []string
	calls       int
}

func (c *countingPermissions) ResolvePermissions(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
	c.calls++
	return auth.PermissionResolution{UserID: 7, Permissions: c.permissions}, nil
}

type tokenOnly struct{}

func (tokenOnly) ValidateToken(context.Context, string) (auth.User, error) {
	return auth.User{ID: "7", UserID: "7", AuthType: "token", TokenID: "1"}, nil
}

type passPrincipal struct{}

func (passPrincipal) ValidatePrincipal(_ context.Context, user auth.User) (auth.User, error) {
	return user, nil
}

// Through the real gate, one request asks the permission resolver ONCE: the
// handler reads models.project_context.view from the set the gate resolved.
func TestResolvedVersionResolvesPermissionsOnce(t *testing.T) {
	const details = `{"tools":[],"project_context":{"content":"PROJECT-CONTEXT-CANARY"}}`
	for _, granted := range [][]string{
		{ResolvedVersionPermission, ProjectContextViewPermission},
		{ResolvedVersionPermission},
	} {
		permissions := &countingPermissions{permissions: granted}
		route, err := NewResolvedVersionRoute(projectContextResolver{details},
			apimw.AuthConfig{Validator: tokenOnly{}, PrincipalValidator: passPrincipal{}}, permissions)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodGet, resolvedURL, nil)
		request.Header.Set("Authorization", "Bearer token")
		recorder := httptest.NewRecorder()
		route.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body)
		}
		if permissions.calls != 1 {
			t.Fatalf("the permission resolver was asked %d times, want 1", permissions.calls)
		}
		if reader := len(granted) == 2; strings.Contains(recorder.Body.String(), "PROJECT-CONTEXT-CANARY") != reader {
			t.Fatalf("granted %v: project context served = %v", granted, !reader)
		}
	}
}
