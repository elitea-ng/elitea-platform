package indexing_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware/authtest"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/indexing"
	indexscheduleapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexschedule"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

func TestCurrentIndexScheduleRoutePreservesPathPermissionDefaultsAndRawResponse(t *testing.T) {
	if handler.CurrentIndexSchedulePath != "/api/v2/elitea_core/index_meta/prompt_lib/{projectID}/{toolkitID}/{indexMetaID}" ||
		handler.CurrentIndexSchedulePermission != "models.applications.index_meta.edit" ||
		handler.CurrentIndexScheduleMode != auth.PermissionModeDefault {
		t.Fatalf(
			"current route drifted: path=%q permission=%q mode=%q",
			handler.CurrentIndexSchedulePath,
			handler.CurrentIndexSchedulePermission,
			handler.CurrentIndexScheduleMode,
		)
	}

	updater := &currentScheduleUpdaterStub{result: indexscheduleapp.MutationResult{
		IndexesMeta: map[string]any{
			"docs": map[string]any{
				"title":     "unchanged",
				"schedules": map[string]any{"-1": map[string]any{"enabled": false}},
			},
		},
	}}
	permissions := permissionResolverFunc(func(
		_ context.Context,
		user auth.User,
		mode, projectID string,
	) (auth.PermissionResolution, error) {
		if user.UserID != "11" || mode != auth.PermissionModeDefault || projectID != "7" {
			t.Fatalf("permission input user=%+v mode=%q project=%q", user, mode, projectID)
		}
		return auth.PermissionResolution{
			UserID: 11, Permissions: []string{handler.CurrentIndexSchedulePermission},
		}, nil
	})
	route := newCurrentScheduleRoute(t, updater, permissions, nil)

	request := currentScheduleRequest(
		"/api/v2/elitea_core/index_meta/prompt_lib/007/009/docs",
		`{"cron":" 0 3 * * * ","timezone":"Europe/Kyiv","credentials":{"elitea_title":"shared-github"}}`,
	)
	response := httptest.NewRecorder()
	route.ServeHTTP(response, request)

	if response.Code != http.StatusOK || updater.calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, updater.calls, response.Body.String())
	}
	if updater.update.ProjectID != 7 || updater.update.ActorUserID != 11 ||
		updater.update.ToolkitID != 9 || updater.update.IndexMetaID != "docs" ||
		updater.update.Cron != " 0 3 * * * " || updater.update.Enabled ||
		updater.update.RequestedUserID != -1 ||
		updater.update.Timezone != "Europe/Kyiv" ||
		updater.update.Credentials == nil ||
		updater.update.Credentials.Private == nil ||
		*updater.update.Credentials.Private ||
		updater.update.Credentials.EliteaTitle != "shared-github" {
		t.Fatalf("update=%#v", updater.update)
	}
	var responseBody map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &responseBody); err != nil ||
		!reflect.DeepEqual(responseBody, updater.result.IndexesMeta) {
		t.Fatalf("raw indexes_meta=%#v error=%v", responseBody, err)
	}
}

func TestCurrentIndexScheduleRouteAppliesExistingRBACBeforeBody(t *testing.T) {
	for _, test := range []struct {
		role       string
		permission bool
		wantStatus int
	}{
		{role: "admin", permission: true, wantStatus: http.StatusOK},
		{role: "editor", permission: true, wantStatus: http.StatusOK},
		{role: "viewer", permission: false, wantStatus: http.StatusForbidden},
	} {
		test := test
		t.Run(test.role, func(t *testing.T) {
			updater := &currentScheduleUpdaterStub{result: indexscheduleapp.MutationResult{
				IndexesMeta: map[string]any{},
			}}
			permissions := permissionResolverFunc(func(
				context.Context,
				auth.User,
				string,
				string,
			) (auth.PermissionResolution, error) {
				resolution := auth.PermissionResolution{UserID: 11}
				if test.permission {
					resolution.Permissions = []string{handler.CurrentIndexSchedulePermission}
				}
				return resolution, nil
			})
			route := newCurrentScheduleRoute(t, updater, permissions, nil)
			body := &trackingBody{Reader: strings.NewReader(
				`{"cron":"0 3 * * *","timezone":"UTC"}`,
			)}
			request := currentScheduleRequest(
				"/api/v2/elitea_core/index_meta/prompt_lib/7/9/docs",
				"",
			)
			request.Body = body
			response := httptest.NewRecorder()
			route.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if test.permission && (!body.read || updater.calls != 1) {
				t.Fatalf("allowed read=%t calls=%d", body.read, updater.calls)
			}
			if !test.permission && (body.read || updater.calls != 0) {
				t.Fatalf("denied read=%t calls=%d", body.read, updater.calls)
			}
		})
	}
}

