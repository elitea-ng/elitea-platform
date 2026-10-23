package scheduling_test

// Unit A14 acceptance for the platform cron table (issue #200).
//
// The bar from #130/#180: every write below is asserted by WRITING and then
// RE-READING through the product's own GET handler. A status code proves
// nothing here — 200 is exactly what the stub this replaces already returned,
// and the handler that preceded this one answered 200 with `[]` on EVERY
// failure, including a missing table.
//
// The three shapes worth naming, because the pre-A14 implementation had all
// three and each looked healthy from the reference UI:
//
//   - ANSWERING A DIFFERENT QUESTION. It read `meta->'trigger'` off tenant
//     application versions and called those schedules. Nothing executes those
//     (#193), while `centry.schedule` — which `services/elitea-scheduler` polls
//     every minute — went unread. TestListingReadsTheTableTheSchedulerRuns is
//     the guard.
//   - A STRUCTURALLY DEAD READ. It short-circuited on `projectID == "0"`, the
//     only projectID the admin page sends, so the tab could not show a row
//     under any circumstance. TestAdministrationListingIgnoresTheProjectIDSegment.
//   - NO WRITE PATH AT ALL. The PUT had no route, so the active switch and the
//     inline cron editor were controls with nothing behind them.
//
// Plus the one this unit adds deliberately: a scheduled run has no interactive
// principal, so `rpc_func`/`rpc_kwargs` must not be settable by a client.
// TestUpdateRefusesToRetargetWhatAScheduleRuns.
//
// Requires a PostgreSQL to create an isolated database in; skipped otherwise,
// like every other *_postgres_integration_test.go in this service.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/scheduling"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
)

/* ── harness ───────────────────────────────────────────────────────────── */

type scheduleRow struct {
	ID        int64          `json:"id"`
	Name      string         `json:"name"`
	ProjectID *int           `json:"project_id"`
	Cron      string         `json:"cron"`
	Active    bool           `json:"active"`
	RPCFunc   string         `json:"rpc_func"`
	RPCKwargs map[string]any `json:"rpc_kwargs"`
	LastRun   *time.Time     `json:"last_run"`
}

type scheduleListing struct {
	Rows  []scheduleRow `json:"rows"`
	Total int           `json:"total"`
}

// schedulesRouter mounts both routes exactly as internal/api/router.go does,
// minus the route-level permission middleware. The gate is covered separately
// by the `gatedSchedulesRouter` cases at the bottom of this file, and by
// TestRequireCentralPermissions* in internal/api/middleware.
// It mounts BOTH registrations — the static `administration` pair and the
// `{mode}` pair — because that composition is itself load-bearing: a static
// segment binds no URL parameter, so a handler that inferred the mode from
// `chi.URLParam(r, "mode")` alone answered 400 to every administration request
// while passing a test that mounted only the `{mode}` route. It did, and this is
// what caught it.
func schedulesRouter(handler *scheduling.Handler) chi.Router {
	router := chi.NewRouter()
	router.Get("/scheduling/schedules/administration/{projectID}", handler.AdministrationSchedules)
	router.Put("/scheduling/schedules/administration/{projectID}", handler.AdministrationSchedulesUpdate)
	router.Get("/scheduling/schedules/{mode}/{projectID}", handler.Schedules)
	router.Put("/scheduling/schedules/{mode}/{projectID}", handler.SchedulesUpdate)
	return router
}

