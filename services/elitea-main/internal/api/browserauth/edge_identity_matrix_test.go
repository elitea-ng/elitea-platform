package browserauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	forwardapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/edgeauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// These tests run the real EdgeAuth handler, copy its response headers onto
// the downstream request the way the edge does, and authenticate that request
// with the real Auth middleware and the same TrustedProxyResolver. Nothing in
// between is stubbed except the credential stores.

const matrixRoute = "/api/v2/projects/7"

var errUnknownBearer = errors.New("unknown bearer")

// bearerTokenValidator accepts exactly one bearer and names its owner.
type bearerTokenValidator struct {
	token string
	user  auth.User
}

func (v bearerTokenValidator) ValidateToken(_ context.Context, token string) (auth.User, error) {
	if token != v.token {
		return auth.User{}, errUnknownBearer
	}
	return v.user, nil
}

type matrixOutcome struct {
	status    int
	principal auth.User
	source    auth.AuthenticationSource
}

func serveThroughAuth(t *testing.T, config apimw.AuthConfig, request *http.Request) matrixOutcome {
	t.Helper()
	var outcome matrixOutcome
	handler := apimw.Auth(config)(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		outcome.principal, _ = auth.UserFromContext(request.Context())
		outcome.source, _ = auth.AuthenticationSourceFromContext(request.Context())
		writer.WriteHeader(http.StatusNoContent)
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	outcome.status = recorder.Code
	return outcome
}

func activePrincipals() apimw.PrincipalValidator {
	return corePrincipalValidatorFunc(func(_ context.Context, principal auth.User) (auth.User, error) {
		return principal, nil
	})
}

// edgeProjection asks the real Main EdgeAuth handler to authorize uri and
// returns the headers the edge copies onto the upstream request.
func edgeProjection(t *testing.T, handler *MainHandler, uri string, credential func(*http.Request)) http.Header {
	t.Helper()
	request := mainRequest(uri)
	credential(request)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	requireCoreOK(t, recorder)
	projected := http.Header{}
	for _, name := range []string{"X-Auth-Type", "X-Auth-ID", "X-Auth-User-ID", "X-Auth-Reference", IdentitySignatureHeader} {
		if value := recorder.Header().Get(name); value != "" {
			projected.Set(name, value)
		}
	}
	return projected
}

func upstreamRequest(method, uri string, headers http.Header) *http.Request {
	request := httptest.NewRequest(method, uri, nil)
	request.RemoteAddr = "10.9.9.9:51000"
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	return request
}

func newMatrixMainHandler(t *testing.T) *MainHandler {
	t.Helper()
	return newMainTestHandler(t,
		coreCredentialFunc(func(context.Context, forwardapp.Source, forwardapp.CredentialInput) (forwardapp.CredentialResult, error) {
			return acceptedCoreToken(), nil
		}),
		coreSessionFunc(func(context.Context, string) (browserapp.Authorization, error) {
			return validCoreBrowserAuthorization(), nil
		}),
		[]forwardapp.PublicRule{},
	)
}

func withSessionCookie(request *http.Request) {
	request.AddCookie(&http.Cookie{Name: "centry_auth_session", Value: CookieValuePrefix + canonicalSessionID(5)})
}

func withProjectToken(request *http.Request) {
	request.Header.Set("Authorization", "Bearer project-token")
}

func TestEdgeIdentityAuthenticationMatrix(t *testing.T) {
	handler := newMatrixMainHandler(t)
	resolver := handler.sources
	config := apimw.AuthConfig{
		ForwardedIdentityVerifier: resolver,
		PrincipalValidator:        activePrincipals(),
		Validator: bearerTokenValidator{
			token: "own-token",
			user:  auth.User{ID: "9", UserID: "9", TokenID: "90", AuthType: "token"},
		},
	}

	sessionProjection := edgeProjection(t, handler, matrixRoute, withSessionCookie)
	tokenProjection := edgeProjection(t, handler, matrixRoute, withProjectToken)

	unsigned := func(authType, id, userID string) http.Header {
		header := http.Header{}
		header.Set("X-Auth-Type", authType)
		header.Set("X-Auth-ID", id)
		header.Set("X-Auth-User-ID", userID)
		return header
	}
	cloneWith := func(source http.Header, name, value string) http.Header {
		header := source.Clone()
		header.Set(name, value)
		return header
	}

	cases := []struct {
		name    string
		request func() *http.Request
		config  apimw.AuthConfig
		want    matrixOutcome
	}{
		{
			name:    "unauthenticated",
			request: func() *http.Request { return upstreamRequest(http.MethodGet, matrixRoute, nil) },
			want:    matrixOutcome{status: http.StatusUnauthorized},
		},
		{
			name: "session user through the edge",
			request: func() *http.Request {
				return upstreamRequest(http.MethodGet, matrixRoute, sessionProjection)
			},
			want: matrixOutcome{
				status:    http.StatusNoContent,
				principal: auth.User{ID: "7", UserID: "7", AuthType: "user"},
				source:    auth.AuthenticationSourceForwarded,
			},
		},
		{
			name: "token through the edge",
			request: func() *http.Request {
				return upstreamRequest(http.MethodGet, matrixRoute, tokenProjection)
			},
			want: matrixOutcome{
				status:    http.StatusNoContent,
				principal: auth.User{ID: "42", UserID: "7", TokenID: "42", AuthType: "token"},
				source:    auth.AuthenticationSourceForwarded,
			},
		},
		{
			name: "unsigned user projection from a trusted peer",
			request: func() *http.Request {
				return upstreamRequest(http.MethodGet, matrixRoute, unsigned("user", "1", "1"))
			},
			want: matrixOutcome{status: http.StatusUnauthorized},
		},
		{
			name: "unsigned token projection from a trusted peer",
			request: func() *http.Request {
				return upstreamRequest(http.MethodGet, matrixRoute, unsigned("token", "1", "1"))
			},
			want: matrixOutcome{status: http.StatusUnauthorized},
		},
		{
			name: "signed projection naming another user",
			request: func() *http.Request {
				return upstreamRequest(http.MethodGet, matrixRoute, cloneWith(sessionProjection, "X-Auth-ID", "1"))
			},
			want: matrixOutcome{status: http.StatusUnauthorized},
		},
		{
			name: "signed token projection naming another owner",
			request: func() *http.Request {
				return upstreamRequest(http.MethodGet, matrixRoute, cloneWith(tokenProjection, "X-Auth-User-ID", "1"))
			},
			want: matrixOutcome{status: http.StatusUnauthorized},
		},
		{
			name: "signed projection presented on another route",
			request: func() *http.Request {
				return upstreamRequest(http.MethodGet, "/api/v2/projects/8", sessionProjection)
			},
			want: matrixOutcome{status: http.StatusUnauthorized},
		},
		{
			name: "signed projection presented with another method",
			request: func() *http.Request {
				return upstreamRequest(http.MethodDelete, matrixRoute, sessionProjection)
			},
			want: matrixOutcome{status: http.StatusUnauthorized},
		},
		{
			name: "signed projection from a peer outside the trusted ranges",
			request: func() *http.Request {
				request := upstreamRequest(http.MethodGet, matrixRoute, sessionProjection)
				request.RemoteAddr = "198.51.100.20:443"
				return request
			},
			want: matrixOutcome{status: http.StatusUnauthorized},
		},
		{
			name: "unsigned projection beside the caller's own bearer",
			request: func() *http.Request {
				request := upstreamRequest(http.MethodGet, matrixRoute, unsigned("user", "1", "1"))
				request.Header.Set("Authorization", "Bearer own-token")
				return request
			},
			want: matrixOutcome{
				status:    http.StatusNoContent,
				principal: auth.User{ID: "9", UserID: "9", TokenID: "90", AuthType: "token"},
				source:    auth.AuthenticationSourceToken,
			},
		},
		{
			name: "projection when the deployment composes no verifier",
			request: func() *http.Request {
				return upstreamRequest(http.MethodGet, matrixRoute, sessionProjection)
			},
			config: apimw.AuthConfig{PrincipalValidator: activePrincipals()},
			want:   matrixOutcome{status: http.StatusUnauthorized},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			current := config
			if testCase.config.PrincipalValidator != nil {
				current = testCase.config
			}
			got := serveThroughAuth(t, current, testCase.request())
			if !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("outcome = %+v, want %+v", got, testCase.want)
			}
		})
	}
}

