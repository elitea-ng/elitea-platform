package admin_test

// Unit A14 acceptance for the admin Projects surface (issue #200).
//
// The defect class here is NOT "the endpoint answers the wrong status".
// `GET /admin/projects/{mode}` already answered 200 with a `rows` array before
// this unit — it just ignored every filter the page sends and omitted three of
// the five columns it renders. A "does it 200?" test passes that straight
// through, and so does a test that only counts rows. Every case below asserts
// WHICH rows come back, in which order, with which field values.
//
// The two shapes worth naming, both of which the pre-A14 implementation had:
//
//   - `total` DISAGREEING WITH THE PAGE. It counted every project regardless of
//     the filters, so the client paginated over pages that do not exist.
//     TestProjectsTotalCountsTheFilteredSet is the guard.
//   - ROW MULTIPLICATION. Resolving each project's admins by joining the
//     project-role tables multiplies the project out once per admin — what the
//     admin USER listing did before A14, where a user with two roles appeared
//     twice while a separate COUNT disagreed.
//     TestProjectsDoNotMultiplyOnMultipleAdmins is the guard.
//
// The write half follows #130/#180's bar: `PUT /admin/project_suspend/...` is
// asserted by WRITING and then RE-READING through the product's own GET
// handler, never by its status code — 200 with nothing written is exactly what
// a stub returns.
//
// Requires a PostgreSQL to create an isolated database in; skipped otherwise,
// like every other *_postgres_integration_test.go in this service.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
)

/* ── harness ───────────────────────────────────────────────────────────── */

type adminProjectRow struct {
	ID            int      `json:"id"`
	Name          string   `json:"name"`
	ProjectName   string   `json:"project_name"`
	OwnerID       int      `json:"owner_id"`
	OwnerName     string   `json:"owner_name"`
	AdminName     string   `json:"admin_name"`
	AdminNames    []string `json:"admin_names"`
	Status        string   `json:"status"`
	Suspended     bool     `json:"suspended"`
	CreateSuccess bool     `json:"create_success"`
	IsPersonal    bool     `json:"is_personal"`
	// A pointer, so a listing that omits the field fails the test.
	WorkspaceBytes *int64 `json:"workspace_bytes"`
}

type adminProjectListing struct {
	Rows   []adminProjectRow `json:"rows"`
	Total  int               `json:"total"`
	Counts struct {
		Team     int `json:"team"`
		Personal int `json:"personal"`
	} `json:"counts"`
}

// adminProjectsRouter mounts the two routes exactly as internal/api/router.go
// does, minus the route-level permission middleware (which needs the whole auth
// stack; TestRequireCentralPermissions* in internal/api/middleware covers that
// layer, and TestProjectSuspendIsRefusedWithoutThePermission below covers the
// composition of gate and handler).
func adminProjectsRouter(handler *admin.Handler) chi.Router {
	router := chi.NewRouter()
	router.Get("/admin/projects/{mode}", handler.Projects)
	router.Put("/admin/project_suspend/{mode}/{projectID}", handler.ProjectSuspend)
	return router
}