func scheduleDo(t *testing.T, router chi.Router, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// readSchedules re-reads through the SAME GET handler the admin page calls.
// This is the assertion an unwired or no-op write cannot pass.
func readSchedules(t *testing.T, router chi.Router, path string) scheduleListing {
	t.Helper()
	recorder := scheduleDo(t, router, http.MethodGet, path, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200 (body %s)", path, recorder.Code, recorder.Body.String())
	}
	var listing scheduleListing
	if err := json.Unmarshal(recorder.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode listing body %q: %v", recorder.Body.String(), err)
	}
	return listing
}

func readAdminSchedules(t *testing.T, router chi.Router) scheduleListing {
	t.Helper()
	// `0` is the projectID the admin page actually sends. The handler this
	// replaces treated it as "return nothing".
	return readSchedules(t, router, "/scheduling/schedules/administration/0")
}

func scheduleByName(t *testing.T, listing scheduleListing, name string) scheduleRow {
	t.Helper()
	for _, row := range listing.Rows {
		if row.Name == name {
			return row
		}
	}
	t.Fatalf("listing does not contain %q (has %v)", name, scheduleNames(listing))
	return scheduleRow{}
}

func scheduleNames(listing scheduleListing) []string {
	names := make([]string, 0, len(listing.Rows))
	for _, row := range listing.Rows {
		names = append(names, row.Name)
	}
	return names
}

func scheduleID(t *testing.T, pool *pgxpool.Pool, name string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM centry.schedule WHERE name = $1`, name).Scan(&id); err != nil {
		t.Fatalf("look up schedule %s: %v", name, err)
	}
	return id
}

// scheduleSQL bypasses the read handler entirely. A re-read through GET proves
// the product agrees with itself; this proves the ROW changed, so a read that
// happened to synthesise the expected value could not pass both.
func scheduleSQL(t *testing.T, pool *pgxpool.Pool, id int64) (cron string, active bool, rpcFunc string, kwargs string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT cron, active, rpc_func, rpc_kwargs::text FROM centry.schedule WHERE id = $1`, id).
		Scan(&cron, &active, &rpcFunc, &kwargs); err != nil {
		t.Fatalf("read schedule %d: %v", id, err)
	}
	return cron, active, rpcFunc, kwargs
}

const schedulesFixture = `
INSERT INTO centry.schedule (name, project_id, cron, active, rpc_func, rpc_kwargs, last_run) VALUES
    ('index_scheduling',          NULL, '*/5 * * * *', true,  'indexer_scan',        '{}'::jsonb, '2026-08-01 10:00:00'),
    ('usage_monitor',             NULL, '0 * * * *',   true,  'usage_collect',       '{"window": 60}'::jsonb, NULL),
    ('storage_used_space_check',  NULL, '0 3 * * *',   false, 'storage_space_check', '{}'::jsonb, NULL),
    ('project_alpha_report',      41,   '0 6 * * 1',   true,  'project_report',      '{"project_id": 41}'::jsonb, NULL),
    ('project_beta_report',       42,   '0 7 * * 1',   true,  'project_report',      '{"project_id": 42}'::jsonb, NULL);
`

func prepareSchedulesFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), schedulesFixture); err != nil {
		t.Fatalf("seed schedules fixture: %v", err)
	}
}

/* ── the read ──────────────────────────────────────────────────────────── */

// TestListingReadsTheTableTheSchedulerRuns pins the listing to
// `centry.schedule` — the table services/elitea-scheduler polls — and to the
// field names the client indexes into. The implementation this replaces read
// tenant application-version trigger metadata and emitted `{"items": …}`.
func TestListingReadsTheTableTheSchedulerRuns(t *testing.T) {
	pool := newSchedulesPool(t)
	prepareSchedulesFixture(t, pool)
	router := schedulesRouter(scheduling.NewHandler(pool))

	listing := readAdminSchedules(t, router)
	if listing.Total != 5 {
		t.Fatalf("total = %d, want 5 (rows %v)", listing.Total, scheduleNames(listing))
	}
	if len(listing.Rows) != listing.Total {
		t.Fatalf("len(rows) = %d but total = %d", len(listing.Rows), listing.Total)
	}

	row := scheduleByName(t, listing, "usage_monitor")
	if row.Cron != "0 * * * *" {
		t.Errorf("cron = %q, want %q", row.Cron, "0 * * * *")
	}
	if row.RPCFunc != "usage_collect" {
		t.Errorf("rpc_func = %q, want %q", row.RPCFunc, "usage_collect")
	}
	if !row.Active {
		t.Error("usage_monitor reports inactive; the fixture has it active")
	}
	if row.ProjectID != nil {
		t.Errorf("project_id = %v, want null for a platform schedule", *row.ProjectID)
	}
	if row.LastRun != nil {
		t.Errorf("last_run = %v, want null", row.LastRun)
	}
	// rpc_kwargs round-trips as an object, not as a JSON string.
	if window, ok := row.RPCKwargs["window"]; !ok || window != float64(60) {
		t.Errorf("rpc_kwargs = %v, want {\"window\": 60}", row.RPCKwargs)
	}

	// `last_run` is what the page's "Last Run" column renders. A row that HAS
	// run must not report "Never".
	ran := scheduleByName(t, listing, "index_scheduling")
	if ran.LastRun == nil {
		t.Fatal("index_scheduling reports last_run null; the fixture stamped it")
	}
	if ran.LastRun.Year() != 2026 || ran.LastRun.Month() != time.August {
		t.Errorf("last_run = %v, want 2026-08-01", ran.LastRun)
	}
}