func TestCurrentIndexScheduleRouteRejectsSuspendedPrincipalAndCrossProjectAccess(t *testing.T) {
	for _, test := range []struct {
		name       string
		validator  principalValidatorFunc
		resolution permissionResolverFunc
		wantStatus int
	}{
		{
			name: "suspended principal",
			validator: principalValidatorFunc(func(
				context.Context,
				auth.User,
			) (auth.User, error) {
				// The sentinel, not a bare error: only auth.ErrPrincipalInactive
				// is a 401, so a database fault stays a 5xx (#537).
				return auth.User{}, auth.ErrPrincipalInactive
			}),
			resolution: permissionResolverFunc(func(
				context.Context,
				auth.User,
				string,
				string,
			) (auth.PermissionResolution, error) {
				t.Fatal("suspended principal reached permissions")
				return auth.PermissionResolution{}, nil
			}),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "cross project denied by current resolver",
			validator: principalValidatorFunc(func(
				_ context.Context,
				user auth.User,
			) (auth.User, error) {
				return user, nil
			}),
			resolution: permissionResolverFunc(func(
				_ context.Context,
				_ auth.User,
				_ string,
				projectID string,
			) (auth.PermissionResolution, error) {
				if projectID != "8" {
					t.Fatalf("project=%q", projectID)
				}
				// A REFUSAL must carry auth.ErrPermissionDenied. The real
				// resolver returns that sentinel when the caller is not a
				// member. Any other error is an infrastructure failure and
				// answers 500.
				return auth.PermissionResolution{}, fmt.Errorf(
					"not a project member: %w", auth.ErrPermissionDenied)
			}),
			wantStatus: http.StatusForbidden,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			updater := &currentScheduleUpdaterStub{}
			route := newCurrentScheduleRoute(t, updater, test.resolution, test.validator)
			body := &trackingBody{Reader: strings.NewReader(
				`{"cron":"0 3 * * *","timezone":"UTC"}`,
			)}
			request := currentScheduleRequest(
				"/api/v2/elitea_core/index_meta/prompt_lib/8/9/docs",
				"",
			)
			request.Body = body
			response := httptest.NewRecorder()
			route.ServeHTTP(response, request)
			if response.Code != test.wantStatus || body.read || updater.calls != 0 {
				t.Fatalf(
					"status=%d read=%t calls=%d body=%s",
					response.Code,
					body.read,
					updater.calls,
					response.Body.String(),
				)
			}
		})
	}
}

