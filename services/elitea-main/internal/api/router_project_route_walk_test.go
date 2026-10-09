package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	v2analytics "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/analytics"
	v2artifacts "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/artifacts"
	v2convs "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	v2evaluation "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	v2folders "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/folders"
	v2memories "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/memories"
	v2pipelinetriggers "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/pipelinetriggers"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/sharedchat"
	v2skills "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	v2tags "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/tags"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
)

// ---------------------------------------------------------------------------
// The route walk: no route that names a project in its path may answer a
// caller who belongs to a DIFFERENT project.
//
// The per-family tables elsewhere in this package prove that the routes someone
// remembered to list are gated. They cannot prove that every route is listed: a
// new registration with {projectID} in its pattern and no gate is invisible to
// all of them, and a route declared inside a mounted package is invisible even
// to a reader of router.go. This test starts from the other end. It asks the
// composed router for every pattern it really serves (chi.Walk), keeps those
// that carry a project path parameter, and sends each one as a caller who is a
// member of project 7 only, addressed to project 8, with the membership and
// permission answers faked so the ONLY thing that can refuse the request is a
// gate in front of the handler.
//
// A route that answers anything other than a refusal must be named in
// projectRouteAllowlist with the reason it is safe to leave open. The
// allowlist is checked from both sides: a route that is gated now but still
// listed, or listed but no longer registered, fails the build, so the list can
// only shrink as routes are fixed.
// ---------------------------------------------------------------------------

// projectParam matches the project path parameter whatever its spelling.
var projectParam = regexp.MustCompile(`(?i)\{project_?id\}`)
var otherParam = regexp.MustCompile(`\{[^}]*\}`)

// projectRouteAllowlist names every project-addressed route that is reachable
// by a caller outside the project, and why that is safe. Keys are
// "METHOD pattern" exactly as chi.Walk reports them.
//
// Nothing belongs here because legacy left it open. Each entry must say what
// the route returns that makes a project check pointless, or what it does
// instead of a project check.
var projectRouteAllowlist = map[string]string{
	// The caller's OWN permissions in the project. The handler resolves the
	// requester's permission set for the project and returns it, so a caller
	// outside the project reads an empty list: there is no project data to
	// protect, and a gate would 403 the request the web app uses to decide what
	// to render.
	"GET /api/v2/elitea_core/permissions/prompt_lib/{projectID}": "self-read of the caller's own permissions; a non-member resolves the empty set",
	"GET /api/v2/auth/permissions/prompt_lib/{projectID}":        "self-read of the caller's own permissions; a non-member resolves the empty set",

	// Global taxonomies that merely have a project id in the path.
	"GET /api/v2/elitea_core/agent_categories/prompt_lib/{projectID}": "global taxonomy: built-in categories plus a globally authored row; the per-project lookup it once made can never match",
	"GET /api/v2/elitea_core/default_icons/prompt_lib/{projectID}":    "the platform's built-in icon catalogue read from the server's own directory; no project data",

	// The segment is not a tenant scope here.
	"GET /api/v2/projects/project/{mode}/{projectID}":       "the segment is the public-project id used as a filter; the query lists the CALLER's own projects, keyed on the authenticated user",
	"POST /api/v2/secrets/secret/{mode}/{projectID}/{name}": "the project (default) mode has no POST and answers 405; the administration-mode form is gated centrally and writes the global vault",

	// Sub-resources a browser requests with no credential.
	"GET /icons/{projectID}/{filename}":   "project icon image fetched by a browser <img>, which carries no credential; public by design (S20b). Display artwork only",
	"GET /avatars/{projectID}/{filename}": "avatar image fetched by a browser <img>, which carries no credential; public by design, same as /icons",

	// Inbound pipeline triggers, authenticated by the secret in the request.
	"POST /api/v2/pipeline_trigger/{projectID}/{tokenID}":            "inbound trigger mounted above the session auth group; its credential is the per-pipeline secret, checked by the handler against the named project",
	"POST /api/v2/pipeline_trigger/{projectID}/{tokenID}/{provider}": "inbound provider trigger, same credential as above",
}

const (
	walkOwnProject     = "7"
	walkForeignProject = "8"
)

