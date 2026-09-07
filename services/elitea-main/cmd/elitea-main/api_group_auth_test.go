package main

import (
	"context"
	"go/ast"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api"
	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/authcomposition"
)

const apiGroupTestSecret = "api-group-session-secret"

// apiGroupTestPath is a real route inside the /api/v2 group. router.go mounts
// it first in r.Route("/api/v2", ...), from the RuntimeRoutes handlers the
// RouterConfig supplies, so a test can put its own handler behind the group's
// authentication middleware and read whether the request arrived.
const apiGroupTestPath = "/api/v2/configurations/validation/1/2"

// reachedHandler records that a request got past the group's authentication.
type reachedHandler struct{ calls int }

func (h *reachedHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.calls++
	w.WriteHeader(http.StatusOK)
}

// newAPIGroupRouter builds the REAL production router from the credential set
// apiGroupAuthConfig composes, wired exactly as main.go wires it. Nothing here
// injects the middleware directly: the four fields travel through
// api.RouterConfig into router.go, which builds one apimw.AuthConfig from them
// and installs it on the group that wraps r.Route("/api/v2", ...).
func newAPIGroupRouter(config apimw.AuthConfig, handler http.Handler) http.Handler {
	return api.NewRouter(api.RouterConfig{
		AuthValidator:      config.Validator,
		PrincipalValidator: config.PrincipalValidator,
		SessionSecret:      config.SessionSecret,
		Auth: api.AuthDeps{
			ForwardedIdentityVerifier: config.ForwardedIdentityVerifier,
		},
		RuntimeRoutes: api.RuntimeRoutes{
			Validation:      handler,
			ExecutionEvents: handler,
		},
	})
}

// TestAPIGroupOIDCOnlyAuthRejectsADeactivatedSession drives a real /api/v2
// route through the real router with a validly signed session cookie.
//
// Before this fix the OIDC-only branch set the session secret and left
// RouterConfig.PrincipalValidator nil, and apimw.validatePrincipal returns the
// session user UNCHANGED when that field is nil. A deactivated user's unexpired
// cookie therefore reached EVERY route in the group with 200 (#370). Deleting
// the PrincipalValidator field from apiGroupAuthConfig's OIDC-only branch turns
// the first row red.
func TestAPIGroupOIDCOnlyAuthRejectsADeactivatedSession(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		principals *countingPrincipals
		wantStatus int
		wantBody   string
	}{
		{
			name:       "deactivated principal is refused",
			principals: &countingPrincipals{inner: deactivatedPrincipals{}},
			wantStatus: http.StatusUnauthorized,
			wantBody:   "authenticated principal is inactive",
		},
		{
			// The control. Without it a change that refuses everybody — a
			// validator that rejects, or a lost session secret — would read as
			// a pass.
			name:       "active principal is served",
			principals: &countingPrincipals{inner: activePrincipals{}},
			wantStatus: http.StatusOK,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// formGraph nil and oidcSessionEnabled true == the OIDC-only
			// deployment. The two production-auth arguments are nil there
			// exactly as they are in main.go, which is the condition that
			// produced the defect.
			config := apiGroupAuthConfig(
				nil, nil, nil, testCase.principals, nil, apiGroupTestSecret, true, nil)
			handler := &reachedHandler{}
			router := newAPIGroupRouter(config, handler)

			request := httptest.NewRequest(http.MethodPost, apiGroupTestPath, nil)
			request.AddCookie(&http.Cookie{
				Name: "elitea_session",
				Value: signedSessionCookie(
					t, apiGroupTestSecret, "42", time.Now().Add(time.Hour),
				),
			})
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			if recorder.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)",
					recorder.Code, testCase.wantStatus, recorder.Body.String())
			}
			if testCase.wantBody != "" &&
				!strings.Contains(recorder.Body.String(), testCase.wantBody) {
				t.Fatalf("body = %q, want it to contain %q",
					recorder.Body.String(), testCase.wantBody)
			}
			// A validator that is never called cannot enforce anything. This is
			// what separates "the config has a non-nil field" from "the session
			// is actually re-checked against the current principal".
			if testCase.principals.consulted() != 1 {
				t.Fatalf("PrincipalValidator consulted %d times, want 1: the "+
					"session reached the /api/v2 group without re-checking the "+
					"principal (#370)", testCase.principals.consulted())
			}
			wantCalls := 0
			if testCase.wantStatus == http.StatusOK {
				wantCalls = 1
			}
			if handler.calls != wantCalls {
				t.Fatalf("route handler ran %d times, want %d: a refusal that "+
					"still runs the handler has already served the request it "+
					"was meant to withhold", handler.calls, wantCalls)
			}
		})
	}
}

