package indexing_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware/authtest"
	handler "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/indexing"
	indexmetaapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexmeta"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

type currentIndexMetaDeleteStub struct {
	request indexmetaapp.DeleteRequest
	err     error
	calls   int
}

func (s *currentIndexMetaDeleteStub) Delete(
	_ context.Context,
	request indexmetaapp.DeleteRequest,
) error {
	s.calls++
	s.request = request
	return s.err
}

func TestCurrentIndexMetaDeleteRoutePreservesCurrentContract(t *testing.T) {
	if handler.CurrentIndexMetaDeletePath !=
		"/api/v2/elitea_core/index_meta/prompt_lib/{projectID}/{toolkitID}/{indexMetaID}" ||
		handler.CurrentIndexMetaDeletePermission !=
			"models.applications.index_meta.delete" ||
		handler.CurrentIndexMetaDeleteMode != auth.PermissionModeDefault {
		t.Fatalf(
			"path=%q permission=%q mode=%q",
			handler.CurrentIndexMetaDeletePath,
			handler.CurrentIndexMetaDeletePermission,
			handler.CurrentIndexMetaDeleteMode,
		)
	}
	deleter := &currentIndexMetaDeleteStub{}
	route := newCurrentIndexMetaDeleteRoute(
		t,
		deleter,
		permissionResolverFunc(func(
			_ context.Context,
			user auth.User,
			mode, projectID string,
		) (auth.PermissionResolution, error) {
			if user.UserID != "11" || mode != auth.PermissionModeDefault ||
				projectID != "7" {
				t.Fatalf(
					"user=%+v mode=%q project=%q",
					user,
					mode,
					projectID,
				)
			}
			return auth.PermissionResolution{
				UserID: 11,
				Permissions: []string{
					handler.CurrentIndexMetaDeletePermission,
				},
			}, nil
		}),
	)
	response := httptest.NewRecorder()
	route.ServeHTTP(
		response,
		currentIndexMetaDeleteRequest(
			"/api/v2/elitea_core/index_meta/prompt_lib/007/009/meta-1",
		),
	)
	if response.Code != http.StatusOK ||
		response.Body.String() != "{\"ok\":true}\n" ||
		deleter.calls != 1 ||
		deleter.request != (indexmetaapp.DeleteRequest{
			ProjectID: 7, ActorUserID: 11, ToolkitID: 9,
			IndexMetaID: "meta-1",
		}) {
		t.Fatalf(
			"status=%d body=%q calls=%d request=%+v",
			response.Code,
			response.Body.String(),
			deleter.calls,
			deleter.request,
		)
	}
}

func TestCurrentIndexMetaDeleteRouteAuthorizesBeforeDelete(t *testing.T) {
	for name, test := range map[string]struct {
		remote      string
		forged      bool
		permissions []string
		want        int
	}{
		"forged X-Auth-* identity without a session": {
			remote: "192.0.2.8:443",
			forged: true,
			permissions: []string{
				handler.CurrentIndexMetaDeletePermission,
			},
			want: http.StatusUnauthorized,
		},
		"viewer denied": {
			remote: "10.0.0.8:43120",
			want:   http.StatusForbidden,
		},
		"editor permission accepted": {
			remote: "10.0.0.8:43120",
			permissions: []string{
				handler.CurrentIndexMetaDeletePermission,
			},
			want: http.StatusOK,
		},
	} {
		t.Run(name, func(t *testing.T) {
			deleter := &currentIndexMetaDeleteStub{}
			route := newCurrentIndexMetaDeleteRoute(
				t,
				deleter,
				permissionResolverFunc(func(
					context.Context,
					auth.User,
					string,
					string,
				) (auth.PermissionResolution, error) {
					return auth.PermissionResolution{
						UserID:      11,
						Permissions: test.permissions,
					}, nil
				}),
			)
			request := currentIndexMetaDeleteRequest(
				"/api/v2/elitea_core/index_meta/prompt_lib/7/9/meta-1",
			)
			request.RemoteAddr = test.remote
			if test.forged {
				// The retired edge projection, and no session (#1359).
				request.Header.Del("Cookie")
				authtest.ForgeXAuth(request, "11")
			}
			response := httptest.NewRecorder()
			route.ServeHTTP(response, request)
			wantCalls := 0
			if test.want == http.StatusOK {
				wantCalls = 1
			}
			if response.Code != test.want || deleter.calls != wantCalls {
				t.Fatalf(
					"status=%d want=%d calls=%d want=%d body=%q",
					response.Code,
					test.want,
					deleter.calls,
					wantCalls,
					response.Body.String(),
				)
			}
		})
	}
}

