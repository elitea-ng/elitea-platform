package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	indexingapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/indexing"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type productionRuntimePrincipalValidatorFunc func(context.Context, auth.User) (auth.User, error)

func (function productionRuntimePrincipalValidatorFunc) ValidatePrincipal(ctx context.Context, user auth.User) (auth.User, error) {
	return function(ctx, user)
}

type productionRuntimePeerVerifierFunc func(*http.Request) error

func (function productionRuntimePeerVerifierFunc) VerifyForwardedIdentityPeer(request *http.Request) error {
	return function(request)
}

func TestProductionRuntimeRoutesRejectIncompleteSecurityComposition(t *testing.T) {
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	principal := productionRuntimePrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
		return user, nil
	})
	peer := productionRuntimePeerVerifierFunc(func(*http.Request) error { return nil })

	for name, test := range map[string]struct {
		validation      http.Handler
		executionEvents http.Handler
		principal       apimw.PrincipalValidator
		peer            apimw.ForwardedIdentityPeerVerifier
	}{
		"missing validation":       {executionEvents: handler, principal: principal, peer: peer},
		"missing execution events": {validation: handler, principal: principal, peer: peer},
		"missing principal":        {validation: handler, executionEvents: handler, peer: peer},
		"missing peer proof":       {validation: handler, executionEvents: handler, principal: principal},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewProductionRuntimeRoutes(
				test.validation,
				test.executionEvents,
				test.principal,
				test.peer,
				apimw.AuthConfig{},
			)
			if !errors.Is(err, ErrInvalidProductionRuntimeRoutes) {
				t.Fatalf("error = %v, want %v", err, ErrInvalidProductionRuntimeRoutes)
			}
		})
	}
}

func TestProductionRuntimeRoutesAcceptOnlyVerifiedForwardedPrincipal(t *testing.T) {
	handlerCalls := 0
	principalCalls := 0
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handlerCalls++
		principal, ok := auth.RuntimePrincipalFromContext(request.Context())
		if !ok || principal.ID != "7" || principal.UserID != "7" || principal.Email != "active@example.test" {
			t.Fatalf("runtime principal = %+v, present=%v", principal, ok)
		}
		source, ok := auth.AuthenticationSourceFromContext(request.Context())
		if !ok || source != auth.AuthenticationSourceForwarded {
			t.Fatalf("authentication source = %d, present=%v", source, ok)
		}
		writer.WriteHeader(http.StatusNoContent)
	})
	principal := productionRuntimePrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
		principalCalls++
		if user.ID != "7" || user.UserID != "7" || user.AuthType != "user" {
			t.Fatalf("unverified principal shape = %+v", user)
		}
		user.Email = "active@example.test"
		return user, nil
	})
	peer := productionRuntimePeerVerifierFunc(func(request *http.Request) error {
		if request.RemoteAddr != "10.0.0.8:43120" {
			return errors.New("untrusted proxy peer")
		}
		return nil
	})
	routes, err := NewProductionRuntimeRoutes(handler, handler, principal, peer, apimw.AuthConfig{})
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter(RouterConfig{ProductionRuntime: routes})

	for _, request := range []*http.Request{
		forwardedRuntimeRequest(http.MethodPost, "/api/v2/configurations/validation/42/revision-1", "10.0.0.8:43120"),
		forwardedRuntimeRequest(http.MethodGet, "/api/v2/executions/42/execution-1/events", "10.0.0.8:43120"),
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("%s %s status=%d body=%s", request.Method, request.URL.Path, response.Code, response.Body.String())
		}
	}
	if handlerCalls != 2 || principalCalls != 2 {
		t.Fatalf("handler calls=%d principal calls=%d, want 2 each", handlerCalls, principalCalls)
	}

	for name, request := range map[string]*http.Request{
		"forged forwarded headers": forwardedRuntimeRequest(
			http.MethodPost,
			"/api/v2/configurations/validation/42/revision-1",
			"192.0.2.9:443",
		),
		"alternate bearer": func() *http.Request {
			request := httptest.NewRequest(http.MethodGet, "/api/v2/executions/42/execution-1/events", nil)
			request.Header.Set("Authorization", "Bearer not-a-forwarded-principal")
			return request
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d, want=%d body=%s", response.Code, http.StatusUnauthorized, response.Body.String())
			}
		})
	}
	if handlerCalls != 2 || principalCalls != 2 {
		t.Fatalf("denied request reached protected path: handler=%d principal=%d", handlerCalls, principalCalls)
	}
}

func TestProductionRuntimeRoutesRejectPrincipalValidationFailure(t *testing.T) {
	handlerCalls := 0
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { handlerCalls++ })
	routes, err := NewProductionRuntimeRoutes(
		handler,
		handler,
		productionRuntimePrincipalValidatorFunc(func(context.Context, auth.User) (auth.User, error) {
			return auth.User{}, auth.ErrPrincipalInactive
		}),
		productionRuntimePeerVerifierFunc(func(*http.Request) error { return nil }),
		apimw.AuthConfig{},
	)
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	NewRouter(RouterConfig{ProductionRuntime: routes}).ServeHTTP(
		response,
		forwardedRuntimeRequest(http.MethodPost, "/api/v2/configurations/validation/42/revision-1", "10.0.0.8:43120"),
	)
	if response.Code != http.StatusUnauthorized || handlerCalls != 0 {
		t.Fatalf("status=%d handler calls=%d body=%s", response.Code, handlerCalls, response.Body.String())
	}
}

