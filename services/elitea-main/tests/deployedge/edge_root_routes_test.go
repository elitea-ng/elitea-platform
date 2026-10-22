// Root-mounted Go routes and the root redirect, gated at the browser edge.
//
// DEFECT 1. The `api` router rule is a hand-written list of paths. The Go
// router mounts route families at the root, outside /api/. Two of them were
// absent from that list: /icons/{projectID}/{filename} and
// /avatars/{projectID}/{filename} (internal/api/router.go, DownloadIcon and
// DownloadAvatar). No router matched them, so Traefik answered its own 404.
// Every uploaded project icon and author avatar rendered as a broken image.
// The upload handler returns exactly this URL shape to the browser. /artifacts/
// was the third such family and was remembered; these two were not.
//
// TestBrowserEdgesClassifyEveryRootMountedGoRoute closes the class. It walks
// the real Go router and checks every registered pattern against the rules in
// the edge file. A pattern that no rule forwards must carry a reason in
// `notAtTheBrowserEdge`. A new root-mounted family therefore cannot ship
// silently. The walk found three families that no rule forwarded: the two
// /app static icon file servers and /llm.
//
// DEFECT 2. The root redirect used the regex "^(https?://[^/]*)/$". Traefik
// matches redirectRegex against the whole request URI, query string included,
// and it passes a request that does not match straight to the next handler. A
// link of the form https://<host>/?utm_source=x therefore skipped the redirect
// and reached the SPA container at path "/", which its nginx does not serve.
//
// THE COMPOSE EDGE IS GO NOW (#1359 P2). Both defects above lived in Traefik's
// dynamic files, which are deleted. elitea-main is the compose browser edge
// (internal/api/webedge), so these gates send the paths through the real
// router with the web relay on and observe which process answers. The
// Gateway API edge, which still has a rule list, is read by
// edge_gateway_api_test.go with the shared classification at the end of this
// file.
package deployedge_test

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/adminui"
	v2auth "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/auth"
	dbrepos "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// The cluster runs a SECOND browser edge, a Gateway API HTTPRoute, and for a
// whole release this file could not see it (#568). edge_gateway_api_test.go
// reads it and holds it to the classification below, through the two helpers
// at the end of this file.

// rootMountedGoPaths are sample request paths that the Go router serves from
// the root, outside the /api/ prefix. Each names the router.go site that
// mounts it. A browser requests every one of them, so the edge must forward
// every one of them to elitea-main.
//
// This list is hand-written, and it is a floor. It gives each path a readable
// reason and a readable failure message.
// TestBrowserEdgesClassifyEveryRootMountedGoRoute is the complete check: it
// derives the paths from the router itself.
var rootMountedGoPaths = map[string]string{
	"/healthz":                      "router.go: health.RoutesWithDeps mounted at /",
	"/auth":                         "router.go: edgeAuth.ServeHTTP",
	"/auth/logout":                  "router.go: SessionHandler routes (auth_paths.go)",
	"/icons/1/abc.png":              "router.go: v2core.DownloadIcon — public, a browser <img src> carries no Authorization header",
	"/avatars/1/abc.png":            "router.go: v2social.DownloadAvatar — public for the same reason",
	"/artifacts/1/bucket/k":         "router.go: the artifacts download family",
	"/admin/app":                    "router.go: the admin SPA mount",
	"/api/v2/branding/bootstrap.js": "router.go: the branding bootstrap, inside /api/",
	"/api/v2/branding/pack.json":    "router.go: the brand pack as JSON (ADR-0025 decision 2), inside /api/",
	"/.well-known/elitea-client":    "router.go: the discovery document (ADR-0025 decision 1)",
	"/api/v2/branding/assets/logo-full/" + strings.Repeat("ab", 32) + ".svg": "router.go: uploaded brand assets, inside /api/, public like /icons",
}

// notForwardedGoPaths are root-mounted Go routes that must NOT reach this
// entrypoint. /readyz and /startupz report each dependency's state by name.
// The Helm probes hit the pod, and the Compose healthcheck curls 127.0.0.1 in
// the container. No caller needs these two paths here. Publication on the
// browser entrypoint only leaks internal state.
var notForwardedGoPaths = map[string]string{
	"/readyz":   "readinessHandler names each dependency's state",
	"/startupz": "the startup probe, same reason",
}

// composeEdgeForwards answers, by sending the request, whether the compose
// edge hands requestPath to elitea-main. The peer is a browser on a published
// port, not a loopback probe.
func composeEdgeForwards(edge *composeEdge) edgeForwards {
	return func(t *testing.T, requestPath string) bool {
		t.Helper()
		return edge.do(t, http.MethodGet, requestPath, nil).reachedMain
	}
}

