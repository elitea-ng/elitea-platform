// The cookie-less paths every browser edge forwards, held to the forward-auth
// policy (#569).
//
// DEFECT. Three lists describe the same fact — "a browser reaches this path
// with no credential" — and each one is hand-written:
//
//   - the `api` router rule in deploy/traefik/dynamic.yml (and its e2e twin);
//   - the elitea-main HTTPRoute in deploy/gateway-api/httproute.yaml;
//   - internal/api/main_public_rules.go, which the cluster's forward-auth
//     ExtensionRef consults BEFORE it forwards.
//
// The edge gates in this directory hold the first two to the Go router. Nothing
// held the third to them. `/healthz` was on both edge lists and absent from the
// policy, so on the cluster a probe of the public hostname answered
//
//	HTTP/2 302
//	location: https://<host>/forward-auth/login?target_to=%2Fhealthz
//
// while every compose test stayed green, because the compose edge runs no
// forwardAuth at all (deploy/traefik/dynamic.yml, `strip-client-identity`).
//
// THE GATE. For each path below it drives the real browserauth.MainHandler,
// composed with the real api.CurrentMainRoutePublicRules(), exactly as the
// cluster's ExtensionRef calls /internal/forward-auth/main: the forwarded
// method, proto, host and URI of a cookie-less browser request. The answer
// must be a public 200, never a 302. It also checks that every browser edge
// forwards the path, so a path that leaves the edges cannot keep a stale
// public rule, and it holds the two probe siblings to the opposite: no edge
// forwards /readyz or /startupz, and the policy sends them to the login form.
//
// RUN IT WITH -count=1, for the reason edge_middlewares_test.go gives: the
// edge files live in deploy/, outside this module.
package deployedge_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/browserauth"
	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	forwardapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/forwardauth"
)

// cookielessEdgePaths are request paths a browser or a probe sends with no
// credential, and that EVERY browser edge forwards to elitea-main. Each names
// the reason it is public. Add a row when router.go registers a new route
// outside the auth groups and an edge forwards it; the row then fails until
// main_public_rules.go carries a matching rule.
var cookielessEdgePaths = map[string]string{
	"/healthz":                      "liveness. A load-balancer probe or an uptime monitor presents no credential; a 302 to the login form scores the service down (#569)",
	"/icons/1/abc.png":              "a browser <img src> carries no Authorization header",
	"/avatars/1/abc.png":            "same as /icons",
	"/app/application_icon/1":       "a Go file server under the SPA prefix, loaded by <img src>",
	"/app/application_tool_icon/1":  "same as /app/application_icon",
	"/api/v2/branding/bootstrap.js": "index.html loads it with a blocking <script src> before any session exists",
	"/api/v2/branding/assets/logo-full/" + strings.Repeat("ab", 32) + ".svg": "uploaded brand assets, loaded by <img src>, <link rel=icon> and @font-face",
}

// probeOnlyPaths are the two health siblings that stay OFF every edge and OUT
// of the policy. Their bodies name each dependency's state, and the Helm probes
// and the compose healthcheck reach the pod directly.
var probeOnlyPaths = []string{"/readyz", "/startupz"}

// panicCredentialAuthenticator fails the test if the policy ever consults a
// credential for a path that must be public.
type panicCredentialAuthenticator struct{ t *testing.T }

func (p panicCredentialAuthenticator) AuthenticateCredential(
	_ context.Context, source forwardapp.Source, _ forwardapp.CredentialInput,
) (forwardapp.CredentialResult, error) {
	p.t.Fatalf("the policy authenticated a credential for %s, so the path is not public", source.URI)
	return forwardapp.CredentialResult{}, nil
}

// panicSessionAuthorizer fails the test the same way for a browser session.
type panicSessionAuthorizer struct{ t *testing.T }

func (p panicSessionAuthorizer) Authorize(context.Context, string) (browserapp.Authorization, error) {
	p.t.Fatal("the policy authorized a browser session for a request that carried none")
	return browserapp.Authorization{}, nil
}