// A projection that names no one carries the explicit placeholder, so the edge
// overwrites any caller value, and it authenticates nobody.
func TestEdgeIdentityPublicProjectionAuthenticatesNobody(t *testing.T) {
	handler := newMainTestHandler(t,
		panicCoreCredential(t),
		panicCoreSession(t),
		[]forwardapp.PublicRule{{
			Name:       "public route",
			Conditions: []forwardapp.RuleCondition{{Field: forwardapp.SourceURI, Pattern: `/api/v2/public`}},
		}},
	)
	projection := edgeProjection(t, handler, "/api/v2/public", func(*http.Request) {})
	if got := projection.Get(IdentitySignatureHeader); got != "-" {
		t.Fatalf("%s = %q, want the placeholder", IdentitySignatureHeader, got)
	}
	got := serveThroughAuth(t, apimw.AuthConfig{
		ForwardedIdentityVerifier: handler.sources,
		PrincipalValidator:        activePrincipals(),
	}, upstreamRequest(http.MethodGet, "/api/v2/public", projection))
	if got.status != http.StatusUnauthorized {
		t.Fatalf("outcome = %+v, want 401", got)
	}
}

// The projection lives only as long as the edge hop needs.
func TestEdgeIdentityProjectionExpires(t *testing.T) {
	handler := newMatrixMainHandler(t)
	projection := edgeProjection(t, handler, matrixRoute, withSessionCookie)
	handler.sources.now = func() time.Time { return time.Now().Add(IdentityProjectionLifetime + 2*time.Second) }

	got := serveThroughAuth(t, apimw.AuthConfig{
		ForwardedIdentityVerifier: handler.sources,
		PrincipalValidator:        activePrincipals(),
	}, upstreamRequest(http.MethodGet, matrixRoute, projection))
	if got.status != http.StatusUnauthorized {
		t.Fatalf("outcome = %+v, want 401", got)
	}
}