func TestBrowserEdgesForwardEveryRootMountedGoRoute(t *testing.T) {
	edge := startComposeEdge(t, browserFacingRouterConfig(), browserRemoteAddr)
	for requestPath, why := range rootMountedGoPaths {
		answer := edge.do(t, http.MethodGet, requestPath, nil)
		if !answer.reachedMain {
			t.Errorf("the compose edge does not hand %s to elitea-main (%s); web=%v status=%d", requestPath, why, answer.reachedWeb, answer.status)
		}
	}
	for requestPath, why := range notForwardedGoPaths {
		answer := edge.do(t, http.MethodGet, requestPath, nil)
		if answer.reachedMain || answer.status != http.StatusNotFound {
			t.Errorf("the compose edge publishes %s to a browser (%s): status %d; it must answer 404", requestPath, why, answer.status)
		}
	}
}

// TestHealthIsPublicAtTheComposeEdge pins the one public probe (#569): a
// browser-port /healthz answers 200 from elitea-main with no session.
func TestHealthIsPublicAtTheComposeEdge(t *testing.T) {
	edge := startComposeEdge(t, browserFacingRouterConfig(), browserRemoteAddr)
	answer := edge.do(t, http.MethodGet, "/healthz", nil)
	if !answer.reachedMain || answer.status != http.StatusOK {
		t.Fatalf("GET /healthz through the compose edge: reachedMain=%v status=%d body=%q", answer.reachedMain, answer.status, answer.body)
	}
}

// TestComposeEdgeLetsALoopbackProbeRead is the other half of notForwardedGoPaths:
// the paths are withheld from a browser, not deleted. A probe inside the
// container reaches them over loopback.
func TestComposeEdgeLetsALoopbackProbeRead(t *testing.T) {
	edge := startComposeEdge(t, browserFacingRouterConfig(), "")
	for requestPath := range notForwardedGoPaths {
		if answer := edge.do(t, http.MethodGet, requestPath, nil); !answer.reachedMain {
			t.Errorf("a loopback GET %s did not reach elitea-main (status %d); the container's own probe would fail", requestPath, answer.status)
		}
	}
}

func TestRootRedirectKeepsTheQueryString(t *testing.T) {
	cases := []struct {
		method     string
		requestURI string
		status     int
		want       string
	}{
		{http.MethodGet, "/", http.StatusFound, "/app/"},
		{http.MethodHead, "/", http.StatusFound, "/app/"},
		{http.MethodGet, "/?utm_source=newsletter", http.StatusFound, "/app/?utm_source=newsletter"},
		{http.MethodGet, "/?a=1&b=2", http.StatusFound, "/app/?a=1&b=2"},
		// Not permanent, and a non-GET keeps its method.
		{http.MethodPost, "/", http.StatusTemporaryRedirect, "/app/"},
	}
	edge := startComposeEdge(t, browserFacingRouterConfig(), browserRemoteAddr)
	for _, testCase := range cases {
		answer := edge.do(t, testCase.method, testCase.requestURI, nil)
		if answer.reachedWeb {
			t.Errorf("%s %s reached the web container at path /, which its nginx does not serve", testCase.method, testCase.requestURI)
			continue
		}
		if answer.status != testCase.status {
			t.Errorf("%s %s answered %d, want %d (a 301/308 would be cached by the browser for good)", testCase.method, testCase.requestURI, answer.status, testCase.status)
		}
		if got := answer.header.Get("Location"); got != testCase.want {
			t.Errorf("%s %s redirects to %q, want %q", testCase.method, testCase.requestURI, got, testCase.want)
		}
	}
}

// notAtTheBrowserEdge names the router patterns that NO browser edge forwards,
// on purpose. Each entry gives the reason. The gate below fails on a pattern
// that is neither forwarded nor listed here, so a new root-mounted family
// cannot reach production without a decision.
//
// This map holds what is true of EVERY edge. An edge that alone cannot carry a
// route declares that beside itself — see notAtTheGatewayAPIEdge in
// edge_gateway_api_test.go — so the reason stays next to the edge it belongs
// to and never widens to the edges that do carry the route.
var notAtTheBrowserEdge = map[string]string{
	"/readyz":   "the readiness body names each dependency's state; the Helm probe and the compose healthcheck reach the pod directly",
	"/startupz": "the startup probe, for the same reason as /readyz",
}

// edgeForwards answers whether one edge sends requestPath to elitea-main. Each
// edge language gets its own implementation: a traefik rule list reads one way
// and a Gateway API match list reads another, and the classification below must
// not care which. The two gates then share ONE definition of "classified", so a
// change to that definition reaches every edge.
type edgeForwards func(t *testing.T, requestPath string) bool

