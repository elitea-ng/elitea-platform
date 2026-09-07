package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/oapiserver"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/shadow"
	agentexecutionapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/agentexecution"
	applicationskillsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/applicationskills"
	v2auth "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/auth"
	configurationapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	v2convs "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	indexingapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/indexing"
	indextypesapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/indextypes"
	notificationsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/notifications"
	projectinfoapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/projectinfo"
	v2projects "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/projects"
	promptcontextreadsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/promptcontextreads"
	v2social "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/social"
	socialapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/social"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/cutover"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
	dbrepos "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// newUnreachableRedisClient returns a redis client pointed at a loopback
// address nothing listens on. cutover.Tracker calls its methods with no nil
// check, so any test that composes CutoverRouter/CutoverTracker and expects
// requests to reach them needs a non-nil client to avoid a nil-pointer
// panic; pointing it at an unreachable address instead of a real redis
// instance gives a fast, deterministic connection error, which the
// production code already treats as "fall through" (see cutover/router.go).
func newUnreachableRedisClient() *goredis.Client {
	return goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond})
}

// reviewedRoutesRouter exercises mountReviewedProductionRoutes in isolation,
// the same way newProductionRouter's single call site does (#243 removed the
// only other call site, which ran through a standalone router that was dead
// in every real deployment; see NewRouter's doc comment). Building just the
// reviewed surface, without the broader legacy-parity registrations
// newProductionRouter also carries, keeps these tests focused on
// mountReviewedProductionRoutes' own per-field gating rather than on
// route-tree interactions with the rest of the router.
func reviewedRoutesRouter(cfg RouterConfig) chi.Router {
	r := chi.NewRouter()
	mountReviewedProductionRoutes(r, cfg)
	return r
}

func TestProductionRouterMountsAllCurrentAgentExecutionPaths(t *testing.T) {
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	router := reviewedRoutesRouter(RouterConfig{CurrentAgentStart: handler})

	for _, test := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodPost, "/api/v2/elitea_core/messages/prompt_lib/2/conversation", http.StatusNoContent},
		{http.MethodPost, "/api/v2/elitea_core/regenerate/prompt_lib/2/message", http.StatusNoContent},
		{http.MethodPost, "/api/v2/elitea_core/continue_predict/prompt_lib/2/conversation", http.StatusNoContent},
		{http.MethodGet, "/api/v2/elitea_core/continue_predict/prompt_lib/2/conversation", http.StatusMethodNotAllowed},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
			if response.Code != test.want {
				t.Fatalf("status=%d want=%d body=%q", response.Code, test.want, response.Body.String())
			}
		})
	}

	uncomposed := reviewedRoutesRouter(RouterConfig{})
	response := httptest.NewRecorder()
	uncomposed.ServeHTTP(
		response,
		httptest.NewRequest(
			http.MethodPost,
			strings.NewReplacer(
				"{projectID}", "2",
				"{conversationID}", "conversation",
			).Replace(agentexecutionapi.CurrentContinuationPath),
			nil,
		),
	)
	if response.Code != http.StatusNotFound {
		t.Fatalf("uncomposed continuation status=%d", response.Code)
	}
}

func TestProductionRouterMountsCurrentAgentCancelOnlyWhenComposed(t *testing.T) {
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	path := "/api/v2/elitea_core/task/prompt_lib/2/10000000-0000-4000-8000-000000000051"
	for _, test := range []struct {
		name   string
		router http.Handler
		method string
		want   int
	}{
		{name: "delete", router: reviewedRoutesRouter(RouterConfig{CurrentAgentCancel: handler}), method: http.MethodDelete, want: http.StatusNoContent},
		{name: "wrong method", router: reviewedRoutesRouter(RouterConfig{CurrentAgentCancel: handler}), method: http.MethodGet, want: http.StatusMethodNotAllowed},
		{name: "uncomposed", router: reviewedRoutesRouter(RouterConfig{}), method: http.MethodDelete, want: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			test.router.ServeHTTP(response, httptest.NewRequest(test.method, path, nil))
			if response.Code != test.want {
				t.Fatalf("status=%d want=%d body=%q", response.Code, test.want, response.Body.String())
			}
		})
	}
}

type productionChatConfigReader struct{}

func (productionChatConfigReader) GetCurrentChatConfig(
	context.Context,
	int64,
) (promptcontextreadsapi.CurrentChatConfig, error) {
	return promptcontextreadsapi.CurrentChatConfig{
		ChatMaxUploadCount:       "10",
		ChatMaxUploadSizeMB:      "150",
		ChatMaxFileUploadSizeMB:  "150",
		ChatMaxImageUploadCount:  "10",
		ChatMaxImageUploadSizeMB: "3",
	}, nil
}

type productionProjectContextReader struct{}

func (productionProjectContextReader) GetCurrentProjectContext(
	context.Context,
	int64,
) (promptcontextreadsapi.CurrentProjectContext, error) {
	return promptcontextreadsapi.CurrentProjectContext{Content: "", Enabled: true}, nil
}

type productionPromptContextPermissionResolver struct{}

func (productionPromptContextPermissionResolver) ResolvePermissions(
	context.Context,
	auth.User,
	string,
	string,
) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{
		UserID: 11,
		Permissions: []string{
			promptcontextreadsapi.CurrentChatConfigPermission,
			promptcontextreadsapi.CurrentProjectContextPermission,
		},
	}, nil
}

func TestProductionRouterMountsOnlyCurrentPromptContextGETs(t *testing.T) {
	routes, err := promptcontextreadsapi.NewCurrentRoutes(
		productionChatConfigReader{},
		productionProjectContextReader{},
		middleware.AuthConfig{
			PrincipalValidator:        productionProjectPrincipalValidator{},
			ForwardedIdentityVerifier: productionProjectPeerVerifier{},
		},
		productionPromptContextPermissionResolver{},
	)
	if err != nil {
		t.Fatal(err)
	}
	// mountReviewedProductionRoutes registers only the chat-config GET for
	// CurrentPromptContextReads. The current project-context GET/PUT is owned
	// unconditionally by newProductionRouter's broad coreHandler.ProjectContext
	// registration at the same literal path
	// (promptcontextreadsapi.CurrentProjectContextPath) even when
	// CurrentPromptContextReads is composed — see
	// internal/api/v2/promptcontextreads/CURRENT_PARITY_EVIDENCE.md and
	// mountReviewedProductionRoutes' doc comment — so asserting its behavior
	// belongs with the broad router's own tests, not this reviewed-surface
	// unit test.
	router := reviewedRoutesRouter(RouterConfig{CurrentPromptContextReads: routes})
	for _, test := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/api/v2/elitea_core/chat_config/prompt_lib/7", http.StatusOK},
		{http.MethodGet, "/api/v2/elitea_core/chat_config/default/7", http.StatusNotFound},
	} {
		request := httptest.NewRequest(test.method, test.path, nil)
		request.RemoteAddr = "10.0.0.8:43120"
		request.Header.Set("X-Auth-Type", "user")
		request.Header.Set("X-Auth-ID", "11")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Fatalf("%s %s status=%d want=%d body=%q", test.method, test.path, response.Code, test.want, response.Body.String())
		}
	}

	uncomposed := reviewedRoutesRouter(RouterConfig{})
	for _, path := range []string{
		"/api/v2/elitea_core/chat_config/prompt_lib/7",
	} {
		response := httptest.NewRecorder()
		uncomposed.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("uncomposed %s status=%d", path, response.Code)
		}
	}
}

func TestProductionAuthRoutesRequireBothReviewedEdges(t *testing.T) {
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for name, test := range map[string]struct {
		browser http.Handler
		main    http.Handler
	}{
		"missing browser": {main: handler},
		"missing main":    {browser: handler},
		"both missing":    {},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewProductionAuthRoutes(test.browser, test.main); !errors.Is(err, ErrInvalidProductionAuthRoutes) {
				t.Fatalf("error = %v, want %v", err, ErrInvalidProductionAuthRoutes)
			}
		})
	}
}

func TestProductionRouterMountsOnlyReviewedAuthEdges(t *testing.T) {
	browser := chi.NewRouter()
	browser.Get("/login", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	browser.Post("/auth_form/authorize", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	main := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	authRoutes, err := NewProductionAuthRoutes(browser, main)
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter(RouterConfig{ProductionAuth: authRoutes})

	for _, route := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/forward-auth/login", http.StatusNoContent},
		{http.MethodPost, "/forward-auth/auth_form/authorize", http.StatusNoContent},
		{http.MethodGet, "/internal/forward-auth/main", http.StatusNoContent},
		{http.MethodGet, "/forward-auth/auth", http.StatusNotFound},
		{http.MethodGet, "/forward-auth/info", http.StatusNotFound},
		{http.MethodPost, "/internal/forward-auth/main", http.StatusMethodNotAllowed},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(route.method, route.path, nil))
			if recorder.Code != route.want {
				t.Fatalf("status = %d, want %d", recorder.Code, route.want)
			}
		})
	}
}

func TestCompatibilityRouterRetainsReviewedProductionAuthEdges(t *testing.T) {
	browser := chi.NewRouter()
	browser.Get("/login", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	main := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	agentStart := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Reviewed-Agent-Route", "true")
		writer.WriteHeader(http.StatusAccepted)
	})
	authRoutes, err := NewProductionAuthRoutes(browser, main)
	if err != nil {
		t.Fatal(err)
	}

	// AppsRepo is representative of the current hybrid composition and forces
	// the compatibility router until that API has moved to production routes.
	router := NewRouter(RouterConfig{
		ProductionAuth:    authRoutes,
		CurrentAgentStart: agentStart,
		AppsRepo:          struct{ applications.Repository }{},
	})

	for _, route := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/forward-auth/login", http.StatusNoContent},
		{http.MethodGet, "/internal/forward-auth/main", http.StatusNoContent},
		{http.MethodPost, "/internal/forward-auth/main", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/v2/elitea_core/messages/prompt_lib/2/conversation", http.StatusAccepted},
		{http.MethodPost, "/api/v2/elitea_core/regenerate/prompt_lib/2/message", http.StatusAccepted},
		{http.MethodPost, "/api/v2/elitea_core/continue_predict/prompt_lib/2/conversation", http.StatusAccepted},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(route.method, route.path, nil))
			if recorder.Code != route.want {
				t.Fatalf("status = %d, want %d", recorder.Code, route.want)
			}
			if route.want == http.StatusAccepted && recorder.Header().Get("X-Reviewed-Agent-Route") != "true" {
				t.Fatal("request did not reach the reviewed current-agent route")
			}
		})
	}
}

type productionProjectStore struct{}

func (productionProjectStore) ListCurrentUserProjects(
	context.Context,
	sqlcgen.ListCurrentUserProjectsParams,
) ([]sqlcgen.ListCurrentUserProjectsRow, error) {
	return []sqlcgen.ListCurrentUserProjectsRow{}, nil
}

type productionProjectPrincipalValidator struct{}

func (productionProjectPrincipalValidator) ValidatePrincipal(_ context.Context, user auth.User) (auth.User, error) {
	return user, nil
}

type productionProjectPeerVerifier struct{}

func (productionProjectPeerVerifier) VerifyForwardedIdentityPeer(*http.Request) error { return nil }

type productionProjectPermissionResolver struct{}

func (productionProjectPermissionResolver) ResolvePermissions(
	context.Context,
	auth.User,
	string,
	string,
) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{}, nil
}

func TestProductionRouterMountsOnlyExactCurrentProjectListPath(t *testing.T) {
	projectList, err := v2projects.NewCurrentProjectListRoute(
		productionProjectStore{},
		middleware.AuthConfig{
			PrincipalValidator:        productionProjectPrincipalValidator{},
			ForwardedIdentityVerifier: productionProjectPeerVerifier{},
		},
		productionProjectPermissionResolver{},
	)
	if err != nil {
		t.Fatal(err)
	}
	router := reviewedRoutesRouter(RouterConfig{CurrentProjectList: projectList})

	for _, test := range []struct {
		method string
		path   string
		want   int
	}{
		{method: http.MethodGet, path: v2projects.CurrentProjectListPath, want: http.StatusUnauthorized},
		{method: http.MethodPost, path: v2projects.CurrentProjectListPath, want: http.StatusMethodNotAllowed},
		{method: http.MethodGet, path: "/projects/project/default/2", want: http.StatusNotFound},
		{method: http.MethodGet, path: "/projects/project/prompt_lib/1", want: http.StatusNotFound},
		{method: http.MethodGet, path: "/projects/project/default/1", want: http.StatusNotFound},
		{method: http.MethodGet, path: "/api/v2/projects/project/default/2", want: http.StatusNotFound},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(test.method, test.path, nil))
			if recorder.Code != test.want {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, test.want, recorder.Body.String())
			}
		})
	}
}

type productionSocialAuthorsReader struct {
	calls int
}

func (reader *productionSocialAuthorsReader) ListCurrentProjectAuthors(
	context.Context,
	int32,
) ([]socialapp.CurrentAuthor, error) {
	reader.calls++
	return []socialapp.CurrentAuthor{}, nil
}

type productionSocialAuthorsPermissionResolver struct{}

