package configurations_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type currentModelDefaultWrite struct {
	ProjectID       int64
	Name            string
	TargetProjectID int64
	Section         string
}

type currentModelDefaultWriterStub struct {
	mu     sync.Mutex
	writes []currentModelDefaultWrite
	err    error
}

func (stub *currentModelDefaultWriterStub) SetCurrentModelDefault(
	_ context.Context,
	selection configurationapp.CurrentModelDefaultSelection,
) error {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.writes = append(stub.writes, currentModelDefaultWrite{
		ProjectID:       int64(selection.ProjectID),
		Name:            selection.Name,
		TargetProjectID: selection.TargetProjectID,
		Section:         selection.Section,
	})
	return stub.err
}

func (stub *currentModelDefaultWriterStub) snapshot() []currentModelDefaultWrite {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return append([]currentModelDefaultWrite(nil), stub.writes...)
}

func TestCurrentModelDefaultRouteBindsCurrentPathAndUpdatePermission(t *testing.T) {
	if handler.CurrentModelDefaultPath != "/api/v2/configurations/models/{projectID}" ||
		handler.CurrentModelDefaultMode != auth.PermissionModeDefault ||
		handler.CurrentModelDefaultPermission != "configurations.configuration.update" {
		t.Fatalf(
			"current model-default contract drifted: path=%q mode=%q permission=%q",
			handler.CurrentModelDefaultPath,
			handler.CurrentModelDefaultMode,
			handler.CurrentModelDefaultPermission,
		)
	}

	writer := &currentModelDefaultWriterStub{}
	principal := currentReadPrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) { return user, nil })
	peer := currentReadPeerVerifierFunc(func(*http.Request) error { return nil })
	permissions := permissionResolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{}, nil
	})
	authConfig := apimw.AuthConfig{PrincipalValidator: principal, ForwardedIdentityVerifier: peer}

	for name, test := range map[string]struct {
		writer      handler.CurrentModelDefaultWriter
		authConfig  apimw.AuthConfig
		permissions auth.PermissionResolver
	}{
		"missing writer":       {authConfig: authConfig, permissions: permissions},
		"missing principal":    {writer: writer, authConfig: apimw.AuthConfig{ForwardedIdentityVerifier: peer}, permissions: permissions},
		"no credential reader": {writer: writer, authConfig: apimw.AuthConfig{PrincipalValidator: principal}, permissions: permissions},
		"missing permissions":  {writer: writer, authConfig: authConfig},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := handler.NewCurrentModelDefaultRoute(
				test.writer,
				test.authConfig,
				test.permissions,
			); !errors.Is(err, handler.ErrInvalidCurrentModelDefaultRoute) {
				t.Fatalf("error = %v, want %v", err, handler.ErrInvalidCurrentModelDefaultRoute)
			}
		})
	}
}

func TestCurrentModelDefaultRoutePreservesBodyAndResponseContract(t *testing.T) {
	writer := &currentModelDefaultWriterStub{}
	permissionCalls := 0
	permissions := permissionResolverFunc(func(_ context.Context, user auth.User, mode, projectID string) (auth.PermissionResolution, error) {
		permissionCalls++
		if user.UserID != "11" || mode != auth.PermissionModeDefault || projectID != "7" {
			t.Fatalf("permission input user=%+v mode=%q project=%q", user, mode, projectID)
		}
		return auth.PermissionResolution{UserID: 11, Permissions: []string{handler.CurrentModelDefaultPermission}}, nil
	})
	route := newCurrentModelDefaultRoute(t, writer, permissions)

	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentModelDefaultRequest(
		`{"name":"gpt-5.1","target_project_id":"2.0","extra":"ignored"}`,
	))
	if response.Code != http.StatusOK || response.Body.String() != "{\"result\":\"success\"}\n" || permissionCalls != 1 {
		t.Fatalf("status=%d permission_calls=%d body=%q", response.Code, permissionCalls, response.Body.String())
	}
	writes := writer.snapshot()
	if len(writes) != 1 || writes[0] != (currentModelDefaultWrite{
		ProjectID: 7, Name: "gpt-5.1", TargetProjectID: 2, Section: "llm",
	}) {
		t.Fatalf("writes=%+v", writes)
	}
}

