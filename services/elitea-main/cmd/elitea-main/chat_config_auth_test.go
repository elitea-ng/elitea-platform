package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/authcomposition"
)

// deactivatedPrincipals is the shape authsvc.PrincipalValidator takes when the
// row behind the session is gone or no longer active: it refuses the principal
// the cookie names. It refuses EVERY principal, so a test using it that still
// observes 200 has proved the validator was never consulted.
type deactivatedPrincipals struct{}

func (deactivatedPrincipals) ValidatePrincipal(context.Context, auth.User) (auth.User, error) {
	return auth.User{}, auth.ErrPrincipalInactive
}

// activePrincipals is the control. It mirrors what the real validator does on
// a live row — normalise the IDs and return the principal — so a 200 through
// it proves the session cookie, the secret and the middleware chain are all
// genuinely working, which is what makes the 401 above attributable to the
// validator and not to a malformed request.
type activePrincipals struct{}

func (activePrincipals) ValidatePrincipal(_ context.Context, principal auth.User) (auth.User, error) {
	principal.ID = principal.UserID
	return principal, nil
}

// TestChatConfigOIDCOnlyAuthRejectsADeactivatedSession exercises the real
// production composition (chatConfigAuthConfig) through the real middleware
// (apimw.Auth) with a real, validly signed session cookie.
//
// Before #301 the OIDC-only branch was `apimw.AuthConfig{SessionSecret: ...}`
// with no PrincipalValidator, and apimw.validatePrincipal returns the session
// user UNCHANGED when that field is nil. A deactivated user's unexpired cookie
// therefore reached the handler with 200. Deleting the PrincipalValidator
// field from chatConfigAuthConfig's OIDC-only branch turns the first row red.
func TestChatConfigOIDCOnlyAuthRejectsADeactivatedSession(t *testing.T) {
	const secret = "chat-config-session-secret"

	for _, testCase := range []struct {
		name       string
		principals interface {
			apimw.PrincipalValidator
			consulted() int
		}
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
			name:       "active principal is served",
			principals: &countingPrincipals{inner: activePrincipals{}},
			wantStatus: http.StatusOK,
			wantBody:   "",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// formGraph nil == the OIDC-only deployment. The three
			// production-auth arguments are nil there exactly as they are in
			// main.go, which is the condition that produced the defect.
			config := chatConfigAuthConfig(nil, nil, nil, testCase.principals, secret)

			var served bool
			handler := apimw.Auth(config)(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					served = true
					w.WriteHeader(http.StatusOK)
				},
			))

			request := httptest.NewRequest(
				http.MethodGet,
				"/api/v2/elitea_core/chat_config/prompt_lib/1",
				nil,
			)
			request.AddCookie(&http.Cookie{
				Name:  "elitea_session",
				Value: signedSessionCookie(t, secret, "42", time.Now().Add(time.Hour)),
			})
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if recorder.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)",
					recorder.Code, testCase.wantStatus, recorder.Body.String())
			}
			if testCase.wantBody != "" && !strings.Contains(recorder.Body.String(), testCase.wantBody) {
				t.Fatalf("body = %q, want it to contain %q", recorder.Body.String(), testCase.wantBody)
			}
			if served != (testCase.wantStatus == http.StatusOK) {
				t.Fatalf("handler served = %v, want %v — a refusal that still runs "+
					"the handler has leaked the read", served, testCase.wantStatus == http.StatusOK)
			}
			// A validator that is never called cannot enforce anything. This
			// is what separates "the config has a non-nil field" from "the
			// session is actually re-checked against the current principal".
			if testCase.principals.consulted() != 1 {
				t.Fatalf("PrincipalValidator consulted %d times, want 1: the "+
					"session was accepted without re-checking the principal (#301)",
					testCase.principals.consulted())
			}
		})
	}
}

// TestChatConfigOIDCOnlyBranchDoesNotReuseTheProductionValidator pins WHICH
// validator the OIDC-only branch takes. Reaching for `principalValidator` here
// would look like a fix and enforce nothing: that variable is assigned only
// inside main.go's `authEnabled` block, i.e. it is nil in precisely the branch
// that needs it.
func TestChatConfigOIDCOnlyBranchDoesNotReuseTheProductionValidator(t *testing.T) {
	production := &countingPrincipals{inner: activePrincipals{}}
	session := &countingPrincipals{inner: activePrincipals{}}

	config := chatConfigAuthConfig(nil, production, nil, session, "session-secret")

	if config.SessionSecret != "session-secret" {
		t.Fatalf("OIDC-only branch lost its SessionSecret: %q", config.SessionSecret)
	}
	if config.PrincipalValidator != apimw.PrincipalValidator(session) {
		t.Fatal("OIDC-only branch did not take the session principal validator; " +
			"the production one is nil in this branch and would enforce nothing (#301)")
	}
	if config.Validator != nil {
		t.Fatal("OIDC-only branch must not carry a token validator: a nil " +
			"*FormGraph in that interface field reads as configured")
	}
}