func TestCurrentIndexScheduleRouteRequiresCurrentJSONMediaTypeBeforeDecoding(t *testing.T) {
	for _, test := range []struct {
		name        string
		contentType string
		accepted    bool
	}{
		{name: "missing"},
		{name: "plain text", contentType: "text/plain"},
		{
			name:        "application json incomplete charset",
			contentType: "application/json; charset",
			accepted:    true,
		},
		{
			name:        "application json charset",
			contentType: "application/json; charset=UTF-8",
			accepted:    true,
		},
		{
			name:        "application json suffix",
			contentType: "application/vnd.elitea+json; charset=utf-8",
			accepted:    true,
		},
		{
			name:        "application json suffix incomplete charset",
			contentType: "application/vnd.elitea+json; charset",
			accepted:    true,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			updater := &currentScheduleUpdaterStub{result: indexscheduleapp.MutationResult{
				IndexesMeta: map[string]any{},
			}}
			route := newCurrentScheduleRoute(
				t,
				updater,
				allowCurrentSchedulePermission,
				nil,
			)
			body := &trackingBody{Reader: strings.NewReader(
				`{"cron":"0 3 * * *","timezone":"UTC"}`,
			)}
			request := currentScheduleRequest(
				"/api/v2/elitea_core/index_meta/prompt_lib/7/9/docs",
				"",
			)
			request.Body = body
			if test.contentType == "" {
				request.Header.Del("Content-Type")
			} else {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			route.ServeHTTP(response, request)

			if test.accepted {
				if response.Code != http.StatusOK || !body.read || updater.calls != 1 {
					t.Fatalf(
						"accepted status=%d read=%t calls=%d body=%s",
						response.Code,
						body.read,
						updater.calls,
						response.Body.String(),
					)
				}
				return
			}
			if response.Code != http.StatusUnsupportedMediaType ||
				body.read ||
				updater.calls != 0 ||
				response.Body.String() !=
					"{\"ok\":false,\"error\":\"Unsupported Media Type\"}\n" {
				t.Fatalf(
					"rejected status=%d read=%t calls=%d body=%q",
					response.Code,
					body.read,
					updater.calls,
					response.Body.String(),
				)
			}
		})
	}
}

