package social_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/social"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type currentFeedbackCreatorStub struct {
	create func(context.Context, int64, int64, string, int, *string, string) (int64, error)
	calls  int
}

func (stub *currentFeedbackCreatorStub) CreateCurrentFeedback(
	ctx context.Context,
	userID int64,
	projectID int64,
	description string,
	rating int,
	referrer *string,
	userAgent string,
) (int64, error) {
	stub.calls++
	if stub.create == nil {
		return 0, nil
	}
	return stub.create(ctx, userID, projectID, description, rating, referrer, userAgent)
}

type currentFeedbackPrincipalValidatorFunc func(context.Context, auth.User) (auth.User, error)

func (function currentFeedbackPrincipalValidatorFunc) ValidatePrincipal(
	ctx context.Context,
	user auth.User,
) (auth.User, error) {
	return function(ctx, user)
}

type currentFeedbackPeerVerifierFunc func(*http.Request) error

func (function currentFeedbackPeerVerifierFunc) VerifyForwardedIdentityPeer(request *http.Request) error {
	return function(request)
}

type currentFeedbackPermissionResolverFunc func(context.Context, auth.User, string, string) (auth.PermissionResolution, error)

func (function currentFeedbackPermissionResolverFunc) ResolvePermissions(
	ctx context.Context,
	user auth.User,
	mode string,
	projectID string,
) (auth.PermissionResolution, error) {
	return function(ctx, user, mode, projectID)
}

func TestCurrentFeedbackCreateRoutePreservesContractAndDerivesProtectedFields(t *testing.T) {
	if handler.CurrentFeedbackCreatePath != "/api/v2/social/feedbacks/default/{projectID}" ||
		handler.CurrentFeedbackCreateAliasPath != "/api/v2/social/feedbacks/{projectID}" ||
		handler.CurrentFeedbackCreateMode != auth.PermissionModeDefault ||
		handler.CurrentFeedbackCreatePermission != "models.social.feedbacks.create" {
		t.Fatalf(
			"current feedback route drifted: path=%q alias=%q mode=%q permission=%q",
			handler.CurrentFeedbackCreatePath,
			handler.CurrentFeedbackCreateAliasPath,
			handler.CurrentFeedbackCreateMode,
			handler.CurrentFeedbackCreatePermission,
		)
	}

	creator := &currentFeedbackCreatorStub{
		create: func(
			_ context.Context,
			userID int64,
			projectID int64,
			description string,
			rating int,
			referrer *string,
			userAgent string,
		) (int64, error) {
			if userID != 41 || projectID != 7 || description != "current feedback" || rating != 5 {
				t.Fatalf("feedback input user=%d project=%d description=%q rating=%d", userID, projectID, description, rating)
			}
			if referrer == nil || *referrer != "https://elitea.example/app/chat" {
				t.Fatalf("referrer=%v", referrer)
			}
			if userAgent != "EliteaUI/current" {
				t.Fatalf("user agent=%q", userAgent)
			}
			return 73, nil
		},
	}
	permissionCalls := 0
	route := newCurrentFeedbackCreateRoute(t, creator, currentFeedbackPermissionResolverFunc(
		func(_ context.Context, user auth.User, mode, projectID string) (auth.PermissionResolution, error) {
			permissionCalls++
			if user.TokenID != "91" || user.UserID != "41" ||
				mode != auth.PermissionModeDefault || projectID != "7" {
				t.Fatalf("permission input user=%+v mode=%q project=%q", user, mode, projectID)
			}
			return auth.PermissionResolution{
				UserID:      41,
				Permissions: []string{handler.CurrentFeedbackCreatePermission},
			}, nil
		},
	))

	request := currentFeedbackRequest(
		`{"description":"current feedback","rating":5,"location":"/app/chat",` +
			`"user_id":999,"referrer":"https://attacker.invalid","user_agent":"forged"}`,
	)
	request.Header.Set("Referer", "https://elitea.example/app/chat")
	request.Header.Set("User-Agent", "EliteaUI/current")
	response := httptest.NewRecorder()
	route.ServeHTTP(response, request)

	if response.Code != http.StatusCreated || response.Body.String() != "{\"id\":73}\n" ||
		creator.calls != 1 || permissionCalls != 1 {
		t.Fatalf(
			"status=%d creator_calls=%d permission_calls=%d body=%q",
			response.Code,
			creator.calls,
			permissionCalls,
			response.Body.String(),
		)
	}
}

