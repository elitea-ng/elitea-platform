package projects_test

// #255 acceptance for the project GROUP writes.
//
// The defect these tests exist for is not a wrong status code. Before this
// change `PUT /projects/groups/prompt_lib/{projectID}` answered 200 with the
// request body echoed back — the client re-rendered the groups it had just
// sent, from a handler that touched no table — and create and delete had no
// route at all. A test that asserted "200" would have passed against the echo.
//
// So every case below writes and then RE-READS: through the product's own
// `GET /projects/groups/prompt_lib` listing, through the project body the write
// itself returns, and through SQL for the association rows neither of those can
// show.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	handler "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/projects"
	dbschema "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/schema"
)

const (
	groupProjectID      = 11
	groupOtherProjectID = 12
)

type groupBody struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type projectBody struct {
	ID     int         `json:"id"`
	Name   string      `json:"name"`
	Groups []groupBody `json:"groups"`
}

type groupListing struct {
	Items []groupBody `json:"items"`
	Total int         `json:"total"`
}

// groupsRouter mounts the four group routes exactly as
// internal/api/v2/projects/handler.go does, minus the permission middleware —
// TestGroupWritesAreGated in handler_test.go covers that layer, and it needs
// the auth stack this test does not build.
func groupsRouter(h *handler.Handler) chi.Router {
	router := chi.NewRouter()
	router.Get("/projects/groups/prompt_lib", h.GroupList)
	router.Put("/projects/groups/prompt_lib/{projectID}", h.PutProjectGroups)
	router.Post("/projects/group/prompt_lib/{projectID}", h.GroupCreate)
	router.Delete("/projects/group/prompt_lib/{projectID}/{groupID}", h.GroupDelete)
	return router
}

