package applicationskills_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/applicationskills"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type currentApplicationSkillsReaderStub struct {
	skills       []handler.CurrentApplicationSkill
	err          error
	projectID    int32
	appVersionID int32
	calls        int
}

func (stub *currentApplicationSkillsReaderStub) ListCurrentApplicationSkills(
	_ context.Context,
	projectID int32,
	appVersionID int32,
) ([]handler.CurrentApplicationSkill, error) {
	stub.calls++
	stub.projectID = projectID
	stub.appVersionID = appVersionID
	return stub.skills, stub.err
}

func TestCurrentApplicationSkillsRoutePreservesExactCurrentContract(t *testing.T) {
	if handler.CurrentApplicationSkillsPath !=
		"/api/v2/elitea_core/application_skills/prompt_lib/{projectID}/{appVersionID}" ||
		handler.CurrentApplicationSkillsMode != auth.PermissionModeDefault ||
		handler.CurrentApplicationSkillsPermission !=
			"models.applications.applications.details" ||
		handler.MaxCurrentApplicationSkills != 5 {
		t.Fatalf(
			"current application-skills contract drifted: path=%q mode=%q permission=%q max=%d",
			handler.CurrentApplicationSkillsPath,
			handler.CurrentApplicationSkillsMode,
			handler.CurrentApplicationSkillsPermission,
			handler.MaxCurrentApplicationSkills,
		)
	}

	versionID := int32(19)
	missingVersionID := int32(29)
	reader := &currentApplicationSkillsReaderStub{
		skills: []handler.CurrentApplicationSkill{
			{
				Name:           "deploy",
				Description:    "Deploy safely",
				SkillID:        17,
				VersionID:      &versionID,
				VersionName:    "release",
				VersionMissing: false,
				IconMeta:       json.RawMessage(`{"url":"/icons/deploy.svg","type":"image/svg+xml"}`),
				// A real timestamp on one row and the zero value on the
				// other: `created_at` must come from the row, and the zero
				// value must still marshal to a parseable instant.
				CreatedAt: time.Date(2026, time.March, 4, 5, 6, 7, 0, time.UTC),
			},
			{
				Name:           "review",
				Description:    "Review changes",
				SkillID:        18,
				VersionID:      &missingVersionID,
				VersionName:    "unknown",
				VersionMissing: true,
				IconMeta:       json.RawMessage(`null`),
			},
		},
	}
	permissionCalls := 0
	route := newCurrentApplicationSkillsRoute(
		t,
		reader,
		currentApplicationSkillsPermissionResolverFunc(
			func(
				_ context.Context,
				user auth.User,
				mode string,
				projectID string,
			) (auth.PermissionResolution, error) {
				permissionCalls++
				if user.UserID != "11" || mode != auth.PermissionModeDefault ||
					projectID != "7" {
					t.Fatalf(
						"permission input user=%+v mode=%q project=%q",
						user,
						mode,
						projectID,
					)
				}
				return auth.PermissionResolution{
					UserID:      11,
					Permissions: []string{handler.CurrentApplicationSkillsPermission},
				}, nil
			},
		),
	)

	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentApplicationSkillsRequest(
		http.MethodGet,
		"/api/v2/elitea_core/application_skills/prompt_lib/007/0031",
		true,
		"10.0.0.8:43120",
	))

	// Both halves of the envelope, in one body (#395). `items` is the
	// published SkillsList contract that apps/elitea-web reads; `skills` and
	// `max_skills` are the Pylon keys apps/elitea-ui reads, unchanged.
	want := "{\"items\":[" +
		"{\"id\":\"17\",\"project_id\":\"7\",\"name\":\"deploy\"," +
		"\"description\":\"Deploy safely\",\"type\":\"skill\",\"is_default\":false," +
		"\"created_at\":\"2026-03-04T05:06:07Z\"," +
		"\"updated_at\":\"0001-01-01T00:00:00Z\"}," +
		"{\"id\":\"18\",\"project_id\":\"7\",\"name\":\"review\"," +
		"\"description\":\"Review changes\",\"type\":\"skill\",\"is_default\":false," +
		"\"created_at\":\"0001-01-01T00:00:00Z\"," +
		"\"updated_at\":\"0001-01-01T00:00:00Z\"}]," +
		"\"total\":2,\"page\":1,\"page_size\":2,\"total_pages\":1," +
		"\"skills\":[" +
		"{\"name\":\"deploy\",\"description\":\"Deploy safely\",\"skill_id\":17," +
		"\"version_id\":19,\"version_name\":\"release\",\"version_missing\":false," +
		"\"icon_meta\":{\"url\":\"/icons/deploy.svg\",\"type\":\"image/svg+xml\"}}," +
		"{\"name\":\"review\",\"description\":\"Review changes\",\"skill_id\":18," +
		"\"version_id\":29,\"version_name\":\"unknown\",\"version_missing\":true," +
		"\"icon_meta\":null}],\"max_skills\":5}\n"
	if response.Code != http.StatusOK || response.Body.String() != want ||
		reader.calls != 1 || reader.projectID != 7 || reader.appVersionID != 31 ||
		permissionCalls != 1 {
		t.Fatalf(
			"status=%d reader=%d project=%d version=%d permissions=%d body=%q",
			response.Code,
			reader.calls,
			reader.projectID,
			reader.appVersionID,
			permissionCalls,
			response.Body.String(),
		)
	}
}

