package eliteacore_test

// Issue #888 — Settings › Project Context answered 200 on every PUT and never
// persisted a byte. Both halves of the write were wrong and both errors were
// discarded:
//
//   - the INSERT omitted `project_id`, which the tenant `configuration` table
//     declares NOT NULL, so every project with no prior row (every project on
//     a fresh install) failed with SQLSTATE 23502;
//   - the conflict target `ON CONFLICT (elitea_title) WHERE type =
//     'project_context'` names a PARTIAL unique index that 001_initial.sql
//     never creates — elitea_title carries a plain UNIQUE constraint — so the
//     statement was refused at plan time whether or not a row existed.
//
// A status assertion cannot see any of that: the handler echoed the submitted
// body back with 200 in both cases. So every test here writes through the
// handler and READS BACK through the handler (and, once, straight out of the
// table), on the real migration corpus rather than a hand-made stub table. A
// hand-written `configuration` would not carry the NOT NULL that produced the
// fault.
//
// Two states are covered because they take different branches: a project with
// NO prior row (the INSERT) and a project that already has one (the UPDATE).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
)

const (
	// Two DIFFERENT projects, so a write that ignored the project id and
	// updated "the project_context row" globally is visible as a cross-write.
	projectContextProjectA = 6101
	projectContextProjectB = 6102
)

type projectContextBody struct {
	Content string `json:"content"`
	Enabled bool   `json:"enabled"`
}

func projectContextRouter(handler *eliteacore.Handler) chi.Router {
	router := chi.NewRouter()
	router.Get("/elitea_core/project_context/prompt_lib/{projectID}/project-context",
		handler.ProjectContext)
	router.Put("/elitea_core/project_context/prompt_lib/{projectID}/project-context",
		handler.UpdateProjectContext)
	return router
}

func projectContextPath(projectID int) string {
	return fmt.Sprintf("/elitea_core/project_context/prompt_lib/%d/project-context", projectID)
}

// putProjectContext writes through the handler and fails on any non-200, so a
// typed 500 from a refused statement stops the test here instead of surfacing
// later as an empty read.
func putProjectContext(
	t *testing.T, router chi.Router, projectID int, body projectContextBody,
) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal project context body: %v", err)
	}
	request := httptest.NewRequest(http.MethodPut,
		projectContextPath(projectID), strings.NewReader(string(encoded)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT project context status = %d, want 200 (body %s)",
			recorder.Code, recorder.Body.String())
	}
}

func getProjectContext(t *testing.T, router chi.Router, projectID int) projectContextBody {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, projectContextPath(projectID), nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET project context status = %d, want 200 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	var body projectContextBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode project context body %q: %v", recorder.Body.String(), err)
	}
	return body
}

// countProjectContextRows reads the tenant table directly. The round-trip
// assertions alone would pass for a handler that wrote two rows per save and
// read the first one back.
func countProjectContextRows(t *testing.T, pool *pgxpool.Pool, projectID int) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var count int
	query := fmt.Sprintf(
		`SELECT COUNT(*) FROM %s.configuration WHERE type = 'project_context'`,
		tenantSchema(projectID))
	if err := pool.QueryRow(ctx, query).Scan(&count); err != nil {
		t.Fatalf("count project_context rows for project %d: %v", projectID, err)
	}
	return count
}

// TestProjectContextPersistsOnAProjectWithNoPriorRow is the #888 acceptance
// proper: the very first save on a project, the state every project on a
// seeded stack is in, and the one the missing project_id column killed.
func TestProjectContextPersistsOnAProjectWithNoPriorRow(t *testing.T) {
	pool := newImportCorpusPool(t)
	provisionTenantProject(t, pool, projectContextProjectA)
	router := projectContextRouter(eliteacore.NewHandler(pool))

	// The precondition is measured, not assumed: if a row already existed the
	// INSERT branch would never run and this test would pass vacuously.
	if rows := countProjectContextRows(t, pool, projectContextProjectA); rows != 0 {
		t.Fatalf("project %d already has %d project_context rows before the first save",
			projectContextProjectA, rows)
	}

	putProjectContext(t, router, projectContextProjectA,
		projectContextBody{Content: "corpus probe one", Enabled: true})

	read := getProjectContext(t, router, projectContextProjectA)
	if read.Content != "corpus probe one" {
		t.Errorf("content read back = %q, want %q", read.Content, "corpus probe one")
	}
	if !read.Enabled {
		t.Error("enabled read back = false, want true")
	}
	if rows := countProjectContextRows(t, pool, projectContextProjectA); rows != 1 {
		t.Errorf("project_context rows after one save = %d, want 1", rows)
	}
}

