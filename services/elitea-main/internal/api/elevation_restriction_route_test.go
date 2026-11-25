package api_test

// What an elevation cannot do, through the router (ADR-0059 decision 5;
// story R2.8a, issue 2522). A Form session, or a sealed session, of a person
// whose resolution reports an active elevation reaches the restriction of the
// action table in front of every route gate:
//
//   - every `deny` entry answers 403 elevation_restricted, at either level;
//   - a `sealed` entry answers 403 elevation_restricted under an approved
//     elevation at either level and under a sealed one at the admin level,
//     and is admitted and flagged under a sealed super_admin elevation;
//   - a `flag` entry is admitted and its admin audit row flagged;
//   - nothing is restricted when the STANDING set admits the action, or when
//     no elevation took part.
//
// The identity provider save classifies in its handler; its cases are in
// internal/api/v2/admin (elevation_trust_internal_test.go), against the
// handler's own store doubles.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/adminsurface"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	v2retention "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/retention"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/browsersession"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/retention"
)

// elevationScenario is who the request is: the credential, the standing set
// and the elevation, if any.
type elevationScenario struct {
	sealed   bool
	standing []string
	// level and path of the one active elevation; level "" for none.
	level, path string
}

const (
	elevationTestUserID     = 31
	elevationTestSessionRef = int64(9)
	elevationTestFormCookie = "elitea_form_elevation_test"
)

// elevationResolver answers the scenario of the request (the context key the
// test sets) for every resolution, in every mode.
type elevationResolver struct{}

type scenarioKey struct{}

func (elevationResolver) ResolvePermissions(ctx context.Context, _ auth.User, mode, _ string) (auth.PermissionResolution, error) {
	scenario, ok := ctx.Value(scenarioKey{}).(elevationScenario)
	if !ok {
		return auth.PermissionResolution{}, auth.ErrPermissionDenied
	}
	resolution := auth.PermissionResolution{UserID: elevationTestUserID, Permissions: append([]string(nil), scenario.standing...)}
	if mode != auth.PermissionModeAdministration || scenario.level == "" {
		if scenario.sealed {
			// A sealed session holds its elevation and nothing else.
			resolution.Permissions = nil
		}
		return resolution, nil
	}
	role, _ := adminaccess.LookupBuiltInRole(scenario.level)
	const id = "0b7c2d4e-0000-4000-8000-0000000e1e70"
	if scenario.sealed {
		resolution.Permissions = nil
	}
	standing := append([]string(nil), resolution.Permissions...)
	for _, permission := range role.Set() {
		if !containsString(standing, permission) {
			resolution.Permissions = append(resolution.Permissions, permission)
			resolution.ElevatedPermissions = append(resolution.ElevatedPermissions, permission)
		}
	}
	resolution.ElevationSessionRef = elevationTestSessionRef
	resolution.ElevationID, resolution.ElevationIDs = id, []string{id}
	resolution.Elevations = []auth.ElevationGrant{{ID: id, Level: scenario.level, Path: scenario.path}}
	return resolution, nil
}

func (r elevationResolver) ResolveMembershipPermissions(ctx context.Context, user auth.User, mode string) (auth.PermissionResolution, error) {
	return r.ResolvePermissions(ctx, user, mode, "")
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// elevationFormSessions authenticates the test's Form cookie.
type elevationFormSessions struct{}

func (elevationFormSessions) AuthenticateFormSession(r *http.Request) (auth.User, error) {
	if _, err := r.Cookie(elevationTestFormCookie); err != nil {
		return auth.User{}, apimw.ErrFormSessionAbsent
	}
	return auth.User{ID: "31", UserID: "31", Email: "elevated@test.local", AuthType: "session",
		BrowserSessionRef: elevationTestSessionRef}, nil
}

// elevationSessionStore reads the sealed cookie as a sealed session, and no
// ordinary one.
type elevationSessionStore struct{}

func (elevationSessionStore) Validate(context.Context, string) (browsersession.Session, error) {
	return browsersession.Session{}, browsersession.ErrNotFound
}

func (elevationSessionStore) ValidateSealed(context.Context, string) (browsersession.Session, error) {
	return browsersession.Session{ID: "sealed", UserID: elevationTestUserID, Provider: browsersession.ProviderSealed,
		Ref: elevationTestSessionRef, Email: "break-glass@centry.user"}, nil
}

// flagRecorder keeps the audit events of the router.
type flagRecorder struct {
	mu     sync.Mutex
	events []audit.Event
}

func (f *flagRecorder) Record(_ context.Context, event audit.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
}

func (f *flagRecorder) last() (audit.Event, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.events) == 0 {
		return audit.Event{}, false
	}
	return f.events[len(f.events)-1], true
}