func groupsDo(t *testing.T, router chi.Router, method, target string, body any) *httptest.ResponseRecorder {
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

// readGroupCatalogue re-reads through the SAME listing the product calls.
func readGroupCatalogue(t *testing.T, router chi.Router) groupListing {
	t.Helper()
	recorder := groupsDo(t, router, http.MethodGet, "/projects/groups/prompt_lib", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET groups status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	var listing groupListing
	if err := json.Unmarshal(recorder.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode listing %q: %v", recorder.Body.String(), err)
	}
	return listing
}

// attachedGroups reads the association table — the state neither the response
// body nor the group catalogue can prove on its own.
func attachedGroups(t *testing.T, pool *pgxpool.Pool, projectID int) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT grp.name
FROM centry.project_group_association association
JOIN centry.project_group grp ON grp.id = association.group_id
WHERE association.project_id = $1
ORDER BY grp.name`, projectID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func decodeProject(t *testing.T, recorder *httptest.ResponseRecorder) projectBody {
	t.Helper()
	var project projectBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &project); err != nil {
		t.Fatalf("decode project %q: %v", recorder.Body.String(), err)
	}
	return project
}

func newGroupsEnvironment(t *testing.T) (*pgxpool.Pool, chi.Router) {
	t.Helper()
	pool := newGroupsPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, statement := range []string{
		fmt.Sprintf(`INSERT INTO centry.project (id, name, owner_id, keycloak_groups, create_success)
			VALUES (%d, 'alpha-team', 1, '{}', true), (%d, 'beta-team', 1, '{}', true)`,
			groupProjectID, groupOtherProjectID),
		`INSERT INTO centry.project_group (id, name) VALUES (1, 'existing')`,
		// The seed sets ids explicitly, so the sequence has to be moved past
		// them or the first generated id collides with 'existing'.
		`SELECT setval('centry.project_group_id_seq', 100, false)`,
		fmt.Sprintf(`INSERT INTO centry.project_group_association (project_id, group_id)
			VALUES (%d, 1)`, groupProjectID),
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	return pool, groupsRouter(handler.NewHandler(pool))
}

/* ── create ────────────────────────────────────────────────────────────── */

func TestGroupCreateAttachesAndIsVisibleInTheListing(t *testing.T) {
	pool, router := newGroupsEnvironment(t)

	recorder := groupsDo(t, router, http.MethodPost,
		fmt.Sprintf("/projects/group/prompt_lib/%d", groupProjectID),
		map[string]any{"name": "platform"})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("POST status = %d, want 201 (body %s)", recorder.Code, recorder.Body.String())
	}

	// The response is the project, as the legacy runtime serializes it — so the
	// client does not have to re-fetch to render the new chip.
	project := decodeProject(t, recorder)
	if project.ID != groupProjectID {
		t.Fatalf("response project id = %d, want %d", project.ID, groupProjectID)
	}
	names := []string{}
	for _, group := range project.Groups {
		names = append(names, group.Name)
	}
	if len(names) != 2 {
		t.Fatalf("response project groups = %v, want the existing one plus platform", names)
	}

	// THE assertions the echo could not pass.
	if got := attachedGroups(t, pool, groupProjectID); !sameStrings(got, []string{"existing", "platform"}) {
		t.Fatalf("attached groups = %v, want [existing platform]", got)
	}
	found := false
	for _, group := range readGroupCatalogue(t, router).Items {
		if group.Name == "platform" {
			found = true
		}
	}
	if !found {
		t.Fatal("the created group is absent from the group listing")
	}
}

func TestGroupCreateReusesAnExistingGroupRow(t *testing.T) {
	pool, router := newGroupsEnvironment(t)

	// `centry.project_group.name` is UNIQUE, so a second project asking for the
	// same group must ATTACH the existing row rather than fail or duplicate.
	for _, projectID := range []int{groupProjectID, groupOtherProjectID} {
		recorder := groupsDo(t, router, http.MethodPost,
			fmt.Sprintf("/projects/group/prompt_lib/%d", projectID),
			map[string]any{"name": "shared-group"})
		if recorder.Code != http.StatusCreated {
			t.Fatalf("POST for project %d status = %d, want 201 (body %s)",
				projectID, recorder.Code, recorder.Body.String())
		}
	}

	var rows int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM centry.project_group WHERE name = 'shared-group'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("project_group rows named shared-group = %d, want 1", rows)
	}
	for _, projectID := range []int{groupProjectID, groupOtherProjectID} {
		attached := attachedGroups(t, pool, projectID)
		if len(attached) == 0 {
			t.Fatalf("project %d has no groups", projectID)
		}
	}

	// Repeating the same attach does not add a second association row.
	if recorder := groupsDo(t, router, http.MethodPost,
		fmt.Sprintf("/projects/group/prompt_lib/%d", groupProjectID),
		map[string]any{"name": "shared-group"}); recorder.Code != http.StatusCreated {
		t.Fatalf("repeat POST status = %d, want 201", recorder.Code)
	}
	if got := attachedGroups(t, pool, groupProjectID); !sameStrings(got, []string{"existing", "shared-group"}) {
		t.Fatalf("attached groups = %v after a repeated attach, want [existing shared-group]", got)
	}
}

func TestGroupCreateRejectsBadInputAndWritesNothing(t *testing.T) {
	pool, router := newGroupsEnvironment(t)

	cases := map[string]struct {
		target string
		body   map[string]any
		want   int
	}{
		"reserved name": {
			fmt.Sprintf("/projects/group/prompt_lib/%d", groupProjectID),
			map[string]any{"name": "no_group"}, http.StatusBadRequest,
		},
		"empty name": {
			fmt.Sprintf("/projects/group/prompt_lib/%d", groupProjectID),
			map[string]any{"name": "   "}, http.StatusBadRequest,
		},
		"unknown project": {
			"/projects/group/prompt_lib/9999",
			map[string]any{"name": "orphan"}, http.StatusBadRequest,
		},
		"invalid project id": {
			"/projects/group/prompt_lib/0",
			map[string]any{"name": "orphan"}, http.StatusBadRequest,
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			recorder := groupsDo(t, router, http.MethodPost, testCase.target, testCase.body)
			if recorder.Code != testCase.want {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, testCase.want, recorder.Body.String())
			}
		})
	}

	// A rejected create must not leave the group row behind — the transaction
	// is what makes "project not found" mean "nothing happened".
	var groups int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM centry.project_group`).Scan(&groups); err != nil {
		t.Fatal(err)
	}
	if groups != 1 {
		t.Fatalf("project_group rows = %d after four rejected creates, want 1", groups)
	}
	if got := attachedGroups(t, pool, groupProjectID); !sameStrings(got, []string{"existing"}) {
		t.Fatalf("attached groups = %v after four rejected creates, want [existing]", got)
	}
}

/* ── delete ────────────────────────────────────────────────────────────── */