// TestProjectContextOverwritesAnExistingRow takes the UPDATE branch, and
// checks that the second save REPLACES the first rather than adding a row the
// GET's `LIMIT 1` would then pick between.
func TestProjectContextOverwritesAnExistingRow(t *testing.T) {
	pool := newImportCorpusPool(t)
	provisionTenantProject(t, pool, projectContextProjectA)
	router := projectContextRouter(eliteacore.NewHandler(pool))

	putProjectContext(t, router, projectContextProjectA,
		projectContextBody{Content: "first", Enabled: true})
	putProjectContext(t, router, projectContextProjectA,
		projectContextBody{Content: "second", Enabled: false})

	read := getProjectContext(t, router, projectContextProjectA)
	if read.Content != "second" {
		t.Errorf("content after the second save = %q, want %q", read.Content, "second")
	}
	// enabled=false is the value that a "write only non-empty fields" handler
	// would drop, so it is asserted rather than the easier true.
	if read.Enabled {
		t.Error("enabled after the second save = true, want false")
	}
	if rows := countProjectContextRows(t, pool, projectContextProjectA); rows != 1 {
		t.Errorf("project_context rows after two saves = %d, want 1", rows)
	}
}

// TestProjectContextIsScopedToItsOwnProject guards the schema predicate: the
// row lives in the project's own tenant schema and a save in one project
// leaves the other's untouched.
func TestProjectContextIsScopedToItsOwnProject(t *testing.T) {
	pool := newImportCorpusPool(t)
	provisionTenantProject(t, pool, projectContextProjectA)
	provisionTenantProject(t, pool, projectContextProjectB)
	router := projectContextRouter(eliteacore.NewHandler(pool))

	putProjectContext(t, router, projectContextProjectA,
		projectContextBody{Content: "only project A", Enabled: true})

	other := getProjectContext(t, router, projectContextProjectB)
	if other.Content != "" || other.Enabled {
		t.Errorf("project %d read back %+v, want the empty default",
			projectContextProjectB, other)
	}

	putProjectContext(t, router, projectContextProjectB,
		projectContextBody{Content: "only project B", Enabled: true})
	if mine := getProjectContext(t, router, projectContextProjectA); mine.Content != "only project A" {
		t.Errorf("project %d content = %q after project %d saved, want it unchanged",
			projectContextProjectA, mine.Content, projectContextProjectB)
	}
}

// TestProjectContextRefusesAWriteItCannotPersist is the anti-swallow guard:
// against a project whose tenant schema does not exist the handler must answer
// a typed 500, not the 200-with-an-echo that hid #888 for the whole of its
// life.
func TestProjectContextRefusesAWriteItCannotPersist(t *testing.T) {
	pool := newImportCorpusPool(t)
	router := projectContextRouter(eliteacore.NewHandler(pool))

	const unprovisioned = 6199
	request := httptest.NewRequest(http.MethodPut, projectContextPath(unprovisioned),
		strings.NewReader(`{"content":"nowhere to go","enabled":true}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("PUT into an unprovisioned project status = %d, want 500 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	var failure map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil {
		t.Fatalf("decode refusal body %q: %v", recorder.Body.String(), err)
	}
	if failure["code"] != "project_context_write_failed" {
		t.Errorf("refusal code = %v, want project_context_write_failed", failure["code"])
	}
	// The refusal names no table and carries no SQLSTATE.
	if text := recorder.Body.String(); strings.Contains(text, "configuration") ||
		strings.Contains(text, "SQLSTATE") {
		t.Errorf("refusal body leaks database detail: %s", text)
	}
}
