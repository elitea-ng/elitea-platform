package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	v2skills "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/webhook"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/redis"
)

// ---------------------------------------------------------------------------
// #496 — the three project-scoped route groups router.go mounts through a
// package `Routes()` method.
//
//	/api/v2/configurations              22 routes, the per-project CREDENTIALS
//	/api/v2/webhooks/prompt_lib/{id}     7 routes, url + rotating `secret`,
//	                                     plus the delivery log (#876's second
//	                                     half: GET .../deliveries and POST
//	                                     .../deliveries/{id}/redeliver)
//	/api/v2/events/prompt_lib/{id}       1 route,  the project SSE bus
//
// None of them carried a gate. They were invisible to the audits that read the
// route registrations in router.go itself, because router.go registers nothing
// for them: it mounts a subrouter, and the routes are declared in the mounted
// package.
//
// THE PACKAGE TESTS ARE NOT ENOUGH, and that is why this file exists. Each of
// the three packages now proves both directions against its own Routes(). What
// no package test can see is whether router.go COMPOSES the resolver those
// gates need. A missing option at the mount leaves every gate fail-closed —
// which is safe, and would still pass every package test, and would break the
// AI-configuration page for every caller. Both faults are measured here, in the
// router NewRouter really builds.
// ---------------------------------------------------------------------------

// projectSurfaceRoute is one route of the three groups, with the permission its
// gate names and the same route addressed to ANOTHER project.
type projectSurfaceRoute struct {
	method     string
	path       string
	otherPath  string
	permission string
}

const (
	configurationListPermission   = "configurations.configurations.list"
	configurationGetPermission    = "configurations.configuration.details"
	configurationCreatePermission = "configurations.configuration.create"
	configurationUpdatePermission = "configurations.configuration.update"
	configurationDeletePermission = "configurations.configuration.delete"
	projectStreamPermission       = "models.project_context.view"
)

