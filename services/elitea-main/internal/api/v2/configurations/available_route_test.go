package configurations_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type currentAvailableCatalogStub struct {
	entries  []configurationapp.CurrentAvailableConfigurationType
	err      error
	sections []string
}

func (stub *currentAvailableCatalogStub) CompleteEntries(sections ...string) ([]configurationapp.CurrentAvailableConfigurationType, error) {
	stub.sections = append([]string(nil), sections...)
	return stub.entries, stub.err
}

func TestCurrentAvailableRoutePreservesCurrentPathsAndSectionFilters(t *testing.T) {
	catalog, err := configurationapp.LoadPinnedCurrentAvailableCatalog()
	if err != nil {
		t.Fatal(err)
	}
	route := newCurrentAvailableRoute(t, catalog)

	for _, target := range []string{
		handler.CurrentAvailablePath + "?section=storage&section=vectorstorage&section=storage",
		handler.CurrentAvailableSlashPath + "?section=storage&section=vectorstorage&section=storage",
		"/api/v2/configurations/available/7?section=storage&section=vectorstorage&section=storage",
	} {
		response := httptest.NewRecorder()
		route.ServeHTTP(response, currentAvailableRequest(target))
		var entries []struct {
			Type string `json:"type"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &entries) != nil ||
			len(entries) != 2 || entries[0].Type != "s3" || entries[1].Type != "pgvector" {
			t.Fatalf("GET %s status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
}

func TestCurrentAvailableRouteFailsClosedBeforeCatalogAndForPartialSnapshot(t *testing.T) {
	stub := &currentAvailableCatalogStub{err: configurationapp.ErrCurrentAvailableCatalogPartial}
	route := newCurrentAvailableRoute(t, stub)

	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentAvailableRequest(handler.CurrentAvailableSlashPath))
	if response.Code != http.StatusServiceUnavailable ||
		response.Body.String() != "{\"error\":\"configuration catalog is unavailable\"}\n" {
		t.Fatalf("partial status=%d body=%q", response.Code, response.Body.String())
	}

	request := currentAvailableRequest(handler.CurrentAvailableSlashPath)
	request.RemoteAddr = "203.0.113.8:43120"
	response = httptest.NewRecorder()
	route.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || len(stub.sections) != 0 {
		t.Fatalf("untrusted status=%d sections=%v body=%s", response.Code, stub.sections, response.Body.String())
	}
}

func TestCurrentAvailableRouteRejectsIncompleteDependencies(t *testing.T) {
	catalog := &currentAvailableCatalogStub{}
	principal := currentReadPrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) { return user, nil })
	peer := currentReadPeerVerifierFunc(func(*http.Request) error { return nil })

	for name, test := range map[string]struct {
		catalog handler.CurrentAvailableCatalogReader
		auth    apimw.AuthConfig
	}{
		"missing catalog":      {auth: apimw.AuthConfig{PrincipalValidator: principal, ForwardedIdentityVerifier: peer}},
		"missing principal":    {catalog: catalog, auth: apimw.AuthConfig{ForwardedIdentityVerifier: peer}},
		"no credential reader": {catalog: catalog, auth: apimw.AuthConfig{PrincipalValidator: principal}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := handler.NewCurrentAvailableRoute(test.catalog, test.auth); !errors.Is(err, handler.ErrInvalidCurrentAvailableRoute) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func newCurrentAvailableRoute(
	t *testing.T,
	catalog handler.CurrentAvailableCatalogReader,
) *handler.CurrentAvailableRoute {
	t.Helper()
	principal := currentReadPrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) { return user, nil })
	peer := currentReadPeerVerifierFunc(func(request *http.Request) error {
		if request.RemoteAddr != "10.0.0.8:43120" {
			return errors.New("untrusted peer")
		}
		return nil
	})
	route, err := handler.NewCurrentAvailableRoute(
		catalog,
		apimw.AuthConfig{PrincipalValidator: principal, ForwardedIdentityVerifier: peer},
	)
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func currentAvailableRequest(target string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "11")
	request.RemoteAddr = "10.0.0.8:43120"
	return request
}