func (productionSocialAuthorsPermissionResolver) ResolvePermissions(
	context.Context,
	auth.User,
	string,
	string,
) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{
		UserID:      41,
		Permissions: []string{v2social.CurrentAuthorsPermission},
	}, nil
}

func TestProductionRouterMountsOnlyExactCurrentSocialAuthorsPaths(t *testing.T) {
	reader := &productionSocialAuthorsReader{}
	authors, err := v2social.NewCurrentAuthorsRoute(
		reader,
		middleware.AuthConfig{
			PrincipalValidator:        productionProjectPrincipalValidator{},
			ForwardedIdentityVerifier: productionProjectPeerVerifier{},
		},
		productionSocialAuthorsPermissionResolver{},
	)
	if err != nil {
		t.Fatal(err)
	}
	router := reviewedRoutesRouter(RouterConfig{CurrentSocialAuthors: authors})

	for _, target := range []string{
		"/api/v2/social/authors/7",
		"/api/v2/social/authors/default/7?limit=5&sort_by=name",
	} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.RemoteAddr = "10.0.0.8:43120"
		request.Header.Set("X-Auth-Type", "user")
		request.Header.Set("X-Auth-ID", "41")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || recorder.Body.String() != "[]\n" {
			t.Fatalf("GET %s status=%d body=%q", target, recorder.Code, recorder.Body.String())
		}
	}
	if reader.calls != 2 {
		t.Fatalf("reader calls=%d want=2", reader.calls)
	}

	for _, test := range []struct {
		method string
		path   string
		want   int
	}{
		{method: http.MethodPost, path: "/api/v2/social/authors/7", want: http.StatusMethodNotAllowed},
		{method: http.MethodGet, path: "/api/v2/social/authors/administration/7", want: http.StatusNotFound},
		{method: http.MethodGet, path: "/api/v2/social/authors/7/extra", want: http.StatusNotFound},
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(test.method, test.path, nil))
		if recorder.Code != test.want {
			t.Fatalf("%s %s status=%d want=%d", test.method, test.path, recorder.Code, test.want)
		}
	}

	unmounted := reviewedRoutesRouter(RouterConfig{})
	recorder := httptest.NewRecorder()
	unmounted.ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodGet, "/api/v2/social/authors/7", nil),
	)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("uncomposed status=%d", recorder.Code)
	}
}

type productionProjectInfoReader struct {
	calls     int
	projectID int32
}

func (reader *productionProjectInfoReader) GetCurrentProjectInfo(
	_ context.Context,
	projectID int32,
) (projectinfoapi.CurrentProjectInfo, error) {
	reader.calls++
	reader.projectID = projectID
	return projectinfoapi.CurrentProjectInfo{
		TeammatesCount: 2,
		IconMeta:       []byte(`{"url":"/project-icons/seven.svg"}`),
	}, nil
}

type productionProjectInfoPermissionResolver struct{}

func (productionProjectInfoPermissionResolver) ResolvePermissions(
	_ context.Context,
	user auth.User,
	mode string,
	projectID string,
) (auth.PermissionResolution, error) {
	if user.UserID != "11" || mode != projectinfoapi.CurrentProjectInfoMode ||
		projectID != "7" {
		return auth.PermissionResolution{}, errors.New("unexpected project-info authorization input")
	}
	return auth.PermissionResolution{
		UserID:      11,
		Permissions: []string{projectinfoapi.CurrentProjectInfoPermission},
	}, nil
}

func TestProductionRouterMountsOnlyExactCurrentProjectInfoPathWhenComposed(t *testing.T) {
	reader := &productionProjectInfoReader{}
	projectInfo, err := projectinfoapi.NewCurrentProjectInfoRoute(
		reader,
		middleware.AuthConfig{
			PrincipalValidator:        productionProjectPrincipalValidator{},
			ForwardedIdentityVerifier: productionProjectPeerVerifier{},
		},
		productionProjectInfoPermissionResolver{},
	)
	if err != nil {
		t.Fatal(err)
	}
	router := reviewedRoutesRouter(RouterConfig{CurrentProjectInfo: projectInfo})

	for _, test := range []struct {
		method        string
		path          string
		authenticated bool
		want          int
	}{
		{
			method:        http.MethodGet,
			path:          "/api/v2/elitea_core/project_info/prompt_lib/007/project-info",
			authenticated: true,
			want:          http.StatusOK,
		},
		{
			method: http.MethodGet,
			path:   "/api/v2/elitea_core/project_info/prompt_lib/7/project-info",
			want:   http.StatusUnauthorized,
		},
		{
			method: http.MethodPut,
			path:   "/api/v2/elitea_core/project_info/prompt_lib/7/project-info",
			want:   http.StatusMethodNotAllowed,
		},
		{
			method: http.MethodGet,
			path:   "/api/v2/elitea_core/project_info/default/7/project-info",
			want:   http.StatusNotFound,
		},
		{
			method: http.MethodGet,
			path:   "/api/v2/elitea_core/project_info/prompt_lib/7/project-info/extra",
			want:   http.StatusNotFound,
		},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(test.method, test.path, nil)
			if test.authenticated {
				request.Header.Set("X-Auth-Type", "user")
				request.Header.Set("X-Auth-ID", "11")
			}
			router.ServeHTTP(recorder, request)
			if recorder.Code != test.want {
				t.Fatalf(
					"status = %d, want %d; body=%s",
					recorder.Code,
					test.want,
					recorder.Body.String(),
				)
			}
			if test.want == http.StatusOK &&
				recorder.Body.String() !=
					"{\"teammates_count\":2,\"icon_meta\":{\"url\":\"/project-icons/seven.svg\"}}\n" {
				t.Fatalf("successful body=%q", recorder.Body.String())
			}
		})
	}
	if reader.calls != 1 || reader.projectID != 7 {
		t.Fatalf("reader calls=%d project=%d", reader.calls, reader.projectID)
	}

	uncomposed := reviewedRoutesRouter(RouterConfig{})
	recorder := httptest.NewRecorder()
	uncomposed.ServeHTTP(
		recorder,
		httptest.NewRequest(
			http.MethodGet,
			"/api/v2/elitea_core/project_info/prompt_lib/7/project-info",
			nil,
		),
	)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("uncomposed project-info status=%d", recorder.Code)
	}
}

type productionIndexTypesReader struct {
	calls     int
	projectID int32
}

func (reader *productionIndexTypesReader) GetCurrentIndexTypes(
	_ context.Context,
	projectID int32,
) (indextypesapi.CurrentIndexTypes, error) {
	reader.calls++
	reader.projectID = projectID
	return indextypesapi.CurrentIndexTypes{
		DocumentTypes: map[string]string{".txt": "text/plain"},
		ImageTypes:    map[string]string{".png": "image/png"},
		CodeTypes:     map[string]string{".go": "text/x-go"},
	}, nil
}

type productionIndexTypesPermissionResolver struct{}

func (productionIndexTypesPermissionResolver) ResolvePermissions(
	_ context.Context,
	user auth.User,
	mode string,
	projectID string,
) (auth.PermissionResolution, error) {
	if user.UserID != "11" || mode != indextypesapi.CurrentIndexTypesMode ||
		projectID != "7" {
		return auth.PermissionResolution{}, errors.New("unexpected index-types authorization input")
	}
	return auth.PermissionResolution{
		UserID:      11,
		Permissions: []string{indextypesapi.CurrentIndexTypesPermission},
	}, nil
}

func TestProductionRouterMountsOnlyExactCurrentIndexTypesPathWhenComposed(t *testing.T) {
	reader := &productionIndexTypesReader{}
	indexTypes, err := indextypesapi.NewCurrentIndexTypesRoute(
		reader,
		middleware.AuthConfig{
			PrincipalValidator:        productionProjectPrincipalValidator{},
			ForwardedIdentityVerifier: productionProjectPeerVerifier{},
		},
		productionIndexTypesPermissionResolver{},
	)
	if err != nil {
		t.Fatal(err)
	}
	router := reviewedRoutesRouter(RouterConfig{CurrentIndexTypes: indexTypes})

	for _, test := range []struct {
		method        string
		path          string
		authenticated bool
		want          int
	}{
		{
			method:        http.MethodGet,
			path:          "/api/v2/elitea_core/index_types/prompt_lib/007",
			authenticated: true,
			want:          http.StatusOK,
		},
		{
			method: http.MethodGet,
			path:   "/api/v2/elitea_core/index_types/prompt_lib/7",
			want:   http.StatusUnauthorized,
		},
		{
			method: http.MethodPost,
			path:   "/api/v2/elitea_core/index_types/prompt_lib/7",
			want:   http.StatusMethodNotAllowed,
		},
		{
			method: http.MethodGet,
			path:   "/api/v2/elitea_core/index_types/default/7",
			want:   http.StatusNotFound,
		},
		{
			method: http.MethodGet,
			path:   "/api/v2/elitea_core/index_types/prompt_lib/7/extra",
			want:   http.StatusNotFound,
		},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(test.method, test.path, nil)
			if test.authenticated {
				request.Header.Set("X-Auth-Type", "user")
				request.Header.Set("X-Auth-ID", "11")
			}
			router.ServeHTTP(recorder, request)
			if recorder.Code != test.want {
				t.Fatalf(
					"status = %d, want %d; body=%s",
					recorder.Code,
					test.want,
					recorder.Body.String(),
				)
			}
			// One body, both key sets (#394): the published
			// DocumentLoadersResponse keys the generated apps/elitea-web
			// client reads, beside the Pylon maps apps/elitea-ui reads. Each
			// item lists the extensions of the map its `type` names, so the
			// two halves cannot disagree.
			if test.want == http.StatusOK &&
				recorder.Body.String() !=
					"{\"items\":["+
						"{\"type\":\"document_types\",\"name\":\"Document types\","+
						"\"description\":\"File extensions the indexer loads as documents.\","+
						"\"supported_extensions\":[\".txt\"]},"+
						"{\"type\":\"image_types\",\"name\":\"Image types\","+
						"\"description\":\"File extensions the indexer loads as images.\","+
						"\"supported_extensions\":[\".png\"]},"+
						"{\"type\":\"code_types\",\"name\":\"Code types\","+
						"\"description\":\"File extensions the indexer loads as plain-text code.\","+
						"\"supported_extensions\":[\".go\"]}"+
						"],\"total\":3,"+
						"\"document_types\":{\".txt\":\"text/plain\"},"+
						"\"image_types\":{\".png\":\"image/png\"},"+
						"\"code_types\":{\".go\":\"text/x-go\"}}\n" {
				t.Fatalf("successful body=%q", recorder.Body.String())
			}
		})
	}
	if reader.calls != 1 || reader.projectID != 7 {
		t.Fatalf("reader calls=%d project=%d", reader.calls, reader.projectID)
	}

	uncomposed := reviewedRoutesRouter(RouterConfig{})
	recorder := httptest.NewRecorder()
	uncomposed.ServeHTTP(
		recorder,
		httptest.NewRequest(
			http.MethodGet,
			"/api/v2/elitea_core/index_types/prompt_lib/7",
			nil,
		),
	)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("uncomposed index-types status=%d", recorder.Code)
	}
}

type productionApplicationSkillsReader struct {
	calls        int
	projectID    int32
	appVersionID int32
}

func (reader *productionApplicationSkillsReader) ListCurrentApplicationSkills(
	_ context.Context,
	projectID int32,
	appVersionID int32,
) ([]applicationskillsapi.CurrentApplicationSkill, error) {
	reader.calls++
	reader.projectID = projectID
	reader.appVersionID = appVersionID
	versionID := int32(19)
	return []applicationskillsapi.CurrentApplicationSkill{
		{
			Name:           "deploy",
			Description:    "Deploy safely",
			SkillID:        17,
			VersionID:      &versionID,
			VersionName:    "release",
			VersionMissing: false,
		},
	}, nil
}

type productionApplicationSkillsPermissionResolver struct{}

func (productionApplicationSkillsPermissionResolver) ResolvePermissions(
	_ context.Context,
	user auth.User,
	mode string,
	projectID string,
) (auth.PermissionResolution, error) {
	if user.UserID != "11" ||
		mode != applicationskillsapi.CurrentApplicationSkillsMode ||
		projectID != "7" {
		return auth.PermissionResolution{}, errors.New(
			"unexpected application-skills authorization input",
		)
	}
	return auth.PermissionResolution{
		UserID: 11,
		Permissions: []string{
			applicationskillsapi.CurrentApplicationSkillsPermission,
		},
	}, nil
}