func readProjects(t *testing.T, router chi.Router, query string) adminProjectListing {
	t.Helper()
	target := "/admin/projects/administration?limit=100&offset=0"
	if query != "" {
		target += "&" + query
	}
	recorder := adminDo(t, router, http.MethodGet, target, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET projects status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var listing adminProjectListing
	if err := json.Unmarshal(recorder.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode listing body %q: %v", recorder.Body.String(), err)
	}
	return listing
}

func projectByName(t *testing.T, listing adminProjectListing, name string) (adminProjectRow, bool) {
	t.Helper()
	for _, row := range listing.Rows {
		if row.Name == name {
			return row, true
		}
	}
	return adminProjectRow{}, false
}

func projectNames(listing adminProjectListing) []string {
	names := make([]string, 0, len(listing.Rows))
	for _, row := range listing.Rows {
		names = append(names, row.Name)
	}
	return names
}

func requireProject(t *testing.T, listing adminProjectListing, name string) adminProjectRow {
	t.Helper()
	row, found := projectByName(t, listing, name)
	if !found {
		t.Fatalf("listing does not contain %q (has %v)", name, projectNames(listing))
	}
	return row
}

func projectID(t *testing.T, pool *pgxpool.Pool, name string) int {
	t.Helper()
	var id int
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM centry.project WHERE name = $1`, name).Scan(&id); err != nil {
		t.Fatalf("look up project %s: %v", name, err)
	}
	return id
}

func suspendedSQL(t *testing.T, pool *pgxpool.Pool, id int) bool {
	t.Helper()
	var suspended bool
	if err := pool.QueryRow(context.Background(),
		`SELECT suspended FROM centry.project WHERE id = $1`, id).Scan(&suspended); err != nil {
		t.Fatalf("read suspended for project %d: %v", id, err)
	}
	return suspended
}

/* ── fixture ───────────────────────────────────────────────────────────── */

// prepareProjectsFixture seeds four projects and the users around them.
//
// The shapes it deliberately creates:
//   - `a14-team-alpha` has TWO admins besides its owner, so a join-based admin
//     lookup would emit it twice.
//   - `project_user_9001` is PERSONAL by pylon's `project_user_%` rule and its
//     admin is an `editor`, which only counts on a personal project.
//   - `a14-team-gamma` is `create_success = false`, the only source of the
//     "failed" status.
//   - one member of alpha is a `viewer` (never an admin), one is an `editor`
//     (an admin on a PERSONAL project only, never on a team one), one is the
//     project's own `@centry.user` service account (always filtered out), and
//     one is the OWNER holding `admin` (reported as `owner_name`, so listing
//     them again under `admin_names` would double-count them). A lookup that
//     forgot any of those four predicates shows a name it should not.
const projectsFixtureSQL = `
INSERT INTO auth_core__user (id, email, name, suspended) VALUES
    (9001, 'a14-owner-alpha@autotest.local',  'Alpha Owner',   false),
    (9002, 'a14-admin-one@autotest.local',    'Admin One',     false),
    (9003, 'a14-admin-two@autotest.local',    'Admin Two',     false),
    (9004, 'a14-viewer@autotest.local',       'Plain Viewer',  false),
    (9005, 'system_user_9001@centry.user',    'System User',   false),
    (9006, 'a14-owner-beta@autotest.local',   'Beta Owner',    false),
    (9007, 'a14-personal-editor@autotest.local', 'Personal Editor', false),
    (9008, 'a14-nameless@autotest.local',     NULL,            false),
    (9009, 'a14-team-editor@autotest.local',  'Team Editor',   false);

INSERT INTO centry.project (id, name, owner_id, keycloak_groups, create_success, suspended) VALUES
    (9101, 'a14-team-alpha',    9001, '{}', true,  false),
    (9102, 'a14-team-beta',     9006, '{}', true,  true),
    (9103, 'a14-team-gamma',    9008, '{}', false, false),
    (9104, 'project_user_9001', 9001, '{}', true,  false);

INSERT INTO auth_core__project_role (id, project_id, name) VALUES
    (9201, 9101, 'admin'),
    (9202, 9101, 'viewer'),
    (9205, 9101, 'editor'),
    (9203, 9104, 'editor'),
    (9204, 9104, 'admin');

INSERT INTO auth_core__project_user_role (project_id, user_id, role_id) VALUES
    (9101, 9002, 9201),
    (9101, 9003, 9201),
    (9101, 9004, 9202),
    (9101, 9005, 9201),
    (9101, 9009, 9205),
    (9101, 9001, 9201),
    (9104, 9007, 9203);
`

func prepareProjectsFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), projectsFixtureSQL); err != nil {
		t.Fatalf("seed projects fixture: %v", err)
	}
}

/* ── the listing ───────────────────────────────────────────────────────── */

func TestProjectsListReportsOwnerAdminsAndStatus(t *testing.T) {
	pool := newProjectsPool(t)
	prepareProjectsFixture(t, pool)
	router := adminProjectsRouter(admin.NewHandler(pool))

	listing := readProjects(t, router, "")

	alpha := requireProject(t, listing, "a14-team-alpha")
	if alpha.OwnerName != "Alpha Owner" {
		t.Errorf("owner_name = %q, want %q", alpha.OwnerName, "Alpha Owner")
	}
	// `admin_name` is the pylon-era alias for the owner; a client reading it
	// must not silently get an admin instead.
	if alpha.AdminName != "Alpha Owner" {
		t.Errorf("admin_name = %q, want the owner name", alpha.AdminName)
	}
	assertSameSet(t, "admin_names", alpha.AdminNames, []string{"Admin One", "Admin Two"})
	if alpha.Status != "active" {
		t.Errorf("status = %q, want active", alpha.Status)
	}
	if alpha.IsPersonal {
		t.Error("a14-team-alpha reported as personal")
	}
	if alpha.ProjectName != alpha.Name {
		t.Errorf("project_name = %q, want %q", alpha.ProjectName, alpha.Name)
	}

	// Four different reasons a project member must NOT appear in `admin_names`,
	// one per seeded row — the viewer holds no admin role; the editor holds one
	// that only counts on a PERSONAL project; the service account is a system
	// user; and the owner is already reported as `owner_name`, so listing them
	// again would double-count them. Each is a separate predicate in the query
	// and each is a mutation that survived until this loop named it.
	for _, unwanted := range []string{"Plain Viewer", "Team Editor", "System User", "Alpha Owner"} {
		for _, name := range alpha.AdminNames {
			if name == unwanted {
				t.Errorf("admin_names contains %q: %v", unwanted, alpha.AdminNames)
			}
		}
	}
}

func TestProjectsStatusDistinguishesSuspendedAndFailed(t *testing.T) {
	pool := newProjectsPool(t)
	prepareProjectsFixture(t, pool)
	router := adminProjectsRouter(admin.NewHandler(pool))

	listing := readProjects(t, router, "")

	for _, testCase := range []struct{ project, status string }{
		{"a14-team-alpha", "active"},
		{"a14-team-beta", "suspended"},
		{"a14-team-gamma", "failed"},
	} {
		if row := requireProject(t, listing, testCase.project); row.Status != testCase.status {
			t.Errorf("%s status = %q, want %q", testCase.project, row.Status, testCase.status)
		}
	}
}

// The owner may have no `name`. pylon falls back to the email; a blank cell
// would be indistinguishable from "this project has no owner".
func TestProjectsFallBackToTheOwnerEmailWhenTheNameIsNull(t *testing.T) {
	pool := newProjectsPool(t)
	prepareProjectsFixture(t, pool)
	router := adminProjectsRouter(admin.NewHandler(pool))

	row := requireProject(t, readProjects(t, router, ""), "a14-team-gamma")
	if row.OwnerName != "a14-nameless@autotest.local" {
		t.Errorf("owner_name = %q, want the email fallback", row.OwnerName)
	}
}

// A LEFT JOIN onto the project-role tables emits one row per admin. Alpha has
// two, so the join version returns 5 rows where this must return 4 — and its
// `total`, computed separately, would say 4 either way.
func TestProjectsDoNotMultiplyOnMultipleAdmins(t *testing.T) {
	pool := newProjectsPool(t)
	prepareProjectsFixture(t, pool)
	router := adminProjectsRouter(admin.NewHandler(pool))

	listing := readProjects(t, router, "")

	seen := map[string]int{}
	for _, row := range listing.Rows {
		seen[row.Name]++
	}
	if seen["a14-team-alpha"] != 1 {
		t.Fatalf("a14-team-alpha appears %d times (rows %v)", seen["a14-team-alpha"], projectNames(listing))
	}
	if len(listing.Rows) != listing.Total {
		t.Fatalf("page holds %d rows but total says %d", len(listing.Rows), listing.Total)
	}
}

func TestProjectsPersonalTabUsesTheNamePrefixAndEditorAdmins(t *testing.T) {
	pool := newProjectsPool(t)
	prepareProjectsFixture(t, pool)
	router := adminProjectsRouter(admin.NewHandler(pool))

	personal := readProjects(t, router, "project_type=personal")
	if names := projectNames(personal); len(names) != 1 || names[0] != "project_user_9001" {
		t.Fatalf("personal tab = %v, want [project_user_9001]", names)
	}
	row := personal.Rows[0]
	if !row.IsPersonal {
		t.Error("is_personal = false on a project_user_ project")
	}
	// `editor` counts as an admin ONLY on a personal project — projects.py's
	// `if is_personal_project:` branch.
	assertSameSet(t, "personal admin_names", row.AdminNames, []string{"Personal Editor"})

	team := readProjects(t, router, "project_type=team")
	for _, name := range projectNames(team) {
		if name == "project_user_9001" {
			t.Errorf("team tab contains the personal project: %v", projectNames(team))
		}
	}
	if len(team.Rows) != 3 {
		t.Errorf("team tab has %d rows, want 3 (%v)", len(team.Rows), projectNames(team))
	}
}

// The tab counts label BOTH tabs, so they must describe the whole deployment
// regardless of which tab is being shown or what is typed in the search box.
func TestProjectsCountsIgnoreTheActiveFilters(t *testing.T) {
	pool := newProjectsPool(t)
	prepareProjectsFixture(t, pool)
	router := adminProjectsRouter(admin.NewHandler(pool))

	for _, query := range []string{"", "project_type=personal", "project_type=team&search=alpha"} {
		listing := readProjects(t, router, query)
		if listing.Counts.Team != 3 || listing.Counts.Personal != 1 {
			t.Errorf("counts for %q = {team:%d personal:%d}, want {team:3 personal:1}",
				query, listing.Counts.Team, listing.Counts.Personal)
		}
	}
}

func TestProjectsTotalCountsTheFilteredSet(t *testing.T) {
	pool := newProjectsPool(t)
	prepareProjectsFixture(t, pool)
	router := adminProjectsRouter(admin.NewHandler(pool))

	// The pre-A14 implementation answered 4 here — the count of every project —
	// while returning one row, so the client paged into emptiness.
	listing := readProjects(t, router, "search=beta")
	if listing.Total != 1 {
		t.Errorf("total = %d for search=beta, want 1 (rows %v)", listing.Total, projectNames(listing))
	}
	if len(listing.Rows) != 1 {
		t.Errorf("rows = %v, want just a14-team-beta", projectNames(listing))
	}
}

func TestProjectsSearchMatchesNameIDAndOwner(t *testing.T) {
	pool := newProjectsPool(t)
	prepareProjectsFixture(t, pool)
	router := adminProjectsRouter(admin.NewHandler(pool))

	for _, testCase := range []struct{ term, want string }{
		{"beta", "a14-team-beta"},                         // the project name
		{"9103", "a14-team-gamma"},                        // the project id, as text
		{"Beta Owner", "a14-team-beta"},                   // the owner's display name
		{"a14-nameless@autotest.local", "a14-team-gamma"}, // the owner's email
	} {
		listing := readProjects(t, router, "search="+url.QueryEscape(testCase.term))
		names := projectNames(listing)
		if len(names) != 1 || names[0] != testCase.want {
			t.Errorf("search %q → %v, want [%s]", testCase.term, names, testCase.want)
		}
	}

	// The owner match is not a name-only accident: `Alpha Owner` owns BOTH
	// a14-team-alpha and the personal project, and searching them must return
	// both — the same behaviour pylon's `auth_search_users` → `owner_ids` gives.
	byOwner := projectNames(readProjects(t, router, "search="+url.QueryEscape("Alpha Owner")))
	assertSameSet(t, `search="Alpha Owner"`, byOwner, []string{"a14-team-alpha", "project_user_9001"})
}

func TestProjectsSortByNameIsStableAndReversible(t *testing.T) {
	pool := newProjectsPool(t)
	prepareProjectsFixture(t, pool)
	router := adminProjectsRouter(admin.NewHandler(pool))

	ascending := projectNames(readProjects(t, router, "project_type=team&sort_by=name&sort_order=asc"))
	want := []string{"a14-team-alpha", "a14-team-beta", "a14-team-gamma"}
	assertSameOrder(t, "sort_by=name asc", ascending, want)

	descending := projectNames(readProjects(t, router, "project_type=team&sort_by=name&sort_order=desc"))
	assertSameOrder(t, "sort_by=name desc", descending,
		[]string{"a14-team-gamma", "a14-team-beta", "a14-team-alpha"})
}

// `sort_by=status` orders active → failed → suspended, per
// list_projects_paginated's sort_map ranks (0, 2, 3).
func TestProjectsSortByStatusFollowsTheLegacyRanking(t *testing.T) {
	pool := newProjectsPool(t)
	prepareProjectsFixture(t, pool)
	router := adminProjectsRouter(admin.NewHandler(pool))

	ordered := projectNames(readProjects(t, router, "project_type=team&sort_by=status&sort_order=asc"))
	assertSameOrder(t, "sort_by=status asc", ordered,
		[]string{"a14-team-alpha", "a14-team-gamma", "a14-team-beta"})
}

// `sort_by` reaches an ORDER BY. An unknown value must fall back to the default
// column, and an injected one must not execute.
func TestProjectsRejectAnInjectedSortColumn(t *testing.T) {
	pool := newProjectsPool(t)
	prepareProjectsFixture(t, pool)
	router := adminProjectsRouter(admin.NewHandler(pool))

	listing := readProjects(t, router, "sort_by=name%3B+DROP+TABLE+centry.project")
	if len(listing.Rows) != 4 {
		t.Fatalf("injected sort_by returned %d rows, want the default ordering's 4", len(listing.Rows))
	}
	var remaining int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM centry.project`).Scan(&remaining); err != nil {
		t.Fatalf("centry.project is gone after an injected sort_by: %v", err)
	}
	if remaining != 4 {
		t.Fatalf("centry.project holds %d rows, want 4", remaining)
	}
}

