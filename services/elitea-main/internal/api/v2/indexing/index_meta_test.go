package indexing_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware/authtest"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/indexing"
	indexmetaapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexmeta"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type currentIndexMetaReaderStub struct {
	request indexmetaapp.Request
	items   []indexmetaapp.Item
	err     error
	calls   int
}

func (stub *currentIndexMetaReaderStub) List(
	_ context.Context,
	request indexmetaapp.Request,
) ([]indexmetaapp.Item, error) {
	stub.calls++
	stub.request = request
	return stub.items, stub.err
}

func TestCurrentIndexMetaRoutePreservesPathPermissionAndRawArray(t *testing.T) {
	if handler.CurrentIndexMetaListPath != "/api/v2/elitea_core/index_meta/prompt_lib/{projectID}/{toolkitID}" ||
		handler.CurrentIndexMetaListPermission != "models.applications.index_meta.details" ||
		handler.CurrentIndexMetaListMode != auth.PermissionModeDefault {
		t.Fatalf("current route contract drifted: path=%q permission=%q mode=%q",
			handler.CurrentIndexMetaListPath,
			handler.CurrentIndexMetaListPermission,
			handler.CurrentIndexMetaListMode,
		)
	}

	reader := &currentIndexMetaReaderStub{items: []indexmetaapp.Item{{
		ID:       "row-1",
		Metadata: map[string]any{"state": "completed"},
		Stale:    false,
	}}}
	permissions := permissionResolverFunc(func(_ context.Context, user auth.User, mode, projectID string) (auth.PermissionResolution, error) {
		if user.UserID != "11" || mode != auth.PermissionModeDefault || projectID != "7" {
			t.Fatalf("permission input user=%+v mode=%q project=%q", user, mode, projectID)
		}
		return auth.PermissionResolution{
			UserID:      11,
			Permissions: []string{handler.CurrentIndexMetaListPermission},
		}, nil
	})
	route := newCurrentIndexMetaRoute(t, reader, permissions)

	request := httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/index_meta/prompt_lib/007/009", nil)
	authtest.SignIn(request, "11")
	request.RemoteAddr = "10.0.0.8:43120"
	response := httptest.NewRecorder()
	route.ServeHTTP(response, request)

	if response.Code != http.StatusOK || reader.calls != 1 ||
		reader.request != (indexmetaapp.Request{ProjectID: 7, ActorUserID: 11, ToolkitID: 9}) {
		t.Fatalf("status=%d calls=%d request=%+v body=%s", response.Code, reader.calls, reader.request, response.Body.String())
	}
	var body []indexmetaapp.Item
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || !reflect.DeepEqual(body, reader.items) {
		t.Fatalf("raw array=%+v error=%v", body, err)
	}
}

func TestCurrentIndexMetaRouteAuthorizesBeforeReading(t *testing.T) {
	reader := &currentIndexMetaReaderStub{}
	permissions := permissionResolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 11}, nil
	})
	route := newCurrentIndexMetaRoute(t, reader, permissions)

	request := httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/index_meta/prompt_lib/7/9", nil)
	authtest.SignIn(request, "11")
	request.RemoteAddr = "10.0.0.8:43120"
	response := httptest.NewRecorder()
	route.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden || reader.calls != 0 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, reader.calls, response.Body.String())
	}
}

func TestCurrentIndexMetaRouteMapsCurrentValidationFailures(t *testing.T) {
	reader := &currentIndexMetaReaderStub{err: indexmetaapp.ErrCurrentIndexMetaTargetMissing}
	permissions := permissionResolverFunc(func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 11, Permissions: []string{handler.CurrentIndexMetaListPermission}}, nil
	})
	route := newCurrentIndexMetaRoute(t, reader, permissions)

	request := httptest.NewRequest(http.MethodGet, "/api/v2/elitea_core/index_meta/prompt_lib/7/9", nil)
	authtest.SignIn(request, "11")
	request.RemoteAddr = "10.0.0.8:43120"
	response := httptest.NewRecorder()
	route.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func newCurrentIndexMetaRoute(
	t *testing.T,
	reader handler.CurrentIndexMetaReader,
	permissions auth.PermissionResolver,
) *handler.CurrentIndexMetaRoute {
	t.Helper()
	route, err := handler.NewCurrentIndexMetaRoute(
		reader,
		apimw.AuthConfig{
			PrincipalValidator: principalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
				if user.ID != "11" {
					return auth.User{}, errors.New("unexpected user")
				}
				return user, nil
			}),
			FormSessions: authtest.FormSessions{},
		},
		permissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	return route
}