// The full route set. Project 7 is the one the caller belongs to; project 8 is
// the one it must never reach.
//
// The `{mode}` twins are listed with `administration` in the segment, because
// that is the value a caller would reach for to escape a project-scoped gate.
// They must resolve exactly like the mode-less twin.
var projectSurfaceRoutes = []projectSurfaceRoute{
	// ── /configurations ────────────────────────────────────────────────────
	{http.MethodGet, "/api/v2/configurations/configurations/7", "/api/v2/configurations/configurations/8", configurationListPermission},
	{http.MethodGet, "/api/v2/configurations/configurations/administration/7", "/api/v2/configurations/configurations/administration/8", configurationListPermission},
	{http.MethodPost, "/api/v2/configurations/configurations/7", "/api/v2/configurations/configurations/8", configurationCreatePermission},
	{http.MethodPost, "/api/v2/configurations/configurations/administration/7", "/api/v2/configurations/configurations/administration/8", configurationCreatePermission},
	{http.MethodGet, "/api/v2/configurations/configuration/7/11", "/api/v2/configurations/configuration/8/11", configurationGetPermission},
	{http.MethodGet, "/api/v2/configurations/configuration/administration/7/11", "/api/v2/configurations/configuration/administration/8/11", configurationGetPermission},
	{http.MethodPut, "/api/v2/configurations/configuration/7/11", "/api/v2/configurations/configuration/8/11", configurationUpdatePermission},
	{http.MethodPut, "/api/v2/configurations/configuration/administration/7/11", "/api/v2/configurations/configuration/administration/8/11", configurationUpdatePermission},
	{http.MethodDelete, "/api/v2/configurations/configuration/7/11", "/api/v2/configurations/configuration/8/11", configurationDeletePermission},
	{http.MethodDelete, "/api/v2/configurations/configuration/administration/7/11", "/api/v2/configurations/configuration/administration/8/11", configurationDeletePermission},
	{http.MethodPost, "/api/v2/configurations/check_connection/7/open_ai", "/api/v2/configurations/check_connection/8/open_ai", configurationCreatePermission},
	{http.MethodPost, "/api/v2/configurations/check_connection/administration/7/open_ai", "/api/v2/configurations/check_connection/administration/8/open_ai", configurationCreatePermission},
	{http.MethodPost, "/api/v2/configurations/check_connections/7", "/api/v2/configurations/check_connections/8", configurationCreatePermission},
	{http.MethodPost, "/api/v2/configurations/check_connections/administration/7", "/api/v2/configurations/check_connections/administration/8", configurationCreatePermission},
	// The STORED checks and the revalidation address an existing row, so they
	// gate on the UPDATE string — see the comment beside their registration in
	// the configurations handler's Routes().
	{http.MethodPost, "/api/v2/configurations/check_stored_connection/7/11", "/api/v2/configurations/check_stored_connection/8/11", configurationUpdatePermission},
	{http.MethodPost, "/api/v2/configurations/check_stored_connection/administration/7/11", "/api/v2/configurations/check_stored_connection/administration/8/11", configurationUpdatePermission},
	{http.MethodPost, "/api/v2/configurations/check_stored_connections/7", "/api/v2/configurations/check_stored_connections/8", configurationUpdatePermission},
	{http.MethodPost, "/api/v2/configurations/check_stored_connections/administration/7", "/api/v2/configurations/check_stored_connections/administration/8", configurationUpdatePermission},
	{http.MethodPost, "/api/v2/configurations/revalidate/7/11", "/api/v2/configurations/revalidate/8/11", configurationUpdatePermission},
	{http.MethodPost, "/api/v2/configurations/revalidate/administration/7/11", "/api/v2/configurations/revalidate/administration/8/11", configurationUpdatePermission},
	{http.MethodGet, "/api/v2/configurations/models/7", "/api/v2/configurations/models/8", configurationListPermission},
	{http.MethodGet, "/api/v2/configurations/models/administration/7", "/api/v2/configurations/models/administration/8", configurationListPermission},
	{http.MethodPost, "/api/v2/configurations/models/7", "/api/v2/configurations/models/8", configurationUpdatePermission},
	{http.MethodPost, "/api/v2/configurations/models/administration/7", "/api/v2/configurations/models/administration/8", configurationUpdatePermission},
	{http.MethodGet, "/api/v2/configurations/types/7", "/api/v2/configurations/types/8", configurationListPermission},
	{http.MethodGet, "/api/v2/configurations/types/administration/7", "/api/v2/configurations/types/administration/8", configurationListPermission},
	{http.MethodGet, "/api/v2/configurations/tts_voices/7", "/api/v2/configurations/tts_voices/8", configurationListPermission},
	{http.MethodGet, "/api/v2/configurations/tts_voices/administration/7", "/api/v2/configurations/tts_voices/administration/8", configurationListPermission},
	// ── /webhooks ──────────────────────────────────────────────────────────
	{http.MethodGet, "/api/v2/webhooks/prompt_lib/7/", "/api/v2/webhooks/prompt_lib/8/", configurationListPermission},
	{http.MethodPost, "/api/v2/webhooks/prompt_lib/7/", "/api/v2/webhooks/prompt_lib/8/", configurationCreatePermission},
	{http.MethodGet, "/api/v2/webhooks/prompt_lib/7/wh-1", "/api/v2/webhooks/prompt_lib/8/wh-1", configurationGetPermission},
	{http.MethodPut, "/api/v2/webhooks/prompt_lib/7/wh-1", "/api/v2/webhooks/prompt_lib/8/wh-1", configurationUpdatePermission},
	{http.MethodDelete, "/api/v2/webhooks/prompt_lib/7/wh-1", "/api/v2/webhooks/prompt_lib/8/wh-1", configurationDeletePermission},
	{http.MethodGet, "/api/v2/webhooks/prompt_lib/7/wh-1/deliveries", "/api/v2/webhooks/prompt_lib/8/wh-1/deliveries", configurationGetPermission},
	{http.MethodPost, "/api/v2/webhooks/prompt_lib/7/wh-1/deliveries/del-1/redeliver", "/api/v2/webhooks/prompt_lib/8/wh-1/deliveries/del-1/redeliver", configurationUpdatePermission},
	// ── /events ────────────────────────────────────────────────────────────
	{http.MethodGet, "/api/v2/events/prompt_lib/7/", "/api/v2/events/prompt_lib/8/", projectStreamPermission},
}