func TestCurrentModelDefaultRoutePreservesOptionalSectionWithoutVisibilityLookup(t *testing.T) {
	writer := &currentModelDefaultWriterStub{}
	permissions := allowCurrentModelDefaultPermission()
	route := newCurrentModelDefaultRoute(t, writer, permissions)

	for _, body := range []string{
		`{"name":"not-in-catalog","target_project_id":-3,"section":"embedding"}`,
		`{"name":"nullable-section","target_project_id":true,"section":null}`,
	} {
		response := httptest.NewRecorder()
		route.ServeHTTP(response, currentModelDefaultRequest(body))
		if response.Code != http.StatusOK {
			t.Fatalf("body=%s status=%d response=%s", body, response.Code, response.Body.String())
		}
	}

	writes := writer.snapshot()
	if len(writes) != 2 ||
		writes[0].Name != "not-in-catalog" || writes[0].TargetProjectID != -3 || writes[0].Section != "embedding" ||
		writes[1].Name != "nullable-section" || writes[1].TargetProjectID != 1 || writes[1].Section != "None" {
		t.Fatalf("writes=%+v", writes)
	}
}

func TestCurrentModelDefaultRouteRejectsInvalidInputAndPermissionBeforeWrite(t *testing.T) {
	for name, body := range map[string]string{
		"missing name":      `{"target_project_id":2}`,
		"numeric name":      `{"name":2,"target_project_id":2}`,
		"missing target":    `{"name":"gpt"}`,
		"fractional target": `{"name":"gpt","target_project_id":2.1}`,
		"exponent string":   `{"name":"gpt","target_project_id":"2e0"}`,
		"numeric section":   `{"name":"gpt","target_project_id":2,"section":3}`,
	} {
		t.Run(name, func(t *testing.T) {
			writer := &currentModelDefaultWriterStub{}
			route := newCurrentModelDefaultRoute(t, writer, allowCurrentModelDefaultPermission())
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentModelDefaultRequest(body))
			if response.Code != http.StatusBadRequest ||
				!strings.Contains(response.Body.String(), `"error":"invalid request body"`) ||
				len(writer.snapshot()) != 0 {
				t.Fatalf("status=%d writes=%+v body=%s", response.Code, writer.snapshot(), response.Body.String())
			}
		})
	}

	deniedWriter := &currentModelDefaultWriterStub{}
	denied := newCurrentModelDefaultRoute(t, deniedWriter, permissionResolverFunc(
		func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{UserID: 11, Permissions: []string{handler.CurrentConfigurationListPermission}}, nil
		},
	))
	response := httptest.NewRecorder()
	denied.ServeHTTP(response, currentModelDefaultRequest(`{"name":"gpt","target_project_id":2}`))
	if response.Code != http.StatusForbidden || len(deniedWriter.snapshot()) != 0 {
		t.Fatalf("status=%d writes=%+v body=%s", response.Code, deniedWriter.snapshot(), response.Body.String())
	}
}

func TestCurrentModelDefaultRouteMapsVaultFailureWithoutLeak(t *testing.T) {
	writer := &currentModelDefaultWriterStub{err: errors.New("vault-database-password-canary")}
	route := newCurrentModelDefaultRoute(t, writer, allowCurrentModelDefaultPermission())
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentModelDefaultRequest(`{"name":"secret-name-canary","target_project_id":2}`))
	if response.Code != http.StatusBadRequest || response.Body.String() != "{\"result\":\"error\"}\n" ||
		strings.Contains(response.Body.String(), "canary") {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestCurrentModelDefaultRouteIsSafeForConcurrentRequests(t *testing.T) {
	writer := &currentModelDefaultWriterStub{}
	route := newCurrentModelDefaultRoute(t, writer, allowCurrentModelDefaultPermission())
	const requests = 32
	statuses := make(chan int, requests)
	var group sync.WaitGroup
	group.Add(requests)
	for index := 0; index < requests; index++ {
		go func() {
			defer group.Done()
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentModelDefaultRequest(`{"name":"gpt","target_project_id":2}`))
			statuses <- response.Code
		}()
	}
	group.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("status=%d", status)
		}
	}
	if writes := writer.snapshot(); len(writes) != requests {
		t.Fatalf("writes=%d want=%d", len(writes), requests)
	}
}

func newCurrentModelDefaultRoute(
	t *testing.T,
	writer handler.CurrentModelDefaultWriter,
	permissions auth.PermissionResolver,
) *handler.CurrentModelDefaultRoute {
	t.Helper()
	principal := currentReadPrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) { return user, nil })
	peer := currentReadPeerVerifierFunc(func(request *http.Request) error {
		if request.RemoteAddr != "10.0.0.8:43120" {
			return errors.New("untrusted peer")
		}
		return nil
	})
	route, err := handler.NewCurrentModelDefaultRoute(
		writer,
		apimw.AuthConfig{PrincipalValidator: principal, ForwardedIdentityVerifier: peer},
		permissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func allowCurrentModelDefaultPermission() auth.PermissionResolver {
	return permissionResolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 11, Permissions: []string{handler.CurrentModelDefaultPermission}}, nil
	})
}

func currentModelDefaultRequest(body string) *http.Request {
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v2/configurations/models/007",
		bytes.NewBufferString(body),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "11")
	request.RemoteAddr = "10.0.0.8:43120"
	return request
}