// TestAPIGroupAuthConfigDoesNotReuseTheProductionValidator pins WHICH validator
// the OIDC-only branch takes. Reaching for `principalValidator` here would look
// like a fix and enforce nothing: that variable is assigned only inside
// main.go's `authEnabled` block, i.e. it is nil in precisely this branch (#314).
func TestAPIGroupAuthConfigDoesNotReuseTheProductionValidator(t *testing.T) {
	production := &countingPrincipals{inner: activePrincipals{}}
	session := &countingPrincipals{inner: activePrincipals{}}

	tokens := &countingTokens{}

	config := apiGroupAuthConfig(
		nil, production, nil, session, tokens, apiGroupTestSecret, true, nil)

	if config.SessionSecret != apiGroupTestSecret {
		t.Fatalf("the OIDC-only branch lost its SessionSecret: %q",
			config.SessionSecret)
	}
	if config.PrincipalValidator != apimw.PrincipalValidator(session) {
		t.Fatal("the OIDC-only branch did not take the session principal " +
			"validator; the production one is nil in this branch and would " +
			"enforce nothing (#370)")
	}
	if config.Validator != apimw.TokenValidator(tokens) {
		t.Fatal("the OIDC-only branch did not take the session token " +
			"validator; without it every personal access token this " +
			"deployment issues is refused with 401")
	}
	if _, isFormGraph := config.Validator.(*authcomposition.FormGraph); isFormGraph {
		t.Fatal("the OIDC-only branch took the production form graph: it is " +
			"nil in this branch, and a nil *FormGraph in that interface " +
			"field reads as configured (#86)")
	}
}

// TestAPIGroupAuthConfigKeepsTheProductionCredentials proves the fix did not
// move the deployment that already worked. With ELITEA_AUTH_CONFIG_FILE set the
// group must keep the token validator, the production principal validator, the
// forwarded identity verifier and the session secret.
func TestAPIGroupAuthConfigKeepsTheProductionCredentials(t *testing.T) {
	production := &countingPrincipals{inner: activePrincipals{}}
	session := &countingPrincipals{inner: activePrincipals{}}
	verifier := &stubForwardedIdentityVerifier{}
	formGraph := &authcomposition.FormGraph{}

	config := apiGroupAuthConfig(
		formGraph, production, verifier, session, &countingTokens{}, apiGroupTestSecret, true, nil)

	if config.Validator != apimw.TokenValidator(formGraph) {
		t.Fatal("the production branch lost its token validator: the form " +
			"graph owns every credential that deployment issues")
	}
	if config.PrincipalValidator != apimw.PrincipalValidator(production) {
		t.Fatal("the production branch must keep the production principal " +
			"validator, which is the one bound to the production auth graph")
	}
	if config.ForwardedIdentityVerifier != apimw.ForwardedIdentityPeerVerifier(verifier) {
		t.Fatal("the production branch lost its forwarded identity verifier: " +
			"the worker and the forward-auth edge authenticate with it")
	}
	if config.SessionSecret != apiGroupTestSecret {
		t.Fatalf("the production branch lost its SessionSecret: %q — the chat "+
			"UI authenticates with a session cookie and nothing else (#291)",
			config.SessionSecret)
	}
}

