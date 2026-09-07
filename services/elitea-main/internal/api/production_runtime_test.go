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