func TestCurrentApplicationSkillsRouteAuthenticatesAndAuthorizesBeforeTenantRead(
	t *testing.T,
) {
	tests := []struct {
		name       string
		method     string
		path       string
		remoteAddr string
		auth       bool
		allowed    bool
		wantStatus int
	}{
		{
			name:       "missing authentication",
			method:     http.MethodGet,
			path:       "/api/v2/elitea_core/application_skills/prompt_lib/7/31",
			remoteAddr: "10.0.0.8:43120",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "untrusted forwarded identity",
			method:     http.MethodGet,
			path:       "/api/v2/elitea_core/application_skills/prompt_lib/7/31",
			remoteAddr: "192.0.2.9:443",
			auth:       true,
			allowed:    true,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "permission denied",
			method:     http.MethodGet,
			path:       "/api/v2/elitea_core/application_skills/prompt_lib/7/31",
			remoteAddr: "10.0.0.8:43120",
			auth:       true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "invalid project",
			method:     http.MethodGet,
			path:       "/api/v2/elitea_core/application_skills/prompt_lib/not-an-id/31",
			remoteAddr: "10.0.0.8:43120",
			auth:       true,
			allowed:    true,
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "invalid application version",
			method:     http.MethodGet,
			path:       "/api/v2/elitea_core/application_skills/prompt_lib/7/not-an-id",
			remoteAddr: "10.0.0.8:43120",
			auth:       true,
			allowed:    true,
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "wrong mode is not exposed",
			method:     http.MethodGet,
			path:       "/api/v2/elitea_core/application_skills/default/7/31",
			remoteAddr: "10.0.0.8:43120",
			auth:       true,
			allowed:    true,
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "post is not exposed",
			method:     http.MethodPost,
			path:       "/api/v2/elitea_core/application_skills/prompt_lib/7/31",
			remoteAddr: "10.0.0.8:43120",
			auth:       true,
			allowed:    true,
			wantStatus: http.StatusMethodNotAllowed,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &currentApplicationSkillsReaderStub{}
			permissions := currentApplicationSkillsPermissionResolverFunc(
				func(
					context.Context,
					auth.User,
					string,
					string,
				) (auth.PermissionResolution, error) {
					resolution := auth.PermissionResolution{UserID: 11}
					if test.allowed {
						resolution.Permissions = []string{
							handler.CurrentApplicationSkillsPermission,
						}
					}
					return resolution, nil
				},
			)
			route := newCurrentApplicationSkillsRoute(t, reader, permissions)
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentApplicationSkillsRequest(
				test.method,
				test.path,
				test.auth,
				test.remoteAddr,
			))
			if response.Code != test.wantStatus || reader.calls != 0 {
				t.Fatalf(
					"status=%d want=%d reader_calls=%d body=%s",
					response.Code,
					test.wantStatus,
					reader.calls,
					response.Body.String(),
				)
			}
		})
	}
}