// Paging must not repeat or drop a row. `ORDER BY name` alone is not a total
// order once two projects share a name, which the fixture below arranges.
func TestProjectsPaginationDoesNotRepeatOrDropRowsOnTiedNames(t *testing.T) {
	pool := newProjectsPool(t)
	if _, err := pool.Exec(context.Background(), `
INSERT INTO auth_core__user (id, email, name, suspended)
VALUES (9500, 'a14-tie@autotest.local', 'Tie Owner', false);
INSERT INTO centry.project (id, name, owner_id, keycloak_groups, create_success, suspended)
SELECT generated, 'a14-tied-name', 9500, '{}', true, false
FROM generate_series(9601, 9610) AS generated;`); err != nil {
		t.Fatalf("seed tied-name projects: %v", err)
	}
	router := adminProjectsRouter(admin.NewHandler(pool))

	seen := map[int]int{}
	for offset := 0; offset < 10; offset += 3 {
		recorder := adminDo(t, router, http.MethodGet,
			fmt.Sprintf("/admin/projects/administration?limit=3&offset=%d&sort_by=name", offset), nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("page at offset %d: status %d", offset, recorder.Code)
		}
		var listing adminProjectListing
		if err := json.Unmarshal(recorder.Body.Bytes(), &listing); err != nil {
			t.Fatalf("decode page at offset %d: %v", offset, err)
		}
		for _, row := range listing.Rows {
			seen[row.ID]++
		}
	}
	if len(seen) != 10 {
		t.Fatalf("paging over 10 tied rows saw %d distinct ids, want 10", len(seen))
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("project %d appeared %d times across the pages", id, count)
		}
	}
}