func TestCurrentIndexScheduleRouteMatchesFrozenPythonDifferentialFixtures(t *testing.T) {
	fixtureBytes, err := os.ReadFile("testdata/current_python_schedule_contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name            string          `json:"name"`
			Payload         json.RawMessage `json:"payload"`
			CurrentAccepted bool            `json:"current_accepted"`
			TargetAccepted  bool            `json:"target_accepted"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(fixtureBytes, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) < 16 {
		t.Fatalf("fixture unexpectedly small: %d", len(fixture.Cases))
	}

	for _, test := range fixture.Cases {
		test := test
		t.Run(test.Name, func(t *testing.T) {
			store := &currentScheduleStoreStub{
				result: indexscheduleapp.MutationResult{
					IndexesMeta: map[string]any{"docs": map[string]any{}},
				},
			}
			service, err := indexscheduleapp.NewService(store)
			if err != nil {
				t.Fatal(err)
			}
			route := newCurrentScheduleRoute(
				t,
				service,
				allowCurrentSchedulePermission,
				nil,
			)
			request := currentScheduleRequest(
				"/api/v2/elitea_core/index_meta/prompt_lib/7/9/docs",
				string(test.Payload),
			)
			response := httptest.NewRecorder()
			route.ServeHTTP(response, request)

			accepted := response.Code == http.StatusOK
			if accepted != test.TargetAccepted {
				t.Fatalf(
					"target accepted=%t want=%t current=%t status=%d body=%s payload=%s",
					accepted,
					test.TargetAccepted,
					test.CurrentAccepted,
					response.Code,
					response.Body.String(),
					test.Payload,
				)
			}
			if store.calls != boolToInt(test.TargetAccepted) {
				t.Fatalf("storage calls=%d accepted=%t", store.calls, test.TargetAccepted)
			}
			if test.Name == "credentials explicit private null" &&
				(store.mutation.Schedule.Credentials == nil ||
					store.mutation.Schedule.Credentials.Private != nil) {
				t.Fatalf("explicit credentials.private null drifted: %#v", store.mutation)
			}
		})
	}
}

func TestCurrentIndexScheduleRouteRestrictsPersonalScopeAndUsesSafeBoundedErrors(t *testing.T) {
	store := &currentScheduleStoreStub{}
	service, err := indexscheduleapp.NewService(store)
	if err != nil {
		t.Fatal(err)
	}
	route := newCurrentScheduleRoute(t, service, allowCurrentSchedulePermission, nil)

	request := currentScheduleRequest(
		"/api/v2/elitea_core/index_meta/prompt_lib/7/9/docs",
		`{"cron":"0 3 * * *","timezone":"UTC","user_id":12}`,
	)
	response := httptest.NewRecorder()
	route.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || store.calls != 0 {
		t.Fatalf(
			"other-user selection status=%d calls=%d mutation=%#v body=%s",
			response.Code,
			store.calls,
			store.mutation,
			response.Body.String(),
		)
	}

	request = currentScheduleRequest(
		"/api/v2/elitea_core/index_meta/prompt_lib/7/9/docs",
		`{"cron":"0 3 * * *","timezone":"UTC","user_id":11}`,
	)
	response = httptest.NewRecorder()
	route.ServeHTTP(response, request)
	if response.Code != http.StatusOK || store.calls != 1 ||
		store.mutation.RequestedUserID != 11 ||
		store.mutation.Schedule.CreatedBy != 11 {
		t.Fatalf(
			"own-user selection status=%d calls=%d mutation=%#v body=%s",
			response.Code,
			store.calls,
			store.mutation,
			response.Body.String(),
		)
	}

	for _, test := range []struct {
		name       string
		body       string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "explicit null user",
			body:       `{"cron":"0 3 * * *","timezone":"UTC","user_id":null}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"ok":false,"error":"Validation error on index schedule update: invalid request body"}` + "\n",
		},
		{
			name: "oversized body",
			body: `{"cron":"0 3 * * *","timezone":"UTC","padding":"` +
				strings.Repeat("x", handler.MaxCurrentIndexScheduleBodyBytes) + `"}`,
			wantStatus: http.StatusRequestEntityTooLarge,
			wantBody:   `{"ok":false,"error":"Index schedule request body is too large"}` + "\n",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			beforeCalls := store.calls
			request := currentScheduleRequest(
				"/api/v2/elitea_core/index_meta/prompt_lib/7/9/docs",
				test.body,
			)
			response := httptest.NewRecorder()
			route.ServeHTTP(response, request)
			if response.Code != test.wantStatus || response.Body.String() != test.wantBody {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
			if store.calls != beforeCalls {
				t.Fatalf("rejected request reached storage: %d -> %d", beforeCalls, store.calls)
			}
		})
	}
}