// TestListingReportsInactiveRowsAsInactive is the guard for the hardcoded
// `Enabled = true` the previous handler carried: with the flag pinned, a
// disabled schedule was unrepresentable, and the page's switch could only ever
// render on.
func TestListingReportsInactiveRowsAsInactive(t *testing.T) {
	pool := newSchedulesPool(t)
	prepareSchedulesFixture(t, pool)
	router := schedulesRouter(scheduling.NewHandler(pool))

	row := scheduleByName(t, readAdminSchedules(t, router), "storage_used_space_check")
	if row.Active {
		t.Fatal("an inactive schedule is reported active")
	}
}

// TestAdministrationListingIgnoresTheProjectIDSegment covers the short-circuit
// that made the admin tab structurally dead: `projectID == "0"` returned an
// empty list, and `0` is the only projectID the admin page ever sends.
func TestAdministrationListingIgnoresTheProjectIDSegment(t *testing.T) {
	pool := newSchedulesPool(t)
	prepareSchedulesFixture(t, pool)
	router := schedulesRouter(scheduling.NewHandler(pool))

	for _, segment := range []string{"0", "1", "999"} {
		listing := readSchedules(t, router, "/scheduling/schedules/administration/"+segment)
		if listing.Total != 5 {
			t.Errorf("administration listing with projectID=%s returned %d rows, want all 5",
				segment, listing.Total)
		}
	}
}

// TestProjectListingIsScopedToItsProject pins the OTHER mode. Platform rows
// (`project_id IS NULL`) must not appear inside one project's settings: they
// are not that project's to disable.
func TestProjectListingIsScopedToItsProject(t *testing.T) {
	pool := newSchedulesPool(t)
	prepareSchedulesFixture(t, pool)
	router := schedulesRouter(scheduling.NewHandler(pool))

	listing := readSchedules(t, router, "/scheduling/schedules/default/41")
	if listing.Total != 1 {
		t.Fatalf("project 41 listing = %v, want only project_alpha_report", scheduleNames(listing))
	}
	if listing.Rows[0].Name != "project_alpha_report" {
		t.Fatalf("project 41 listing = %v, want project_alpha_report", scheduleNames(listing))
	}
}

// TestListingIsAStableTotalOrder guards the ORDER BY tiebreaker: `name` has no
// unique constraint on this table, so name alone is not a total order and two
// reads of an unchanged table could disagree.
func TestListingIsAStableTotalOrder(t *testing.T) {
	pool := newSchedulesPool(t)
	if _, err := pool.Exec(context.Background(), `
INSERT INTO centry.schedule (name, cron, active, rpc_func) VALUES
    ('duplicate', '0 1 * * *', true, 'a'),
    ('duplicate', '0 2 * * *', true, 'b'),
    ('duplicate', '0 3 * * *', true, 'c');`); err != nil {
		t.Fatalf("seed duplicate names: %v", err)
	}
	router := schedulesRouter(scheduling.NewHandler(pool))

	first := readAdminSchedules(t, router)
	if len(first.Rows) != 3 {
		t.Fatalf("fixture produced %d rows, want 3", len(first.Rows))
	}

	// The discriminating step. Repeating an unchanged read proves nothing: on a
	// three-row table PostgreSQL happily returns the same sequential-scan order
	// every time, with or without the tiebreaker. UPDATING a tied row moves it
	// in the heap, so a query ordered by `name` ALONE returns it in a different
	// position — which is what a client paging through this listing would see as
	// a row repeating on one page and vanishing from another.
	middle := first.Rows[1].ID
	if _, err := pool.Exec(context.Background(),
		`UPDATE centry.schedule SET cron = '0 9 * * *' WHERE id = $1`, middle); err != nil {
		t.Fatalf("update the tied row: %v", err)
	}

	again := readAdminSchedules(t, router)
	for index := range first.Rows {
		if first.Rows[index].ID != again.Rows[index].ID {
			t.Fatalf("row %d changed identity after an unrelated update: %d then %d",
				index, first.Rows[index].ID, again.Rows[index].ID)
		}
	}
}

