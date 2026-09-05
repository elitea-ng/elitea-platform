package main

import (
	"go/ast"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/authcomposition"
)

// configurationsWritePath is the credential CREATE route of the Configurations
// plane, with its real doubled "configurations" segment. An OIDC-only install
// has to be able to reach it: it is the only way to store an LLM credential
// without deploy/scripts/seed-llm-api.py or raw SQL (gap G2).
const configurationsWritePath = "/api/v2/configurations/configurations/1"

// configurationsUnregisteredPath is the discriminator for the registration
// assertion below. Chi runs the /api/v2 group middleware BEFORE it decides
// that no route matches, so an unauthenticated 401 proves nothing about
// registration — every unknown path under the group answers 401 too. This
// path is the control that keeps the 404 test honest.
const configurationsUnregisteredPath = "/api/v2/configurations/no_such_resource/1"

// TestConfigurationsGateAcceptsAnOIDCOnlyCredentialPlane pins the predicate the
// ELITEA_CONFIGURATIONS_ENABLED gate now asks.
//
// The gate used to read `formGraph == nil || principalValidator == nil ||
// forwardedIdentityVerifier == nil`. ELITEA_AUTH_CONFIG_FILE is the only thing
// that builds a FormGraph, so an install with corporate single sign-on and no
// Form document was refused the whole Configurations plane, however real its
// authentication. Restoring the FormGraph test turns the second row red.
//
// The third row is the half that must NOT move: a deployment with no
// credential plane at all is still refused. AUTH_DEV_MODE cannot reach here —
// developmentFlagsFromEnv rejects it at startup (ADR-0017).
func TestConfigurationsGateAcceptsAnOIDCOnlyCredentialPlane(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name string
		auth apimw.AuthConfig
		want bool
	}{
		{
			name: "production form authentication is admitted",
			auth: apiGroupAuthConfig(
				&authcomposition.FormGraph{},
				&countingPrincipals{inner: activePrincipals{}},
				&stubForwardedIdentityVerifier{},
				&countingPrincipals{inner: activePrincipals{}},
				&countingTokens{},
				apiGroupTestSecret,
				false,
			),
			want: true,
		},
		{
			name: "single sign-on without a form document is admitted",
			auth: apiGroupAuthConfig(
				nil, nil, nil,
				&countingPrincipals{inner: activePrincipals{}},
				&countingTokens{},
				apiGroupTestSecret,
				true,
			),
			want: true,
		},
		{
			name: "a deployment with no credential plane is refused",
			auth: apiGroupAuthConfig(
				nil, nil, nil,
				&countingPrincipals{inner: activePrincipals{}},
				&countingTokens{},
				apiGroupTestSecret,
				false,
			),
			want: false,
		},
		{
			// A credential reader with no principal validator is NOT an
			// authenticated deployment. apimw.validatePrincipal returns the
			// session user unchanged when the field is nil, so a deactivated
			// user's unexpired credential would open every configuration route
			// (#301, #314, #370).
			name: "a credential reader without a principal validator is refused",
			auth: apimw.AuthConfig{SessionSecret: apiGroupTestSecret},
			want: false,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := productionAuthenticationComposed(testCase.auth); got != testCase.want {
				t.Fatalf("productionAuthenticationComposed = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestConfigurationsWriteRouteIsRegisteredAndGatedOnOIDCOnly drives the real
// production router with the credential set an OIDC-only deployment composes.
//
// Two claims, and the second needs the first: the credential CREATE route is
// REGISTERED on that composition, and it refuses an anonymous caller. A 401 on
// its own would be worthless evidence of registration, so the unregistered
// sibling path proves the router really does answer 404 for a path it does not
// serve, and the authenticated row proves the 401 came from authentication
// rather than from a router that refuses everything.
func TestConfigurationsWriteRouteIsRegisteredAndGatedOnOIDCOnly(t *testing.T) {
	// formGraph nil and oidcSessionEnabled true == the OIDC-only deployment.
	config := apiGroupAuthConfig(
		nil, nil, nil, &countingPrincipals{inner: activePrincipals{}}, nil, apiGroupTestSecret, true,
	)
	if !productionAuthenticationComposed(config) {
		t.Fatal("the OIDC-only credential plane no longer satisfies the " +
			"Configurations gate; this deployment composes no configuration " +
			"routes at all (gap G2)")
	}
	router := newAPIGroupRouter(config, &reachedHandler{})

	anonymous := httptest.NewRecorder()
	router.ServeHTTP(anonymous, httptest.NewRequest(http.MethodPost, configurationsWritePath, nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous POST %s status = %d, want 401: the credential "+
			"create route is not authentication-gated (body %q)",
			configurationsWritePath, anonymous.Code, anonymous.Body.String())
	}

	withSession := func(path string) int {
		request := httptest.NewRequest(http.MethodPost, path, nil)
		request.AddCookie(&http.Cookie{
			Name:  "elitea_session",
			Value: signedSessionCookie(t, apiGroupTestSecret, "42", time.Now().Add(time.Hour)),
		})
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		return recorder.Code
	}

	if status := withSession(configurationsUnregisteredPath); status != http.StatusNotFound {
		t.Fatalf("authenticated POST %s status = %d, want 404: this test "+
			"cannot tell a registered route from an unregistered one",
			configurationsUnregisteredPath, status)
	}
	if status := withSession(configurationsWritePath); status == http.StatusNotFound {
		t.Fatalf("authenticated POST %s status = 404: the credential create "+
			"route is not registered on an OIDC-only composition (gap G2)",
			configurationsWritePath)
	}
}

// TestConfigurationsGateReadsTheSharedAuthComposition guards the call site.
// The predicate is only worth anything if main.go still asks it, and no router
// test reads main.go's own gate.
func TestConfigurationsGateReadsTheSharedAuthComposition(t *testing.T) {
	file := parseMainFile(t)

	if !identifierIsArgument(file, "productionAuthenticationComposed", "apiGroupAuth") {
		t.Fatal("main.go no longer asks productionAuthenticationComposed(apiGroupAuth); " +
			"the ELITEA_CONFIGURATIONS_ENABLED gate can silently return to " +
			"testing the FormGraph, which no single-sign-on install has (gap G2)")
	}
	if !assignsIdentifier(file, "currentAuth", "apiGroupAuth") {
		t.Fatal("the Configurations routes no longer authenticate with the " +
			"apiGroupAuth composition; an inline AuthConfig built from " +
			"formGraph is nil on every OIDC-only deployment (gap G2)")
	}
}

// identifierIsArgument reports whether any call to `callee` passes the bare
// identifier `argument`.
func identifierIsArgument(file *ast.File, callee, argument string) bool {
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || calleeName(call.Fun) != callee {
			return true
		}
		for _, candidate := range call.Args {
			if ident, ok := candidate.(*ast.Ident); ok && ident.Name == argument {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// assignsIdentifier reports whether `name` is assigned the bare identifier
// `value` anywhere in the file.
func assignsIdentifier(file *ast.File, name, value string) bool {
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		target, ok := assign.Lhs[0].(*ast.Ident)
		if !ok || target.Name != name {
			return true
		}
		if source, ok := assign.Rhs[0].(*ast.Ident); ok && source.Name == value {
			found = true
			return false
		}
		return true
	})
	return found
}