func TestCurrentFeedbackCreateRouteSupportsCurrentProjectScopedAliases(t *testing.T) {
	for _, target := range []string{
		"/api/v2/social/feedbacks/default/7",
		"/api/v2/social/feedbacks/7",
	} {
		t.Run(target, func(t *testing.T) {
			creator := &currentFeedbackCreatorStub{
				create: func(
					_ context.Context,
					userID int64,
					projectID int64,
					description string,
					rating int,
					referrer *string,
					userAgent string,
				) (int64, error) {
					if userID != 41 || description != "feedback" || rating != 5 ||
						referrer != nil || userAgent != "" {
						t.Fatalf(
							"feedback input user=%d description=%q rating=%d referrer=%v user_agent=%q",
							userID,
							description,
							rating,
							referrer,
							userAgent,
						)
					}
					return 73, nil
				},
			}
			route := newAuthorizedCurrentFeedbackCreateRoute(t, creator)
			request := currentFeedbackRequest(`{"description":"feedback","rating":5}`)
			request.URL.Path = target
			response := httptest.NewRecorder()
			route.ServeHTTP(response, request)

			if response.Code != http.StatusCreated ||
				response.Body.String() != "{\"id\":73}\n" ||
				creator.calls != 1 {
				t.Fatalf(
					"status=%d creator_calls=%d body=%q",
					response.Code,
					creator.calls,
					response.Body.String(),
				)
			}
		})
	}
}

func TestCurrentFeedbackCreateRouteRejectsBeforeReadingDeniedBody(t *testing.T) {
	creator := &currentFeedbackCreatorStub{}
	permissions := currentFeedbackPermissionResolverFunc(
		func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{UserID: 41}, nil
		},
	)
	route := newCurrentFeedbackCreateRoute(t, creator, permissions)

	for name, test := range map[string]struct {
		target string
		remote string
	}{
		"untrusted peer":    {target: "/api/v2/social/feedbacks/default/7", remote: "192.0.2.9:443"},
		"permission denied": {target: "/api/v2/social/feedbacks/default/7", remote: "10.0.0.8:43120"},
		"invalid project":   {target: "/api/v2/social/feedbacks/default/007", remote: "10.0.0.8:43120"},
	} {
		t.Run(name, func(t *testing.T) {
			body := &currentFeedbackTrackingBody{Reader: strings.NewReader(`{"description":"private","rating":5}`)}
			request := currentFeedbackRequest("")
			request.URL.Path = test.target
			request.Body = body
			request.RemoteAddr = test.remote
			response := httptest.NewRecorder()
			route.ServeHTTP(response, request)

			if response.Code != http.StatusUnauthorized && response.Code != http.StatusForbidden {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if body.read {
				t.Fatal("denied request body was read")
			}
		})
	}
	if creator.calls != 0 {
		t.Fatalf("denied requests reached creator %d times", creator.calls)
	}
}