// shorteningStore is a retention store whose stored maximum is 90 days: a
// write's check sees the move from 90 to the body's maximum.
type shorteningStore struct{ v2retention.Store }

func (shorteningStore) SetPlatformPolicy(_ context.Context, p retention.Policy, _ int64, check retention.WriteCheck) (retention.Policy, error) {
	ninety := 90
	if err := check.Run(retention.Bounds{MaxAgeDays: &ninety}, retention.Bounds{MinAgeDays: p.MinAgeDays, MaxAgeDays: p.MaxAgeDays}); err != nil {
		return retention.Policy{}, err
	}
	return p, nil
}

type elevationRouter struct {
	handler  http.Handler
	recorder *flagRecorder
}

func newElevationRouter(t *testing.T) elevationRouter {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://elevation@127.0.0.1:1/elevation?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	recorder := &flagRecorder{}
	resolver := elevationResolver{}
	handler := api.NewRouter(api.RouterConfig{
		Pool:                      pool,
		AuditRecorder:             recorder,
		PrincipalValidator:        elevationPrincipalValidator{},
		PermissionResolver:        resolver,
		ProjectPermissionResolver: resolver,
		RetentionAdmin:            v2retention.NewHandler(shorteningStore{}),
		Auth:                      api.AuthDeps{FormSessions: elevationFormSessions{}, SessionStore: elevationSessionStore{}},
	})
	return elevationRouter{handler: handler, recorder: recorder}
}

type elevationPrincipalValidator struct{}

func (elevationPrincipalValidator) ValidatePrincipal(_ context.Context, user auth.User) (auth.User, error) {
	return user, nil
}

type elevationAnswer struct {
	status  int
	code    string
	body    string
	flagged bool
}

func (e elevationRouter) send(t *testing.T, scenario elevationScenario, method, path, body string) elevationAnswer {
	t.Helper()
	if body == "" && method != http.MethodGet {
		body = "{}"
	}
	request := httptest.NewRequestWithContext(context.WithValue(context.Background(), scenarioKey{}, scenario),
		method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	if scenario.sealed {
		request.AddCookie(&http.Cookie{Name: browsersession.SealedCookieName, Value: "s1.sealed-elevation-test-0123456789abcdefghij"})
	} else {
		request.AddCookie(&http.Cookie{Name: elevationTestFormCookie, Value: "1"})
	}
	w := httptest.NewRecorder()
	e.handler.ServeHTTP(w, request)
	answer := elevationAnswer{status: w.Code, body: w.Body.String()}
	var decoded struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(w.Body.Bytes(), &decoded) == nil {
		answer.code = decoded.Code
	}
	if event, ok := e.recorder.last(); ok && event.HTTPMethod == method {
		answer.flagged = event.ElevationFlagged
	}
	e.recorder.mu.Lock()
	e.recorder.events = nil
	e.recorder.mu.Unlock()
	return answer
}

func (a elevationAnswer) restricted() bool {
	return a.status == http.StatusForbidden && a.code == adminaccess.CodeElevationRestricted
}

var (
	approvedAdmin      = elevationScenario{level: adminaccess.ElevationLevelAdmin, path: adminaccess.ElevationPathApproved}
	approvedSuperAdmin = elevationScenario{level: adminaccess.ElevationLevelSuperAdmin, path: adminaccess.ElevationPathApproved}
	sealedAdmin        = elevationScenario{sealed: true, level: adminaccess.ElevationLevelAdmin, path: adminaccess.ElevationPathSealed}
	sealedSuperAdmin   = elevationScenario{sealed: true, level: adminaccess.ElevationLevelSuperAdmin, path: adminaccess.ElevationPathSealed}
)

// entryRequest answers the path and body that reach entry.
func entryRequest(entry adminaccess.Action) (string, string) {
	params := map[string]string{}
	if entry.Selector.In == adminaccess.SelectPath {
		params[entry.Selector.Field] = entry.Selector.Value
	}
	path := strings.ReplaceAll(entry.Pattern, "{mode}", auth.PermissionModeAdministration)
	path = adminsurface.RequestPath(path, params)
	return path, entry.Selector.Body()
}

// One route test per `deny` entry the router decides from the request: 403
// elevation_restricted at both levels. The classified deny entries (a
// retention shortening) are decided in the handler, below.
func TestEveryDeniedActionIsRestrictedUnderAnElevation(t *testing.T) {
	router := newElevationRouter(t)
	denied := 0
	for _, entry := range adminaccess.ActionTable().Entries() {
		if entry.Elevation != adminaccess.ElevationDeny || entry.Classifier != "" {
			continue
		}
		denied++
		t.Run(entry.Key(), func(t *testing.T) {
			path, body := entryRequest(entry)
			for name, scenario := range map[string]elevationScenario{"admin": approvedAdmin, "super_admin": approvedSuperAdmin} {
				if answer := router.send(t, scenario, entry.Method, path, body); !answer.restricted() {
					t.Fatalf("%s elevation: %d %s, want 403 %s", name, answer.status, answer.body,
						adminaccess.CodeElevationRestricted)
				}
			}
		})
	}
	if denied < 10 {
		t.Fatalf("only %d deny entries; the action table denies more", denied)
	}
}

// The SCIM group bindings, named: an elevated session that creates or changes
// one is refused.
func TestAnElevatedSessionCannotCreateOrChangeASCIMGroupBinding(t *testing.T) {
	router := newElevationRouter(t)
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/api/v2/admin/scim_group_bindings/administration"},
		{http.MethodPut, "/api/v2/admin/scim_group_bindings/administration/4"},
		{http.MethodDelete, "/api/v2/admin/scim_group_bindings/administration/4"},
	} {
		body := `{"group":"engineering","role":"admin"}`
		if answer := router.send(t, approvedSuperAdmin, route.method, route.path, body); !answer.restricted() {
			t.Errorf("%s %s: %d %s, want 403 %s", route.method, route.path, answer.status, answer.body,
				adminaccess.CodeElevationRestricted)
		}
	}
}