// allProjectSurfacePermissions is the distinct set the routes draw from.
var allProjectSurfacePermissions = []string{
	configurationListPermission,
	configurationGetPermission,
	configurationCreatePermission,
	configurationUpdatePermission,
	configurationDeletePermission,
	projectStreamPermission,
}

// projectSurfacePermissionsExcept is what makes the under-privileged case
// discriminate: a caller who holds everything these groups name EXCEPT one
// string. Without it, "member of the project" and "entitled to THIS route" are
// the same observation.
func projectSurfacePermissionsExcept(withheld string) []string {
	granted := make([]string, 0, len(allProjectSurfacePermissions))
	for _, permission := range allProjectSurfacePermissions {
		if permission != withheld {
			granted = append(granted, permission)
		}
	}
	return granted
}

// emptyWebhookRepo answers for ANY project id, exactly as the /elitea_core
// project-scope doubles do. A repository that refused project 8 on its own
// would let these tests pass without proving anything about authorization, so a
// refusal can only have come from the gate.
type emptyWebhookRepo struct{ webhook.Repository }

func (emptyWebhookRepo) List(context.Context, string) ([]webhook.Webhook, error) {
	return []webhook.Webhook{}, nil
}

func (emptyWebhookRepo) Get(_ context.Context, projectID, id string) (webhook.Webhook, error) {
	return webhook.Webhook{ID: id, ProjectID: projectID, Secret: "s3cr3t"}, nil
}

func (emptyWebhookRepo) Create(_ context.Context, projectID string, wh webhook.Webhook) (webhook.Webhook, error) {
	wh.ProjectID = projectID
	return wh, nil
}

func (emptyWebhookRepo) Update(_ context.Context, projectID, id string, wh webhook.Webhook) (webhook.Webhook, error) {
	wh.ID, wh.ProjectID = id, projectID
	return wh, nil
}

func (emptyWebhookRepo) Delete(context.Context, string, string) error { return nil }

// closedEventSource satisfies the SSE transport with a channel that is already
// closed, so an ADMITTED stream returns at once rather than blocking the test.
// A REFUSED one never reaches it, and channelAsked records which.
type closedEventSource struct{ asked chan string }

func (s closedEventSource) Raw(_ context.Context, channel string) (<-chan redis.Event, func(), error) {
	select {
	case s.asked <- channel:
	default:
	}
	events := make(chan redis.Event)
	close(events)
	return events, func() {}, nil
}

// newProjectSurfaceRouter composes the three groups in the router NewRouter
// really builds, and injects the authorization answer through the same field
// the sibling groups' tests use. Production leaves it unset and every group
// gets the live Pool-backed resolver.
func newProjectSurfaceRouter(resolver fakePermissionResolver, source closedEventSource) chi.Router {
	return NewRouter(RouterConfig{
		SkillsRepo:                struct{ v2skills.Repository }{},
		AuthValidator:             testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:        testPrincipalValidator{},
		WebhookRepo:               emptyWebhookRepo{},
		EventSource:               source,
		ProjectPermissionResolver: resolver,
	})
}

func newEventSource() closedEventSource {
	return closedEventSource{asked: make(chan string, 4)}
}

// entitledForProjectSevenOnTheseSurfaces holds every permission these groups
// name, in project 7 alone — a fully-privileged member of one project and a
// stranger to every other.
func entitledForProjectSevenOnTheseSurfaces() fakePermissionResolver {
	return fakePermissionResolver{granted: allProjectSurfacePermissions, forProject: "7"}
}

/* ── the routes exist, and there are as many as the table says ─────────── */