func TestCurrentIndexScheduleRouteMapsCurrentAndSafeApplicationErrors(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		wantStatus int
		wantBody   string
	}{
		{
			name:       "toolkit missing",
			err:        indexscheduleapp.ErrToolkitNotFound,
			wantStatus: http.StatusNotFound,
			wantBody:   `{"ok":false,"error":"Toolkit not found"}` + "\n",
		},
		{
			name:       "metadata bounded",
			err:        indexscheduleapp.ErrScheduleResultTooLarge,
			wantStatus: http.StatusRequestEntityTooLarge,
			wantBody:   `{"ok":false,"error":"Index schedule metadata is too large"}` + "\n",
		},
		{
			name:       "storage detail redacted",
			err:        errors.New("postgres password=must-not-leak"),
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"ok":false,"error":"Error occurred while updating index_meta"}` + "\n",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			updater := &currentScheduleUpdaterStub{err: test.err}
			route := newCurrentScheduleRoute(
				t,
				updater,
				allowCurrentSchedulePermission,
				nil,
			)
			request := currentScheduleRequest(
				"/api/v2/elitea_core/index_meta/prompt_lib/7/9/docs",
				`{"cron":"0 3 * * *","timezone":"UTC"}`,
			)
			response := httptest.NewRecorder()
			route.ServeHTTP(response, request)
			if response.Code != test.wantStatus || response.Body.String() != test.wantBody {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "password") {
				t.Fatalf("unsafe response=%s", response.Body.String())
			}
		})
	}
}

func TestCurrentIndexScheduleRouteRejectsIncompleteCompositionAndOtherMethods(t *testing.T) {
	updater := &currentScheduleUpdaterStub{}
	principal := principalValidatorFunc(func(
		_ context.Context,
		user auth.User,
	) (auth.User, error) {
		return user, nil
	})
	for name, test := range map[string]struct {
		updater     handler.CurrentIndexScheduleUpdater
		authConfig  apimw.AuthConfig
		permissions auth.PermissionResolver
	}{
		"missing updater": {
			authConfig:  apimw.AuthConfig{PrincipalValidator: principal, FormSessions: authtest.FormSessions{}},
			permissions: allowCurrentSchedulePermission,
		},
		"missing principal": {
			updater:     updater,
			authConfig:  apimw.AuthConfig{FormSessions: authtest.FormSessions{}},
			permissions: allowCurrentSchedulePermission,
		},
		"missing peer proof": {
			updater:     updater,
			authConfig:  apimw.AuthConfig{PrincipalValidator: principal},
			permissions: allowCurrentSchedulePermission,
		},
		"missing permissions": {
			updater:    updater,
			authConfig: apimw.AuthConfig{PrincipalValidator: principal, FormSessions: authtest.FormSessions{}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := handler.NewCurrentIndexScheduleRoute(
				test.updater,
				test.authConfig,
				test.permissions,
			); !errors.Is(err, handler.ErrInvalidCurrentIndexScheduleRoute) {
				t.Fatalf("error=%v", err)
			}
		})
	}

	route := newCurrentScheduleRoute(t, updater, allowCurrentSchedulePermission, nil)
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/v2/elitea_core/index_meta/prompt_lib/7/9/docs",
		nil,
	)
	response := httptest.NewRecorder()
	route.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed || updater.calls != 0 {
		t.Fatalf("status=%d calls=%d", response.Code, updater.calls)
	}
}

type currentScheduleUpdaterStub struct {
	update indexscheduleapp.Update
	result indexscheduleapp.MutationResult
	err    error
	calls  int
}

func (stub *currentScheduleUpdaterStub) Update(
	_ context.Context,
	update indexscheduleapp.Update,
) (indexscheduleapp.MutationResult, error) {
	stub.calls++
	stub.update = update
	return stub.result, stub.err
}

type currentScheduleStoreStub struct {
	mutation indexscheduleapp.Mutation
	result   indexscheduleapp.MutationResult
	err      error
	calls    int
}

func (stub *currentScheduleStoreStub) Patch(
	_ context.Context,
	mutation indexscheduleapp.Mutation,
) (indexscheduleapp.MutationResult, error) {
	stub.calls++
	stub.mutation = mutation
	return stub.result, stub.err
}

var allowCurrentSchedulePermission = permissionResolverFunc(func(
	context.Context,
	auth.User,
	string,
	string,
) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{
		UserID: 11, Permissions: []string{handler.CurrentIndexSchedulePermission},
	}, nil
})

func newCurrentScheduleRoute(
	t *testing.T,
	updater handler.CurrentIndexScheduleUpdater,
	permissions auth.PermissionResolver,
	validator principalValidatorFunc,
) *handler.CurrentIndexScheduleRoute {
	t.Helper()
	if validator == nil {
		validator = principalValidatorFunc(func(
			_ context.Context,
			user auth.User,
		) (auth.User, error) {
			if user.ID != "11" {
				return auth.User{}, errors.New("unexpected user")
			}
			return user, nil
		})
	}
	route, err := handler.NewCurrentIndexScheduleRoute(
		updater,
		apimw.AuthConfig{
			PrincipalValidator: validator,
			FormSessions:       authtest.FormSessions{},
		},
		permissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func currentScheduleRequest(path, body string) *http.Request {
	request := httptest.NewRequest(http.MethodPatch, path, bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	authtest.SignIn(request, "11")
	request.RemoteAddr = "10.0.0.8:43120"
	return request
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