// TestProductionRuntimeRoutesRejectDevelopmentFallback pins that
// AUTH_DEV_MODE is INERT, not merely unused. The bypass it once enabled is
// deleted (ADR-0017, #260), but a deleted feature leaves no failing test
// behind — so this keeps setting the variable and asserts the request is
// still rejected. Reintroducing any env-var-driven principal injection into
// middleware.Auth fails here.
//
// The router-level guard lives in cmd/elitea-main: AUTH_DEV_MODE=true is now
// a fatal startup error. This is the middleware-level half.
func TestProductionRuntimeRoutesRejectDevelopmentFallback(t *testing.T) {
	t.Setenv("AUTH_DEV_MODE", "true")
	handlerCalls := 0
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { handlerCalls++ })
	routes, err := NewProductionRuntimeRoutes(
		handler,
		handler,
		productionRuntimePrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
			return user, nil
		}),
		productionRuntimePeerVerifierFunc(func(*http.Request) error { return nil }),
		apimw.AuthConfig{},
	)
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	NewRouter(RouterConfig{ProductionRuntime: routes}).ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/api/v2/executions/42/execution-1/events", nil),
	)
	if response.Code != http.StatusUnauthorized || handlerCalls != 0 {
		t.Fatalf("status=%d handler calls=%d body=%s", response.Code, handlerCalls, response.Body.String())
	}
}

func TestProductionRuntimeRoutesKeepIndexStartUnmountedWithoutCompleteDataPlane(t *testing.T) {
	handlerCalls := 0
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { handlerCalls++ })
	routes, err := NewProductionRuntimeRoutes(
		handler,
		handler,
		productionRuntimePrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
			return user, nil
		}),
		productionRuntimePeerVerifierFunc(func(*http.Request) error { return nil }),
		apimw.AuthConfig{},
	)
	if err != nil {
		t.Fatal(err)
	}

	// Dispatch and output validation can run safely before worker credential
	// redemption and artifact upload exist. The public start route cannot: it
	// would admit work that has no authorized path to completion.
	request := forwardedRuntimeRequest(
		http.MethodPost,
		"/api/v2/elitea_core/test_toolkit_tool/prompt_lib/7?await_response=false",
		"10.0.0.8:43120",
	)
	response := httptest.NewRecorder()
	reviewedRoutesRouter(RouterConfig{ProductionRuntime: routes}).ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || handlerCalls != 0 {
		t.Fatalf("%s unexpectedly mounted: status=%d handler_calls=%d", indexingapi.CurrentIndexStartPath, response.Code, handlerCalls)
	}
}

func forwardedRuntimeRequest(method, target, remoteAddress string) *http.Request {
	request := httptest.NewRequest(method, target, nil)
	request.RemoteAddr = remoteAddress
	request.Header.Set("X-Auth-Type", "user")
	request.Header.Set("X-Auth-ID", "7")
	return request
}

type productionRuntimeTokenValidatorFunc func(context.Context, string) (auth.User, error)

func (function productionRuntimeTokenValidatorFunc) ValidateToken(ctx context.Context, token string) (auth.User, error) {
	return function(ctx, token)
}

// TestProductionRuntimeRoutesAcceptAPersonalAccessToken is #289 (regression
// finding F2). A personal access token starts a turn on the agent-start route,
// which takes the group's credentials, and the events URL that start answers
// with must accept the SAME token. Before the fix the group's Validator was
// dropped here, so a bearer was refused 401 `token_rejected` on the events
// stream of a run it had just admitted.
func TestProductionRuntimeRoutesAcceptAPersonalAccessToken(t *testing.T) {
	handlerCalls := 0
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handlerCalls++
		principal, ok := auth.RuntimePrincipalFromContext(request.Context())
		if !ok || principal.UserID != "10" {
			t.Errorf("runtime principal = %+v, present=%v", principal, ok)
		}
		source, _ := auth.AuthenticationSourceFromContext(request.Context())
		if source != auth.AuthenticationSourceToken && source != auth.AuthenticationSourceAPIKey {
			t.Errorf("authentication source = %d, want token or API key", source)
		}
		writer.WriteHeader(http.StatusOK)
	})
	validator := productionRuntimeTokenValidatorFunc(func(_ context.Context, token string) (auth.User, error) {
		if token != "pat-of-user-10" {
			return auth.User{}, auth.ErrCredentialRejected
		}
		return auth.User{ID: "10", UserID: "10", TokenID: "3", AuthType: "token"}, nil
	})
	principal := productionRuntimePrincipalValidatorFunc(func(_ context.Context, user auth.User) (auth.User, error) {
		return user, nil
	})
	peer := productionRuntimePeerVerifierFunc(func(*http.Request) error { return errors.New("no edge here") })
	routes, err := NewProductionRuntimeRoutes(handler, handler, principal, peer, apimw.AuthConfig{
		Validator:          validator,
		PrincipalValidator: principal,
	})
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter(RouterConfig{ProductionRuntime: routes})

	const eventsPath = "/api/v2/executions/6/c878ec191be8ebb35feb4315a65f695f/events"
	for _, test := range []struct {
		name, method, path, header, value string
		want                              int
	}{
		{"bearer PAT on the events stream", http.MethodGet, eventsPath, "Authorization", "Bearer pat-of-user-10", http.StatusOK},
		{"API key on the events stream", http.MethodGet, eventsPath, "X-API-Key", "pat-of-user-10", http.StatusOK},
		{"bearer PAT on configuration validation", http.MethodPost, "/api/v2/configurations/validation/6/revision-1", "Authorization", "Bearer pat-of-user-10", http.StatusOK},
		{"an unknown bearer is still refused", http.MethodGet, eventsPath, "Authorization", "Bearer someone-else", http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			request.Header.Set(test.header, test.value)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.want, response.Body.String())
			}
		})
	}
	if handlerCalls != 3 {
		t.Fatalf("handler calls = %d, want 3 (the refused bearer must not reach it)", handlerCalls)
	}
}
