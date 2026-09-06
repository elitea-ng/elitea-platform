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

// TestChatConfigAuthConfigReturnsAPIGroupAuthUnchanged pins the fix: since #813
// chatConfigAuthConfig no longer re-composes an AuthConfig from raw
// dependencies — it returns apiGroupAuth as-is. Every field must therefore
// survive the call untouched, the OIDC-only token validator (`Validator`)
// included. Re-introducing a branch that builds a new
// apimw.AuthConfig{...} literal here — even one that copies every field it
// remembers — reopens the exact drift this closed: a field added to
// apiGroupAuthConfig later has no reason to also be added here.
func TestChatConfigAuthConfigReturnsAPIGroupAuthUnchanged(t *testing.T) {
	tokens := &countingTokens{}
	principals := &countingPrincipals{inner: activePrincipals{}}
	forwardedIdentity := stubForwardedIdentityVerifier{}

	apiGroupAuth := apimw.AuthConfig{
		Validator:                 tokens,
		PrincipalValidator:        principals,
		ForwardedIdentityVerifier: forwardedIdentity,
		SessionSecret:             "api-group-secret",
	}

	got := chatConfigAuthConfig(apiGroupAuth)

	if got.Validator != apimw.TokenValidator(tokens) {
		t.Fatal("chatConfigAuthConfig dropped apiGroupAuth.Validator — the " +
			"OIDC-only token validator this fix added would silently vanish again")
	}
	if got.PrincipalValidator != apimw.PrincipalValidator(principals) {
		t.Fatal("chatConfigAuthConfig dropped apiGroupAuth.PrincipalValidator (#301)")
	}
	if got.ForwardedIdentityVerifier != apimw.ForwardedIdentityPeerVerifier(forwardedIdentity) {
		t.Fatal("chatConfigAuthConfig dropped apiGroupAuth.ForwardedIdentityVerifier")
	}
	if got.SessionSecret != apiGroupAuth.SessionSecret {
		t.Fatalf("chatConfigAuthConfig.SessionSecret = %q, want %q",
			got.SessionSecret, apiGroupAuth.SessionSecret)
	}
}

// TestChatConfigOIDCOnlyAuthAcceptsAPersonalAccessToken is the regression test
// for the defect this fix closes.
//
// apiGroupAuthConfig's OIDC-only branch carries `Validator: sessionTokens`,
// the pool-backed personal-access-token validator (see its comment for the
// deliberate widening — a PAT works on every OTHER /api/v2 route in that
// deployment shape). chatConfigAuthConfig used to recompose its own
// AuthConfig from the same raw inputs and never gained that field, so the
// SAME token that authenticated every other route answered 401 on
// GET /api/v2/elitea_core/chat_config/prompt_lib/{projectID}.
//
// chatConfigAuthConfig now returns apiGroupAuth unchanged, so the OIDC-only
// shape below — Validator set, no FormGraph involved — carries the token
// validator by construction. countingTokens and apiGroupTestToken are the
// same fixtures api_group_auth_test.go drives the equivalent /api/v2-group
// assertion with. A config missing `Validator` (the pre-fix shape) turns the
// first two rows red with 401.
func TestChatConfigOIDCOnlyAuthAcceptsAPersonalAccessToken(t *testing.T) {
	const secret = "chat-config-oidc-session-secret"

	for _, testCase := range []struct {
		name       string
		header     string
		value      string
		wantStatus int
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
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			tokens := &countingTokens{}
			principals := &countingPrincipals{inner: activePrincipals{}}

			// The OIDC-only shape apiGroupAuthConfig builds when formGraph is
			// nil and oidcSessionEnabled is true: SessionSecret, the session
			// PrincipalValidator, and the pool-backed token Validator.
			apiGroupAuth := apimw.AuthConfig{
				Validator:          tokens,
				PrincipalValidator: principals,
				SessionSecret:      secret,
			}
			config := chatConfigAuthConfig(apiGroupAuth)

			handler := apimw.Auth(config)(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
			))

			request := httptest.NewRequest(
				http.MethodGet,
				"/api/v2/elitea_core/chat_config/prompt_lib/1",
				nil,
			)
			request.Header.Set(testCase.header, testCase.value)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if recorder.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)",
					recorder.Code, testCase.wantStatus, recorder.Body.String())
			}
			if tokens.consulted() != 1 {
				t.Fatalf("TokenValidator consulted %d times, want 1: the "+
					"chat_config route carries no token validator in an "+
					"OIDC-only deployment, so no personal access token this "+
					"deployment issues can ever work", tokens.consulted())
			}
			wantPrincipalChecks := 0
			if testCase.wantStatus == http.StatusOK {
				wantPrincipalChecks = 1
			}
			if principals.consulted() != wantPrincipalChecks {
				t.Fatalf("PrincipalValidator consulted %d times, want %d",
					principals.consulted(), wantPrincipalChecks)
			}
		})
	}
}

// TestChatConfigOIDCOnlyAuthRejectsADeactivatedSession exercises the real
// production composition (chatConfigAuthConfig) through the real middleware
// (apimw.Auth) with a real, validly signed session cookie, using the
// OIDC-only shape apiGroupAuth takes when formGraph is nil.
//
// Before #301 that shape was `apimw.AuthConfig{SessionSecret: ...}` with no
// PrincipalValidator, and apimw.validatePrincipal returns the session user
// UNCHANGED when that field is nil. A deactivated user's unexpired cookie
// therefore reached the handler with 200.
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
			// Validator nil, formGraph absent == the OIDC-only deployment
			// shape apiGroupAuth carries when formGraph is nil.
			apiGroupAuth := apimw.AuthConfig{
				SessionSecret:      secret,
				PrincipalValidator: testCase.principals,
			}
			config := chatConfigAuthConfig(apiGroupAuth)

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
// shape must carry.
//
// `GET /api/v2/elitea_core/chat_config/prompt_lib/{projectID}` is called by
// apps/elitea-web's artifacts page and by nothing else. Only a browser calls
// it, and a browser's credential is the `elitea_session` cookie. The form
// shape used to carry no SessionSecret, so apimw.Auth's cookie branch was
// inert and the request fell through to `401 missing authorization header` —
// while every other /api/v2 route accepted the same cookie, because
// apiGroupAuthConfig's form branch has always carried the secret.
//
// The SPA reads a 401 as an expired session (`needsReauth` in
// apps/elitea-web/src/shared/api/http.ts is 401-only, deliberately), so the
// page opened a fresh OIDC round-trip on every visit.
//
// A config missing `SessionSecret` here turns this red. The deactivated row
// is the control: a 200 for BOTH principals would mean the cookie was
// accepted without re-checking the principal, which is the #301 defect in a
// second shape.
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
			// A non-nil Validator is the whole condition that selects the
			// form shape in apiGroupAuthConfig. Its methods are never
			// reached here: no Authorization header is sent, so the token
			// validator is not consulted.
			apiGroupAuth := apimw.AuthConfig{
				Validator:          &authcomposition.FormGraph{},
				PrincipalValidator: testCase.principals,
				SessionSecret:      secret,
			}
			config := chatConfigAuthConfig(apiGroupAuth)
			if config.SessionSecret != secret {
				t.Fatalf("form shape SessionSecret = %q, want %q: without it "+
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