// Every `sealed` entry the router decides: refused under an approved
// elevation at either level and a sealed one at the admin level; admitted and
// flagged under a sealed super_admin elevation.
func TestSealedActionsOpenOnlyToTheSealedSuperAdminElevation(t *testing.T) {
	router := newElevationRouter(t)
	sealedEntries := 0
	for _, entry := range adminaccess.ActionTable().Entries() {
		if entry.Elevation != adminaccess.ElevationSealed || entry.Classifier != "" {
			continue
		}
		sealedEntries++
		t.Run(entry.Key(), func(t *testing.T) {
			path, body := entryRequest(entry)
			for name, scenario := range map[string]elevationScenario{
				"approved admin": approvedAdmin, "approved super_admin": approvedSuperAdmin, "sealed admin": sealedAdmin,
			} {
				if answer := router.send(t, scenario, entry.Method, path, body); !answer.restricted() {
					t.Errorf("%s: %d %s, want 403 %s", name, answer.status, answer.body, adminaccess.CodeElevationRestricted)
				}
			}
			answer := router.send(t, sealedSuperAdmin, entry.Method, path, body)
			if answer.restricted() || answer.status == http.StatusForbidden || answer.status == http.StatusUnauthorized {
				t.Errorf("sealed super_admin: %d %s, want admitted", answer.status, answer.body)
			}
			if !answer.flagged {
				t.Errorf("sealed super_admin: the audit row is not flagged (%d %s)", answer.status, answer.body)
			}
		})
	}
	if sealedEntries < 3 {
		t.Fatalf("only %d sealed entries", sealedEntries)
	}
}

// A `flag` entry is admitted under an elevation and its audit row flagged; the
// same request without an elevation is not flagged.
func TestAFlaggedActionIsAdmittedAndMarkedForTheReview(t *testing.T) {
	router := newElevationRouter(t)
	const path = "/api/v2/admin/user_suspend/administration/5"
	elevated := router.send(t, approvedAdmin, http.MethodPut, path, `{"suspended":true}`)
	if elevated.restricted() || !elevated.flagged {
		t.Fatalf("an elevated suspend: %d %s, flagged %v; want admitted and flagged", elevated.status, elevated.body, elevated.flagged)
	}
	role, _ := adminaccess.LookupBuiltInRole(adminaccess.RoleAdmin)
	standing := router.send(t, elevationScenario{standing: role.Set()}, http.MethodPut, path, `{"suspended":true}`)
	if standing.flagged {
		t.Fatalf("a standing suspend is flagged: %d %s", standing.status, standing.body)
	}
}

