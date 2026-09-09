package api

// No route serves data to a caller with no credentials (O7).
//
// WHY THIS EXISTS. router_permission_grant_gate_test.go proves that every
// permission string a gate NAMES is granted by a migration. It cannot see a
// route registered with no gate at all — the string it would look for is not
// there. production_router.go carries no permission gate of its own (its
// handlers gate inside their own packages), so gate placement is non-uniform
// and a missing one leaves no hole in any source-level inventory.
//
// WHY IT IS BEHAVIOURAL. A structural check would have to decide, from a
// chi.Walk, whether one of a route's middleware closures is a permission gate.
// chi hands over `func(http.Handler) http.Handler` values; nothing about a
// closure says what it enforces. So this asks the router the question directly:
// send a request with no credentials and see whether it answers with data.
//
// 2xx IS THE FAILURE, and nothing else is. A 401 or 403 is the gate working.
// A 404, 405 or 400 means the request never reached a handler that could serve
// anything, which is also not a leak — asserting a specific refusal code here
// would turn every legitimate difference in refusal style into a failure and
// the suite would be edited until it stopped meaning anything.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	v2analytics "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/analytics"
	v2auth "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/auth"
	v2convs "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	v2deepwiki "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/deepwiki"
	v2events "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/events"
	v2folders "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/folders"
	v2skills "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	v2social "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/social"
	v2tags "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/tags"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/webhook"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
)

// publicByDesign lists the routes that answer without credentials, each with
// the reason it must.
//
// This is not a dumping ground. An entry asserts "serving this to an anonymous
// caller is correct", and every one of them is either a liveness surface, a
// login surface, or a browser sub-resource that carries no credential by
// construction. Adding a route here to make the test pass, without meaning it,
// reintroduces exactly the invisibility this guards against — which is the
// warning router_nil_gate_test.go's own allowlist carries, for the same
// reason.
var publicByDesign = map[string]string{
	"/healthz":  "liveness. The kubelet presents no credential and a gated liveness probe restarts a healthy process.",
	"/readyz":   "readiness, same reason.",
	"/startupz": "startup, same reason.",

	// The served API documentation (S251). These three are the static v2.yaml
	// spec and the page that renders it — the same bytes for every caller,
	// describing the shape of the API and carrying no tenant data. They are
	// registered above the Auth group at router.go:902-906 with that intent
	// stated, and the legacy shared plugin served them anonymously too.
	//
	// Read this as a standing obligation, not a permanent pass: the spec is
	// only safe to publish while it stays a DESCRIPTION. If an operation is
	// ever documented with a real example carrying real data, that data
	// becomes public here.
	"/api/openapi.yaml": "the OpenAPI document. Static, tenant-free, and public on purpose (router.go:903).",
	"/api/openapi.json": "the same document as JSON.",
	"/api/docs":         "the page that renders the document above. It fetches /api/openapi.json and nothing else.",
}

// publicPrefixes covers surfaces whose whole subtree is anonymous.
var publicPrefixes = []struct{ prefix, reason string }{
	{"/forward-auth", "the login surface itself. A caller here has no session yet — that is what it is for."},
	{"/api/v2/branding", "the brand pack a browser loads BEFORE it has a session, so the login page can be branded."},
	{"/admin/app", "the admin SPA's static assets. A <script src> carries no credential; the page's data calls are gated."},
}

var pathParameter = regexp.MustCompile(`\{[^}]*\}`)

// concretePath turns a chi pattern into a requestable path.
//
// Numeric substitution matters: most path parameters here are project ids, and
// a route that rejects a non-numeric id with a 400 would look "refused" for the
// wrong reason and hide a missing gate behind a parser.
func concretePath(pattern string) string {
	path := pathParameter.ReplaceAllString(pattern, "1")
	path = strings.TrimSuffix(path, "/*")
	if path == "" {
		return "/"
	}
	return path
}

func isPublic(pattern string) (string, bool) {
	if reason, ok := publicByDesign[pattern]; ok {
		return reason, true
	}
	for _, p := range publicPrefixes {
		if strings.HasPrefix(pattern, p.prefix) {
			return p.reason, true
		}
	}
	return "", false
}