func TestCurrentApplicationSkillsRoutePreservesEmptyListAndSafeGenericFailure(
	t *testing.T,
) {
	permissions := currentApplicationSkillsPermissionResolverFunc(
		func(
			context.Context,
			auth.User,
			string,
			string,
		) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{
				UserID:      11,
				Permissions: []string{handler.CurrentApplicationSkillsPermission},
			}, nil
		},
	)

	empty := &currentApplicationSkillsReaderStub{}
	route := newCurrentApplicationSkillsRoute(t, empty, permissions)
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentApplicationSkillsRequest(
		http.MethodGet,
		"/api/v2/elitea_core/application_skills/prompt_lib/7/31",
		true,
		"10.0.0.8:43120",
	))
	if response.Code != http.StatusOK ||
		response.Body.String() != "{\"items\":[],\"total\":0,\"page\":1,\"page_size\":0,\"total_pages\":0,\"skills\":[],\"max_skills\":5}\n" {
		t.Fatalf("empty response status=%d body=%q", response.Code, response.Body.String())
	}

	failure := &currentApplicationSkillsReaderStub{
		err: errors.New("pq: password=do-not-return"),
	}
	route = newCurrentApplicationSkillsRoute(t, failure, permissions)
	response = httptest.NewRecorder()
	route.ServeHTTP(response, currentApplicationSkillsRequest(
		http.MethodGet,
		"/api/v2/elitea_core/application_skills/prompt_lib/7/31",
		true,
		"10.0.0.8:43120",
	))
	if response.Code != http.StatusInternalServerError ||
		response.Body.String() !=
			"{\"message\":\"Internal Server Error\"}\n" ||
		strings.Contains(response.Body.String(), "password") {
		t.Fatalf("failure response status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestCurrentApplicationSkillsRouteAcceptsFlaskIntegerDomainAndBoundsPostgresIDs(
	t *testing.T,
) {
	const tooLarge = "9999999999999999999999999999999999999999"
	tests := []struct {
		name              string
		projectID         string
		appVersionID      string
		wantRBACProject   string
		wantReaderCalls   int
		wantReaderProject int32
		wantReaderVersion int32
	}{
		{
			name:            "zero application version is absent",
			projectID:       "7",
			appVersionID:    "0",
			wantRBACProject: "7",
		},
		{
			name:            "arbitrarily large application version is absent",
			projectID:       "7",
			appVersionID:    tooLarge,
			wantRBACProject: "7",
		},
		{
			name:            "zero project is accepted by routing",
			projectID:       "0",
			appVersionID:    "31",
			wantRBACProject: "0",
		},
		{
			name:            "arbitrarily large project is accepted by routing",
			projectID:       tooLarge,
			appVersionID:    "31",
			wantRBACProject: tooLarge,
		},
		{
			name:              "leading zeroes normalize like Flask integers",
			projectID:         "0007",
			appVersionID:      "00000000000031",
			wantRBACProject:   "7",
			wantReaderCalls:   1,
			wantReaderProject: 7,
			wantReaderVersion: 31,
		},
		{
			name:              "maximum PostgreSQL integer is queried",
			projectID:         "7",
			appVersionID:      "2147483647",
			wantRBACProject:   "7",
			wantReaderCalls:   1,
			wantReaderProject: 7,
			wantReaderVersion: 2147483647,
		},
		{
			name:            "one above PostgreSQL integer is absent",
			projectID:       "7",
			appVersionID:    "2147483648",
			wantRBACProject: "7",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &currentApplicationSkillsReaderStub{}
			route := newCurrentApplicationSkillsRoute(
				t,
				reader,
				currentApplicationSkillsPermissionResolverFunc(
					func(
						_ context.Context,
						_ auth.User,
						mode string,
						projectID string,
					) (auth.PermissionResolution, error) {
						if mode != auth.PermissionModeDefault ||
							projectID != test.wantRBACProject {
							t.Fatalf(
								"mode=%q project=%q want_project=%q",
								mode,
								projectID,
								test.wantRBACProject,
							)
						}
						return auth.PermissionResolution{
							UserID: 11,
							Permissions: []string{
								handler.CurrentApplicationSkillsPermission,
							},
						}, nil
					},
				),
			)
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentApplicationSkillsRequest(
				http.MethodGet,
				"/api/v2/elitea_core/application_skills/prompt_lib/"+
					test.projectID+"/"+test.appVersionID,
				true,
				"10.0.0.8:43120",
			))
			if response.Code != http.StatusOK ||
				response.Body.String() != "{\"items\":[],\"total\":0,\"page\":1,\"page_size\":0,\"total_pages\":0,\"skills\":[],\"max_skills\":5}\n" ||
				reader.calls != test.wantReaderCalls ||
				reader.projectID != test.wantReaderProject ||
				reader.appVersionID != test.wantReaderVersion {
				t.Fatalf(
					"status=%d body=%q reader_calls=%d project=%d version=%d",
					response.Code,
					response.Body.String(),
					reader.calls,
					reader.projectID,
					reader.appVersionID,
				)
			}
		})
	}
}