// TestListingSurfacesAFailureInsteadOfAnEmptyList is the guard for the swallowed
// error. With the table gone, "the schedule table is missing" and "no schedules
// are configured" must not render as the same screen.
func TestListingSurfacesAFailureInsteadOfAnEmptyList(t *testing.T) {
	pool := newSchedulesPool(t)
	if _, err := pool.Exec(context.Background(), `DROP TABLE centry.schedule`); err != nil {
		t.Fatalf("drop centry.schedule: %v", err)
	}
	router := schedulesRouter(scheduling.NewHandler(pool))

	recorder := scheduleDo(t, router, http.MethodGet, "/scheduling/schedules/administration/0", nil)
	if recorder.Code == http.StatusOK {
		t.Fatalf("a missing schedule table answered 200 (%s)", recorder.Body.String())
	}
}

/* ── the write ─────────────────────────────────────────────────────────── */

// TestDisablingAScheduleWritesAndReReads is the write this unit exists for: it
// is how the indexing transition disables the `index_scheduling` row
// (services/elitea-scheduler/RETIREMENT.md).
func TestDisablingAScheduleWritesAndReReads(t *testing.T) {
	pool := newSchedulesPool(t)
	prepareSchedulesFixture(t, pool)
	router := schedulesRouter(scheduling.NewHandler(pool))
	id := scheduleID(t, pool, "index_scheduling")

	if recorder := scheduleDo(t, router, http.MethodPut, "/scheduling/schedules/administration/0",
		map[string]any{"id": id, "active": false}); recorder.Code != http.StatusOK {
		t.Fatalf("PUT active=false status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	if row := scheduleByName(t, readAdminSchedules(t, router), "index_scheduling"); row.Active {
		t.Fatal("the re-read still reports index_scheduling active")
	}
	if _, active, _, _ := scheduleSQL(t, pool, id); active {
		t.Fatal("centry.schedule.active is still true in SQL")
	}

	// And back on again, so the write is not a one-way constant.
	if recorder := scheduleDo(t, router, http.MethodPut, "/scheduling/schedules/administration/0",
		map[string]any{"id": id, "active": true}); recorder.Code != http.StatusOK {
		t.Fatalf("PUT active=true status = %d, want 200", recorder.Code)
	}
	if row := scheduleByName(t, readAdminSchedules(t, router), "index_scheduling"); !row.Active {
		t.Fatal("the re-read still reports index_scheduling inactive")
	}
}

// TestUpdatingCronWritesAndReReads covers the inline cron editor.
func TestUpdatingCronWritesAndReReads(t *testing.T) {
	pool := newSchedulesPool(t)
	prepareSchedulesFixture(t, pool)
	router := schedulesRouter(scheduling.NewHandler(pool))
	id := scheduleID(t, pool, "usage_monitor")

	if recorder := scheduleDo(t, router, http.MethodPut, "/scheduling/schedules/administration/0",
		map[string]any{"id": id, "cron": "*/15 * * * *"}); recorder.Code != http.StatusOK {
		t.Fatalf("PUT cron status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	row := scheduleByName(t, readAdminSchedules(t, router), "usage_monitor")
	if row.Cron != "*/15 * * * *" {
		t.Fatalf("re-read cron = %q, want %q", row.Cron, "*/15 * * * *")
	}
	// The partial update must leave everything else alone: the page PUTs the
	// cron alone and expects the active flag to survive.
	if !row.Active {
		t.Error("updating cron cleared the active flag")
	}
	if row.RPCFunc != "usage_collect" {
		t.Errorf("updating cron changed rpc_func to %q", row.RPCFunc)
	}
	if window, ok := row.RPCKwargs["window"]; !ok || window != float64(60) {
		t.Errorf("updating cron changed rpc_kwargs to %v", row.RPCKwargs)
	}
}

// TestUpdateRejectsACronTheSchedulerCannotRun is the defect this validation
// exists to stop. `services/elitea-scheduler`'s `timeToRun` returns FALSE on a
// parse failure, so an unparseable cron does not error at run time — the job
// silently never fires again, which on a platform schedule is indistinguishable
// from a job with nothing to do.
func TestUpdateRejectsACronTheSchedulerCannotRun(t *testing.T) {
	pool := newSchedulesPool(t)
	prepareSchedulesFixture(t, pool)
	router := schedulesRouter(scheduling.NewHandler(pool))
	id := scheduleID(t, pool, "usage_monitor")

	// `@daily` is included deliberately: robfig accepts it WITH `cron.Descriptor`,
	// and the scheduler's parser is built without it. A validation that used a
	// more permissive parser than the runner would let this through.
	for _, expression := range []string{"not a cron", "* * * *", "0 0 0 0 0 0 0", "@daily", "99 * * * *", ""} {
		recorder := scheduleDo(t, router, http.MethodPut, "/scheduling/schedules/administration/0",
			map[string]any{"id": id, "cron": expression})
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("PUT cron=%q status = %d, want 400", expression, recorder.Code)
		}
	}

	if cron, _, _, _ := scheduleSQL(t, pool, id); cron != "0 * * * *" {
		t.Fatalf("a refused cron was written anyway: %q", cron)
	}
}

// TestUpdateRefusesToRetargetWhatAScheduleRuns is the run-as-identity guard.
//
// A scheduled run has NO interactive principal: the scheduler publishes
// `rpc_func` onto the Arbiter bus fire-and-forget, and the handler on the other
// end is an internal platform function with full privilege. A client able to
// set `rpc_func`/`rpc_kwargs` could name any internal RPC with any arguments
// and have the platform invoke it unattended a minute later.
//
// The refusal is EXPLICIT (400), not a silent drop: a caller that believes it
// retargeted a schedule and received 200 is the failure this unit exists to
// stop.
func TestUpdateRefusesToRetargetWhatAScheduleRuns(t *testing.T) {
	pool := newSchedulesPool(t)
	prepareSchedulesFixture(t, pool)
	router := schedulesRouter(scheduling.NewHandler(pool))
	id := scheduleID(t, pool, "usage_monitor")

	bodies := []map[string]any{
		{"id": id, "rpc_func": "delete_everything"},
		{"id": id, "rpc_kwargs": map[string]any{"force": true}},
		// Smuggled alongside a legitimate field: the field the caller is allowed
		// to set must not carry the one it is not.
		{"id": id, "active": false, "rpc_func": "delete_everything"},
	}
	for _, body := range bodies {
		recorder := scheduleDo(t, router, http.MethodPut, "/scheduling/schedules/administration/0", body)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("PUT %v status = %d, want 400", body, recorder.Code)
		}
	}

	cron, active, rpcFunc, kwargs := scheduleSQL(t, pool, id)
	if rpcFunc != "usage_collect" {
		t.Fatalf("rpc_func was changed to %q", rpcFunc)
	}
	if kwargs != `{"window": 60}` {
		t.Fatalf("rpc_kwargs was changed to %s", kwargs)
	}
	// The third body's legitimate half must not have applied either — a partial
	// application of a refused request is still a write nobody asked for.
	if !active {
		t.Fatal("a refused request disabled the schedule anyway")
	}
	if cron != "0 * * * *" {
		t.Fatalf("a refused request changed the cron to %q", cron)
	}
}

// TestUpdateRejectsAnUnknownID: an id matching no row is a 404, not a 200 that
// changed nothing.
func TestUpdateRejectsAnUnknownID(t *testing.T) {
	pool := newSchedulesPool(t)
	prepareSchedulesFixture(t, pool)
	router := schedulesRouter(scheduling.NewHandler(pool))

	recorder := scheduleDo(t, router, http.MethodPut, "/scheduling/schedules/administration/0",
		map[string]any{"id": 999999, "active": false})
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("PUT with an unknown id status = %d, want 404 (body %s)", recorder.Code, recorder.Body.String())
	}
}