// The table must cover the registrations. chi.Walk reports what NewRouter
// really mounts, so a route added to any of the three packages without a row
// here is named rather than shipped unmeasured.
//
// This is the check the three groups did not have. #496 exists because a route
// declared inside a mounted package is invisible to a reader of router.go.
func TestTheThreeProjectSurfacesRegisterExactlyTheLedgeredRoutes(t *testing.T) {
	router := newProjectSurfaceRouter(entitledForProjectSevenOnTheseSurfaces(), newEventSource())
	registered := walkRoutes(t, router)

	// The catalogue is the one route of the group that names no project.
	const catalogue = "GET /api/v2/configurations/available/"
	if _, ok := registered[catalogue]; !ok {
		t.Errorf("%s is not registered", catalogue)
	}

	counted := 0
	for route := range registered {
		switch {
		case strings.HasPrefix(route, "GET /api/v2/configurations/available"),
			strings.Contains(route, "/api/v2/notifications/"):
			// The catalogue, and the notification group's own /events path,
			// which is a different surface with its own gate.
		case strings.Contains(route, "/api/v2/configurations/"),
			strings.Contains(route, "/api/v2/webhooks/"),
			strings.Contains(route, "/api/v2/events/"):
			counted++
		}
	}
	if counted != len(projectSurfaceRoutes) {
		t.Fatalf("NewRouter mounts %d routes across the three surfaces and the table lists %d.\n"+
			"  Add the new route to projectSurfaceRoutes with the permission it gates on.\n"+
			"  A route absent from the table is a route no direction of this file measures.",
			counted, len(projectSurfaceRoutes))
	}
}

/* ── the refused direction: another project ────────────────────────────── */

// The caller is genuinely entitled to project 7 and holds every permission
// these routes name there. The only thing that changes is the project id in the
// path.
//
// Every row FAILS on the pre-#496 router: without a gate the request reaches
// the handler, which builds `p_8` (or `webhooks WHERE project_id = 8`, or
// `project:8:events`) from the path segment and answers something other than
// 403.
func TestTheThreeProjectSurfacesRefuseAnotherProject(t *testing.T) {
	for _, route := range projectSurfaceRoutes {
		t.Run(route.method+" "+route.otherPath, func(t *testing.T) {
			source := newEventSource()
			router := newProjectSurfaceRouter(entitledForProjectSevenOnTheseSurfaces(), source)

			if status := serveStatus(t, router, route.method, route.otherPath); status != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 — the {projectID} path segment must not be "+
					"enough to reach another tenant's data", status)
			}
			select {
			case channel := <-source.asked:
				t.Fatalf("the refused request still subscribed to %q", channel)
			default:
			}
		})
	}
}

/* ── the refused direction: an under-privileged member ─────────────────── */

// The caller IS a member of project 7 and holds every permission these groups
// name except the one the route under test needs.
//
// This is the only case that separates real RBAC from a membership check: a
// member passes both and a non-member fails both, so without this row a gate
// wired to "is a member" would be indistinguishable from the gate that shipped.
func TestTheThreeProjectSurfacesRefuseAnUnderPrivilegedMember(t *testing.T) {
	for _, route := range projectSurfaceRoutes {
		t.Run(route.method+" "+route.path+" without "+route.permission, func(t *testing.T) {
			source := newEventSource()
			router := newProjectSurfaceRouter(fakePermissionResolver{
				granted:    projectSurfacePermissionsExcept(route.permission),
				forProject: "7",
			}, source)

			if status := serveStatus(t, router, route.method, route.path); status != http.StatusForbidden {
				t.Fatalf("status = %d for a member who does not hold %q, want 403",
					status, route.permission)
			}
		})
	}
}

/* ── the entitled direction ────────────────────────────────────────────── */

// A caller entitled to project 7 reaches every one of its own routes.
//
// Without this direction a gate that refuses EVERY caller reads as a working
// gate — the shape of #354, #359 and #402, and the reason the allowlist in
// router_permission_grant_gate_test.go is empty.
//
// The router has no Pool, so the configuration handlers reach pgx and fail. The
// assertion is therefore "not 403 and not 404": the request passed the gate and
// the route exists. serveStatus records a handler panic as 500 for exactly this.
func TestTheThreeProjectSurfacesAdmitAnEntitledMember(t *testing.T) {
	for _, route := range projectSurfaceRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			source := newEventSource()
			router := newProjectSurfaceRouter(entitledForProjectSevenOnTheseSurfaces(), source)

			status := serveStatus(t, router, route.method, route.path)
			switch status {
			case http.StatusForbidden:
				t.Fatalf("an entitled member was refused. %q is granted in DEFAULT mode by the "+
					"migration corpus, so this is a wiring fault, not a missing grant.",
					route.permission)
			case http.StatusNotFound:
				t.Fatalf("the route is not mounted at all")
			}
		})
	}
}

