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
// RUN IT WITH -count=1, for the reason edge_middlewares_test.go gives: these
// files live in deploy/, outside this module, so the test cache does not track
// them.
package deployedge_test

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/adminui"
	v2auth "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/auth"
	dbrepos "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// This gate reads `browserEdgeFiles` from edge_identity_strip_test.go: the two
// edge files that serve a browser directly. The centry-hybrid set is out of
// scope for the same reason it is there. Its base.yml holds a catch-all to
// pylon, so a root-mounted Go path resolves by a different mechanism.
//
// The cluster runs a THIRD browser edge, a Gateway API HTTPRoute, and for a
// whole release this file could not see it (#568). edge_gateway_api_test.go
// now reads it and holds it to the classification below, through the two
// helpers at the end of this file.

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
	"/auth":                         "router.go: forwardAuth.ServeHTTP",
	"/forward-auth/logout":          "router.go: SessionHandler routes",
	"/icons/1/abc.png":              "router.go: v2core.DownloadIcon — public, a browser <img src> carries no Authorization header",
	"/avatars/1/abc.png":            "router.go: v2social.DownloadAvatar — public for the same reason",
	"/artifacts/1/bucket/k":         "router.go: the artifacts download family",
	"/admin/app":                    "router.go: the admin SPA mount",
	"/api/v2/branding/bootstrap.js": "router.go: the branding bootstrap, inside /api/",
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

// edgeWithRules is the subset of the dynamic configuration this gate reads.
type edgeWithRules struct {
	HTTP struct {
		Routers map[string]struct {
			Rule    string `yaml:"rule"`
			Service string `yaml:"service"`
		} `yaml:"routers"`
		Middlewares map[string]struct {
			RedirectRegex struct {
				Regex       string `yaml:"regex"`
				Replacement string `yaml:"replacement"`
			} `yaml:"redirectRegex"`
		} `yaml:"middlewares"`
	} `yaml:"http"`
}

func parseEdgeWithRules(t *testing.T, path string) edgeWithRules {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var parsed edgeWithRules
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return parsed
}

// matcherPattern extracts the path arguments of a Traefik rule. The rules this
// gate reads are flat disjunctions of Path, PathPrefix and PathRegexp, so a
// full expression parser is not needed. This gate reports any other matcher,
// and does not ignore it. An unread matcher makes this gate answer "no match"
// for a path that the edge does forward.
var matcherPattern = regexp.MustCompile("(Path|PathPrefix|PathRegexp)\\(`([^`]*)`\\)")

// ruleMatches answers whether the rule forwards requestPath.
func ruleMatches(t *testing.T, rule, requestPath string) bool {
	t.Helper()
	stripped := matcherPattern.ReplaceAllString(rule, "")
	stripped = strings.NewReplacer("||", "", "(", "", ")", "", " ", "").Replace(stripped)
	if stripped != "" {
		t.Fatalf("the rule uses a matcher this gate cannot read: %q leaves %q", rule, stripped)
	}
	for _, matcher := range matcherPattern.FindAllStringSubmatch(rule, -1) {
		kind, value := matcher[1], matcher[2]
		if kind == "Path" && requestPath == value {
			return true
		}
		if kind == "PathPrefix" && strings.HasPrefix(requestPath, value) {
			return true
		}
		if kind == "PathRegexp" && regexp.MustCompile(value).MatchString(requestPath) {
			return true
		}
	}
	return false
}

func TestBrowserEdgesForwardEveryRootMountedGoRoute(t *testing.T) {
	root := repoRoot(t)
	for _, relative := range browserEdgeFiles {
		t.Run(relative, func(t *testing.T) {
			edge := parseEdgeWithRules(t, filepath.Join(root, relative))
			api, ok := edge.HTTP.Routers["api"]
			if !ok {
				t.Fatalf("%s has no `api` router", relative)
			}
			for requestPath, why := range rootMountedGoPaths {
				if !ruleMatches(t, api.Rule, requestPath) {
					t.Errorf("%s: the `api` rule does not forward %s (%s); traefik answers its own 404 for it", relative, requestPath, why)
				}
			}
			for requestPath, why := range notForwardedGoPaths {
				if ruleMatches(t, api.Rule, requestPath) {
					t.Errorf("%s: the `api` rule forwards %s (%s); this entrypoint serves browsers and must not publish it", relative, requestPath, why)
				}
			}
		})
	}
}

