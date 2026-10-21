package indexing_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware/authtest"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/indexing"
	indexscheduleapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexschedule"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type currentScheduleDeleterStub struct {
	request indexscheduleapp.DeleteRequest
	result  indexscheduleapp.DeleteResult
	err     error
	calls   int
}

func (stub *currentScheduleDeleterStub) Delete(
	_ context.Context,
	request indexscheduleapp.DeleteRequest,
) (indexscheduleapp.DeleteResult, error) {
	stub.calls++
	stub.request = request
	return stub.result, stub.err
}

func TestCurrentIndexScheduleDeleteRoutePreservesContract(t *testing.T) {
	if handler.SourceOnlyIndexScheduleDeletePath !=
		"/api/v2/elitea_core/index_schedule/prompt_lib/{projectID}/{toolkitID}/{indexMetaID}" ||
		handler.SourceOnlyIndexScheduleDeletePermission !=
			"models.applications.index_meta.edit" {
		t.Fatalf(
			"path=%q permission=%q",
			handler.SourceOnlyIndexScheduleDeletePath,
			handler.SourceOnlyIndexScheduleDeletePermission,
		)
	}
	deleter := &currentScheduleDeleterStub{
		result: indexscheduleapp.DeleteResult{
			IndexesMeta: map[string]any{
				"docs": map[string]any{"schedules": map[string]any{}},
			},
		},
	}
	route := newCurrentScheduleDeleteRoute(
		t,
		deleter,
		allowCurrentSchedulePermission,
	)
	request := currentScheduleDeleteRequest(
		"/api/v2/elitea_core/index_schedule/prompt_lib/7/9/docs?user_id=-1",
	)
	response := httptest.NewRecorder()
	route.ServeHTTP(response, request)
	if response.Code != http.StatusOK || deleter.calls != 1 ||
		deleter.request.ProjectID != 7 || deleter.request.ActorUserID != 11 ||
		deleter.request.ToolkitID != 9 ||
		deleter.request.IndexMetaID != "docs" ||
		deleter.request.TargetUserID == nil ||
		*deleter.request.TargetUserID != "-1" {
		t.Fatalf(
			"status=%d calls=%d request=%+v body=%s",
			response.Code,
			deleter.calls,
			deleter.request,
			response.Body.String(),
		)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil ||
		!reflect.DeepEqual(body, deleter.result.IndexesMeta) {
		t.Fatalf("body=%#v error=%v", body, err)
	}
}

func TestCurrentIndexScheduleDeleteRouteAppliesRBACBeforeApplication(t *testing.T) {
	for _, test := range []struct {
		name       string
		permission bool
		want       int
	}{
		{name: "editor", permission: true, want: http.StatusOK},
		{name: "viewer", want: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			deleter := &currentScheduleDeleterStub{
				result: indexscheduleapp.DeleteResult{
					IndexesMeta: map[string]any{},
				},
			}
			permissions := permissionResolverFunc(func(
				context.Context,
				auth.User,
				string,
				string,
			) (auth.PermissionResolution, error) {
				resolution := auth.PermissionResolution{UserID: 11}
				if test.permission {
					resolution.Permissions = []string{
						handler.SourceOnlyIndexScheduleDeletePermission,
					}
				}
				return resolution, nil
			})
			route := newCurrentScheduleDeleteRoute(
				t,
				deleter,
				permissions,
			)
			response := httptest.NewRecorder()
			route.ServeHTTP(
				response,
				currentScheduleDeleteRequest(
					"/api/v2/elitea_core/index_schedule/prompt_lib/7/9/docs",
				),
			)
			if response.Code != test.want ||
				deleter.calls != boolToInt(test.permission) {
				t.Fatalf(
					"status=%d calls=%d body=%s",
					response.Code,
					deleter.calls,
					response.Body.String(),
				)
			}
		})
	}
}

func newCurrentScheduleDeleteRoute(
	t *testing.T,
	deleter handler.CurrentIndexScheduleDeleter,
	permissions auth.PermissionResolver,
) *handler.CurrentIndexScheduleDeleteRoute {
	t.Helper()
	route, err := handler.NewCurrentIndexScheduleDeleteRoute(
		deleter,
		apimw.AuthConfig{
			PrincipalValidator: principalValidatorFunc(func(
				_ context.Context,
				user auth.User,
			) (auth.User, error) {
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

func currentScheduleDeleteRequest(path string) *http.Request {
	request := httptest.NewRequest(http.MethodDelete, path, nil)
	authtest.SignIn(request, "11")
	request.RemoteAddr = "10.0.0.8:43120"
	return request
}