func TestCurrentFeedbackCreateRouteValidatesBoundedBody(t *testing.T) {
	for name, body := range map[string]string{
		"missing description": `{"rating":5}`,
		"null description":    `{"description":null,"rating":5}`,
		"missing rating":      `{"description":"feedback"}`,
		"rating below range":  `{"description":"feedback","rating":-1}`,
		"rating above range":  `{"description":"feedback","rating":6}`,
		"fractional rating":   `{"description":"feedback","rating":4.5}`,
		"malformed":           `{"description":`,
		"trailing value":      `{"description":"feedback","rating":4} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			creator := &currentFeedbackCreatorStub{}
			route := newAuthorizedCurrentFeedbackCreateRoute(t, creator)
			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentFeedbackRequest(body))

			if response.Code != http.StatusBadRequest || creator.calls != 0 ||
				response.Body.String() != "{\"error\":\"invalid request\"}\n" {
				t.Fatalf(
					"status=%d creator_calls=%d body=%q",
					response.Code,
					creator.calls,
					response.Body.String(),
				)
			}
		})
	}

	creator := &currentFeedbackCreatorStub{}
	route := newAuthorizedCurrentFeedbackCreateRoute(t, creator)
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentFeedbackRequest(
		`{"description":"`+strings.Repeat("x", handler.MaxCurrentFeedbackBodyBytes)+`","rating":5}`,
	))
	if response.Code != http.StatusRequestEntityTooLarge || creator.calls != 0 ||
		response.Body.String() != "{\"error\":\"request body is too large\"}\n" {
		t.Fatalf(
			"oversize status=%d creator_calls=%d body=%q",
			response.Code,
			creator.calls,
			response.Body.String(),
		)
	}
}

func TestCurrentFeedbackCreateRoutePreservesCurrentJSONMediaTypeRequirement(t *testing.T) {
	for name, test := range map[string]struct {
		contentType string
		wantStatus  int
		wantBody    string
		wantCalls   int
	}{
		"missing": {
			wantStatus: http.StatusUnsupportedMediaType,
			wantBody:   "{\"error\":\"unsupported media type\"}\n",
		},
		"text": {
			contentType: "text/plain",
			wantStatus:  http.StatusUnsupportedMediaType,
			wantBody:    "{\"error\":\"unsupported media type\"}\n",
		},
		"vendor JSON": {
			contentType: "application/vnd.elitea+json; charset=utf-8",
			wantStatus:  http.StatusCreated,
			wantBody:    "{\"id\":73}\n",
			wantCalls:   1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			creator := &currentFeedbackCreatorStub{
				create: func(
					context.Context,
					int64,
					int64,
					string,
					int,
					*string,
					string,
				) (int64, error) {
					return 73, nil
				},
			}
			route := newAuthorizedCurrentFeedbackCreateRoute(t, creator)
			request := currentFeedbackRequest(`{"description":"feedback","rating":5}`)
			if test.contentType == "" {
				request.Header.Del("Content-Type")
			} else {
				request.Header.Set("Content-Type", test.contentType)
			}
			response := httptest.NewRecorder()
			route.ServeHTTP(response, request)

			if response.Code != test.wantStatus ||
				response.Body.String() != test.wantBody ||
				creator.calls != test.wantCalls {
				t.Fatalf(
					"status=%d creator_calls=%d body=%q",
					response.Code,
					creator.calls,
					response.Body.String(),
				)
			}
		})
	}
}

func TestCurrentFeedbackCreateRouteAllowsBaselineEmptyDescriptionAndHandlesStorageFailure(t *testing.T) {
	databaseFailure := errors.New("database unavailable with private detail")
	creator := &currentFeedbackCreatorStub{
		create: func(
			_ context.Context,
			_ int64,
			_ int64,
			description string,
			rating int,
			referrer *string,
			userAgent string,
		) (int64, error) {
			if description != "" || rating != 0 || referrer != nil || userAgent != "" {
				t.Fatalf(
					"feedback input description=%q rating=%d referrer=%v user_agent=%q",
					description,
					rating,
					referrer,
					userAgent,
				)
			}
			return 0, databaseFailure
		},
	}
	route := newAuthorizedCurrentFeedbackCreateRoute(t, creator)
	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentFeedbackRequest(`{"description":"","rating":0}`))

	if response.Code != http.StatusInternalServerError || creator.calls != 1 ||
		response.Body.String() != "{\"error\":\"internal server error\"}\n" ||
		strings.Contains(response.Body.String(), "private detail") {
		t.Fatalf("status=%d creator_calls=%d body=%q", response.Code, creator.calls, response.Body.String())
	}
}

func TestCurrentFeedbackCreateRouteRejectsIncompleteCompositionAndOtherMethods(t *testing.T) {
	creator := &currentFeedbackCreatorStub{}
	principal := currentFeedbackPrincipalValidatorFunc(
		func(_ context.Context, user auth.User) (auth.User, error) { return user, nil },
	)
	peer := currentFeedbackPeerVerifierFunc(func(*http.Request) error { return nil })
	permissions := currentFeedbackPermissionResolverFunc(
		func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{}, nil
		},
	)
	authConfig := apimw.AuthConfig{PrincipalValidator: principal, ForwardedIdentityVerifier: peer}

	for name, test := range map[string]struct {
		creator     handler.CurrentFeedbackCreator
		authConfig  apimw.AuthConfig
		permissions auth.PermissionResolver
	}{
		"missing creator":     {authConfig: authConfig, permissions: permissions},
		"missing principal":   {creator: creator, authConfig: apimw.AuthConfig{ForwardedIdentityVerifier: peer}, permissions: permissions},
		"missing peer proof":  {creator: creator, authConfig: apimw.AuthConfig{PrincipalValidator: principal}, permissions: permissions},
		"missing permissions": {creator: creator, authConfig: authConfig},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := handler.NewCurrentFeedbackCreateRoute(
				test.creator,
				test.authConfig,
				test.permissions,
			); !errors.Is(err, handler.ErrInvalidCurrentFeedbackCreateRoute) {
				t.Fatalf("error=%v, want %v", err, handler.ErrInvalidCurrentFeedbackCreateRoute)
			}
		})
	}

	route := newCurrentFeedbackCreateRoute(t, creator, permissions)
	request := currentFeedbackRequest(`{"description":"feedback","rating":5}`)
	request.Method = http.MethodGet
	response := httptest.NewRecorder()
	route.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
}

func newAuthorizedCurrentFeedbackCreateRoute(
	t *testing.T,
	creator handler.CurrentFeedbackCreator,
) *handler.CurrentFeedbackCreateRoute {
	t.Helper()
	return newCurrentFeedbackCreateRoute(t, creator, currentFeedbackPermissionResolverFunc(
		func(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
			return auth.PermissionResolution{
				UserID:      41,
				Permissions: []string{handler.CurrentFeedbackCreatePermission},
			}, nil
		},
	))
}

func newCurrentFeedbackCreateRoute(
	t *testing.T,
	creator handler.CurrentFeedbackCreator,
	permissions auth.PermissionResolver,
) *handler.CurrentFeedbackCreateRoute {
	t.Helper()
	route, err := handler.NewCurrentFeedbackCreateRoute(
		creator,
		apimw.AuthConfig{
			PrincipalValidator: currentFeedbackPrincipalValidatorFunc(
				func(_ context.Context, user auth.User) (auth.User, error) { return user, nil },
			),
			ForwardedIdentityVerifier: currentFeedbackPeerVerifierFunc(
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

func currentFeedbackRequest(body string) *http.Request {
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/v2/social/feedbacks/default/7",
		strings.NewReader(body),
	)
	request.RemoteAddr = "10.0.0.8:43120"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Auth-Type", "token")
	request.Header.Set("X-Auth-ID", "91")
	request.Header.Set("X-Auth-User-ID", "41")
	return request
}

type currentFeedbackTrackingBody struct {
	io.Reader
	read bool
}

func (body *currentFeedbackTrackingBody) Read(buffer []byte) (int, error) {
	body.read = true
	return body.Reader.Read(buffer)
}

func (body *currentFeedbackTrackingBody) Close() error {
	return nil
}