// TestAPIGroupAuthConfigAdmitsNothingWithoutACredentialPlane pins the third
// shape: no auth config file and no OIDC handler. The group must then carry no
// credential at all, because a session secret alone admits any cookie an
// operator can mint.
func TestAPIGroupAuthConfigAdmitsNothingWithoutACredentialPlane(t *testing.T) {
	session := &countingPrincipals{inner: activePrincipals{}}

	config := apiGroupAuthConfig(
		nil, nil, nil, session, &countingTokens{}, apiGroupTestSecret, false, nil)

	// AuthConfig holds a slice, so it is not comparable as a whole.
	if config.SessionSecret != "" {
		t.Fatalf("a deployment with no credential plane carries a session "+
			"secret: %q — it would admit any cookie signed with it, and with "+
			"no principal validator behind it", config.SessionSecret)
	}
	if config.Validator != nil || config.Client != nil {
		t.Fatal("a deployment with no credential plane carries a token validator")
	}
	if config.PrincipalValidator != nil {
		t.Fatal("a deployment with no credential plane carries a principal " +
			"validator: it validates a principal no credential can name")
	}
	if config.ForwardedIdentityVerifier != nil {
		t.Fatal("a deployment with no credential plane carries a forwarded " +
			"identity verifier")
	}
}

// TestAPIGroupAuthUsesTheSharedComposition guards the call site. The helper is
// only worth anything if main.go still routes the group through it, and every
// router test composes its own RouterConfig, so nothing else in the build reads
// what production actually wires.
func TestAPIGroupAuthUsesTheSharedComposition(t *testing.T) {
	file := parseMainFile(t)
	literal := routerConfigLiteral(t, file)

	for _, field := range []string{
		"AuthValidator", "PrincipalValidator", "SessionSecret",
	} {
		if !fieldReadsAPIGroupAuth(literal, field) {
			t.Fatalf("api.RouterConfig.%s in main.go no longer reads the "+
				"apiGroupAuth composition — the /api/v2 group can silently "+
				"lose its PrincipalValidator again (#370)", field)
		}
	}
}

// TestAPIGroupAuthConfigAlwaysTakesAPoolBackedValidator closes the trap the
// issue names. `apiGroupAuthConfig(..., principalValidator, ..., nil)` in the
// session position would read like a fix and enforce nothing, because main.go
// assigns that variable only inside the `authEnabled` block. Every call must
// therefore build a validator from the pool instead.
func TestAPIGroupAuthConfigAlwaysTakesAPoolBackedValidator(t *testing.T) {
	file := parseMainFile(t)

	calls, poolBacked := countArgumentCalls(
		file, "apiGroupAuthConfig", 3, "NewPrincipalValidator",
	)
	if calls == 0 {
		t.Fatal("main.go no longer calls apiGroupAuthConfig at all (#370)")
	}
	if calls != poolBacked {
		t.Fatalf("%d of %d apiGroupAuthConfig calls build their session "+
			"validator with authsvc.NewPrincipalValidator; the rest pass "+
			"something that is nil in this branch and enforce nothing (#370)",
			poolBacked, calls)
	}
}

// fieldReadsAPIGroupAuth reports whether the named RouterConfig field takes its
// value from the apiGroupAuth composition, as `apiGroupAuth.Something`.
func fieldReadsAPIGroupAuth(literal *ast.CompositeLit, field string) bool {
	found := false
	ast.Inspect(literal, func(node ast.Node) bool {
		keyValue, ok := node.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := keyValue.Key.(*ast.Ident)
		if !ok || key.Name != field {
			return true
		}
		selector, ok := keyValue.Value.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if source, ok := selector.X.(*ast.Ident); ok && source.Name == "apiGroupAuth" {
			found = true
			return false
		}
		return true
	})
	return found
}

// stubForwardedIdentityVerifier stands in for the production peer verifier.
type stubForwardedIdentityVerifier struct{}

func (stubForwardedIdentityVerifier) VerifyForwardedIdentityPeer(*http.Request) error {
	return nil
}

