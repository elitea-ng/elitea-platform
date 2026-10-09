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