// newProjectRouteWalkRouter composes as much of the production route surface
// as test doubles allow. Every repository is an empty embedding of its
// interface: it answers for ANY project id, so a refusal can only have come
// from a gate in front of the handler.
func newProjectRouteWalkRouter(t *testing.T) chi.Router {
	t.Helper()
	pool, _ := newRecordingPostgresPool(t)
	members := &projectMembers{members: map[[2]int]bool{{7, 1}: true}}
	return NewRouter(RouterConfig{
		Pool:                      pool,
		AuthValidator:             testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:        testPrincipalValidator{},
		AppsRepo:                  struct{ applications.Repository }{},
		SkillsRepo:                struct{ v2skills.Repository }{},
		FoldersRepo:               struct{ v2folders.Repository }{},
		TagsRepo:                  struct{ v2tags.Repository }{},
		ConvsRepo:                 struct{ v2convs.Repository }{},
		AnalyticsRepo:             struct{ v2analytics.Repository }{},
		MemoriesRepo:              struct{ v2memories.Repository }{},
		EvalDimensionsRepo:        struct{ v2evaluation.Repository }{},
		EvalDatasetsRepo:          struct{ v2evaluation.DatasetRepository }{},
		EvalRunsRepo:              struct{ v2evaluation.RunRepository }{},
		WebhookRepo:               emptyWebhookRepo{},
		EventSource:               closedEventSource{asked: make(chan string, 64)},
		PipelineTriggers:          v2pipelinetriggers.NewHandler(nil),
		ArtifactHandler:           v2artifacts.NewHandler(alwaysSucceedsArtifactRepo{}, alwaysSucceedsArtifactStore{}),
		SharedChatStore:           struct{ sharedchat.Store }{},
		SharedChatTranscript:      struct{ sharedchat.TranscriptStore }{},
		ProjectAccessQuerier:      members,
		ProjectPermissionResolver: fakePermissionResolver{granted: allEliteaCorePermissions, forProject: walkOwnProject},
	})
}

type walkedRoute struct{ method, pattern string }

func (r walkedRoute) key() string { return r.method + " " + r.pattern }

// projectRoutes returns every registered route whose pattern names a project.
func projectRoutes(t *testing.T, router chi.Router) []walkedRoute {
	t.Helper()
	var routes []walkedRoute
	if err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !projectParam.MatchString(route) || strings.HasSuffix(route, "/*") {
			return nil
		}
		routes = append(routes, walkedRoute{method, route})
		return nil
	}); err != nil {
		t.Fatalf("walk router: %v", err)
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].key() < routes[j].key() })
	return routes
}

// walkConcretePath fills a pattern with the foreign project id, the `default`
// mode where a route takes one (a route that serves two modes answers 404 to
// any other value, which would read as "refused" for the wrong reason), and a
// placeholder for every other parameter.
func walkConcretePath(pattern string) string {
	path := projectParam.ReplaceAllString(pattern, walkForeignProject)
	path = strings.ReplaceAll(path, "{mode}", "default")
	path = otherParam.ReplaceAllString(path, "1")
	return strings.ReplaceAll(path, "*", "x")
}

// refused reports whether the answer is a gate's refusal rather than a handler
// running for the foreign project. A project gate built over the pool directly
// (not the injected membership answer) cannot reach a database in this test and
// fails closed with 503 "project authorization unavailable"; that is the gate
// refusing, and nothing but the gate writes that message.
func refused(code int, body string) bool {
	return code == http.StatusForbidden || code == http.StatusUnauthorized ||
		(code == http.StatusServiceUnavailable && strings.Contains(body, "project authorization unavailable"))
}

func TestNoProjectRouteAdmitsAForeignProjectActor(t *testing.T) {
	router := newProjectRouteWalkRouter(t)
	routes := projectRoutes(t, router)

	// The walk is only meaningful over a large surface. This floor is a
	// tripwire against a composition change that silently registers almost
	// nothing; it is not a count to keep in step.
	const minimumProjectRoutes = 250
	if len(routes) < minimumProjectRoutes {
		t.Fatalf("the walk found %d project-addressed routes, want at least %d: the router composition shrank", len(routes), minimumProjectRoutes)
	}

	registered := map[string]bool{}
	admitted := map[string]int{}
	for _, route := range routes {
		registered[route.key()] = true
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		request := testAuthHeader(httptest.NewRequest(route.method, walkConcretePath(route.pattern), nil)).WithContext(ctx)
		recorder := httptest.NewRecorder()
		func() {
			defer func() {
				if recover() != nil {
					recorder.Code = http.StatusInternalServerError
				}
			}()
			router.ServeHTTP(recorder, request)
		}()
		cancel()
		if !refused(recorder.Code, recorder.Body.String()) {
			admitted[route.key()] = recorder.Code
		}
	}

	var unlisted []string
	for key, code := range admitted {
		if _, ok := projectRouteAllowlist[key]; !ok {
			unlisted = append(unlisted, key+" -> "+http.StatusText(code))
		}
	}
	sort.Strings(unlisted)
	if len(unlisted) > 0 {
		t.Errorf("%d route(s) named a project and answered a caller who belongs to a different project.\n"+
			"Give each a project gate (projectScoped, or projectPermission with the legacy permission), or add it to\n"+
			"projectRouteAllowlist with the reason it is safe to leave open:\n  %s",
			len(unlisted), strings.Join(unlisted, "\n  "))
	}
	for key, reason := range projectRouteAllowlist {
		switch {
		case strings.TrimSpace(reason) == "":
			t.Errorf("allowlist entry %q has no reason", key)
		case !registered[key]:
			t.Errorf("allowlist entry %q is not a registered project route any more; remove it", key)
		default:
			if _, open := admitted[key]; !open {
				t.Errorf("allowlist entry %q is gated now; remove it", key)
			}
		}
	}
	t.Logf("walked %d project-addressed routes; %d reachable from a foreign project and allowlisted", len(routes), len(admitted))
}