func TestProjectsListSurvivesAnEmptyDeployment(t *testing.T) {
	pool := newProjectsPool(t)
	router := adminProjectsRouter(admin.NewHandler(pool))

	listing := readProjects(t, router, "")
	if listing.Rows == nil {
		t.Error("rows is null, not an empty array — the client maps over it")
	}
	if len(listing.Rows) != 0 || listing.Total != 0 {
		t.Errorf("empty deployment reported %d rows / total %d", len(listing.Rows), listing.Total)
	}
	if listing.Counts.Team != 0 || listing.Counts.Personal != 0 {
		t.Errorf("empty deployment reported counts %+v", listing.Counts)
	}
}

// A read failure must be REPORTED. Degrading it to an empty page renders
// exactly like "this deployment has no projects" — the shape #130's post-mortem
// named as worse than a 404, and the one the implementation this replaces had
// in all three of its error branches.
func TestProjectsReportAFailedListingAsAnError(t *testing.T) {
	pool := newProjectsPool(t)
	prepareProjectsFixture(t, pool)
	router := adminProjectsRouter(admin.NewHandler(pool))

	// The listing works first, so a 500 below cannot be passing for an
	// unrelated reason.
	if len(readProjects(t, router, "").Rows) == 0 {
		t.Fatal("fixture did not load")
	}

	if _, err := pool.Exec(context.Background(), `DROP TABLE centry.project CASCADE`); err != nil {
		t.Fatalf("drop centry.project: %v", err)
	}

	recorder := adminDo(t, router, http.MethodGet,
		"/admin/projects/administration?limit=100&offset=0", nil)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("GET with a broken table status = %d, want 500 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	// And the body must not be a page: a 500 carrying `{"rows":[],"total":0}`
	// is still a client rendering an empty state.
	if bytes.Contains(recorder.Body.Bytes(), []byte(`"rows"`)) {
		t.Errorf("the error body looks like a listing: %s", recorder.Body.String())
	}
}

/* ── the write ─────────────────────────────────────────────────────────── */

func TestProjectSuspendWritesAndTheListingReportsIt(t *testing.T) {
	pool := newProjectsPool(t)
	prepareProjectsFixture(t, pool)
	router := adminProjectsRouter(admin.NewHandler(pool))

	id := projectID(t, pool, "a14-team-alpha")
	if before := requireProject(t, readProjects(t, router, ""), "a14-team-alpha"); before.Suspended {
		t.Fatalf("fixture project starts suspended")
	}

	recorder := adminDo(t, router, http.MethodPut,
		fmt.Sprintf("/admin/project_suspend/administration/%d", id),
		map[string]any{"suspended": true})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT suspend status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	// RE-READ through the product's own GET. A handler that answers 200 and
	// writes nothing — the #130/#180 shape — cannot pass this.
	after := requireProject(t, readProjects(t, router, ""), "a14-team-alpha")
	if !after.Suspended {
		t.Error("re-read still reports suspended = false")
	}
	if after.Status != "suspended" {
		t.Errorf("re-read status = %q, want suspended", after.Status)
	}
	if !suspendedSQL(t, pool, id) {
		t.Error("centry.project.suspended is still false in SQL")
	}
}

func TestProjectSuspendUnsuspends(t *testing.T) {
	pool := newProjectsPool(t)
	prepareProjectsFixture(t, pool)
	router := adminProjectsRouter(admin.NewHandler(pool))

	id := projectID(t, pool, "a14-team-beta")
	if !suspendedSQL(t, pool, id) {
		t.Fatalf("fixture project a14-team-beta does not start suspended")
	}

	recorder := adminDo(t, router, http.MethodPut,
		fmt.Sprintf("/admin/project_suspend/administration/%d", id),
		map[string]any{"suspended": false})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT unsuspend status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	after := requireProject(t, readProjects(t, router, ""), "a14-team-beta")
	if after.Suspended || after.Status != "active" {
		t.Errorf("re-read reports suspended=%v status=%q, want false/active", after.Suspended, after.Status)
	}
	if suspendedSQL(t, pool, id) {
		t.Error("centry.project.suspended is still true in SQL")
	}
}

// A missing `suspended` must not be read as `false`: the pre-A14 handler
// decoded into a plain bool, so a body that omitted the field UNSUSPENDED the
// project silently.
func TestProjectSuspendRequiresTheSuspendedField(t *testing.T) {
	pool := newProjectsPool(t)
	prepareProjectsFixture(t, pool)
	router := adminProjectsRouter(admin.NewHandler(pool))

	id := projectID(t, pool, "a14-team-beta")
	recorder := adminDo(t, router, http.MethodPut,
		fmt.Sprintf("/admin/project_suspend/administration/%d", id), map[string]any{})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("PUT without `suspended` status = %d, want 400", recorder.Code)
	}
	if !suspendedSQL(t, pool, id) {
		t.Error("the rejected request unsuspended the project anyway")
	}
}