// A session whose STANDING set admits a denied action is not refused by the
// restriction when it holds no elevation, nor when it holds one that adds
// nothing the action needs.
func TestTheStandingSetIsNotRestricted(t *testing.T) {
	router := newElevationRouter(t)
	role, _ := adminaccess.LookupBuiltInRole(adminaccess.RoleSuperAdmin)
	standingOnly := elevationScenario{standing: role.Set()}
	standingAndElevated := elevationScenario{standing: role.Set(), level: adminaccess.ElevationLevelAdmin,
		path: adminaccess.ElevationPathApproved}
	for _, entry := range adminaccess.ActionTable().Entries() {
		if entry.Elevation != adminaccess.ElevationDeny || entry.Classifier != "" {
			continue
		}
		path, body := entryRequest(entry)
		for name, scenario := range map[string]elevationScenario{"no elevation": standingOnly, "elevated": standingAndElevated} {
			if answer := router.send(t, scenario, entry.Method, path, body); answer.restricted() || answer.flagged {
				t.Errorf("%s, %s: %d %s; the standing set admits it", entry.Key(), name, answer.status, answer.body)
			}
		}
	}
}

// A retention policy shortened under an elevation is refused in the handler,
// after it classified the move against the stored row; lengthened, it is
// written.
func TestAnElevationCannotShortenARetentionPolicy(t *testing.T) {
	router := newElevationRouter(t)
	const path = "/api/v2/admin/retention/policies/conversation"
	body := func(maxDays int) string {
		return `{"min_age_days":0,"max_age_days":` + itoa(maxDays) + `,"allow_project_override":false}`
	}
	if answer := router.send(t, approvedSuperAdmin, http.MethodPut, path, body(30)); !answer.restricted() {
		t.Fatalf("a shortening: %d %s, want 403 %s", answer.status, answer.body, adminaccess.CodeElevationRestricted)
	}
	if answer := router.send(t, approvedSuperAdmin, http.MethodPut, path, body(120)); answer.status != http.StatusOK {
		t.Fatalf("a lengthening: %d %s, want 200", answer.status, answer.body)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// elevationDecisionCaller is the function of the restriction that reads the
// caller's resolution, as the recording resolver names it.
const elevationDecisionCaller = "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api." +
	"(*elevationRestrictions).elevationCaller"

// maintenanceGateCaller is the maintenance window's bypass check.
const maintenanceGateCaller = "/internal/api/middleware.(*maintenanceGate).admits"

// The surface test: every route of the administration surface the
// composed-router driver finds that an action table entry decides, sent as an
// elevated Form session, reaches the restriction's decision before anything
// else resolves: no in-handler check and no route gate decides first, so none
// bypasses it. A route no entry decides (a read), or whose entries all allow
// an elevation, makes no decision to bypass, and the restriction does not
// read the resolution for it.
func TestEveryAdministrationRouteReachesTheElevationRestriction(t *testing.T) {
	driven := drivenComposedSurface(t)
	reached := 0
	for _, entry := range driven.result.Surface {
		restricts := false
		for _, action := range adminaccess.ActionTable().Route(entry.Method, adminaccess.RoutePattern(entry.Pattern)) {
			// The entries the variant's request selects: every entry of a
			// route without a selector, or the one whose selector value the
			// variant sends.
			selected := action.Selector.Value == "" || strings.HasSuffix(" "+entry.Variant, " "+action.Selector.Value)
			restricts = restricts || (selected && action.Elevation != adminaccess.ElevationAllow)
		}
		if !restricts {
			continue
		}
		variant := variantFor(driven.opts, entry)
		variant.Elevation = &adminsurface.Elevation{Level: adminaccess.ElevationLevelAdmin, Path: adminaccess.ElevationPathApproved}
		answer := adminsurface.Send(driven.composed.Handler, entry.Route, variant, driven.opts, adminaccess.Names())
		var administration []adminsurface.Call
		for _, call := range answer.Calls {
			// The maintenance gate asks whether the caller bypasses an open
			// window, in front of every route: it decides no action.
			if call.Mode == auth.PermissionModeAdministration && !strings.HasSuffix(call.Caller, maintenanceGateCaller) {
				administration = append(administration, call)
			}
		}
		if len(administration) == 0 || administration[0].Caller != elevationDecisionCaller {
			first := "none"
			if len(administration) > 0 {
				first = administration[0].Caller
			}
			t.Errorf("%s (%s): the first administration-mode resolution is %s, not the elevation restriction's",
				entry.String(), entry.Variant, first)
			continue
		}
		reached++
	}
	if reached < 35 {
		t.Fatalf("only %d routes reached the restriction; the action table restricts more", reached)
	}
}