// TestUpdateRejectsABodyWithNothingToApply: a PUT that changes nothing must not
// report a save. 200 with no write is precisely the shape #130 shipped.
func TestUpdateRejectsABodyWithNothingToApply(t *testing.T) {
	pool := newSchedulesPool(t)
	prepareSchedulesFixture(t, pool)
	router := schedulesRouter(scheduling.NewHandler(pool))
	id := scheduleID(t, pool, "usage_monitor")

	for _, body := range []map[string]any{{"id": id}, {"active": false}} {
		recorder := scheduleDo(t, router, http.MethodPut, "/scheduling/schedules/administration/0", body)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("PUT %v status = %d, want 400", body, recorder.Code)
		}
	}
}

// TestUpdateRejectsANameLongerThanTheColumn: VARCHAR(64) overflow is a 400 with
// a sentence, not a 500 that reads as "the save broke".
func TestUpdateRejectsANameLongerThanTheColumn(t *testing.T) {
	pool := newSchedulesPool(t)
	prepareSchedulesFixture(t, pool)
	router := schedulesRouter(scheduling.NewHandler(pool))
	id := scheduleID(t, pool, "usage_monitor")

	long := ""
	for len(long) <= 64 {
		long += "x"
	}
	for _, name := range []string{long, "", "   "} {
		recorder := scheduleDo(t, router, http.MethodPut, "/scheduling/schedules/administration/0",
			map[string]any{"id": id, "name": name})
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("PUT name=%q status = %d, want 400", name, recorder.Code)
		}
	}

	// A legitimate rename still works, so the guard above is not refusing
	// everything.
	if recorder := scheduleDo(t, router, http.MethodPut, "/scheduling/schedules/administration/0",
		map[string]any{"id": id, "name": "usage_monitor_v2"}); recorder.Code != http.StatusOK {
		t.Fatalf("PUT a valid name status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	scheduleByName(t, readAdminSchedules(t, router), "usage_monitor_v2")
}

// TestProjectUpdateCannotReachAnotherProjectsSchedule: in a project mode the id
// travels in the body while the project travels in the path, so without the
// `AND project_id = $n` constraint a project member could retime any schedule
// on the platform by guessing an id.
func TestProjectUpdateCannotReachAnotherProjectsSchedule(t *testing.T) {
	pool := newSchedulesPool(t)
	prepareSchedulesFixture(t, pool)
	router := schedulesRouter(scheduling.NewHandler(pool))
	foreign := scheduleID(t, pool, "project_beta_report")
	platform := scheduleID(t, pool, "index_scheduling")

	for _, target := range []int64{foreign, platform} {
		recorder := scheduleDo(t, router, http.MethodPut, "/scheduling/schedules/default/41",
			map[string]any{"id": target, "active": false})
		if recorder.Code != http.StatusNotFound {
			t.Errorf("project 41 PUT against schedule %d status = %d, want 404", target, recorder.Code)
		}
		if _, active, _, _ := scheduleSQL(t, pool, target); !active {
			t.Errorf("project 41 disabled schedule %d anyway", target)
		}
	}

	// Its OWN schedule still updates, so the constraint is scoping rather than
	// blocking.
	own := scheduleID(t, pool, "project_alpha_report")
	if recorder := scheduleDo(t, router, http.MethodPut, "/scheduling/schedules/default/41",
		map[string]any{"id": own, "active": false}); recorder.Code != http.StatusOK {
		t.Fatalf("project 41 PUT against its own schedule status = %d, want 200", recorder.Code)
	}
	if _, active, _, _ := scheduleSQL(t, pool, own); active {
		t.Fatal("the permitted project write did not apply")
	}
}

/* ── the gate ──────────────────────────────────────────────────────────── */

// gatedSchedulesRouter mounts the administration pair WITH the route-level
// middleware, exactly as internal/api/router.go composes them.
func gatedSchedulesRouter(
	handler *scheduling.Handler, resolver auth.PermissionResolver, principal auth.User,
) chi.Router {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), principal)))
		})
	})
	router.With(apimw.RequireCentralPermissions(
		resolver, auth.PermissionModeAdministration, "configuration.scheduling.schedules.view",
	)).Get("/scheduling/schedules/administration/{projectID}", handler.AdministrationSchedules)
	router.With(apimw.RequireCentralPermissions(
		resolver, auth.PermissionModeAdministration, "configuration.scheduling.schedules.edit",
	)).Put("/scheduling/schedules/administration/{projectID}", handler.AdministrationSchedulesUpdate)
	return router
}