func TestGroupDeleteDetachesWithoutDestroyingTheGroup(t *testing.T) {
	pool, router := newGroupsEnvironment(t)

	// Attach group 1 to the second project too: the delete must detach ONE
	// project, not the group everywhere.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO centry.project_group_association (project_id, group_id) VALUES ($1, 1)`,
		groupOtherProjectID); err != nil {
		t.Fatal(err)
	}

	recorder := groupsDo(t, router, http.MethodDelete,
		fmt.Sprintf("/projects/group/prompt_lib/%d/1", groupProjectID), nil)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204 (body %s)", recorder.Code, recorder.Body.String())
	}

	if got := attachedGroups(t, pool, groupProjectID); len(got) != 0 {
		t.Fatalf("attached groups = %v after the detach, want none", got)
	}
	if got := attachedGroups(t, pool, groupOtherProjectID); !sameStrings(got, []string{"existing"}) {
		t.Fatalf("the other project's groups = %v, want [existing]", got)
	}
	// The group row itself survives: it belongs to the deployment, not to the
	// project that just detached from it.
	found := false
	for _, group := range readGroupCatalogue(t, router).Items {
		if group.Name == "existing" {
			found = true
		}
	}
	if !found {
		t.Fatal("detaching a group deleted the group row")
	}

	// A detach that detaches nothing says so. The legacy runtime answers 204
	// here, having swallowed the ValueError its list.remove raises.
	if again := groupsDo(t, router, http.MethodDelete,
		fmt.Sprintf("/projects/group/prompt_lib/%d/1", groupProjectID), nil,
	); again.Code != http.StatusNotFound {
		t.Fatalf("repeat DELETE status = %d, want 404 (body %s)", again.Code, again.Body.String())
	}
	// An unknown group is a 400, as it is in the reference.
	if unknown := groupsDo(t, router, http.MethodDelete,
		fmt.Sprintf("/projects/group/prompt_lib/%d/9999", groupProjectID), nil,
	); unknown.Code != http.StatusBadRequest {
		t.Fatalf("DELETE of an unknown group status = %d, want 400", unknown.Code)
	}
}

/* ── replace ───────────────────────────────────────────────────────────── */

// TestPutProjectGroupsReplacesTheSet is the case the pre-#255 handler cannot
// pass: it echoed the submitted body back and wrote nothing at all.
func TestPutProjectGroupsReplacesTheSet(t *testing.T) {
	pool, router := newGroupsEnvironment(t)

	recorder := groupsDo(t, router, http.MethodPut,
		fmt.Sprintf("/projects/groups/prompt_lib/%d", groupProjectID),
		map[string]any{"groups": []string{"platform", "research"}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	project := decodeProject(t, recorder)
	if len(project.Groups) != 2 {
		t.Fatalf("response project groups = %+v, want two", project.Groups)
	}
	// Replace, not merge: `existing` was attached and is not in the submission.
	if got := attachedGroups(t, pool, groupProjectID); !sameStrings(got, []string{"platform", "research"}) {
		t.Fatalf("attached groups = %v, want [platform research]", got)
	}
	// The groups that did not exist were created, and the detached one survives
	// as a row.
	catalogue := readGroupCatalogue(t, router)
	if catalogue.Total != 3 {
		t.Fatalf("group catalogue total = %d, want 3 (existing, platform, research)", catalogue.Total)
	}

	// An empty submission detaches everything.
	if empty := groupsDo(t, router, http.MethodPut,
		fmt.Sprintf("/projects/groups/prompt_lib/%d", groupProjectID),
		map[string]any{"groups": []string{}}); empty.Code != http.StatusOK {
		t.Fatalf("PUT with an empty set status = %d, want 200 (body %s)", empty.Code, empty.Body.String())
	}
	if got := attachedGroups(t, pool, groupProjectID); len(got) != 0 {
		t.Fatalf("attached groups = %v after an empty submission, want none", got)
	}
	// …and leaves the group rows themselves alone.
	if readGroupCatalogue(t, router).Total != 3 {
		t.Fatal("an empty submission deleted group rows")
	}
}

func TestPutProjectGroupsRejectsBadInput(t *testing.T) {
	pool, router := newGroupsEnvironment(t)

	for name, testCase := range map[string]struct {
		target string
		body   any
	}{
		"no groups key":   {fmt.Sprintf("/projects/groups/prompt_lib/%d", groupProjectID), map[string]any{}},
		"reserved name":   {fmt.Sprintf("/projects/groups/prompt_lib/%d", groupProjectID), map[string]any{"groups": []string{"no_group"}}},
		"unknown project": {"/projects/groups/prompt_lib/9999", map[string]any{"groups": []string{"platform"}}},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := groupsDo(t, router, http.MethodPut, testCase.target, testCase.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
			}
		})
	}
	if got := attachedGroups(t, pool, groupProjectID); !sameStrings(got, []string{"existing"}) {
		t.Fatalf("attached groups = %v after three rejected saves, want [existing]", got)
	}
}

/* ── database ──────────────────────────────────────────────────────────── */

func newGroupsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := testpg.DSN(os.Getenv(environment))
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL service-integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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

	databaseName := fmt.Sprintf("elitea_project_groups_it_%d_%d", os.Getpid(), time.Now().UnixNano())
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

	// The CURRENT legacy baseline, deliberately: its
	// centry.project_group_association carries no primary key, so an
	// `ON CONFLICT` in the writes would fail here and pass against the
	// bootstrap schema.
	if _, err := pool.Exec(ctx, dbschema.CentryProjectsBaselineSQLCProjection); err != nil {
		t.Fatalf("apply centry baseline projection: %v", err)
	}
	return pool
}