func TestCurrentIndexMetaDeleteRouteMatchesWerkzeugIntegerEdges(t *testing.T) {
	hugePythonInteger := strings.Repeat("9", 4_301)
	for name, test := range map[string]struct {
		project string
		toolkit string
		err     error
		want    int
		calls   int
	}{
		"leading zero": {
			project: "0007", toolkit: "0009",
			want: http.StatusOK, calls: 1,
		},
		"unicode decimal": {
			project: "٧", toolkit: "९",
			want: http.StatusOK, calls: 1,
		},
		"zero project is denied before lookup": {
			project: "0", toolkit: "9",
			want: http.StatusForbidden,
		},
		"zero toolkit reaches current validation": {
			project: "7", toolkit: "0",
			want: http.StatusBadRequest,
		},
		"negative is not a Werkzeug int route": {
			project: "-7", toolkit: "9",
			want: http.StatusNotFound,
		},
		"nonnumeric is not a Werkzeug int route": {
			project: "seven", toolkit: "9",
			want: http.StatusNotFound,
		},
		"above current integer schema is validation": {
			project: "2147483648", toolkit: "9",
			err:  indexmetaapp.ErrInvalidCurrentIndexMetaRequest,
			want: http.StatusBadRequest, calls: 1,
		},
		"python decimal conversion limit is not a route": {
			project: hugePythonInteger, toolkit: "9",
			want: http.StatusNotFound,
		},
	} {
		t.Run(name, func(t *testing.T) {
			deleter := &currentIndexMetaDeleteStub{err: test.err}
			route := newCurrentIndexMetaDeleteRoute(
				t,
				deleter,
				permissionResolverFunc(func(
					context.Context,
					auth.User,
					string,
					string,
				) (auth.PermissionResolution, error) {
					return auth.PermissionResolution{
						UserID: 11,
						Permissions: []string{
							handler.CurrentIndexMetaDeletePermission,
						},
					}, nil
				}),
			)
			response := httptest.NewRecorder()
			route.ServeHTTP(
				response,
				currentIndexMetaDeleteRequest(
					"/api/v2/elitea_core/index_meta/prompt_lib/"+
						test.project+"/"+test.toolkit+"/meta-1",
				),
			)
			if response.Code != test.want || deleter.calls != test.calls {
				t.Fatalf(
					"status=%d want=%d calls=%d want=%d body=%q",
					response.Code,
					test.want,
					deleter.calls,
					test.calls,
					response.Body.String(),
				)
			}
		})
	}
}

func TestCurrentIndexMetaDeleteRouteCurrentErrorsAndSafeCleanupDivergence(
	t *testing.T,
) {
	for name, test := range map[string]struct {
		err       error
		want      int
		wantError string
	}{
		"metadata missing": {
			err:       indexmetaapp.ErrCurrentIndexMetaNotFound,
			want:      http.StatusNotFound,
			wantError: "index_meta meta-1 not found",
		},
		"toolkit validation": {
			err:       indexmetaapp.ErrCurrentIndexMetaToolkitMissing,
			want:      http.StatusBadRequest,
			wantError: "Toolkit id is missing for toolkit None",
		},
		"index storage validation": {
			err:       indexmetaapp.ErrCurrentIndexMetaTargetMissing,
			want:      http.StatusBadRequest,
			wantError: "Index storage is not available for toolkit 9",
		},
		"cleanup toolkit disappears": {
			err: &indexmetaapp.ScheduleToolkitMissingError{
				ProjectID: 7,
				ToolkitID: 9,
				IndexName: "Docs",
			},
			want:      http.StatusNotFound,
			wantError: "Toolkit 9 not found (project_id=7, index_name='Docs')",
		},
		"index storage failure": {
			err:       indexmetaapp.ErrCurrentIndexMetaUnavailable,
			want:      http.StatusBadRequest,
			wantError: "Error occurred while deleting index_meta",
		},
		"schedule failure intentionally redacts current raw exception": {
			err: &indexmetaapp.ScheduleCleanupError{
				ProjectID: 7,
				ToolkitID: 9,
				IndexName: "Docs",
			},
			want: http.StatusBadRequest,
			wantError: "Error during index deletion (Toolkit 9project_id=7, index_name='Docs') " +
				"current index metadata schedule cleanup failed",
		},
	} {
		t.Run(name, func(t *testing.T) {
			deleter := &currentIndexMetaDeleteStub{err: test.err}
			route := newCurrentIndexMetaDeleteRoute(
				t,
				deleter,
				permissionResolverFunc(func(
					context.Context,
					auth.User,
					string,
					string,
				) (auth.PermissionResolution, error) {
					return auth.PermissionResolution{
						UserID: 11,
						Permissions: []string{
							handler.CurrentIndexMetaDeletePermission,
						},
					}, nil
				}),
			)
			response := httptest.NewRecorder()
			route.ServeHTTP(
				response,
				currentIndexMetaDeleteRequest(
					"/api/v2/elitea_core/index_meta/prompt_lib/7/9/meta-1",
				),
			)
			var body struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			err := json.Unmarshal(response.Body.Bytes(), &body)
			if err != nil || response.Code != test.want || body.OK ||
				body.Error != test.wantError {
				t.Fatalf(
					"status=%d want=%d body=%q decoded=%+v err=%v",
					response.Code,
					test.want,
					response.Body.String(),
					body,
					err,
				)
			}
		})
	}
}

func currentIndexMetaDeleteRequest(path string) *http.Request {
	request := httptest.NewRequest(http.MethodDelete, path, nil)
	authtest.SignIn(request, "11")
	request.RemoteAddr = "10.0.0.8:43120"
	return request
}

func newCurrentIndexMetaDeleteRoute(
	t *testing.T,
	deleter handler.CurrentIndexMetaDeleter,
	permissions auth.PermissionResolver,
) *handler.CurrentIndexMetaDeleteRoute {
	t.Helper()
	route, err := handler.NewCurrentIndexMetaDeleteRoute(
		deleter,
		apimw.AuthConfig{
			PrincipalValidator: principalValidatorFunc(func(
				_ context.Context,
				user auth.User,
			) (auth.User, error) {
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