// The entitled SSE caller reaches the channel of the project in the path, and
// no other.
//
// The status alone cannot show this: Stream takes over the connection before it
// reads anything, so a gate that admitted and then subscribed to the wrong
// channel would look identical.
func TestTheEntitledStreamSubscribesToItsOwnProject(t *testing.T) {
	source := newEventSource()
	router := newProjectSurfaceRouter(entitledForProjectSevenOnTheseSurfaces(), source)

	if status := serveStatus(t, router, http.MethodGet, "/api/v2/events/prompt_lib/7/"); status != http.StatusOK {
		t.Fatalf("status = %d for an entitled member, want 200", status)
	}
	select {
	case channel := <-source.asked:
		if channel != "project:7:events" {
			t.Fatalf("subscribed to %q, want project:7:events", channel)
		}
	default:
		t.Fatal("the admitted stream subscribed to nothing")
	}
}

/* ── the method-fallthrough shape #495 found ───────────────────────────── */

// With ELITEA_CONFIGURATIONS_ENABLED on, production composes the reviewed READ
// route, which registers GET on the ROOT router for the same path the
// compatibility mount owns. Only GET.
//
// chi does not stop at a node whose method set misses: it keeps searching, so
// the POST, PUT and DELETE on those paths fall through to the mount. That is
// the shape #495 recorded on `/admin/users/administration/{projectID}`, and it
// is why gating the mount is not optional in the profile that composes the
// reviewed reads. deploy/README.md records
// ELITEA_CONFIGURATIONS_MUTATION_ENABLED as off in BOTH deployment profiles, so
// the write always lands here.
func TestConfigurationWritesFallThroughToTheGatedMountWhenOnlyTheReadIsComposed(t *testing.T) {
	reviewedRead := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	router := NewRouter(RouterConfig{
		SkillsRepo:                struct{ v2skills.Repository }{},
		AuthValidator:             testTokenValidator{user: authenticatedTestUser()},
		PrincipalValidator:        testPrincipalValidator{},
		CurrentConfigurationRead:  reviewedRead,
		ProjectPermissionResolver: fakePermissionResolver{},
	})

	// The reviewed route owns the GET.
	if status := serveStatus(t, router, http.MethodGet, "/api/v2/configurations/configurations/7"); status != http.StatusTeapot {
		t.Fatalf("GET status = %d, want the reviewed route (%d)", status, http.StatusTeapot)
	}

	// Every other method falls through to the mount, which must refuse a caller
	// resolving nothing. A 200 here is the defect #496 names.
	for _, target := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v2/configurations/configurations/7"},
		{http.MethodPut, "/api/v2/configurations/configuration/7/11"},
		{http.MethodDelete, "/api/v2/configurations/configuration/7/11"},
	} {
		status := serveStatus(t, router, target.method, target.path)
		if status != http.StatusForbidden {
			t.Errorf("%s %s status = %d, want 403 from the compatibility mount",
				target.method, target.path, status)
		}
	}
}

/* ── the control ───────────────────────────────────────────────────────── */

// Without this, "every route answers 403" satisfies the tests above for the
// wrong reason: a router that had stopped serving these paths altogether would
// read as perfectly gated.
//
// GET /configurations/available/ is the one route of the group that names no
// project and carries no gate. It serves the pinned, credential-free registry
// snapshot, and NewCurrentAvailableRoute serves the same snapshot behind
// authentication alone.
func TestTheProjectSurfaceGatesAreNotVacuous(t *testing.T) {
	router := newProjectSurfaceRouter(fakePermissionResolver{}, newEventSource())

	if status := serveStatus(t, router, http.MethodGet, "/api/v2/configurations/available/"); status == http.StatusForbidden {
		t.Fatal("the deliberately ungated catalogue also answered 403; the assertions above " +
			"cannot distinguish a gate from a dead router")
	}
}

// An unauthenticated caller is refused at every route, which is the /api/v2
// group's own contract and is unchanged by this work. Stated so a rebase that
// moved one of the three mounts out of that group fails here.
func TestTheThreeProjectSurfacesStillRequireAuthentication(t *testing.T) {
	router := newProjectSurfaceRouter(entitledForProjectSevenOnTheseSurfaces(), newEventSource())

	for _, route := range projectSurfaceRoutes {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(route.method, route.path, nil))
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s %s status = %d without a credential, want 401",
				route.method, route.path, recorder.Code)
		}
	}
}