// TestChatConfigRouteUsesTheSharedAuthComposition guards the call site: the
// helper is only worth anything if main.go still routes through it. Every
// route test composes its own AuthConfig, so nothing else in the build reads
// what production actually wires (same reasoning as
// TestChatWriteRoutesAcceptABrowserSession).
func TestChatConfigRouteUsesTheSharedAuthComposition(t *testing.T) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	if !callPassesCallee(file, "NewCurrentRoutes", "chatConfigAuthConfig") {
		t.Fatal("the ungated chat_config composition no longer builds its " +
			"AuthConfig with chatConfigAuthConfig — the OIDC-only branch can " +
			"silently lose its PrincipalValidator again (#301)")
	}
}

// callPassesCallee reports whether any call to `outer` has, among its
// arguments, a call to `inner`. Both are matched on the trailing identifier so
// a package-qualified callee matches too.
func callPassesCallee(file *ast.File, outer, inner string) bool {
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || calleeName(call.Fun) != outer {
			return true
		}
		for _, argument := range call.Args {
			if nested, ok := argument.(*ast.CallExpr); ok && calleeName(nested.Fun) == inner {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

func calleeName(expr ast.Expr) string {
	switch fun := expr.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		if fun.Sel != nil {
			return fun.Sel.Name
		}
	}
	return ""
}

// countingPrincipals adapts a validator to the table's interface and counts.
type countingPrincipals struct {
	inner apimw.PrincipalValidator
	calls int
}

func (v *countingPrincipals) ValidatePrincipal(ctx context.Context, principal auth.User) (auth.User, error) {
	v.calls++
	return v.inner.ValidatePrincipal(ctx, principal)
}

func (v *countingPrincipals) consulted() int { return v.calls }

// signedSessionCookie mints the exact cookie shape verifySessionCookie accepts:
// base64url(JSON claims) + "." + hex(HMAC-SHA256(payload, secret)).
func signedSessionCookie(t *testing.T, secret, userID string, expiry time.Time) string {
	t.Helper()
	claims, err := json.Marshal(map[string]any{
		"uid":   userID,
		"email": "deactivated@example.com",
		"exp":   expiry.Unix(),
	})
	if err != nil {
		t.Fatalf("marshal session claims: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(claims)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

// TestChatConfigFormAuthAcceptsABrowserSession pins the credential the FORM
// branch had lost.
//
// `GET /api/v2/elitea_core/chat_config/prompt_lib/{projectID}` is called by
// apps/elitea-web's artifacts page and by nothing else. Only a browser calls
// it, and a browser's credential is the `elitea_session` cookie. The form
// branch carried no SessionSecret, so apimw.Auth's cookie branch was inert and
// the request fell through to `401 missing authorization header` — while every
// other /api/v2 route accepted the same cookie, because apiGroupAuthConfig's
// form branch has always carried the secret.
//
// The SPA reads a 401 as an expired session (`needsReauth` in
// apps/elitea-web/src/shared/api/http.ts is 401-only, deliberately), so the
// page opened a fresh OIDC round-trip on every visit.
//
// Removing `SessionSecret` from chatConfigAuthConfig's form branch turns this
// red. The deactivated row is the control: a 200 for BOTH principals would
// mean the cookie was accepted without re-checking the principal, which is the
// #301 defect in a second shape.
func TestChatConfigFormAuthAcceptsABrowserSession(t *testing.T) {
	const secret = "chat-config-form-session-secret"

	for _, testCase := range []struct {
		name       string
		principals *countingPrincipals
		wantStatus int
	}{
		{
			name:       "active principal is served",
			principals: &countingPrincipals{inner: activePrincipals{}},
			wantStatus: http.StatusOK,
		},
		{
			name:       "deactivated principal is still refused",
			principals: &countingPrincipals{inner: deactivatedPrincipals{}},
			wantStatus: http.StatusUnauthorized,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// A non-nil *FormGraph is the whole condition that selects the
			// branch. Its methods are never reached: no Authorization header
			// is sent, so the token validator is not consulted.
			config := chatConfigAuthConfig(
				&authcomposition.FormGraph{},
				testCase.principals,
				nil,
				nil,
				secret,
			)
			if config.SessionSecret != secret {
				t.Fatalf("form branch SessionSecret = %q, want %q: without it "+
					"apimw.Auth's cookie branch is inert and a browser has no "+
					"credential this route accepts", config.SessionSecret, secret)
			}

			handler := apimw.Auth(config)(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
			))
			request := httptest.NewRequest(
				http.MethodGet,
				"/api/v2/elitea_core/chat_config/prompt_lib/1",
				nil,
			)
			request.AddCookie(&http.Cookie{
				Name:  "elitea_session",
				Value: signedSessionCookie(t, secret, "42", time.Now().Add(time.Hour)),
			})
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if recorder.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)",
					recorder.Code, testCase.wantStatus, recorder.Body.String())
			}
			if testCase.principals.consulted() != 1 {
				t.Fatalf("PrincipalValidator consulted %d times, want 1",
					testCase.principals.consulted())
			}
		})
	}
}