func TestProductionRouterMountsOnlyExactCurrentApplicationSkillsPathWhenComposed(
	t *testing.T,
) {
	reader := &productionApplicationSkillsReader{}
	route, err := applicationskillsapi.NewCurrentApplicationSkillsRoute(
		reader,
		middleware.AuthConfig{
			PrincipalValidator:        productionProjectPrincipalValidator{},
			ForwardedIdentityVerifier: productionProjectPeerVerifier{},
		},
		productionApplicationSkillsPermissionResolver{},
	)
	if err != nil {
		t.Fatal(err)
	}
	router := reviewedRoutesRouter(RouterConfig{CurrentApplicationSkills: route})

	for _, test := range []struct {
		method        string
		path          string
		authenticated bool
		want          int
		wantBody      string
	}{
		{
			method:        http.MethodGet,
			path:          "/api/v2/elitea_core/application_skills/prompt_lib/007/0031",
			authenticated: true,
			want:          http.StatusOK,
			// One body, both clients (#395): `items` is the published
			// SkillsList contract apps/elitea-web reads, `skills` and
			// `max_skills` are the Pylon keys apps/elitea-ui reads.
			wantBody: "{\"items\":[{\"id\":\"17\",\"project_id\":\"7\",\"name\":\"deploy\"," +
				"\"description\":\"Deploy safely\",\"type\":\"skill\",\"is_default\":false," +
				"\"created_at\":\"0001-01-01T00:00:00Z\",\"updated_at\":\"0001-01-01T00:00:00Z\"}]," +
				"\"total\":1,\"page\":1,\"page_size\":1,\"total_pages\":1," +
				"\"skills\":[{\"name\":\"deploy\",\"description\":\"Deploy safely\",\"skill_id\":17," +
				"\"version_id\":19,\"version_name\":\"release\",\"version_missing\":false," +
				"\"icon_meta\":null}],\"max_skills\":5}\n",
		},
		{
			method: http.MethodGet,
			path:   "/api/v2/elitea_core/application_skills/prompt_lib/7/31",
			want:   http.StatusUnauthorized,
		},
		{
			method: http.MethodPost,
			path:   "/api/v2/elitea_core/application_skills/prompt_lib/7/31",
			want:   http.StatusMethodNotAllowed,
		},
		{
			method:        http.MethodGet,
			path:          "/api/v2/elitea_core/application_skills/prompt_lib/7/not-an-id",
			authenticated: true,
			want:          http.StatusNotFound,
			wantBody:      "{\"message\":\"The requested URL was not found on the server. If you entered the URL manually please check your spelling and try again.\"}\n",
		},
		{
			method:        http.MethodGet,
			path:          "/api/v2/elitea_core/application_skills/prompt_lib/7/0",
			authenticated: true,
			want:          http.StatusOK,
			wantBody:      "{\"items\":[],\"total\":0,\"page\":1,\"page_size\":0,\"total_pages\":0,\"skills\":[],\"max_skills\":5}\n",
		},
		{
			method:        http.MethodGet,
			path:          "/api/v2/elitea_core/application_skills/prompt_lib/7/9999999999999999999999999999999999999999",
			authenticated: true,
			want:          http.StatusOK,
			wantBody:      "{\"items\":[],\"total\":0,\"page\":1,\"page_size\":0,\"total_pages\":0,\"skills\":[],\"max_skills\":5}\n",
		},
		{
			method: http.MethodGet,
			path:   "/api/v2/elitea_core/application_skills/default/7/31",
			want:   http.StatusNotFound,
		},
		{
			method: http.MethodGet,
			path:   "/api/v2/elitea_core/application_skills/prompt_lib/7/31/extra",
			want:   http.StatusNotFound,
		},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(test.method, test.path, nil)
			if test.authenticated {
				request.Header.Set("X-Auth-Type", "user")
				request.Header.Set("X-Auth-ID", "11")
			}
			router.ServeHTTP(recorder, request)
			if recorder.Code != test.want {
				t.Fatalf(
					"status = %d, want %d; body=%s",
					recorder.Code,
					test.want,
					recorder.Body.String(),
				)
			}
			if test.wantBody != "" && recorder.Body.String() != test.wantBody {
				t.Fatalf(
					"body=%q want=%q",
					recorder.Body.String(),
					test.wantBody,
				)
			}
		})
	}
	if reader.calls != 1 || reader.projectID != 7 || reader.appVersionID != 31 {
		t.Fatalf(
			"reader calls=%d project=%d app_version=%d",
			reader.calls,
			reader.projectID,
			reader.appVersionID,
		)
	}

	uncomposed := reviewedRoutesRouter(RouterConfig{})
	recorder := httptest.NewRecorder()
	uncomposed.ServeHTTP(
		recorder,
		httptest.NewRequest(
			http.MethodGet,
			"/api/v2/elitea_core/application_skills/prompt_lib/7/31",
			nil,
		),
	)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("uncomposed application-skills status=%d", recorder.Code)
	}
}

func TestProductionRouterMountsOnlyExactCurrentIndexStartPathWhenComposed(t *testing.T) {
	calls := 0
	indexStart := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if projectID := chi.URLParam(request, "projectID"); projectID != "7" {
			t.Fatalf("projectID = %q, want 7", projectID)
		}
		writer.WriteHeader(http.StatusNoContent)
	})
	router := reviewedRoutesRouter(RouterConfig{CurrentIndexStart: indexStart})

	for _, test := range []struct {
		method string
		path   string
		want   int
	}{
		{method: http.MethodPost, path: "/api/v2/elitea_core/test_toolkit_tool/prompt_lib/7?await_response=false", want: http.StatusNoContent},
		{method: http.MethodGet, path: "/api/v2/elitea_core/test_toolkit_tool/prompt_lib/7", want: http.StatusMethodNotAllowed},
		{method: http.MethodPost, path: "/api/v2/elitea_core/test_toolkit_tool/prompt_lib/7/extra", want: http.StatusNotFound},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(test.method, test.path, nil))
			if recorder.Code != test.want {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, test.want, recorder.Body.String())
			}
		})
	}
	if calls != 1 {
		t.Fatalf("index start calls = %d, want 1", calls)
	}
	if indexingapi.CurrentIndexStartPath != "/api/v2/elitea_core/test_toolkit_tool/prompt_lib/{projectID}" {
		t.Fatalf("current index path drifted: %s", indexingapi.CurrentIndexStartPath)
	}
}

func TestProductionRouterMountsOnlyExactCurrentIndexCancelPathWhenComposed(t *testing.T) {
	calls := 0
	indexCancel := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		for parameter, want := range map[string]string{
			"projectID": "7",
			"toolkitID": "9",
			"indexName": "docs",
			"taskID":    "0123456789abcdef0123456789abcdef",
		} {
			if got := chi.URLParam(request, parameter); got != want {
				t.Fatalf("%s = %q, want %q", parameter, got, want)
			}
		}
		writer.WriteHeader(http.StatusNoContent)
	})
	router := reviewedRoutesRouter(RouterConfig{CurrentIndexCancel: indexCancel})

	for _, test := range []struct {
		method string
		path   string
		want   int
	}{
		{
			method: http.MethodDelete,
			path:   "/api/v2/elitea_core/index_cancel/prompt_lib/7/9/docs/0123456789abcdef0123456789abcdef",
			want:   http.StatusNoContent,
		},
		{
			method: http.MethodPost,
			path:   "/api/v2/elitea_core/index_cancel/prompt_lib/7/9/docs/0123456789abcdef0123456789abcdef",
			want:   http.StatusMethodNotAllowed,
		},
		{
			method: http.MethodDelete,
			path:   "/api/v2/elitea_core/index_cancel/prompt_lib/7/9/docs/0123456789abcdef0123456789abcdef/extra",
			want:   http.StatusNotFound,
		},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(test.method, test.path, nil))
			if recorder.Code != test.want {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, test.want, recorder.Body.String())
			}
		})
	}
	if calls != 1 {
		t.Fatalf("index cancel calls = %d, want 1", calls)
	}
	if indexingapi.CurrentIndexCancelPath != "/api/v2/elitea_core/index_cancel/prompt_lib/{projectID}/{toolkitID}/{indexName}/{taskID}" {
		t.Fatalf("current index cancel path drifted: %s", indexingapi.CurrentIndexCancelPath)
	}
}

func TestProductionRouterMountsCurrentModelAndExternalIndexMetaPaths(t *testing.T) {
	modelCalls := 0
	modelDefaultCalls := 0
	indexMetaCalls := 0
	router := reviewedRoutesRouter(RouterConfig{
		CurrentModelCatalog: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			modelCalls++
			if chi.URLParam(request, "projectID") != "7" {
				t.Fatalf("model project id = %q", chi.URLParam(request, "projectID"))
			}
			writer.WriteHeader(http.StatusNoContent)
		}),
		CurrentModelDefault: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			modelDefaultCalls++
			if chi.URLParam(request, "projectID") != "7" {
				t.Fatalf("model-default project id = %q", chi.URLParam(request, "projectID"))
			}
			writer.WriteHeader(http.StatusNoContent)
		}),
		CurrentIndexMeta: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			indexMetaCalls++
			if chi.URLParam(request, "projectID") != "7" || chi.URLParam(request, "toolkitID") != "9" {
				t.Fatalf("index meta params project=%q toolkit=%q", chi.URLParam(request, "projectID"), chi.URLParam(request, "toolkitID"))
			}
			writer.WriteHeader(http.StatusNoContent)
		}),
	})

	for _, target := range []string{
		"/api/v2/configurations/models/7?section=embedding&include_shared=true",
		"/api/v2/elitea_core/index_meta/prompt_lib/7/9",
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, testAuthHeader(httptest.NewRequest(http.MethodGet, target, nil)))
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("GET %s status=%d body=%s", target, recorder.Code, recorder.Body.String())
		}
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v2/configurations/models/7", strings.NewReader(`{"name":"gpt","target_project_id":7}`)))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("POST model default status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if modelCalls != 1 || modelDefaultCalls != 1 || indexMetaCalls != 1 {
		t.Fatalf("model calls=%d model-default calls=%d index-meta calls=%d", modelCalls, modelDefaultCalls, indexMetaCalls)
	}

	unmounted := reviewedRoutesRouter(RouterConfig{})
	for _, target := range []string{
		"/api/v2/configurations/models/7",
		"/api/v2/elitea_core/index_meta/prompt_lib/7/9",
	} {
		recorder := httptest.NewRecorder()
		unmounted.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("uncomposed GET %s status=%d", target, recorder.Code)
		}
	}
	recorder = httptest.NewRecorder()
	unmounted.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v2/configurations/models/7", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("uncomposed POST model-default status=%d", recorder.Code)
	}
}

func TestProductionRouterKeepsUncomposedIndexDeleteScheduleAndSearchSourceOnly(t *testing.T) {
	calls := 0
	current := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls++
	})
	router := reviewedRoutesRouter(RouterConfig{
		CurrentIndexStart:  current,
		CurrentIndexCancel: current,
		CurrentIndexMeta:   current,
	})

	for _, test := range []struct {
		name   string
		method string
		path   string
	}{
		{
			name:   "delete",
			method: indexingapi.SourceOnlyIndexDeleteMethod,
			path:   "/api/v2/elitea_core/index_meta/prompt_lib/7/9/meta-1",
		},
		{
			name:   "schedule",
			method: indexingapi.SourceOnlyIndexScheduleMethod,
			path:   "/api/v2/elitea_core/index_meta/prompt_lib/7/9/meta-1",
		},
		{
			name:   "schedule delete",
			method: indexingapi.SourceOnlyIndexScheduleDeleteMethod,
			path:   "/api/v2/elitea_core/index_schedule/prompt_lib/7/9/meta-1",
		},
		{
			name:   "search",
			method: indexingapi.SourceOnlyIndexSearchMethod,
			path:   "/api/v2/elitea_core/search_options/prompt_lib/7?entities%5B%5D=toolkit",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			router.ServeHTTP(
				response,
				httptest.NewRequest(test.method, test.path, nil),
			)
			if response.Code != http.StatusNotFound {
				t.Fatalf(
					"%s %s status=%d want=%d body=%s",
					test.method,
					test.path,
					response.Code,
					http.StatusNotFound,
					response.Body.String(),
				)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("source-only index routes reached mounted handlers %d times", calls)
	}
}

func TestProductionRouterMountsCurrentIndexScheduleCRUDOnlyWhenComposed(
	t *testing.T,
) {
	calls := make(map[string]int)
	handler := func(name string) http.Handler {
		return http.HandlerFunc(
			func(writer http.ResponseWriter, request *http.Request) {
				calls[name]++
				if chi.URLParam(request, "projectID") != "7" ||
					chi.URLParam(request, "toolkitID") != "9" ||
					chi.URLParam(request, "indexMetaID") != "meta-1" {
					t.Fatalf(
						"%s params project=%q toolkit=%q index=%q",
						name,
						chi.URLParam(request, "projectID"),
						chi.URLParam(request, "toolkitID"),
						chi.URLParam(request, "indexMetaID"),
					)
				}
				writer.WriteHeader(http.StatusNoContent)
			},
		)
	}
	router := reviewedRoutesRouter(RouterConfig{
		CurrentIndexScheduleUpdate: handler("update"),
		CurrentIndexScheduleDelete: handler("delete"),
	})
	for _, test := range []struct {
		name   string
		method string
		path   string
	}{
		{
			name:   "update",
			method: http.MethodPatch,
			path:   "/api/v2/elitea_core/index_meta/prompt_lib/7/9/meta-1",
		},
		{
			name:   "delete",
			method: http.MethodDelete,
			path:   "/api/v2/elitea_core/index_schedule/prompt_lib/7/9/meta-1",
		},
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(
			response,
			httptest.NewRequest(test.method, test.path, nil),
		)
		if response.Code != http.StatusNoContent || calls[test.name] != 1 {
			t.Fatalf(
				"%s status=%d calls=%d body=%s",
				test.name,
				response.Code,
				calls[test.name],
				response.Body.String(),
			)
		}
	}

	uncomposed := reviewedRoutesRouter(RouterConfig{})
	for _, test := range []struct {
		method string
		path   string
	}{
		{
			method: http.MethodPatch,
			path:   "/api/v2/elitea_core/index_meta/prompt_lib/7/9/meta-1",
		},
		{
			method: http.MethodDelete,
			path:   "/api/v2/elitea_core/index_schedule/prompt_lib/7/9/meta-1",
		},
	} {
		response := httptest.NewRecorder()
		uncomposed.ServeHTTP(
			response,
			httptest.NewRequest(test.method, test.path, nil),
		)
		if response.Code != http.StatusNotFound {
			t.Fatalf(
				"uncomposed %s %s status=%d",
				test.method,
				test.path,
				response.Code,
			)
		}
	}
}

func TestProductionRouterMountsOnlyCurrentNotificationEventsGETWhenComposed(
	t *testing.T,
) {
	target := strings.Replace(
		notificationsapi.CurrentNotificationEventsPath,
		"{projectID}",
		"7",
		1,
	)
	calls := 0
	events := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if chi.URLParam(request, "projectID") != "7" {
			t.Fatalf("projectID = %q, want 7", chi.URLParam(request, "projectID"))
		}
		writer.WriteHeader(http.StatusNoContent)
	})
	router := reviewedRoutesRouter(RouterConfig{CurrentNotificationEvents: events})

	for _, test := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, target, http.StatusNoContent},
		{http.MethodPost, target, http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/v2/notifications/events/default/7", http.StatusNotFound},
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
		if response.Code != test.want {
			t.Fatalf(
				"%s %s status=%d want=%d body=%s",
				test.method,
				test.path,
				response.Code,
				test.want,
				response.Body.String(),
			)
		}
	}
	if calls != 1 {
		t.Fatalf("notification events handler calls = %d, want 1", calls)
	}

	uncomposed := reviewedRoutesRouter(RouterConfig{})
	response := httptest.NewRecorder()
	uncomposed.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, target, nil),
	)
	if response.Code != http.StatusNotFound {
		t.Fatalf("uncomposed status=%d want=%d", response.Code, http.StatusNotFound)
	}
}