func TestNoRouteServesDataWithoutCredentials(t *testing.T) {
	router := NewRouter(fullSurfaceRouterConfig(t))

	type leak struct {
		method, pattern string
		status          int
	}
	var leaks []leak
	checked := 0

	err := chi.Walk(router, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if _, public := isPublic(pattern); public {
			return nil
		}
		// CONNECT and TRACE are not part of this surface and chi registers
		// them only through method-agnostic mounts.
		if method == http.MethodConnect || method == http.MethodTrace {
			return nil
		}
		checked++

		request := httptest.NewRequest(method, concretePath(pattern), nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)

		if response.Code >= 200 && response.Code < 300 {
			leaks = append(leaks, leak{method: method, pattern: pattern, status: response.Code})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the router: %v", err)
	}

	// The floor, and it is MEASURED against the degraded case rather than set
	// to a round number.
	//
	// The first version of this floor was 200, and it did not discriminate. A
	// router composed from a bare RouterConfig{} — every repository nil, every
	// optional route unmounted — still registers 361 routes, because most of
	// the surface is unconditional. So a config that silently stopped composing
	// would have sailed past a floor of 200 and this gate would have checked a
	// surface with the interesting routes missing, reporting a pass.
	//
	// Two measurements, both taken on this tree: bare RouterConfig{} = 361,
	// fullSurfaceRouterConfig = 440. The floor sits above the degraded value,
	// with enough headroom that removing a handful of routes does not turn this
	// red for the wrong reason. Raise it when the surface grows; never lower it
	// to make a run green, because below 361 it stops meaning anything.
	const minimumRoutesChecked = 430
	if checked < minimumRoutesChecked {
		t.Fatalf("only %d routes were checked (floor %d); the router did not compose or the walk matched nothing, so this gate measured nothing",
			checked, minimumRoutesChecked)
	}

	if len(leaks) > 0 {
		var lines []string
		for _, l := range leaks {
			lines = append(lines, fmt.Sprintf("  %s %s -> %d", l.method, l.pattern, l.status))
		}
		t.Fatalf("%d route(s) served a 2xx to a request carrying no credentials:\n%s\n\n"+
			"Either gate the route (apimw.Auth + a permission middleware, as internal/api/v2/indexing does), "+
			"or add it to publicByDesign/publicPrefixes in this file WITH the reason it must answer anonymously.",
			len(leaks), strings.Join(lines, "\n"))
	}

	t.Logf("%d routes checked; none served data without credentials", checked)
}

// fullSurfaceRouterConfig composes the widest route surface this router can
// register, so the walk below sees every route rather than the subset a
// narrower config happens to mount.
//
// It mirrors oapiserver's buildFullSurfaceConfig, which cannot be imported
// (it lives in that package's own _test file). The doubles are zero-value
// embedded interfaces: they satisfy the `!= nil` checks that gate registration
// and panic if actually called.
//
// A PANIC IS NOT A HOLE, and that is why the doubles are safe here even though
// this test — unlike oapiserver's — really does serve requests. apimw.Recover
// turns one into a 500, and 500 is not 2xx. What this test looks for is a route
// that answers with DATA, and a route that reaches a nil repository was gated
// far enough to be somebody else's problem.
//
// AuthValidator is deliberately absent: a request carrying no credential must
// be refused before any validator is consulted, and supplying one would let a
// bug in this test authenticate the caller it is supposed to be denying.
func fullSurfaceRouterConfig(t *testing.T) RouterConfig {
	t.Helper()
	return RouterConfig{
		Auth: AuthDeps{
			SessionHandler:     &v2auth.SessionHandler{},
			OIDCHandler:        &v2auth.OIDCHandler{},
			PrincipalValidator: testPrincipalValidator{},
		},
		PrincipalValidator:  testPrincipalValidator{},
		AppsRepo:            struct{ applications.Repository }{},
		SkillsRepo:          struct{ v2skills.Repository }{},
		FoldersRepo:         struct{ v2folders.Repository }{},
		TagsRepo:            struct{ v2tags.Repository }{},
		AnalyticsRepo:       struct{ v2analytics.Repository }{},
		ConvsRepo:           struct{ v2convs.Repository }{},
		WebhookRepo:         struct{ webhook.Repository }{},
		EventSource:         struct{ v2events.EventSource }{},
		LLMProxy:            http.NotFoundHandler(),
		CurrentSocialAvatar: &v2social.CurrentAvatarRoute{},
		DeepWiki:            &v2deepwiki.Route{},
	}
}