// browserFacingRouterConfig builds the router with the browser-facing families
// composed. A field left empty removes routes from the walk, and this gate
// would then report nothing about them. The floor in walkRootMountedGoRoutes
// catches that.
func browserFacingRouterConfig() api.RouterConfig {
	pool := &pgxpool.Pool{}
	return api.RouterConfig{
		Pool:           pool,
		SessionHandler: &v2auth.SessionHandler{},
		OIDCHandler:    &v2auth.OIDCHandler{},
		AdminUI:        &adminui.Config{BasePath: "/admin/app"},
		AppsRepo:       dbrepos.NewApplicationsRepo(pool),
		ConvsRepo:      dbrepos.NewConversationsRepo(pool),
		SkillsRepo:     dbrepos.NewSkillsRepo(pool),
		FoldersRepo:    dbrepos.NewFoldersRepo(pool),
		TagsRepo:       dbrepos.NewTagsRepo(pool),
		AnalyticsRepo:  dbrepos.NewAnalyticsRepo(pool),
		WebhookRepo:    dbrepos.NewWebhooksRepo(pool),
		LLMProxy:       http.NotFoundHandler(),
	}
}

// requiredWalkedFamilies is the floor for the walk. The router must register
// each of these. An empty or shrunken walk would otherwise pass this gate and
// report nothing.
var requiredWalkedFamilies = []string{
	"/admin/app",
	"/app/application_icon",
	"/artifacts/",
	"/auth",
	"/auth/",
	"/avatars/",
	"/auth/",
	"/.well-known/elitea-client",
	"/healthz",
	"/icons/",
	"/llm/",
	"/readyz",
}

// walkRootMountedGoRoutes returns every pattern the Go router registers
// outside the /api/ prefix. The /api/ prefix is one rule on every edge file,
// so those patterns need no per-route check.
func walkRootMountedGoRoutes(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	walk := func(method, route string, handler http.Handler, middlewares ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/api/") {
			seen[route] = true
		}
		return nil
	}
	if err := chi.Walk(api.NewRouter(browserFacingRouterConfig()), walk); err != nil {
		t.Fatalf("walk the production router: %v", err)
	}
	patterns := make([]string, 0, len(seen))
	for pattern := range seen {
		patterns = append(patterns, pattern)
	}
	sort.Strings(patterns)
	for _, family := range requiredWalkedFamilies {
		if !hasPrefixIn(patterns, family) {
			t.Fatalf("the walk found no pattern under %s; the test config stopped composing it, so this gate reads an incomplete router: %v", family, patterns)
		}
	}
	return patterns
}

func hasPrefixIn(patterns []string, family string) bool {
	for _, pattern := range patterns {
		if strings.HasPrefix(pattern, family) {
			return true
		}
	}
	return false
}

// sampleRequestPath turns a chi pattern into a request path a browser sends.
// A parameter becomes "1", which also satisfies the numeric project id in the
// `mcp` router's PathRegexp. A wildcard becomes one segment.
func sampleRequestPath(pattern string) string {
	segments := strings.Split(pattern, "/")
	for index, segment := range segments {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			segments[index] = "1"
		}
		if segment == "*" {
			segments[index] = "sample"
		}
	}
	return strings.Join(segments, "/")
}

// TestBrowserEdgesClassifyEveryRootMountedGoRoute derives the check from the
// router, so a new root-mounted family cannot pass unnoticed. On the compose
// edge it also catches the opposite fault: a Go route the relay sends to the
// web container (a new route under /app or /docs), which no rule list would
// have shown.
func TestBrowserEdgesClassifyEveryRootMountedGoRoute(t *testing.T) {
	patterns := walkRootMountedGoRoutes(t)
	edge := startComposeEdge(t, browserFacingRouterConfig(), browserRemoteAddr)
	const relative = "compose edge (internal/api/webedge)"
	forwards := composeEdgeForwards(edge)
	assertPatternsClassified(t, relative, patterns, forwards, notAtTheBrowserEdge)
	assertNoStaleExclusions(t, relative, patterns, forwards, notAtTheBrowserEdge)
}

// assertPatternsClassified fails on a root-mounted route that this edge neither
// forwards nor excludes with a reason.
func assertPatternsClassified(t *testing.T, relative string, patterns []string, forwards edgeForwards, excluded map[string]string) {
	t.Helper()
	for _, pattern := range patterns {
		if forwards(t, sampleRequestPath(pattern)) {
			continue
		}
		if _, listed := excluded[pattern]; listed {
			continue
		}
		t.Errorf("%s: the Go router serves %s and no rule forwards it; the edge answers its own 404. Add a rule, or add the pattern to the exclusion list for this edge with the reason", relative, pattern)
	}
}

// assertNoStaleExclusions keeps an exclusion list honest. An entry for a
// pattern the router dropped, or for a pattern the edge now forwards, states
// something untrue. The caller passes the list that applies to its edge.
func assertNoStaleExclusions(t *testing.T, relative string, patterns []string, forwards edgeForwards, excluded map[string]string) {
	t.Helper()
	registered := map[string]bool{}
	for _, pattern := range patterns {
		registered[pattern] = true
	}
	for pattern, why := range excluded {
		if !registered[pattern] {
			t.Errorf("the exclusion list names %s (%s), but the Go router does not register it; delete the entry", pattern, why)
			continue
		}
		if forwards(t, sampleRequestPath(pattern)) {
			t.Errorf("%s: the exclusion list says %s stays off this edge (%s), but a rule forwards it", relative, pattern, why)
		}
	}
}