func TestProductionRouterMountsOnlyCurrentNotificationAPIMethodsWhenComposed(t *testing.T) {
	calls := 0
	notifications := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if chi.URLParam(request, "projectID") != "7" {
			t.Fatalf("projectID = %q, want 7", chi.URLParam(request, "projectID"))
		}
		writer.WriteHeader(http.StatusNoContent)
	})
	router := reviewedRoutesRouter(RouterConfig{CurrentNotifications: notifications})

	tests := []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/api/v2/notifications/notifications/prompt_lib/7", http.StatusNoContent},
		{http.MethodPut, "/api/v2/notifications/notifications/prompt_lib/7", http.StatusNoContent},
		{http.MethodDelete, "/api/v2/notifications/notifications/prompt_lib/7", http.StatusNoContent},
		{http.MethodGet, "/api/v2/notifications/notification/prompt_lib/7/15", http.StatusNoContent},
		{http.MethodPut, "/api/v2/notifications/notification/prompt_lib/7/15", http.StatusNoContent},
		{http.MethodDelete, "/api/v2/notifications/notification/prompt_lib/7/15", http.StatusNoContent},
		{http.MethodPost, "/api/v2/notifications/notifications/prompt_lib/7", http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/v2/notifications/notification/default/7/15", http.StatusNotFound},
	}
	for _, test := range tests {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
		if response.Code != test.want {
			t.Fatalf("%s %s status=%d want=%d body=%s", test.method, test.path, response.Code, test.want, response.Body.String())
		}
	}
	if calls != 6 {
		t.Fatalf("notification API handler calls = %d, want 6", calls)
	}

	uncomposed := reviewedRoutesRouter(RouterConfig{})
	response := httptest.NewRecorder()
	uncomposed.ServeHTTP(response, httptest.NewRequest(
		http.MethodGet, "/api/v2/notifications/notifications/prompt_lib/7", nil,
	))
	if response.Code != http.StatusNotFound {
		t.Fatalf("uncomposed status=%d want=%d", response.Code, http.StatusNotFound)
	}
}

func TestProductionRouterMountsCurrentIndexMetaDeleteOnlyWhenComposed(t *testing.T) {
	const target = "/api/v2/elitea_core/index_meta/prompt_lib/7/9/meta-1"
	calls := 0
	router := reviewedRoutesRouter(RouterConfig{
		CurrentIndexMetaDelete: http.HandlerFunc(
			func(writer http.ResponseWriter, request *http.Request) {
				calls++
				if request.Method != http.MethodDelete ||
					chi.URLParam(request, "projectID") != "7" ||
					chi.URLParam(request, "toolkitID") != "9" ||
					chi.URLParam(request, "indexMetaID") != "meta-1" {
					t.Fatalf(
						"method=%s project=%q toolkit=%q index_meta=%q",
						request.Method,
						chi.URLParam(request, "projectID"),
						chi.URLParam(request, "toolkitID"),
						chi.URLParam(request, "indexMetaID"),
					)
				}
				writer.WriteHeader(http.StatusNoContent)
			},
		),
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodDelete, target, nil),
	)
	if response.Code != http.StatusNoContent || calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, calls, response.Body.String())
	}

	unmounted := httptest.NewRecorder()
	reviewedRoutesRouter(RouterConfig{}).ServeHTTP(
		unmounted,
		httptest.NewRequest(http.MethodDelete, target, nil),
	)
	if unmounted.Code != http.StatusNotFound {
		t.Fatalf("unmounted status=%d body=%s", unmounted.Code, unmounted.Body.String())
	}
}

func TestProductionRouterMountsOnlyCurrentConfigurationReadMethods(t *testing.T) {
	calls := 0
	reader := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		writer.WriteHeader(http.StatusNoContent)
	})
	router := reviewedRoutesRouter(RouterConfig{CurrentConfigurationRead: reader})
	for _, target := range []string{
		"/api/v2/configurations/configurations/7?include_shared=true",
		"/api/v2/configurations/configuration/7/11",
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, testAuthHeader(httptest.NewRequest(http.MethodGet, target, nil)))
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("GET %s status=%d body=%s", target, recorder.Code, recorder.Body.String())
		}
	}
	for _, target := range []string{
		"/api/v2/configurations/configurations/7",
		"/api/v2/configurations/configuration/7/11",
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, target, nil))
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s status=%d body=%s", target, recorder.Code, recorder.Body.String())
		}
	}
	if calls != 2 {
		t.Fatalf("read calls=%d", calls)
	}
}

func TestProductionRouterMountsOnlyCurrentConfigurationTypesMethod(t *testing.T) {
	calls := 0
	types := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if chi.URLParam(request, "projectID") != "7" {
			t.Fatalf("projectID=%q", chi.URLParam(request, "projectID"))
		}
		writer.WriteHeader(http.StatusNoContent)
	})
	router := reviewedRoutesRouter(RouterConfig{CurrentConfigurationTypes: types})

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(
		http.MethodGet,
		"/api/v2/configurations/types/7?section=credentials",
		nil,
	))
	if response.Code != http.StatusNoContent || calls != 1 {
		t.Fatalf("GET status=%d calls=%d body=%s", response.Code, calls, response.Body.String())
	}

	for _, test := range []struct {
		method string
		path   string
		want   int
	}{
		{method: http.MethodPost, path: "/api/v2/configurations/types/7", want: http.StatusMethodNotAllowed},
		{method: http.MethodGet, path: "/api/v2/configurations/types/7/extra", want: http.StatusNotFound},
	} {
		response = httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
		if response.Code != test.want {
			t.Fatalf("%s %s status=%d, want %d", test.method, test.path, response.Code, test.want)
		}
	}

	unmounted := reviewedRoutesRouter(RouterConfig{})
	response = httptest.NewRecorder()
	unmounted.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v2/configurations/types/7", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("uncomposed status=%d", response.Code)
	}
}

func TestProductionRouterMountsOnlyCurrentConfigurationMutationMethods(t *testing.T) {
	calls := make(map[string]int)
	mutation := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls[request.Method+" "+request.URL.Path]++
		writer.WriteHeader(http.StatusNoContent)
	})
	router := reviewedRoutesRouter(RouterConfig{CurrentConfigurationMutation: mutation})
	for _, target := range []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: "/api/v2/configurations/configurations/7"},
		{method: http.MethodPut, path: "/api/v2/configurations/configuration/7/11"},
		{method: http.MethodDelete, path: "/api/v2/configurations/configuration/7/11"},
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(target.method, target.path, nil))
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("%s %s status=%d body=%s", target.method, target.path, recorder.Code, recorder.Body.String())
		}
		if calls[target.method+" "+target.path] != 1 {
			t.Fatalf("%s %s calls=%d", target.method, target.path, calls[target.method+" "+target.path])
		}
	}

	for _, target := range []struct {
		method string
		path   string
		want   int
	}{
		{method: http.MethodGet, path: "/api/v2/configurations/configurations/7", want: http.StatusMethodNotAllowed},
		{method: http.MethodPatch, path: "/api/v2/configurations/configuration/7/11", want: http.StatusMethodNotAllowed},
		{method: http.MethodPost, path: "/api/v2/configurations/configuration/7/11", want: http.StatusMethodNotAllowed},
		{method: http.MethodPost, path: "/api/v2/configurations/configurations/7/extra", want: http.StatusNotFound},
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(target.method, target.path, nil))
		if recorder.Code != target.want {
			t.Fatalf("%s %s status=%d, want %d", target.method, target.path, recorder.Code, target.want)
		}
	}
}

func TestProductionRouterComposesConfigurationReadAndMutationOnSameCurrentPaths(t *testing.T) {
	reader := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	})
	mutation := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	router := NewRouter(RouterConfig{
		CurrentConfigurationRead:     reader,
		CurrentConfigurationMutation: mutation,
	})
	for _, target := range []struct {
		method string
		path   string
		want   int
	}{
		{method: http.MethodGet, path: "/api/v2/configurations/configurations/7", want: http.StatusOK},
		{method: http.MethodGet, path: "/api/v2/configurations/configuration/7/11", want: http.StatusOK},
		{method: http.MethodPost, path: "/api/v2/configurations/configurations/7", want: http.StatusNoContent},
		{method: http.MethodPut, path: "/api/v2/configurations/configuration/7/11", want: http.StatusNoContent},
		{method: http.MethodDelete, path: "/api/v2/configurations/configuration/7/11", want: http.StatusNoContent},
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(target.method, target.path, nil))
		if recorder.Code != target.want {
			t.Fatalf("%s %s status=%d, want %d", target.method, target.path, recorder.Code, target.want)
		}
	}
}

func TestProductionRouterMountsCurrentAvailableAliasesAsGetOnly(t *testing.T) {
	calls := 0
	available := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		writer.WriteHeader(http.StatusNoContent)
	})
	router := reviewedRoutesRouter(RouterConfig{CurrentConfigurationAvailable: available})
	for _, target := range []string{
		configurationapi.CurrentAvailablePath,
		configurationapi.CurrentAvailableSlashPath,
		"/api/v2/configurations/available/7?section=credentials",
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, testAuthHeader(httptest.NewRequest(http.MethodGet, target, nil)))
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("GET %s status=%d body=%s", target, recorder.Code, recorder.Body.String())
		}
	}
	if calls != 3 {
		t.Fatalf("available calls=%d", calls)
	}

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, configurationapi.CurrentAvailableSlashPath, nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST available status=%d", recorder.Code)
	}
}