// A no-op UPDATE reports 200 with zero rows affected. Answering 200 there tells
// the operator a project they mistyped was suspended.
func TestProjectSuspendAnswers404ForAnUnknownProject(t *testing.T) {
	pool := newProjectsPool(t)
	prepareProjectsFixture(t, pool)
	router := adminProjectsRouter(admin.NewHandler(pool))

	recorder := adminDo(t, router, http.MethodPut,
		"/admin/project_suspend/administration/987654", map[string]any{"suspended": true})
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("PUT for an unknown project status = %d, want 404 (body %s)",
			recorder.Code, recorder.Body.String())
	}
}

func TestProjectSuspendRejectsABadProjectID(t *testing.T) {
	pool := newProjectsPool(t)
	prepareProjectsFixture(t, pool)
	router := adminProjectsRouter(admin.NewHandler(pool))

	for _, id := range []string{"0", "-1", "abc"} {
		recorder := adminDo(t, router, http.MethodPut,
			"/admin/project_suspend/administration/"+id, map[string]any{"suspended": true})
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("PUT with project id %q status = %d, want 400", id, recorder.Code)
		}
	}
}

// Only `administration` has a handler, in pylon and here. Another mode must not
// reach the write at all.
func TestProjectSuspendRejectsANonAdministrationMode(t *testing.T) {
	pool := newProjectsPool(t)
	prepareProjectsFixture(t, pool)
	router := adminProjectsRouter(admin.NewHandler(pool))

	id := projectID(t, pool, "a14-team-alpha")
	recorder := adminDo(t, router, http.MethodPut,
		fmt.Sprintf("/admin/project_suspend/default/%d", id), map[string]any{"suspended": true})
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("PUT in default mode status = %d, want 404", recorder.Code)
	}
	if suspendedSQL(t, pool, id) {
		t.Error("the rejected request suspended the project anyway")
	}
}