// productionForwardAuthHandler composes the handler the way cmd/elitea-main
// does: the real public-rule catalog behind the real kernel and the real
// trusted-proxy resolver. Only the two credential paths are stubbed, and they
// fail the test when reached.
func productionForwardAuthHandler(t *testing.T) http.Handler {
	t.Helper()
	policy, err := forwardapp.NewPublicPolicy(api.CurrentMainRoutePublicRules())
	if err != nil {
		t.Fatalf("CurrentMainRoutePublicRules() does not compile into a policy: %v", err)
	}
	kernel, err := forwardapp.NewKernel(panicCredentialAuthenticator{t}, panicSessionAuthorizer{t}, policy)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := browserauth.NewTrustedProxyResolver(browserauth.TrustedProxyConfig{
		TrustedProxyCIDRs: []string{"10.0.0.0/8"},
		PublicOrigin:      "https://elitea.example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	cookies, err := browserauth.NewCookiePolicy(browserauth.CookieConfig{
		Name:     "centry_auth_session",
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Lifetime: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := browserauth.NewMainHandler(kernel, resolver, cookies, browserauth.MainConfig{
		CredentialHeaders: []browserauth.CredentialHeader{{Name: "X-API-Key", Type: "bearer"}},
		PublicOrigin:      "https://elitea.example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

// forwardAuthRequest is what an edge sends to /internal/forward-auth/main for
// one cookie-less browser request: the X-Forwarded-* tuple, nothing else.
func forwardAuthRequest(uri string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, browserauth.MainForwardAuthPath, nil)
	request.RemoteAddr = "10.1.2.3:43120"
	request.Header.Set("X-Forwarded-Method", http.MethodGet)
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-Forwarded-Host", "elitea.example.test")
	request.Header.Set("X-Forwarded-Uri", uri)
	request.Header.Set("X-Forwarded-For", "198.51.100.8")
	return request
}

// everyEdgeForwards reports, per edge file, whether the path reaches
// elitea-main. It reads the same files and the same matchers the other gates
// read, so one definition of "forwarded" serves all of them.
func everyEdgeForwards(t *testing.T, root, requestPath string) map[string]bool {
	t.Helper()
	forwarded := map[string]bool{}
	for _, relative := range browserEdgeFiles {
		rules := mainServiceRules(parseEdgeWithRules(t, filepath.Join(root, relative)))
		forwarded[relative] = anyRuleMatches(t, rules, requestPath)
	}
	for _, relative := range gatewayAPIEdgeFiles {
		rules := mainBackendRules(parseGatewayAPIEdge(t, root, relative))
		forwarded[relative] = gatewayRulesForward(t, relative, rules, requestPath)
	}
	return forwarded
}

// TestForwardAuthPolicyAdmitsEveryCookielessEdgePath is the gate.
func TestForwardAuthPolicyAdmitsEveryCookielessEdgePath(t *testing.T) {
	root := repoRoot(t)
	handler := productionForwardAuthHandler(t)

	for requestPath, why := range cookielessEdgePaths {
		t.Run(requestPath, func(t *testing.T) {
			for relative, forwards := range everyEdgeForwards(t, root, requestPath) {
				if !forwards {
					t.Errorf(
						"%s does not forward %s (%s).\n"+
							"A public rule for a path no edge carries is a stale rule; add the path to the "+
							"edge, or delete the row here together with its rule.",
						relative, requestPath, why,
					)
				}
			}

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, forwardAuthRequest(requestPath))
			if recorder.Code != http.StatusOK || recorder.Header().Get("X-Auth-Type") != "public" {
				t.Fatalf(
					"the forward-auth policy answers %s with %d (Location %q, X-Auth-Type %q), want a public 200.\n"+
						"Reason the path is public: %s.\n"+
						"Every browser edge forwards it, and the cluster's forward-auth ExtensionRef asks "+
						"internal/api/main_public_rules.go first. A path that is public in router.go and absent "+
						"there gets a 302 to the login form — the #569 shape. Add a `go.*` rule for it.",
					requestPath, recorder.Code, recorder.Header().Get("Location"),
					recorder.Header().Get("X-Auth-Type"), why,
				)
			}
		})
	}
}

// TestForwardAuthPolicyKeepsTheProbeSiblingsPrivate is the negative half. The
// readiness and startup bodies name each dependency's state, so they must stay
// off every edge AND out of the policy: a rule that admitted them would publish
// that state the day an edge forwarded them.
func TestForwardAuthPolicyKeepsTheProbeSiblingsPrivate(t *testing.T) {
	root := repoRoot(t)
	handler := productionForwardAuthHandler(t)

	for _, requestPath := range probeOnlyPaths {
		t.Run(requestPath, func(t *testing.T) {
			for relative, forwards := range everyEdgeForwards(t, root, requestPath) {
				if forwards {
					t.Errorf("%s forwards %s; the readiness body names each dependency's state and no caller reaches it through an edge", relative, requestPath)
				}
			}

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, forwardAuthRequest(requestPath))
			if recorder.Code != http.StatusFound || !strings.Contains(recorder.Header().Get("Location"), "/forward-auth/login?") {
				t.Fatalf(
					"the forward-auth policy answers %s with %d (Location %q), want the login redirect.\n"+
						"A public rule now admits a probe sibling. Only /healthz is public (go.health.healthz); "+
						"narrow the rule.",
					requestPath, recorder.Code, recorder.Header().Get("Location"),
				)
			}
		})
	}
}