// The /llm routing contract after the LiteLLM removal.
//
// This replaces TestProductionRouterMountsCurrentLLMFacadeWithoutStrippingContractPath,
// which pinned the opposite contract: a bare RouterConfig carrying only a
// CurrentLLMFacade served /llm from that facade. There is no facade and no
// last-resort arm any more — the Bifrost gateway is the only backend.
//
// Issue #463 changed HOW the uncomposed state answers, not WHAT it serves. The
// path is now registered and answers 503 llm_gateway_not_configured. So the
// discriminating signal can no longer be "no /llm pattern exists": it is the
// not-configured code in the body, which no real backend would ever produce.
// A reintroduced fallback turns this test from that code into a served
// response, exactly as the 404 assertion used to.
func TestProductionRouterLLMRouteHasNoLastResortBackend(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewRouter(RouterConfig{}).ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`)),
	)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("uncomposed /llm status=%d body=%s (want 503: no backend is composed)", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), LLMNotConfiguredCode) {
		t.Fatalf("uncomposed /llm body=%s — want the %s code, which proves nothing served the request", recorder.Body.String(), LLMNotConfiguredCode)
	}

	// With the gateway composed the pattern appears, so the assertion above is
	// reading a real registration and not a router that never mounts /llm.
	withGateway := routePatterns(t, NewRouter(RouterConfig{GatewayProxy: http.NotFoundHandler()}))
	mounted := false
	for _, pattern := range withGateway {
		if strings.Contains(pattern, "/llm") {
			mounted = true
		}
	}
	if !mounted {
		t.Fatalf("GatewayProxy composed but no /llm pattern registered; patterns=%v", withGateway)
	}
}

func TestProductionRouterMatchesMainComposedRouteSurface(t *testing.T) {
	// Pins #243's core invariant: cmd/elitea-main/main.go always sets
	// AppsRepo, ConvsRepo, SkillsRepo, FoldersRepo, TagsRepo, AnalyticsRepo,
	// and WebhookRepo, so prototypeCompatibilityRequested(cfg) was always
	// true in every real deployment and the "reviewed production router"
	// top-level branch NewRouter used to build inline was unreachable dead
	// code. This test builds a RouterConfig with exactly those fields — the
	// minimal shape main.go always produces — and asserts the route table is
	// byte-identical to a snapshot taken from the pre-#243 code with the same
	// config. Any diff here means the cleanup changed what a real deployment
	// serves.
	//
	// Uses oapiserver.CollectRoutes/RouteSet.Patterns() — the same
	// chi.Walk-plus-compat-shim-exclusion machinery internal/api/oapiserver
	// already built for spec-conformance testing — rather than a second,
	// independent chi.Walk, so this snapshot and that suite agree on what
	// counts as router plumbing (doubled /api/v2 prefix rewrite, the /llm
	// reverse proxy, static icon file servers) versus real API surface.
	pool := &pgxpool.Pool{}
	cfg := RouterConfig{
		Pool:          pool,
		AppsRepo:      dbrepos.NewApplicationsRepo(pool),
		ConvsRepo:     dbrepos.NewConversationsRepo(pool),
		SkillsRepo:    dbrepos.NewSkillsRepo(pool),
		FoldersRepo:   dbrepos.NewFoldersRepo(pool),
		TagsRepo:      dbrepos.NewTagsRepo(pool),
		AnalyticsRepo: dbrepos.NewAnalyticsRepo(pool),
		WebhookRepo:   dbrepos.NewWebhooksRepo(pool),
		// main.go sets both of these too (share a conversation by link). They
		// contribute five patterns: three owner-facing ones inside
		// /api/v2/elitea_core, and TWO ANONYMOUS ones that appear in this list
		// without any auth-group ancestor. That is the point of pinning them
		// here — if a later change moves shared_chat_view under the Auth group,
		// or moves anything else out of it, this snapshot is what says so.
		SharedChatStore:      dbrepos.NewSharedChatLinksRepo(pool),
		SharedChatTranscript: dbrepos.NewSharedChatLinksRepo(pool),
	}
	router := NewRouter(cfg)

	routeSet, err := oapiserver.CollectRoutes(router)
	if err != nil {
		t.Fatal(err)
	}
	got := routeSet.Patterns()

	want := []string{
		"DELETE /api/v2/admin/gateway/governance/{id}",
		"DELETE /api/v2/admin/gateway/models/{id}",
		"DELETE /api/v2/admin/gateway/platform_models/{configID}",
		"DELETE /api/v2/admin/gateway/providers/{configID}",
		"DELETE /api/v2/admin/identity_providers/administration/{key}",
		"DELETE /api/v2/admin/mcp_prebuilt_servers/administration/{key}",
		"DELETE /api/v2/admin/moderation_status/{mode}/{projectID}/{entityID}",
		"DELETE /api/v2/admin/modes/administration",
		"DELETE /api/v2/admin/roles/{scope}/{mode}",
		"DELETE /api/v2/admin/scim_group_bindings/administration/{id}",
		"DELETE /api/v2/admin/toolkit_types/administration/{type}/projects/{projectID}",
		"DELETE /api/v2/admin/users/{mode}/{projectID}",
		"DELETE /api/v2/artifacts/buckets/{projectID}/{bucket}",
		"DELETE /api/v2/artifacts/objects/{projectID}/{bucket}/*",
		"DELETE /api/v2/auth/token/{tokenUUID}",
		"DELETE /api/v2/configurations/configuration/{mode}/{projectID}/{configID}",
		"DELETE /api/v2/configurations/configuration/{projectID}/{configID}",
		"DELETE /api/v2/context_manager/summary/{projectID}/{conversationID}/{summaryID}",
		"DELETE /api/v2/elitea_core/application/prompt_lib/{projectID}/{applicationID}",
		"DELETE /api/v2/elitea_core/attachments/prompt_lib/{projectID}/{conversationID}",
		"DELETE /api/v2/elitea_core/conversation/prompt_lib/{projectID}/{conversationID}",
		"DELETE /api/v2/elitea_core/folder/prompt_lib/{projectID}/{folderID}",
		"DELETE /api/v2/elitea_core/index_cancel/prompt_lib/{projectID}/{toolkitID}/{indexName}/{taskID}",
		"DELETE /api/v2/elitea_core/index_meta/prompt_lib/{projectID}/{toolkitID}/{indexMetaID}",
		"DELETE /api/v2/elitea_core/message/prompt_lib/{projectID}/{messageID}",
		"DELETE /api/v2/elitea_core/messages/prompt_lib/{projectID}/{conversationID}",
		"DELETE /api/v2/elitea_core/participant/prompt_lib/{projectID}/{conversationID}/{participantID}",
		"DELETE /api/v2/elitea_core/pin/prompt_lib/{projectID}/{entityType}/{entityID}",
		"DELETE /api/v2/elitea_core/project_budget/administration/{projectID}/budget",
		"DELETE /api/v2/elitea_core/project_icon/prompt_lib/{projectID}/{name}",
		"DELETE /api/v2/elitea_core/register_descriptor/{projectID}",
		"DELETE /api/v2/elitea_core/select_conversation/prompt_lib/{projectID}",
		"DELETE /api/v2/elitea_core/shared_chat_link/prompt_lib/{projectID}/{conversationID}/{linkID}",
		"DELETE /api/v2/elitea_core/skill/{mode}/{projectID}/{skillID}",
		"DELETE /api/v2/elitea_core/tags/prompt_lib/{projectID}/{tagID}",
		"DELETE /api/v2/elitea_core/tool/prompt_lib/{projectID}/{toolkitID}",
		"DELETE /api/v2/elitea_core/upload_icon/prompt_lib/{projectID}/{name}",
		"DELETE /api/v2/elitea_core/upload_skill_icon/prompt_lib/{projectID}/{name}",
		"DELETE /api/v2/elitea_core/user_budget/administration/{projectID}/user_budget/{userID}",
		"DELETE /api/v2/elitea_core/version/prompt_lib/{projectID}/{applicationID}/{versionID}",
		"DELETE /api/v2/notifications/notification/prompt_lib/{projectID}/{notificationID}",
		"DELETE /api/v2/notifications/notifications/prompt_lib/{projectID}",
		"DELETE /api/v2/projects/group/prompt_lib/{projectID}/{groupID}",
		"DELETE /api/v2/projects/project/{mode}/{projectID}",
		"DELETE /api/v2/scim/v2/Groups/{id}",
		"DELETE /api/v2/scim/v2/Users/{id}",
		"DELETE /api/v2/secrets/secret/{mode}/{projectID}/{name}",
		"DELETE /api/v2/social/like/prompt_lib/{projectID}/application/{applicationID}",
		"DELETE /api/v2/social/like/prompt_lib/{projectID}/{entityType}/{entityID}",
		"DELETE /api/v2/social/pin/prompt_lib/{projectID}/{entityType}/{entityID}",
		"DELETE /api/v2/webhooks/prompt_lib/{projectID}/{webhookID}",
		"DELETE /artifacts/s3/{bucket}/*",
		"GET /api/openapi.json",
		"GET /api/openapi.yaml",
		"GET /api/v2/admin/active_tasks/{mode}",
		"GET /api/v2/admin/auth_users/{mode}",
		"GET /api/v2/admin/branding/administration",
		"GET /api/v2/admin/branding/package/administration",
		"GET /api/v2/admin/branding/package/administration/versions",
		"GET /api/v2/admin/email/administration",
		"GET /api/v2/admin/gateway/*/budget-alerts",
		"GET /api/v2/admin/gateway/governance",
		"GET /api/v2/admin/gateway/logs",
		"GET /api/v2/admin/gateway/models",
		"GET /api/v2/admin/gateway/platform_models/",
		"GET /api/v2/admin/gateway/providers/",
		"GET /api/v2/admin/gateway/status",
		"GET /api/v2/admin/gateway/usage",
		"GET /api/v2/admin/identity_providers/administration",
		"GET /api/v2/admin/maintenance/{mode}",
		"GET /api/v2/admin/mcp_prebuilt_servers/administration",
		"GET /api/v2/admin/moderation_status/{mode}/{projectID}/{entityID}",
		"GET /api/v2/admin/moderation_statuses/administration",
		"GET /api/v2/admin/modes/administration",
		"GET /api/v2/admin/permissions/{scope}/{mode}",
		"GET /api/v2/admin/plugin_config_schemas/{mode}",
		"GET /api/v2/admin/plugin_config_suggestions/{mode}/{key}",
		"GET /api/v2/admin/plugin_config_values/administration/{plugin}",
		"GET /api/v2/admin/plugin_config_values/prompt_lib/resources",
		"GET /api/v2/admin/projects/{mode}",
		"GET /api/v2/admin/roles/administration/{projectID}",
		"GET /api/v2/admin/roles/{mode}/{projectID}",
		"GET /api/v2/admin/runtime_plugin/{mode}/{pluginName}",
		"GET /api/v2/admin/runtime_remote/{mode}",
		"GET /api/v2/admin/runtime_remote_config/{mode}/{pluginID}",
		"GET /api/v2/admin/scim_group_bindings/administration",
		"GET /api/v2/admin/scim_group_bindings/administration/project_roles/{projectID}",
		"GET /api/v2/admin/system_info/prompt_lib",
		"GET /api/v2/admin/system_info/{mode}",
		"GET /api/v2/admin/tasks/{mode}",
		"GET /api/v2/admin/tasks/{mode}/",
		"GET /api/v2/admin/toolkit_types/administration",
		"GET /api/v2/admin/user_project_permissions/administration",
		"GET /api/v2/admin/users/administration/{projectID}",
		"GET /api/v2/admin/users/{mode}/{projectID}",
		"GET /api/v2/artifacts/buckets/{projectID}",
		"GET /api/v2/artifacts/buckets/{projectID}/{bucket}",
		"GET /api/v2/artifacts/objects/{projectID}/{bucket}",
		"GET /api/v2/artifacts/objects/{projectID}/{bucket}/*",
		"GET /api/v2/auth/permissions/prompt_lib/{projectID}",
		"GET /api/v2/auth/token/",
		"GET /api/v2/auth/token/{tokenUUID}",
		"GET /api/v2/branding/assets/{kind}/{file}",
		"GET /api/v2/branding/bootstrap.js",
		"GET /api/v2/configurations/available/",
		"GET /api/v2/configurations/configuration/{mode}/{projectID}/{configID}",
		"GET /api/v2/configurations/configuration/{projectID}/{configID}",
		"GET /api/v2/configurations/configurations/{mode}/{projectID}",
		"GET /api/v2/configurations/configurations/{projectID}",
		"GET /api/v2/configurations/models/{mode}/{projectID}",
		"GET /api/v2/configurations/models/{projectID}",
		"GET /api/v2/configurations/tts_voices/{mode}/{projectID}",
		"GET /api/v2/configurations/tts_voices/{projectID}",
		"GET /api/v2/configurations/types/{mode}/{projectID}",
		"GET /api/v2/configurations/types/{projectID}",
		"GET /api/v2/context_manager/analytics/{projectID}/{conversationID}",
		"GET /api/v2/context_manager/summaries/{projectID}/{conversationID}",
		"GET /api/v2/elitea_core/admin/administration",
		"GET /api/v2/elitea_core/admin_published_agents/administration",
		"GET /api/v2/elitea_core/agent_categories/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/agents_with_skill/prompt_lib/{projectID}/{skillID}",
		"GET /api/v2/elitea_core/analytics/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/analytics_agent_detail/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/analytics_agents/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/analytics_costs/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/analytics_tool_detail/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/analytics_tools/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/analytics_user_detail/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/analytics_users/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/application/prompt_lib/{projectID}/{applicationID}",
		"GET /api/v2/elitea_core/application_relation/prompt_lib/{projectID}/{appID}/{versionID}",
		"GET /api/v2/elitea_core/applications/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/audit/{mode}",
		"GET /api/v2/elitea_core/audit_heatmap/{mode}",
		"GET /api/v2/elitea_core/audit_trace_heatmap/{mode}",
		"GET /api/v2/elitea_core/audit_traces/{mode}",
		"GET /api/v2/elitea_core/author/prompt_lib/{authorID}",
		"GET /api/v2/elitea_core/canvas/prompt_lib/{projectID}/{canvasID}",
		"GET /api/v2/elitea_core/check_version_in_use/prompt_lib/{projectID}/{appID}/{versionID}",
		"GET /api/v2/elitea_core/context_analytics/prompt_lib/{projectID}/{conversationID}",
		"GET /api/v2/elitea_core/context_strategy/prompt_lib/{projectID}/{conversationID}",
		"GET /api/v2/elitea_core/conversation/prompt_lib/{projectID}/{conversationID}",
		"GET /api/v2/elitea_core/conversations/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/default_icons/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/default_version/prompt_lib/{projectID}/{applicationID}",
		"GET /api/v2/elitea_core/export_import/prompt_lib/{projectID}/{entityID}",
		"GET /api/v2/elitea_core/export_toolkit/prompt_lib/{projectID}/{toolkitID}",
		"GET /api/v2/elitea_core/feedbacks/default/{projectID}",
		"GET /api/v2/elitea_core/folder/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/folder/prompt_lib/{projectID}/{folderID}",
		"GET /api/v2/elitea_core/index_meta/prompt_lib/{projectID}/{toolkitID}",
		"GET /api/v2/elitea_core/index_meta/prompt_lib/{projectID}/{toolkitID}/{indexMetaID}",
		"GET /api/v2/elitea_core/internal_mcp_pat_status/prompt_lib/{projectID}/{toolkitType}",
		"GET /api/v2/elitea_core/message/prompt_lib/{projectID}/{messageID}",
		"GET /api/v2/elitea_core/message_trace/prompt_lib/{projectID}/{stepID}",
		"GET /api/v2/elitea_core/message_traces/prompt_lib/{projectID}/{conversationID}",
		"GET /api/v2/elitea_core/messages/prompt_lib/{projectID}/{conversationID}",
		"GET /api/v2/elitea_core/permissions/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/platform_settings/prompt_lib",
		"GET /api/v2/elitea_core/platform_settings/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/project_budget/administration/{projectID}/budget",
		"GET /api/v2/elitea_core/project_budget/prompt_lib/{projectID}/budget",
		"GET /api/v2/elitea_core/project_budgets/administration",
		"GET /api/v2/elitea_core/project_context/prompt_lib/{projectID}/project-context",
		"GET /api/v2/elitea_core/project_icon/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/project_info/prompt_lib/{projectID}/project-info",
		"GET /api/v2/elitea_core/project_user_activity/{mode}",
		"GET /api/v2/elitea_core/public_application/prompt_lib/{applicationID}",
		"GET /api/v2/elitea_core/public_application/prompt_lib/{applicationID}/{versionName}",
		"GET /api/v2/elitea_core/public_applications/prompt_lib",
		"GET /api/v2/elitea_core/public_applications/prompt_lib/",
		"GET /api/v2/elitea_core/public_skill/prompt_lib/{skillID}",
		"GET /api/v2/elitea_core/public_skill/prompt_lib/{skillID}/{versionName}",
		"GET /api/v2/elitea_core/public_skills/prompt_lib",
		"GET /api/v2/elitea_core/public_skills/prompt_lib/",
		"GET /api/v2/elitea_core/publish_validate/prompt_lib/{projectID}/{versionID}",
		"GET /api/v2/elitea_core/recommendations/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/roles/{mode}/{projectID}",
		"GET /api/v2/elitea_core/search_options/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/shared_chat_links/prompt_lib/{projectID}/{conversationID}",
		"GET /api/v2/elitea_core/shared_chat_view/prompt_lib/{token}",
		"GET /api/v2/elitea_core/skill/{mode}/{projectID}/{skillID}",
		"GET /api/v2/elitea_core/skill_categories/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/skill_export/{mode}/{projectID}/{skillID}",
		"GET /api/v2/elitea_core/skill_export/{mode}/{projectID}/{skillID}/{versionID}",
		"GET /api/v2/elitea_core/skill_export_fork/prompt_lib/{projectID}/{skillID}",
		"GET /api/v2/elitea_core/skill_export_fork/prompt_lib/{projectID}/{skillID}/{versionID}",
		"GET /api/v2/elitea_core/skills/{mode}/{projectID}",
		"GET /api/v2/elitea_core/tags/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/tool/prompt_lib/{projectID}/{toolkitID}",
		"GET /api/v2/elitea_core/toolkit_available_tools/prompt_lib/{projectID}/{toolkitID}",
		"GET /api/v2/elitea_core/toolkit_types/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/toolkit_validator/prompt_lib/{projectID}/{toolkitID}",
		"GET /api/v2/elitea_core/toolkits/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/tools/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/tools_list/default/{projectID}",
		"GET /api/v2/elitea_core/tools_list/{projectID}",
		"GET /api/v2/elitea_core/trending_authors/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/upload_icon/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/upload_skill_icon/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/usage/prompt_lib/{projectID}/usage",
		"GET /api/v2/elitea_core/user_budget/administration/{projectID}/user_budget/{userID}",
		"GET /api/v2/elitea_core/user_budget/prompt_lib/{projectID}/user_budget/{userID}",
		"GET /api/v2/elitea_core/user_budgets/administration/{projectID}",
		"GET /api/v2/elitea_core/user_budgets/prompt_lib/{projectID}",
		"GET /api/v2/elitea_core/users/{mode}/{projectID}",
		"GET /api/v2/elitea_core/version/prompt_lib/{projectID}/{applicationID}/{versionID}",
		"GET /api/v2/elitea_core/version_validator/prompt_lib/{projectID}/{applicationID}/{versionID}",
		"GET /api/v2/elitea_core/versions/prompt_lib/{projectID}/{applicationID}",
		"GET /api/v2/notifications/notifications/prompt_lib/{projectID}",
		"GET /api/v2/projects/groups/prompt_lib",
		"GET /api/v2/projects/project/{mode}/{projectID}",
		"GET /api/v2/projects/quota/{projectID}",
		"GET /api/v2/projects/statistics/{projectID}",
		"GET /api/v2/scheduling/schedules/administration/{projectID}",
		"GET /api/v2/scheduling/schedules/{mode}/{projectID}",
		"GET /api/v2/scim/v2/Groups",
		"GET /api/v2/scim/v2/Groups/{id}",
		"GET /api/v2/scim/v2/ResourceTypes",
		"GET /api/v2/scim/v2/Schemas",
		"GET /api/v2/scim/v2/ServiceProviderConfig",
		"GET /api/v2/scim/v2/Users",
		"GET /api/v2/scim/v2/Users/{id}",
		"GET /api/v2/secrets/secret/{mode}/{projectID}/{name}",
		"GET /api/v2/secrets/secret/{projectID}/{name}",
		"GET /api/v2/secrets/secrets/{mode}/{projectID}",
		"GET /api/v2/social/author",
		"GET /api/v2/social/author/",
		"GET /api/v2/social/authors/{projectID}",
		"GET /api/v2/social/feedbacks/default/{projectID}",
		"GET /api/v2/social/trending_authors/prompt_lib/{projectID}",
		"GET /api/v2/support_assistant/config",
		"GET /api/v2/support_assistant/config/",
		"GET /api/v2/support_assistant/conversation/{conversationUUID}",
		"GET /api/v2/support_assistant/conversations",
		"GET /api/v2/support_assistant/conversations/",
		"GET /api/v2/tracing/status/administration",
		"GET /api/v2/tracing/status/prompt_lib/{projectID}",
		"GET /api/v2/webhooks/prompt_lib/{projectID}/",
		"GET /api/v2/webhooks/prompt_lib/{projectID}/{webhookID}",
		"GET /app/{projectID}/mcp",
		"GET /app/{projectID}/mcp/*",
		"GET /artifacts/s3/{bucket}",
		"GET /artifacts/s3/{bucket}/*",
		"GET /auth",
		"GET /avatars/{projectID}/{filename}",
		"GET /docs",
		"GET /healthz",
		"GET /icons/{projectID}/{filename}",
		"GET /readyz",
		"GET /startupz",
		"HEAD /api/v2/artifacts/objects/{projectID}/{bucket}/*",
		"HEAD /api/v2/branding/assets/{kind}/{file}",
		"HEAD /api/v2/branding/bootstrap.js",
		"HEAD /artifacts/s3/{bucket}/*",
		"PATCH /api/v2/artifacts/buckets/{projectID}/{bucket}",
		"PATCH /api/v2/elitea_core/application_relation/prompt_lib/{projectID}/{appID}/{versionID}",
		"PATCH /api/v2/elitea_core/default_version/prompt_lib/{projectID}/{applicationID}/{versionID}",
		"PATCH /api/v2/elitea_core/entity_settings/prompt_lib/{projectID}/{conversationID}",
		"PATCH /api/v2/elitea_core/folder/prompt_lib/{projectID}/{folderID}",
		"PATCH /api/v2/elitea_core/index_meta/prompt_lib/{projectID}/{toolkitID}/{indexMetaID}",
		"PATCH /api/v2/elitea_core/skill/{mode}/{projectID}/{skillID}",
		"PATCH /api/v2/elitea_core/skill_default_version/{mode}/{projectID}/{skillID}",
		"PATCH /api/v2/elitea_core/tool/prompt_lib/{projectID}/{toolkitID}",
		"PATCH /api/v2/elitea_core/version/prompt_lib/{projectID}/{applicationID}/{versionID}",
		"PATCH /api/v2/scim/v2/Groups/{id}",
		"PATCH /api/v2/scim/v2/Users/{id}",
		"POST /api/v2/admin/auth_users/{mode}",
		"POST /api/v2/admin/branding/assets/{kind}",
		"POST /api/v2/admin/branding/package/administration",
		"POST /api/v2/admin/branding/package/administration/versions/{digest}/restore",
		"POST /api/v2/admin/branding/test_email/administration",
		"POST /api/v2/admin/email/test/administration",
		"POST /api/v2/admin/gateway/governance",
		"POST /api/v2/admin/gateway/governance/validate-cel",
		"POST /api/v2/admin/gateway/platform_models/",
		"POST /api/v2/admin/gateway/providers/",
		// The two STORED operations on a platform credential: a live provider
		// round trip that writes nothing, and an admission re-run that writes
		// status_ok. Both are the project plane's own executors pointed at the
		// public project's row (configurations/global_providers.go).
		"POST /api/v2/admin/gateway/providers/{configID}/check",
		"POST /api/v2/admin/gateway/providers/{configID}/models",
		"POST /api/v2/admin/gateway/providers/{configID}/revalidate",
		"POST /api/v2/admin/moderation_status/{mode}/{projectID}/{entityID}",
		"POST /api/v2/admin/modes/administration",
		"POST /api/v2/admin/permissions/{scope}/{mode}",
		"POST /api/v2/admin/plugin_config_restart/{mode}/{pylonID}",
		"POST /api/v2/admin/roles/{scope}/{mode}",
		"POST /api/v2/admin/runtime_pylons/{mode}",
		"POST /api/v2/admin/runtime_remote_config/{mode}/{pluginID}",
		"POST /api/v2/admin/scim_group_bindings/administration",
		"POST /api/v2/admin/toolkit_types/administration/bulk",
		"POST /api/v2/admin/user_invite/administration",
		"POST /api/v2/admin/users/administration/{projectID}",
		"POST /api/v2/admin/users/{mode}/{projectID}",
		"POST /api/v2/artifacts/buckets/{projectID}",
		"POST /api/v2/artifacts/grants/{projectID}/{bucket}",
		"POST /api/v2/artifacts/grants/{projectID}/{grantID}/parts/{partNumber}",
		"POST /api/v2/artifacts/grants/{projectID}/{grantID}:abortMultipart",
		"POST /api/v2/artifacts/grants/{projectID}/{grantID}:commit",
		"POST /api/v2/artifacts/grants/{projectID}/{grantID}:completeMultipart",
		"POST /api/v2/artifacts/objects/{projectID}/{bucket}",
		"POST /api/v2/artifacts/objects/{projectID}/{bucket}:batchDelete",
		"POST /api/v2/auth/token/",
		"POST /api/v2/configurations/check_connection/{mode}/{projectID}/{configType}",
		"POST /api/v2/configurations/check_connection/{projectID}/{configType}",
		"POST /api/v2/configurations/check_connections/{mode}/{projectID}",
		"POST /api/v2/configurations/check_connections/{projectID}",
		"POST /api/v2/configurations/check_stored_connection/{mode}/{projectID}/{configID}",
		"POST /api/v2/configurations/check_stored_connection/{projectID}/{configID}",
		"POST /api/v2/configurations/check_stored_connections/{mode}/{projectID}",
		"POST /api/v2/configurations/check_stored_connections/{projectID}",
		"POST /api/v2/configurations/configurations/{mode}/{projectID}",
		"POST /api/v2/configurations/configurations/{projectID}",
		"POST /api/v2/configurations/models/{mode}/{projectID}",
		"POST /api/v2/configurations/models/{projectID}",
		"POST /api/v2/configurations/revalidate/{mode}/{projectID}/{configID}",
		"POST /api/v2/configurations/revalidate/{projectID}/{configID}",
		"POST /api/v2/context_manager/optimize_context/{projectID}/{conversationID}",
		"POST /api/v2/context_manager/summaries/{projectID}/{conversationID}",
		"POST /api/v2/elitea_core/applications/prompt_lib/{projectID}",
		"POST /api/v2/elitea_core/attach_public_skill/prompt_lib/{projectID}",
		"POST /api/v2/elitea_core/attachments/prompt_lib/{projectID}/{conversationID}",
		"POST /api/v2/elitea_core/batch_replace_version/prompt_lib/{projectID}/{oldVersionID}/{newVersionID}",
		"POST /api/v2/elitea_core/canvases/prompt_lib/{projectID}",
		"POST /api/v2/elitea_core/conversations/prompt_lib/{projectID}",
		"POST /api/v2/elitea_core/export_converter/prompt_lib",
		"POST /api/v2/elitea_core/export_import/prompt_lib/{projectID}/{entityID}",
		"POST /api/v2/elitea_core/folder/prompt_lib/{projectID}",
		"POST /api/v2/elitea_core/fork/prompt_lib/{projectID}",
		"POST /api/v2/elitea_core/fork_toolkit/prompt_lib/{projectID}",
		"POST /api/v2/elitea_core/generate_application_draft/prompt_lib/{projectID}",
		"POST /api/v2/elitea_core/generate_project_context_draft/prompt_lib/{projectID}",
		"POST /api/v2/elitea_core/generate_skill_draft/prompt_lib/{projectID}",
		"POST /api/v2/elitea_core/import_wizard/prompt_lib/{projectID}",
		"POST /api/v2/elitea_core/mcp_dcr_proxy/{projectID}",
		"POST /api/v2/elitea_core/mcp_oauth_proxy/{projectID}",
		"POST /api/v2/elitea_core/mcp_sync_tools/prompt_lib/{projectID}",
		"POST /api/v2/elitea_core/participants/prompt_lib/{projectID}/{conversationID}",
		"POST /api/v2/elitea_core/pin/prompt_lib/{projectID}/{entityType}/{entityID}",
		"POST /api/v2/elitea_core/predict_llm/prompt_lib/{projectID}",
		"POST /api/v2/elitea_core/project_icon/prompt_lib/{projectID}",
		"POST /api/v2/elitea_core/publish/prompt_lib/{projectID}/{versionID}",
		"POST /api/v2/elitea_core/publish_skill/prompt_lib/{projectID}/{skillID}/{versionID}",
		"POST /api/v2/elitea_core/publish_skill_validate/prompt_lib/{projectID}/{skillID}/{versionID}",
		"POST /api/v2/elitea_core/publish_validate/prompt_lib/{projectID}/{versionID}",
		"POST /api/v2/elitea_core/regenerate/prompt_lib/{projectID}/{conversationID}",
		"POST /api/v2/elitea_core/register_descriptor/{projectID}",
		// ADR-0023 S3, migration 0109. Two routes, not one, and gated on
		// `provider_hub.descriptor.activate` rather than `.register`: a facade
		// registrar files a registration at boot, while activation is the switch
		// that lets agents call the provider.
		"POST /api/v2/elitea_core/register_descriptor/{projectID}/activate",
		"POST /api/v2/elitea_core/register_descriptor/{projectID}/deactivate",
		"POST /api/v2/elitea_core/select_conversation/prompt_lib/{projectID}/{conversationID}",
		"POST /api/v2/elitea_core/shared_chat_links/prompt_lib/{projectID}/{conversationID}",
		"POST /api/v2/elitea_core/shared_chat_view_unlock/prompt_lib/{token}/unlock",
		"POST /api/v2/elitea_core/skill/{mode}/{projectID}/{skillID}",
		"POST /api/v2/elitea_core/skill_import/{mode}/{projectID}",
		"POST /api/v2/elitea_core/skills/{mode}/{projectID}",
		"POST /api/v2/elitea_core/tags/prompt_lib/{projectID}",
		"POST /api/v2/elitea_core/test_tool/prompt_lib/{projectID}/{toolID}",
		"POST /api/v2/elitea_core/test_toolkit_tool/prompt_lib/{projectID}",
		"POST /api/v2/elitea_core/toolkit_discover_tools/prompt_lib/{projectID}/{toolkitType}",
		"POST /api/v2/elitea_core/toolkit_validator/prompt_lib/{projectID}/{toolkitID}",
		"POST /api/v2/elitea_core/tools/prompt_lib/{projectID}",
		"POST /api/v2/elitea_core/tools_call/default/{projectID}",
		"POST /api/v2/elitea_core/unpublish/prompt_lib/{projectID}/{versionID}",
		"POST /api/v2/elitea_core/unpublish_skill/prompt_lib/{projectID}/{skillID}/{versionID}",
		"POST /api/v2/elitea_core/upload_icon/prompt_lib/{projectID}",
		"POST /api/v2/elitea_core/upload_icon/prompt_lib/{projectID}/{entityID}",
		"POST /api/v2/elitea_core/upload_skill_icon/prompt_lib/{projectID}",
		"POST /api/v2/elitea_core/upload_skill_icon/prompt_lib/{projectID}/{versionId}",
		"POST /api/v2/elitea_core/version_validator/prompt_lib/{projectID}/{applicationID}/{versionID}",
		"POST /api/v2/elitea_core/versions/prompt_lib/{projectID}/{applicationID}",
		"POST /api/v2/projects/group/prompt_lib/{projectID}",
		"POST /api/v2/projects/project/{mode}",
		"POST /api/v2/scim/v2/Groups",
		"POST /api/v2/scim/v2/Users",
		"POST /api/v2/secrets/hide/{mode}/{projectID}/{name}",
		"POST /api/v2/secrets/secret/{mode}/{projectID}/{name}",
		"POST /api/v2/secrets/secrets/{mode}/{projectID}",
		"POST /api/v2/social/feedbacks/default/{projectID}",
		"POST /api/v2/social/like/prompt_lib/{projectID}/application/{applicationID}",
		"POST /api/v2/social/like/prompt_lib/{projectID}/{entityType}/{entityID}",
		"POST /api/v2/social/pin/prompt_lib/{projectID}/{entityType}/{entityID}",
		"POST /api/v2/support_assistant/conversations",
		"POST /api/v2/support_assistant/conversations/",
		"POST /api/v2/support_assistant/predict/{conversationUUID}",
		"POST /api/v2/tracing/collect/prompt_lib",
		"POST /api/v2/tracing/collect/prompt_lib/{projectID}",
		"POST /api/v2/tracing/otlp/prompt_lib",
		"POST /api/v2/tracing/otlp/prompt_lib/{projectID}",
		"POST /api/v2/webhooks/prompt_lib/{projectID}/",
		"POST /app/{projectID}/mcp",
		"POST /app/{projectID}/mcp/*",
		"PUT /api/v2/admin/branding/administration",
		"PUT /api/v2/admin/email/administration",
		"PUT /api/v2/admin/gateway/*/budget-alerts",
		"PUT /api/v2/admin/gateway/governance/{id}",
		"PUT /api/v2/admin/gateway/models",
		"PUT /api/v2/admin/gateway/platform_models/{configID}",
		"PUT /api/v2/admin/gateway/providers/{configID}",
		"PUT /api/v2/admin/identity_providers/administration/{key}",
		"PUT /api/v2/admin/maintenance/{mode}",
		"PUT /api/v2/admin/mcp_prebuilt_servers/administration/{key}",
		"PUT /api/v2/admin/moderation_status/administration",
		"PUT /api/v2/admin/permissions/{scope}/{mode}",
		"PUT /api/v2/admin/plugin_config_values/administration/{plugin}",
		"PUT /api/v2/admin/project_suspend/{mode}/{projectID}",
		"PUT /api/v2/admin/roles/{scope}/{mode}",
		"PUT /api/v2/admin/runtime_plugin/{mode}/{pluginName}",
		"PUT /api/v2/admin/scim_group_bindings/administration/{id}",
		"PUT /api/v2/admin/toolkit_types/administration/{type}",
		"PUT /api/v2/admin/toolkit_types/administration/{type}/projects/{projectID}",
		"PUT /api/v2/admin/user_project_permissions/administration",
		"PUT /api/v2/admin/user_suspend/{mode}/{userID}",
		"PUT /api/v2/admin/users/administration/{projectID}",
		"PUT /api/v2/admin/users/{mode}/{projectID}",
		"PUT /api/v2/configurations/configuration/{mode}/{projectID}/{configID}",
		"PUT /api/v2/configurations/configuration/{projectID}/{configID}",
		"PUT /api/v2/context_manager/summary/{projectID}/{conversationID}/{summaryID}",
		"PUT /api/v2/elitea_core/application/prompt_lib/{projectID}/{applicationID}",
		"PUT /api/v2/elitea_core/application_attachment_storage/prompt_lib/{projectID}/{applicationID}/{versionID}",
		"PUT /api/v2/elitea_core/attachment_storage/prompt_lib/{projectID}/{conversationID}",
		"PUT /api/v2/elitea_core/canvas/prompt_lib/{projectID}/{canvasID}",
		"PUT /api/v2/elitea_core/context_strategy/prompt_lib/{projectID}/{conversationID}",
		"PUT /api/v2/elitea_core/conversation/prompt_lib/{projectID}/{conversationID}",
		"PUT /api/v2/elitea_core/entity_settings/prompt_lib/{projectID}/{conversationID}/{participantID}",
		"PUT /api/v2/elitea_core/folder/prompt_lib/{projectID}/{folderID}",
		"PUT /api/v2/elitea_core/project_budget/administration/{projectID}/budget",
		"PUT /api/v2/elitea_core/project_context/prompt_lib/{projectID}/project-context",
		"PUT /api/v2/elitea_core/project_info/prompt_lib/{projectID}/project-info",
		"PUT /api/v2/elitea_core/skill/{mode}/{projectID}/{skillID}",
		"PUT /api/v2/elitea_core/tool/prompt_lib/{projectID}/{toolkitID}",
		"PUT /api/v2/elitea_core/upload_icon/prompt_lib/{projectID}/{versionId}",
		"PUT /api/v2/elitea_core/upload_skill_icon/prompt_lib/{projectID}/{versionId}",
		"PUT /api/v2/elitea_core/user_budget/administration/{projectID}/user_budget/{userID}",
		"PUT /api/v2/elitea_core/version/prompt_lib/{projectID}/{applicationID}/{versionID}",
		"PUT /api/v2/notifications/notification/prompt_lib/{projectID}/{notificationID}",
		"PUT /api/v2/notifications/notifications/prompt_lib/{projectID}",
		"PUT /api/v2/projects/groups/prompt_lib/{projectID}",
		"PUT /api/v2/projects/quota/{projectID}",
		"PUT /api/v2/scheduling/schedules/administration/{projectID}",
		"PUT /api/v2/scheduling/schedules/{mode}/{projectID}",
		"PUT /api/v2/scim/v2/Groups/{id}",
		"PUT /api/v2/scim/v2/Users/{id}",
		"PUT /api/v2/secrets/secret/{mode}/{projectID}/{name}",
		"PUT /api/v2/social/author",
		"PUT /api/v2/social/author/",
		"PUT /api/v2/webhooks/prompt_lib/{projectID}/{webhookID}",
		"PUT /artifacts/s3/{bucket}/*",
	}

	if len(got) != len(want) {
		t.Fatalf("main-composed route surface changed: got %d routes, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("main-composed route surface changed at entry %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestProductionRouterPreservesRawSocketPeer(t *testing.T) {
	router := NewRouter(RouterConfig{})
	router.Get("/__test/raw-peer", func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(request.RemoteAddr))
	})

	request := httptest.NewRequest(http.MethodGet, "/__test/raw-peer", nil)
	request.RemoteAddr = "10.20.30.40:43120"
	request.Header.Set("X-Forwarded-For", "198.51.100.25")
	request.Header.Set("X-Real-IP", "203.0.113.17")
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got, want := recorder.Body.String(), request.RemoteAddr; got != want {
		t.Fatalf("RemoteAddr = %q, want raw socket peer %q", got, want)
	}
}

// TestProductionRouterLeavesUnreviewedPrototypeSurfacesUnmounted pins that
// none of these source-only prototype paths is reachable on the production
// mount.
//
// Requests are AUTHENTICATED on purpose. An unauthenticated request would 401
// at the mounted-but-unrouted prefixes (e.g. /api/v2/artifacts/…) before chi
// ever consults the inner routing table, so a 404 assertion would pass without
// proving anything about which routes exist. Presenting a valid credential
// makes 404 mean "no such route" rather than "no such caller".
//
// This test previously set AUTH_DEV_MODE=true and claimed "even explicit
// development identity cannot make a prototype route appear". That claim held
// only because of a dead NewRouter branch (#243); against the router every
// deployment actually runs it was never true. The bypass is gone (ADR-0017,
// #260) and the assertion is now made honestly.
func TestProductionRouterLeavesUnreviewedPrototypeSurfacesUnmounted(t *testing.T) {
	router := newCompleteProductionRouter("")

	for _, target := range []string{
		"/socket.io/",
		"/admin/app/",
		"/app/application_icon/icon.svg",
	} {
		t.Run(target, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, testAuthHeader(httptest.NewRequest(http.MethodGet, target, nil)))
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("GET %s status = %d, want %d", target, recorder.Code, http.StatusNotFound)
			}
		})
	}
}

func TestProductionAuthCandidatesRejectEveryForgedCredentialShape(t *testing.T) {
	// v2auth's token/permission endpoints are mounted unconditionally by
	// newProductionRouter (router.go's `r.Mount("/auth", v2auth.NewHandler(...))`)
	// — they are not gated by any RouterConfig.Current* field, so they have
	// always been reachable in every real deployment. #243 removed the dead
	// "reviewed production router" branch that a bare RouterConfig used to
	// reach in this test, which never wired these routes at all and made
	// them look unmounted; that was an artifact of the dead branch, not a
	// real production guarantee. What actually protects these routes is
	// authentication: no forged credential shape gets past it.
	router := newCompleteProductionRouter("0123456789abcdef0123456789abcdef")
	routes := []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/api/v2/auth/permissions/prompt_lib/7"},
		{method: http.MethodGet, path: "/api/v2/auth/token/"},
		{method: http.MethodGet, path: "/api/v2/auth/token/00000000-0000-0000-0000-000000000001"},
		{method: http.MethodPost, path: "/api/v2/auth/token/"},
		{method: http.MethodDelete, path: "/api/v2/auth/token/00000000-0000-0000-0000-000000000001"},
	}

	for _, route := range routes {
		for _, credential := range []string{"missing", "invalid", "forwarded", "session"} {
			t.Run(route.method+" "+route.path+" "+credential, func(t *testing.T) {
				request := httptest.NewRequest(route.method, route.path, nil)
				switch credential {
				case "invalid":
					request.Header.Set("Authorization", "Bearer invalid")
				case "forwarded":
					request.Header.Set("X-Auth-Type", "user")
					request.Header.Set("X-Auth-Id", "1")
				case "session":
					request.AddCookie(&http.Cookie{Name: "elitea_session", Value: "forged.session"})
				}
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, request)
				if recorder.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
				}
			})
		}
	}
}

func TestProductionBrowserAuthSurfaceNeverSucceedsWithoutCredentials(t *testing.T) {
	// #243: this used to assert every /forward-auth/* path was unmounted
	// (404) for a "complete" production config. That was only true because
	// the config exploited RouterConfig's flat SessionHandler/OIDCHandler
	// fields (as opposed to the Auth.SessionHandler/Auth.OIDCHandler fields
	// cmd/elitea-main/main.go actually sets) to dodge
	// prototypeCompatibilityRequested and land on the dead "reviewed
	// production router" branch, which never wired /forward-auth at all.
	// Real deployments with OIDC session auth configured (main.go's
	// oidcSessionHandler/oidcOIDCHandler, wired through Auth.SessionHandler)
	// already served every one of these paths before #243, same as after —
	// the login/logout endpoints are the real browser session flow, not a
	// leftover prototype surface. What actually matters: no request without
	// valid credentials gets a bare success.
	router := newCompleteProductionRouter("0123456789abcdef0123456789abcdef")
	routes := []struct {
		method string
		path   string
		want   int
	}{
		// Not a registered route at all: "/forward-auth/auth" (the Traefik
		// ForwardAuth check) is a top-level "/auth" route, distinct from the
		// "/forward-auth/*" browser session group.
		{method: http.MethodGet, path: "/forward-auth/auth", want: http.StatusNotFound},
		{method: http.MethodHead, path: "/forward-auth/auth", want: http.StatusNotFound},
		{method: http.MethodOptions, path: "/forward-auth/auth", want: http.StatusNotFound},
		// GET redirects into the login flow; only GET is registered.
		{method: http.MethodGet, path: "/forward-auth/login", want: http.StatusFound},
		{method: http.MethodHead, path: "/forward-auth/login", want: http.StatusMethodNotAllowed},
		{method: http.MethodOptions, path: "/forward-auth/login", want: http.StatusMethodNotAllowed},
		// Legacy form-auth login/authorize are never mounted: SessionHandler
		// wires the OIDC session flow only (see router.go's "/forward-auth"
		// Route block), not the legacy form-auth handlers.
		{method: http.MethodGet, path: "/forward-auth/auth_form/login", want: http.StatusNotFound},
		{method: http.MethodHead, path: "/forward-auth/auth_form/login", want: http.StatusNotFound},
		{method: http.MethodOptions, path: "/forward-auth/auth_form/login", want: http.StatusNotFound},
		{method: http.MethodPost, path: "/forward-auth/auth_form/authorize", want: http.StatusNotFound},
		{method: http.MethodOptions, path: "/forward-auth/auth_form/authorize", want: http.StatusNotFound},
		{method: http.MethodGet, path: "/forward-auth/logout", want: http.StatusFound},
		{method: http.MethodHead, path: "/forward-auth/logout", want: http.StatusMethodNotAllowed},
		{method: http.MethodOptions, path: "/forward-auth/logout", want: http.StatusMethodNotAllowed},
		{method: http.MethodGet, path: "/forward-auth/auth_form/logout", want: http.StatusFound},
		{method: http.MethodHead, path: "/forward-auth/auth_form/logout", want: http.StatusMethodNotAllowed},
		{method: http.MethodOptions, path: "/forward-auth/auth_form/logout", want: http.StatusMethodNotAllowed},
		// GET 503s here because newCompleteProductionRouter's OIDCHandler is a
		// zero-value test double: it holds neither an environment runtime nor a
		// provider store, so it resolves no identity provider and says so. A
		// real OIDCHandler redirects (302) instead.
		//
		// This WAS a 500, and the 500 was a nil dereference on the handler's
		// oauth2 configuration. Resolving the provider per request replaced the
		// crash with a refusal that names the condition. Pinned exactly rather
		// than accepted as "any non-2xx", so a change back to a crash does not
		// slip past either.
		{method: http.MethodGet, path: "/forward-auth/auth_oidc/login", want: http.StatusServiceUnavailable},
		{method: http.MethodHead, path: "/forward-auth/auth_oidc/login", want: http.StatusMethodNotAllowed},
		{method: http.MethodOptions, path: "/forward-auth/auth_oidc/login", want: http.StatusMethodNotAllowed},
		// Not a registered route: the OIDC callback path is "auth_oidc/callback",
		// not "auth_oidc/login_callback".
		{method: http.MethodGet, path: "/forward-auth/auth_oidc/login_callback", want: http.StatusNotFound},
		{method: http.MethodHead, path: "/forward-auth/auth_oidc/login_callback", want: http.StatusNotFound},
		{method: http.MethodPost, path: "/forward-auth/auth_oidc/login_callback", want: http.StatusNotFound},
		{method: http.MethodOptions, path: "/forward-auth/auth_oidc/login_callback", want: http.StatusNotFound},
		// SAML. All three refuse with 503 here: the handler holds no provider
		// store, so it federates nothing and says so. None of them can answer a
		// success without a verified assertion, which is what this test is for.
		{method: http.MethodGet, path: "/forward-auth/auth_saml/metadata", want: http.StatusServiceUnavailable},
		{method: http.MethodGet, path: "/forward-auth/auth_saml/login", want: http.StatusServiceUnavailable},
		{method: http.MethodPost, path: "/forward-auth/auth_saml/acs", want: http.StatusServiceUnavailable},
		// The assertion consumer service is POST-only: the authentication
		// request asks for the HTTP-POST binding, and a GET here would be a
		// login attempt carrying the assertion in the URL.
		{method: http.MethodGet, path: "/forward-auth/auth_saml/acs", want: http.StatusMethodNotAllowed},
		{method: http.MethodPost, path: "/forward-auth/auth_saml/login", want: http.StatusMethodNotAllowed},
	}

	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(route.method, route.path, nil)
			router.ServeHTTP(recorder, request)
			if recorder.Code != route.want {
				t.Fatalf("status = %d, want %d", recorder.Code, route.want)
			}
			if recorder.Code >= 200 && recorder.Code < 300 {
				t.Fatalf("status = %d, a bare success without credentials", recorder.Code)
			}
		})
	}
}

func newCompleteProductionRouter(sessionSecret string) chi.Router {
	runtimeHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		panic(fmt.Errorf("route coverage test must not execute runtime handler"))
	})
	// NewRouter's dead "reviewed production router" branch never wired
	// CutoverRouter/CutoverTracker, so nothing exercised them before #243;
	// see newUnreachableRedisClient for why a nil client isn't safe here.
	unreachableRedis := newUnreachableRedisClient()
	return NewRouter(RouterConfig{
		AuthValidator:      testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator: testPrincipalValidator{},
		SessionHandler:     v2auth.NewSessionHandler(nil, sessionSecret),
		OIDCHandler:        &v2auth.OIDCHandler{},
		// A zero-value SAML handler: it holds no provider store, so every one
		// of its routes resolves no provider and refuses. It is wired anyway so
		// the browser-auth surface below PINS the SAML paths — a handler left
		// out of this router would let all three routes be removed without a
		// test noticing.
		SAMLHandler:    &v2auth.SAMLHandler{},
		SessionSecret:  sessionSecret,
		Shadow:         shadow.NewComparator(shadow.Config{Timeout: time.Second}),
		ShadowMetrics:  shadow.NewMetrics(10),
		CutoverTracker: cutover.NewTracker(unreachableRedis),
		CutoverRouter: cutover.NewRouter(cutover.RouterConfig{
			Tracker:   cutover.NewTracker(unreachableRedis),
			LegacyURL: "http://127.0.0.1:1",
		}),
		InternalAdminToken: strings.Repeat("i", middleware.MinimumInternalAdminTokenBytes),
		RuntimeRoutes: RuntimeRoutes{
			Validation:      runtimeHandler,
			ExecutionEvents: runtimeHandler,
		},
	})
}

// ---------------------------------------------------------------------------
// The per-message read: GET /elitea_core/message/prompt_lib/{projectID}/{messageID}
// ---------------------------------------------------------------------------

// recordingMessageRepo answers the one Repository method the per-message read
// uses and records what it was asked for. Everything else is the embedded
// interface, nil — the conversations group registers on `ConvsRepo != nil`
// alone, so no other method is reached by these two requests.
//
// It records rather than merely answering because the status code is not the
// discriminating signal here; see the test below.
type recordingMessageRepo struct {
	v2convs.Repository
	gotProjectID  string
	gotMessageUID string
}

func (r *recordingMessageRepo) GetMessageByUUID(
	_ context.Context, projectID, messageUUID string,
) (map[string]any, error) {
	r.gotProjectID = projectID
	r.gotMessageUID = messageUUID
	return map[string]any{"uuid": messageUUID, "message_items": []any{}}, nil
}

func perMessageReadRouter(repo v2convs.Repository, granted ...string) http.Handler {
	return NewRouter(RouterConfig{
		AuthValidator:             testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:        testPrincipalValidator{},
		ConvsRepo:                 repo,
		ProjectAccessQuerier:      &memberOfProject{project: "7"},
		ProjectPermissionResolver: fakePermissionResolver{granted: granted, forProject: "7"},
	})
}

const perMessageReadUID = "0f8f6d1e-9c2a-4a1b-8f3e-2b7c5d4e6a90"

// pylon declares `get` and `delete` on ONE module — elitea_core/api/v2/
// message.py:176-183 — and this router bound only the delete. The read was
// implemented (`(*conversations.Handler).GetMessage`) and registered by
// nothing, which is #126's dead-wiring class: `git log -S` finds no commit
// that ever routed it, so it is a gap rather than a regression.
//
// TWO claims are asserted together, because either alone passes against a
// route that does not work:
//
//  1. the request REACHES the handler through the permission gate — the
//     wiring claim, which a 401/403/404 would falsify; and
//  2. the uuid FROM THE PATH is the one the repository is asked for.
//
// (2) is the discriminating half and the reason this test builds the real
// router rather than the conversations package's own test router. GetMessage
// read `chi.URLParam(r, "messageUUID")`, a param name NO route declares, so
// mounting it unchanged would have handed the repository the empty string on
// every request and answered 404 for every message that exists. A status-only
// assertion cannot see that — the handler still returns 200 for whatever the
// repository hands back — and the conversations package's test router could
// not see it either, because it names its own params.
func TestProductionRouterServesThePerMessageRead(t *testing.T) {
	repo := &recordingMessageRepo{}
	router := perMessageReadRouter(repo, "models.chat.messages.details")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, testAuthHeader(httptest.NewRequest(
		http.MethodGet, "/api/v2/elitea_core/message/prompt_lib/7/"+perMessageReadUID, nil)))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if repo.gotProjectID != "7" {
		t.Errorf("repository asked for project %q, want \"7\"", repo.gotProjectID)
	}
	if repo.gotMessageUID != perMessageReadUID {
		t.Fatalf("repository asked for message %q, want %q — the handler is reading a "+
			"path param the route does not declare", repo.gotMessageUID, perMessageReadUID)
	}

	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, recorder.Body.String())
	}
	if body["uuid"] != perMessageReadUID {
		t.Errorf("response uuid = %v, want %q", body["uuid"], perMessageReadUID)
	}
}

// The read carries its OWN permission. pylon gates the get on
// `models.chat.messages.details` and the delete on `models.chat.messages.delete`
// (message.py:39,73), and the two are withheld differently — delete is denied
// to a viewer in both modes, details is granted to a viewer in both.
//
// A caller holding only the sibling's delete string must therefore be refused
// here. Without this, a gate copied from the DELETE one line below it would
// look correct in every other test in this file: the route would still answer,
// still reach the handler, and still return the right message — for a
// principal pylon does not let read it.
func TestPerMessageReadDoesNotAcceptTheDeleteSiblingsPermission(t *testing.T) {
	repo := &recordingMessageRepo{}
	router := perMessageReadRouter(repo, "models.chat.messages.delete")

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, testAuthHeader(httptest.NewRequest(
		http.MethodGet, "/api/v2/elitea_core/message/prompt_lib/7/"+perMessageReadUID, nil)))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", recorder.Code, recorder.Body.String())
	}
	if repo.gotMessageUID != "" {
		t.Fatalf("the repository was reached (%q) despite the refusal", repo.gotMessageUID)
	}
}