/* ── the gate ──────────────────────────────────────────────────────────── */

// gatedProjectsRouter mounts the two routes WITH the route-level permission
// middleware, exactly as internal/api/router.go composes them.
//
// The cases below are the ones the brief on #200 calls the tenancy bar: a
// caller who lacks the permission must be REFUSED by the server, not merely
// shown no button. `window.admin_ui_config.permissions` hands every session the
// same hardcoded array, so hiding a control changes nothing about what a
// crafted request can do.
func gatedProjectsRouter(
	handler *admin.Handler, resolver auth.PermissionResolver, principal auth.User,
) chi.Router {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), principal)))
		})
	})
	router.With(apimw.RequireCentralPermissions(
		resolver, auth.PermissionModeAdministration, "projects.projects.projects.view",
	)).Get("/admin/projects/{mode}", handler.Projects)
	router.With(apimw.RequireCentralPermissions(
		resolver, auth.PermissionModeAdministration, "projects.projects.projects.edit",
	)).Put("/admin/project_suspend/{mode}/{projectID}", handler.ProjectSuspend)
	return router
}

func TestProjectSuspendIsRefusedWithoutTheEditPermission(t *testing.T) {
	pool := newProjectsPool(t)
	prepareProjectsFixture(t, pool)
	handler := admin.NewHandler(pool)
	principal := auth.User{ID: "1", UserID: "1"}
	id := projectID(t, pool, "a14-team-alpha")

	// The caller holds the READ permission and nothing else — the shape a
	// viewer-shaped administrator has, and the one a hidden button "protects".
	gated := gatedProjectsRouter(handler, grantingResolver("projects.projects.projects.view"), principal)

	recorder := adminDo(t, gated, http.MethodPut,
		fmt.Sprintf("/admin/project_suspend/administration/%d", id),
		map[string]any{"suspended": true})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("PUT suspend without the edit permission status = %d, want 403", recorder.Code)
	}
	if suspendedSQL(t, pool, id) {
		t.Fatal("the refused request suspended the project anyway")
	}

	// And the same request DOES go through once the permission is held, so the
	// refusal above cannot be passing for an unrelated reason.
	allowed := gatedProjectsRouter(handler,
		grantingResolver("projects.projects.projects.view", "projects.projects.projects.edit"), principal)
	if recorder := adminDo(t, allowed, http.MethodPut,
		fmt.Sprintf("/admin/project_suspend/administration/%d", id),
		map[string]any{"suspended": true}); recorder.Code != http.StatusOK {
		t.Fatalf("PUT suspend WITH the edit permission status = %d, want 200 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	if !suspendedSQL(t, pool, id) {
		t.Fatal("the permitted request did not suspend the project")
	}
}