func TestCurrentApplicationSkillsRouteRejectsIncompleteComposition(t *testing.T) {
	reader := &currentApplicationSkillsReaderStub{}
	principal := currentApplicationSkillsPrincipalValidatorFunc(
		func(_ context.Context, user auth.User) (auth.User, error) { return user, nil },
	)
	peer := currentApplicationSkillsPeerVerifierFunc(func(*http.Request) error { return nil })
	permissions := currentApplicationSkillsPermissionResolverFunc(
		func(
			context.Context,
			auth.User,
			string,
			string,
		) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{}, nil
		},
	)
	authConfig := apimw.AuthConfig{
		PrincipalValidator:        principal,
		ForwardedIdentityVerifier: peer,
	}

	for name, test := range map[string]struct {
		reader      handler.CurrentApplicationSkillsReader
		authConfig  apimw.AuthConfig
		permissions auth.PermissionResolver
	}{
		"missing reader":      {authConfig: authConfig, permissions: permissions},
		"missing principal":   {reader: reader, authConfig: apimw.AuthConfig{ForwardedIdentityVerifier: peer}, permissions: permissions},
		"missing permissions": {reader: reader, authConfig: authConfig},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := handler.NewCurrentApplicationSkillsRoute(
				test.reader,
				test.authConfig,
				test.permissions,
			); !errors.Is(err, handler.ErrInvalidCurrentApplicationSkillsRoute) {
				t.Fatalf(
					"error=%v want=%v",
					err,
					handler.ErrInvalidCurrentApplicationSkillsRoute,
				)
			}
		})
	}

	// NOTE(#395): "missing peer proof" stood in the table above. A missing
	// ForwardedIdentityVerifier is the shape an OIDC or SAML install has, and
	// apimw.Auth authenticates that caller through the session cookie or the
	// bearer token. Refusing it kept the capability Form-only, and the route is
	// the only handler for its path now. PrincipalValidator stays mandatory.
	t.Run("composes without a peer verifier", func(t *testing.T) {
		route, err := handler.NewCurrentApplicationSkillsRoute(
			reader,
			apimw.AuthConfig{PrincipalValidator: principal},
			permissions,
		)
		if err != nil {
			t.Fatalf("an OIDC-only composition was refused: %v", err)
		}
		if route == nil {
			t.Fatal("an OIDC-only composition returned no route")
		}
	})

	var nilRoute *handler.CurrentApplicationSkillsRoute
	response := httptest.NewRecorder()
	nilRoute.ServeHTTP(
		response,
		httptest.NewRequest(
			http.MethodGet,
			"/api/v2/elitea_core/application_skills/prompt_lib/7/31",
			nil,
		),
	)
	if response.Code != http.StatusNotFound {
		t.Fatalf("nil route status=%d", response.Code)
	}
}