func TestRootRedirectKeepsTheQueryString(t *testing.T) {
	// Traefik applies redirectRegex with Go's own regexp.ReplaceAllString, so
	// the pair below is evaluated exactly as the edge evaluates it.
	cases := []struct {
		requestURI string
		want       string
	}{
		{"http://elitea.example/", "http://elitea.example/app/"},
		{"https://elitea.example/", "https://elitea.example/app/"},
		{"http://elitea.example/?utm_source=newsletter", "http://elitea.example/app/?utm_source=newsletter"},
		{"http://elitea.example/?a=1&b=2", "http://elitea.example/app/?a=1&b=2"},
	}

	root := repoRoot(t)
	for _, relative := range browserEdgeFiles {
		t.Run(relative, func(t *testing.T) {
			edge := parseEdgeWithRules(t, filepath.Join(root, relative))
			middleware, ok := edge.HTTP.Middlewares["redirect-to-app"]
			if !ok {
				t.Fatalf("%s has no `redirect-to-app` middleware", relative)
			}
			pattern, err := regexp.Compile(middleware.RedirectRegex.Regex)
			if err != nil {
				t.Fatalf("%s: the redirect regex does not compile: %v", relative, err)
			}
			for _, testCase := range cases {
				if !pattern.MatchString(testCase.requestURI) {
					t.Errorf("%s: %s does not match the redirect regex, so traefik passes it to the SPA container at path /, which its nginx does not serve", relative, testCase.requestURI)
					continue
				}
				got := pattern.ReplaceAllString(testCase.requestURI, middleware.RedirectRegex.Replacement)
				if got != testCase.want {
					t.Errorf("%s: %s redirects to %s, want %s", relative, testCase.requestURI, got, testCase.want)
				}
			}
		})
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
	"/avatars/",
	"/forward-auth/",
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

// mainServiceRules returns the rules of every router that sends traffic to
// elitea-main. The `api` router is not the only one: the `mcp` router carves
// /app/<id>/mcp out of the SPA prefix.
func mainServiceRules(edge edgeWithRules) []string {
	rules := make([]string, 0, len(edge.HTTP.Routers))
	for _, router := range edge.HTTP.Routers {
		if router.Service == "elitea-main" {
			rules = append(rules, router.Rule)
		}
	}
	return rules
}

func anyRuleMatches(t *testing.T, rules []string, requestPath string) bool {
	t.Helper()
	for _, rule := range rules {
		if ruleMatches(t, rule, requestPath) {
			return true
		}
	}
	return false
}

// TestBrowserEdgesClassifyEveryRootMountedGoRoute derives the check from the
// router, so a new root-mounted family cannot pass unnoticed.
func TestBrowserEdgesClassifyEveryRootMountedGoRoute(t *testing.T) {
	patterns := walkRootMountedGoRoutes(t)
	root := repoRoot(t)
	for _, relative := range browserEdgeFiles {
		t.Run(relative, func(t *testing.T) {
			rules := mainServiceRules(parseEdgeWithRules(t, filepath.Join(root, relative)))
			if len(rules) == 0 {
				t.Fatalf("%s has no router that serves elitea-main", relative)
			}
			forwards := func(t *testing.T, requestPath string) bool {
				return anyRuleMatches(t, rules, requestPath)
			}
			assertPatternsClassified(t, relative, patterns, forwards, notAtTheBrowserEdge)
			assertNoStaleExclusions(t, relative, patterns, forwards, notAtTheBrowserEdge)
		})
	}
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