func TestProjectsListingIsRefusedWithoutTheViewPermission(t *testing.T) {
	pool := newProjectsPool(t)
	prepareProjectsFixture(t, pool)
	handler := admin.NewHandler(pool)
	principal := auth.User{ID: "1", UserID: "1"}

	// A project row names the project, its owner and its admins across every
	// tenant, so the listing itself is the sensitive part.
	gated := gatedProjectsRouter(handler, grantingResolver("projects.projects.projects.edit"), principal)
	if recorder := adminDo(t, gated, http.MethodGet,
		"/admin/projects/administration?limit=100&offset=0", nil); recorder.Code != http.StatusForbidden {
		t.Fatalf("GET projects without the view permission status = %d, want 403", recorder.Code)
	}

	allowed := gatedProjectsRouter(handler, grantingResolver("projects.projects.projects.view"), principal)
	if recorder := adminDo(t, allowed, http.MethodGet,
		"/admin/projects/administration?limit=100&offset=0", nil); recorder.Code != http.StatusOK {
		t.Fatalf("GET projects WITH the view permission status = %d, want 200", recorder.Code)
	}
}

/* ── helpers ───────────────────────────────────────────────────────────── */

func assertSameSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", label, got, want)
		return
	}
	seen := map[string]bool{}
	for _, value := range got {
		seen[value] = true
	}
	for _, value := range want {
		if !seen[value] {
			t.Errorf("%s = %v, want %v", label, got, want)
			return
		}
	}
}