type permissionResolverFunc func(context.Context, auth.User, string, string) (auth.PermissionResolution, error)

func (f permissionResolverFunc) ResolvePermissions(
	ctx context.Context, principal auth.User, mode, projectID string,
) (auth.PermissionResolution, error) {
	return f(ctx, principal, mode, projectID)
}

func grantingResolver(permissions ...string) permissionResolverFunc {
	return func(_ context.Context, _ auth.User, _, _ string) (auth.PermissionResolution, error) {
		return auth.PermissionResolution{UserID: 1, Permissions: permissions}, nil
	}
}

// TestScheduleUpdateIsRefusedWithoutTheEditPermission is the tenancy bar from
// the #200 brief: a caller lacking the permission is REFUSED by the server, not
// merely shown no switch. `window.admin_ui_config.permissions` hands every
// session the same hardcoded array, so hiding the control changes nothing about
// what a crafted request can do — and this write disables platform jobs.
func TestScheduleUpdateIsRefusedWithoutTheEditPermission(t *testing.T) {
	pool := newSchedulesPool(t)
	prepareSchedulesFixture(t, pool)
	handler := scheduling.NewHandler(pool)
	principal := auth.User{ID: "1", UserID: "1"}
	id := scheduleID(t, pool, "index_scheduling")

	// The caller holds the READ permission and nothing else — the shape a
	// viewer-shaped administrator has.
	gated := gatedSchedulesRouter(handler,
		grantingResolver("configuration.scheduling.schedules.view"), principal)
	recorder := scheduleDo(t, gated, http.MethodPut, "/scheduling/schedules/administration/0",
		map[string]any{"id": id, "active": false})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("PUT without the edit permission status = %d, want 403", recorder.Code)
	}
	if _, active, _, _ := scheduleSQL(t, pool, id); !active {
		t.Fatal("the refused request disabled the schedule anyway")
	}

	// And the same request DOES go through once the permission is held, so the
	// refusal cannot be passing for an unrelated reason.
	allowed := gatedSchedulesRouter(handler, grantingResolver(
		"configuration.scheduling.schedules.view", "configuration.scheduling.schedules.edit"), principal)
	if recorder := scheduleDo(t, allowed, http.MethodPut, "/scheduling/schedules/administration/0",
		map[string]any{"id": id, "active": false}); recorder.Code != http.StatusOK {
		t.Fatalf("PUT WITH the edit permission status = %d, want 200 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	if _, active, _, _ := scheduleSQL(t, pool, id); active {
		t.Fatal("the permitted request did not disable the schedule")
	}
}