// countingTokens is a stand-in for the pool-backed personal-access-token
// validator. It accepts one token and counts every call. A test can therefore
// tell "the group has a non-nil field" apart from "the credential was really
// read".
type countingTokens struct {
	calls int
}

const apiGroupTestToken = "api-group-personal-access-token"

func (v *countingTokens) ValidateToken(_ context.Context, token string) (auth.User, error) {
	v.calls++
	if token != apiGroupTestToken {
		return auth.User{}, auth.ErrCredentialRejected
	}
	return auth.User{ID: "42", UserID: "42", TokenID: "7", AuthType: "token"}, nil
}

func (v *countingTokens) consulted() int { return v.calls }

// TestAPIGroupOIDCOnlyAuthAcceptsAPersonalAccessToken drives a Bearer
// credential and an X-API-Key credential through the real router in the
// OIDC-only composition.
//
// THE DEFECT. The OIDC-only branch of apiGroupAuthConfig set SessionSecret and
// PrincipalValidator, and left Validator and Client nil. That same
// SessionSecret is the personal-access-token signing key
// (router.go WithTokenSigningKey). POST /api/v2/auth/token therefore answered
// 200 and showed the user a token. tokens.go tokenServiceAvailable reads the
// pool and the key only. apimw.validateToken then found both Validator and
// Client nil and returned "authentication validator is not configured".
//
// EVERY Bearer request then answered 401 "token validation failed". Every
// X-API-Key request answered 401 "invalid api key". The deployment issued credentials that no
// route could accept. deploy/docker-compose.yml is exactly this shape.
//
// EVIDENCE. Remove `Validator: sessionTokens` from the OIDC-only branch and
// both accepted rows go red with 401.
func TestAPIGroupOIDCOnlyAuthAcceptsAPersonalAccessToken(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		header     string
		value      string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "bearer token is served",
			header:     "Authorization",
			value:      "Bearer " + apiGroupTestToken,
			wantStatus: http.StatusOK,
		},
		{
			name:       "api key is served",
			header:     "X-API-Key",
			value:      apiGroupTestToken,
			wantStatus: http.StatusOK,
		},
		{
			// The control. A validator that accepts everything would make the
			// two rows above pass while proving nothing.
			name:       "an unknown token is refused",
			header:     "Authorization",
			value:      "Bearer wrong-token",
			wantStatus: http.StatusUnauthorized,
			wantBody:   "token validation failed",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			principals := &countingPrincipals{inner: activePrincipals{}}
			tokens := &countingTokens{}
			config := apiGroupAuthConfig(
				nil, nil, nil, principals, tokens, apiGroupTestSecret, true, nil)
			handler := &reachedHandler{}
			router := newAPIGroupRouter(config, handler)

			request := httptest.NewRequest(http.MethodPost, apiGroupTestPath, nil)
			request.Header.Set(testCase.header, testCase.value)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			if recorder.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)",
					recorder.Code, testCase.wantStatus, recorder.Body.String())
			}
			if testCase.wantBody != "" &&
				!strings.Contains(recorder.Body.String(), testCase.wantBody) {
				t.Fatalf("body = %q, want it to contain %q",
					recorder.Body.String(), testCase.wantBody)
			}
			if tokens.consulted() != 1 {
				t.Fatalf("TokenValidator consulted %d times, want 1: the "+
					"/api/v2 group carries no token validator, so no personal "+
					"access token this deployment issues can ever work",
					tokens.consulted())
			}
			wantCalls := 0
			wantPrincipalChecks := 0
			if testCase.wantStatus == http.StatusOK {
				wantCalls = 1
				// A token credential must still re-check the principal: a
				// deactivated user's unrevoked token is not a live account.
				wantPrincipalChecks = 1
			}
			if principals.consulted() != wantPrincipalChecks {
				t.Fatalf("PrincipalValidator consulted %d times, want %d",
					principals.consulted(), wantPrincipalChecks)
			}
			if handler.calls != wantCalls {
				t.Fatalf("route handler ran %d times, want %d",
					handler.calls, wantCalls)
			}
		})
	}
}