func assertSameOrder(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("%s = %v, want %v", label, got, want)
		}
	}
}

// newProjectsPool creates an isolated database and applies the REAL bootstrap
// migration — the same 001_initial.sql a fresh deployment gets — so the
// centry.project and auth_core__* DDL the tests read through is the shipped one
// rather than a second copy that could drift from it.
func newProjectsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_admin_projects_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		if _, dropErr := adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); dropErr != nil {
			t.Errorf("drop database after pool open failure: %v", dropErr)
		}
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s, not the old 20 s to 30 s. This DROP queues behind the
		// CREATE DATABASE calls of every package that `go test ./...` runs at
		// the same time, so the wait is server load and not a hang. Two full
		// runs failed here with "drop isolated ... database: timeout" (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})

	initial, err := os.ReadFile(filepath.Join("..", "..", "..", "infra", "db", "migrations", "001_initial.sql"))
	if err != nil {
		t.Fatalf("read 001_initial.sql: %v", err)
	}
	if _, err := pool.Exec(ctx, string(initial)); err != nil {
		t.Fatalf("apply 001_initial.sql: %v", err)
	}
	// 001_initial.sql seeds one bootstrap project ("Default Project") and its
	// owner. Every assertion below is about which rows the HANDLER returns for
	// the rows a test seeded, so the bootstrap ones are cleared: leaving them
	// would make each expected count carry a silent "+1 for the fixture" that
	// drifts the moment the bootstrap seed changes.
	if _, err := pool.Exec(ctx, `DELETE FROM centry.project`); err != nil {
		t.Fatalf("clear bootstrap projects: %v", err)
	}
	// The listing reads the workspace store's segments (shared 0168), which a
	// deployment always has. This database carries only 001_initial.sql, so
	// the columns the listing reads are created here.
	if _, err := pool.Exec(ctx, `
CREATE SCHEMA IF NOT EXISTS elitea_runtime;
CREATE TABLE IF NOT EXISTS elitea_runtime.workspace_segments (
    project_id integer NOT NULL, conversation_uuid uuid NOT NULL, byte_length bigint NOT NULL,
    superseded_at timestamptz)`); err != nil {
		t.Fatalf("create the workspace segments: %v", err)
	}
	// The naive-clock probe the dbtest template installs, after the last
	// schema statement: a writer that stores the session-local clock in a
	// `timestamp` WITHOUT time zone column fails instead of passing unread.
	if err := dbtest.InstallNaiveClockProbe(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

// The listing reports each project's cloud copy storage (ADR-0034 decision
// 3): the live workspace segments only, not superseded ones, and not another
// project's. It sorts by it.
func TestProjectsReportWorkspaceBytes(t *testing.T) {
	pool := newProjectsPool(t)
	prepareProjectsFixture(t, pool)
	router := adminProjectsRouter(admin.NewHandler(pool))
	ctx := context.Background()
	alpha := projectID(t, pool, "a14-team-alpha")
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.workspace_segments (project_id, conversation_uuid, byte_length, superseded_at) VALUES
    ($1, gen_random_uuid(), 3000, NULL),
    ($1, gen_random_uuid(), 500, NULL),
    ($1, gen_random_uuid(), 9999, clock_timestamp()),
    ($2, gen_random_uuid(), 7, NULL)`, alpha, alpha+100000); err != nil {
		t.Fatal(err)
	}
	listing := readProjects(t, router, "sort_by=workspace_bytes&sort_order=desc")
	row := requireProject(t, listing, "a14-team-alpha")
	if row.WorkspaceBytes == nil || *row.WorkspaceBytes != 3500 {
		t.Fatalf("workspace_bytes = %v, want 3500 (live segments of this project only)", row.WorkspaceBytes)
	}
	if listing.Rows[0].Name != "a14-team-alpha" {
		t.Fatalf("sorted by workspace_bytes desc, first = %q", listing.Rows[0].Name)
	}
	for _, other := range listing.Rows[1:] {
		if other.WorkspaceBytes == nil || *other.WorkspaceBytes != 0 {
			t.Fatalf("%s workspace_bytes = %v, want 0", other.Name, other.WorkspaceBytes)
		}
	}
}