// TestScheduleListingIsRefusedWithoutTheViewPermission: the listing names every
// internal RPC the platform invokes on a timer, which is reconnaissance on its
// own.
func TestScheduleListingIsRefusedWithoutTheViewPermission(t *testing.T) {
	pool := newSchedulesPool(t)
	prepareSchedulesFixture(t, pool)
	handler := scheduling.NewHandler(pool)
	principal := auth.User{ID: "1", UserID: "1"}

	gated := gatedSchedulesRouter(handler,
		grantingResolver("configuration.scheduling.schedules.edit"), principal)
	if recorder := scheduleDo(t, gated, http.MethodGet,
		"/scheduling/schedules/administration/0", nil); recorder.Code != http.StatusForbidden {
		t.Fatalf("GET without the view permission status = %d, want 403", recorder.Code)
	}

	allowed := gatedSchedulesRouter(handler,
		grantingResolver("configuration.scheduling.schedules.view"), principal)
	if recorder := scheduleDo(t, allowed, http.MethodGet,
		"/scheduling/schedules/administration/0", nil); recorder.Code != http.StatusOK {
		t.Fatalf("GET WITH the view permission status = %d, want 200", recorder.Code)
	}
}

/* ── harness bootstrap ─────────────────────────────────────────────────── */

// newSchedulesPool creates an isolated database and applies the REAL bootstrap
// migration — the same 001_initial.sql a fresh deployment gets — so the
// centry.schedule DDL these tests read through is the shipped one rather than a
// second copy that could drift from it. That matters more here than elsewhere:
// this unit ADDED that table to the migration, and a test that created its own
// copy would pass whether or not deployments get one.
func newSchedulesPool(t *testing.T) *pgxpool.Pool {
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

	databaseName := fmt.Sprintf("elitea_schedules_it_%d_%d", os.Getpid(), time.Now().UnixNano())
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
	// The naive-clock probe the dbtest template installs, after the last
	// schema statement: a writer that stores the session-local clock in a
	// `timestamp` WITHOUT time zone column fails instead of passing unread.
	if err := dbtest.InstallNaiveClockProbe(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}