func TestCurrentApplicationSkillsRepositoryRejectsInvalidDependenciesAndRequest(t *testing.T) {
	if _, err := handler.NewCurrentApplicationSkillsRepository(nil); err == nil {
		t.Fatal("nil database was accepted")
	}

	var repository *handler.CurrentApplicationSkillsRepository
	if _, err := repository.ListCurrentApplicationSkills(
		context.Background(),
		1,
		1,
	); !errors.Is(err, handler.ErrInvalidCurrentApplicationSkillsRequest) {
		t.Fatalf(
			"nil repository error=%v want=%v",
			err,
			handler.ErrInvalidCurrentApplicationSkillsRequest,
		)
	}
}

func newCurrentApplicationSkillsRoute(
	t *testing.T,
	reader handler.CurrentApplicationSkillsReader,
	permissions auth.PermissionResolver,
) *handler.CurrentApplicationSkillsRoute {
	t.Helper()
	route, err := handler.NewCurrentApplicationSkillsRoute(
		reader,
		apimw.AuthConfig{
			PrincipalValidator: currentApplicationSkillsPrincipalValidatorFunc(
				func(_ context.Context, user auth.User) (auth.User, error) {
					return user, nil
				},
			),
			ForwardedIdentityVerifier: currentApplicationSkillsPeerVerifierFunc(
				func(request *http.Request) error {
					if request.RemoteAddr != "10.0.0.8:43120" {
						return errors.New("untrusted peer")
					}
					return nil
				},
			),
		},
		permissions,
	)
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func currentApplicationSkillsRequest(
	method string,
	path string,
	authenticated bool,
	remoteAddr string,
) *http.Request {
	request := httptest.NewRequest(method, path, nil)
	request.RemoteAddr = remoteAddr
	if authenticated {
		request.Header.Set("X-Auth-Type", "user")
		request.Header.Set("X-Auth-ID", "11")
	}
	return request
}

type currentApplicationSkillsPermissionResolverFunc func(
	context.Context,
	auth.User,
	string,
	string,
) (auth.PermissionResolution, error)

func (function currentApplicationSkillsPermissionResolverFunc) ResolvePermissions(
	ctx context.Context,
	user auth.User,
	mode string,
	projectID string,
) (auth.PermissionResolution, error) {
	return function(ctx, user, mode, projectID)
}

type currentApplicationSkillsPrincipalValidatorFunc func(
	context.Context,
	auth.User,
) (auth.User, error)

func (function currentApplicationSkillsPrincipalValidatorFunc) ValidatePrincipal(
	ctx context.Context,
	user auth.User,
) (auth.User, error) {
	return function(ctx, user)
}

type currentApplicationSkillsPeerVerifierFunc func(*http.Request) error

func (function currentApplicationSkillsPeerVerifierFunc) VerifyForwardedIdentityPeer(
	request *http.Request,
) error {
	return function(request)
}

// TestCurrentApplicationSkillsRouteAnswersThePublishedSkillsListContract pins
// the half of the envelope that made ELITEA_APPLICATION_SKILLS_ENABLED
// unturnable (#395).
//
// The published contract for this path is SkillsList — {items, total, page,
// page_size, total_pages} — in api/openapi/v2.yaml. apps/elitea-web reads it
// through shared/api/unwrap.ts, which takes `items` first and reports any
// other shape as unrecognised. So a body with only the Pylon keys renders as
// "this agent version has no skills" while the route answers 200 with the
// right rows, and turning the flag on broke the web client instead of fixing
// it.
//
// The Pylon keys are asserted in the same test on purpose: correcting the
// contract half must not cost the parity half, which apps/elitea-ui reads and
// the edge cutover needs.
func TestCurrentApplicationSkillsRouteAnswersThePublishedSkillsListContract(
	t *testing.T,
) {
	versionID := int32(19)
	reader := &currentApplicationSkillsReaderStub{
		skills: []handler.CurrentApplicationSkill{
			{
				Name:           "deploy",
				Description:    "Deploy safely",
				SkillID:        17,
				VersionID:      &versionID,
				VersionName:    "release",
				VersionMissing: false,
				IconMeta:       json.RawMessage(`null`),
				CreatedAt:      time.Date(2026, time.March, 4, 5, 6, 7, 0, time.UTC),
			},
		},
	}
	route := newCurrentApplicationSkillsRoute(
		t,
		reader,
		currentApplicationSkillsPermissionResolverFunc(
			func(
				context.Context,
				auth.User,
				string,
				string,
			) (auth.PermissionResolution, error) {
				return auth.PermissionResolution{
					UserID:      11,
					Permissions: []string{handler.CurrentApplicationSkillsPermission},
				}, nil
			},
		),
	)

	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentApplicationSkillsRequest(
		http.MethodGet,
		"/api/v2/elitea_core/application_skills/prompt_lib/7/31",
		true,
		"10.0.0.8:43120",
	))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v (%q)", err, response.Body.String())
	}
	// SkillsList declares these five keys required.
	for _, key := range []string{"items", "total", "page", "page_size", "total_pages"} {
		if _, present := body[key]; !present {
			t.Fatalf(
				"published SkillsList key %q missing; apps/elitea-web renders this as "+
					"an empty skill list. body=%q",
				key,
				response.Body.String(),
			)
		}
	}
	// The Pylon keys apps/elitea-ui reads must survive beside them.
	for _, key := range []string{"skills", "max_skills"} {
		if _, present := body[key]; !present {
			t.Fatalf("Pylon key %q lost from the parity envelope: %q", key, response.Body.String())
		}
	}

	var envelope struct {
		Items []struct {
			ID          string `json:"id"`
			ProjectID   string `json:"project_id"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Type        string `json:"type"`
			CreatedAt   string `json:"created_at"`
		} `json:"items"`
		Total      int `json:"total"`
		Page       int `json:"page"`
		PageSize   int `json:"page_size"`
		TotalPages int `json:"total_pages"`
		Skills     []struct {
			SkillID int32 `json:"skill_id"`
		} `json:"skills"`
		MaxSkills int `json:"max_skills"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if len(envelope.Items) != 1 || len(envelope.Skills) != 1 {
		t.Fatalf("halves disagree: items=%d skills=%d", len(envelope.Items), len(envelope.Skills))
	}
	item := envelope.Items[0]
	// The two halves project the same row, so the ids must agree.
	if item.ID != "17" || envelope.Skills[0].SkillID != 17 {
		t.Fatalf("item id=%q skills[0].skill_id=%d", item.ID, envelope.Skills[0].SkillID)
	}
	if item.ProjectID != "7" || item.Name != "deploy" ||
		item.Description != "Deploy safely" || item.Type != "skill" ||
		item.CreatedAt != "2026-03-04T05:06:07Z" {
		t.Fatalf("item=%+v", item)
	}
	// One page, sized by the attached set — the same numbers the prototype
	// handler answered before #395 deleted it, so a client that read them from
	// the old route keeps reading the same values.
	if envelope.Total != 1 || envelope.Page != 1 || envelope.PageSize != 1 ||
		envelope.TotalPages != 1 || envelope.MaxSkills != 5 {
		t.Fatalf(
			"pagination total=%d page=%d page_size=%d total_pages=%d max=%d",
			envelope.Total,
			envelope.Page,
			envelope.PageSize,
			envelope.TotalPages,
			envelope.MaxSkills,
		)
	}
}