// The compatibility Auth Core rpc projection is signed by the same rule.
func TestCoreHandlerRPCProjectionIsSignedForTheAuthorizedRequest(t *testing.T) {
	handler := newCoreTestHandler(t,
		panicCoreCredential(t),
		coreSessionFunc(func(context.Context, string) (browserapp.Authorization, error) {
			return validCoreBrowserAuthorization(), nil
		}),
		nil,
	)
	forwardRequest := coreRequest("/auth/check?target=rpc")
	withSessionCookie(forwardRequest)
	forwardResponse := httptest.NewRecorder()
	handler.ServeHTTP(forwardResponse, forwardRequest)
	requireCoreOK(t, forwardResponse)

	projected := http.Header{}
	for _, name := range []string{"X-Auth-Type", "X-Auth-ID", "X-Auth-User-ID", IdentitySignatureHeader} {
		projected.Set(name, forwardResponse.Header().Get(name))
	}
	config := apimw.AuthConfig{ForwardedIdentityVerifier: handler.sources, PrincipalValidator: activePrincipals()}

	if got := serveThroughAuth(t, config, upstreamRequest(http.MethodGet, "/api/private?project=7", projected)); got.status != http.StatusNoContent {
		t.Fatalf("authorized request outcome = %+v, want 204", got)
	}
	if got := serveThroughAuth(t, config, upstreamRequest(http.MethodGet, "/api/private?project=8", projected)); got.status != http.StatusUnauthorized {
		t.Fatalf("other request outcome = %+v, want 401", got)
	}
}
