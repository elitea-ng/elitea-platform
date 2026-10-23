package toolkits

// Write-then-RE-READ tests for the three index write paths (#180).
//
// The point of this file is the re-read. Before the fix all three handlers
// answered success — `{"ok":true}` for PATCH/cancel, 204 for DELETE — without
// opening a connection, so ANY assertion that stops at the status code passes
// against the stub and proves nothing. Every test below performs the write
// through the real chi route and then queries the database directly for the
// state the write claims to have produced. Each was run against the stubs and
// fails there for the right reason (the row / the schedule / the status is
// unchanged), not because of a status-code mismatch.
//
// This is the same trap #130 documents for the users endpoints and the same
// shape as #147's `replacement_version_id` — accepted by a spec, never read
// by the handler, and invisible to a status-code test.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL), which ci-go.yml
// provides; the shared helper in create_toolkit_owner_id_test.go creates a
// throwaway database per test.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db"
)

// itoa keeps the URL building below readable.
func itoa(value int64) string { return strconv.FormatInt(value, 10) }

// indexWriteFixture is one project schema holding one toolkit and one index,
// reachable through the same routes the router mounts.
type indexWriteFixture struct {
	pool      *pgxpool.Pool
	router    chi.Router
	toolkitID int64
	indexID   int64
	indexName string
}

func newIndexWriteFixture(t *testing.T) *indexWriteFixture {
	t.Helper()
	pool := newToolkitsIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run baseline migrations: %v", err)
	}

	var toolkitID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.elitea_tools (name, type, description, settings, meta, owner_id, author_id, created_at)
		VALUES ('index-write-fixture', 'github', '', '{}'::jsonb, '{}'::jsonb, 1, 7, now() AT TIME ZONE 'UTC')
		RETURNING id`).Scan(&toolkitID); err != nil {
		t.Fatalf("insert fixture toolkit: %v", err)
	}

	const indexName = "fixture-collection"
	var indexID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.index_meta (toolkit_id, name, status, progress, meta, created_at)
		VALUES ($1, $2, 'in_progress', 42, '{"task_id":"task-abc"}'::jsonb, now() AT TIME ZONE 'UTC')
		RETURNING id`, toolkitID, indexName).Scan(&indexID); err != nil {
		t.Fatalf("insert fixture index_meta: %v", err)
	}

	handler := NewHandler(pool)
	router := chi.NewRouter()
	router.Patch("/index_meta/prompt_lib/{projectID}/{toolkitID}/{indexMetaID}", handler.IndexMetaUpdate)
	router.Delete("/index_meta/prompt_lib/{projectID}/{toolkitID}/{indexMetaID}", handler.IndexMetaDelete)
	router.Delete("/index_cancel/prompt_lib/{projectID}/{toolkitID}/{indexName}/{taskID}", handler.IndexCancel)

	return &indexWriteFixture{pool: pool, router: router, toolkitID: toolkitID, indexID: indexID, indexName: indexName}
}

func (f *indexWriteFixture) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, request)
	return recorder
}

// countIndexRows re-reads the table the delete claims to have emptied.
func (f *indexWriteFixture) countIndexRows(t *testing.T) int {
	t.Helper()
	var count int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM p_1.index_meta WHERE toolkit_id = $1 AND name = $2`,
		f.toolkitID, f.indexName,
	).Scan(&count); err != nil {
		t.Fatalf("re-read index_meta rows: %v", err)
	}
	return count
}

// readToolkitMeta re-reads the document the schedule write claims to have
// changed.
func (f *indexWriteFixture) readToolkitMeta(t *testing.T) map[string]any {
	t.Helper()
	var raw []byte
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COALESCE(meta, '{}'::jsonb) FROM p_1.elitea_tools WHERE id = $1`, f.toolkitID,
	).Scan(&raw); err != nil {
		t.Fatalf("re-read toolkit meta: %v", err)
	}
	meta := map[string]any{}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("decode toolkit meta %s: %v", raw, err)
	}
	return meta
}

func (f *indexWriteFixture) readIndexStatus(t *testing.T) string {
	t.Helper()
	var status string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status FROM p_1.index_meta WHERE id = $1`, f.indexID,
	).Scan(&status); err != nil {
		t.Fatalf("re-read index status: %v", err)
	}
	return status
}

// scheduleFor digs out meta.indexes_meta[<name>].schedules[<user>] — the exact
// path `resolveScheduleData` (IndexActions.tsx:97) reads. Returns nil when any
// level is missing, which is what a handler that writes nothing produces.
func scheduleFor(meta map[string]any, indexName, userID string) map[string]any {
	indexesMeta, _ := meta["indexes_meta"].(map[string]any)
	entry, _ := indexesMeta[indexName].(map[string]any)
	schedules, _ := entry["schedules"].(map[string]any)
	schedule, _ := schedules[userID].(map[string]any)
	return schedule
}

// ── DELETE ───────────────────────────────────────────────────────────────

// The headline defect: "deleting an index from the Indexes tab reports success
// and deletes nothing. The row is still there on reload."
//
// The status assertion is kept only to catch an outright error; the row count
// is what discriminates. Against the stub (204, no query) the count stays 1.
func TestIndexMetaDeleteRemovesTheRowAndItsSchedule(t *testing.T) {
	fixture := newIndexWriteFixture(t)

	// A schedule exists for this index before the delete, so the cleanup half
	// has something to remove. Written through the PATCH route rather than by
	// hand so the two handlers are forced to agree on the storage shape.
	if code := fixture.do(t, http.MethodPatch,
		"/index_meta/prompt_lib/1/"+itoa(fixture.toolkitID)+"/"+fixture.indexName,
		`{"timezone":"UTC","cron":"0 3 * * *","enabled":true}`).Code; code != http.StatusOK {
		t.Fatalf("seed schedule: status %d", code)
	}
	if scheduleFor(fixture.readToolkitMeta(t), fixture.indexName, "-1") == nil {
		t.Fatalf("precondition failed: no schedule to clean up after seeding one")
	}
	if got := fixture.countIndexRows(t); got != 1 {
		t.Fatalf("precondition failed: %d index rows before the delete, want 1", got)
	}

	response := fixture.do(t, http.MethodDelete,
		"/index_meta/prompt_lib/1/"+itoa(fixture.toolkitID)+"/"+itoa(fixture.indexID), `{"is_hidden":true}`)
	if response.Code != http.StatusOK {
		t.Fatalf("DELETE status %d, body %s", response.Code, response.Body.String())
	}

	// THE ASSERTION. A success status proves nothing here — it is exactly what
	// the stub returned.
	if got := fixture.countIndexRows(t); got != 0 {
		t.Errorf("%d index_meta rows survive the delete, want 0 — the row is still there on reload", got)
	}
	if schedule := scheduleFor(fixture.readToolkitMeta(t), fixture.indexName, "-1"); schedule != nil {
		t.Errorf("the deleted index's schedule survives in elitea_tools.meta: %#v", schedule)
	}
}

// A delete that addressed nothing must not report success — otherwise the
// client removes a row from its view that the server still has.
func TestIndexMetaDeleteReportsNotFoundForAnUnknownIndex(t *testing.T) {
	fixture := newIndexWriteFixture(t)

	response := fixture.do(t, http.MethodDelete,
		"/index_meta/prompt_lib/1/"+itoa(fixture.toolkitID)+"/999999", "")
	if response.Code != http.StatusNotFound {
		t.Errorf("DELETE of a nonexistent index answered %d, want 404 (body %s)", response.Code, response.Body.String())
	}
	if got := fixture.countIndexRows(t); got != 1 {
		t.Errorf("the real index was deleted by a request that addressed a different id: %d rows left", got)
	}
}

// An index belonging to another toolkit must not be deletable by naming the
// wrong toolkit in the path.
func TestIndexMetaDeleteIsScopedToTheNamedToolkit(t *testing.T) {
	fixture := newIndexWriteFixture(t)
	ctx := context.Background()

	var otherToolkitID int64
	if err := fixture.pool.QueryRow(ctx, `
		INSERT INTO p_1.elitea_tools (name, type, settings, meta, owner_id, author_id, created_at)
		VALUES ('other-toolkit', 'github', '{}'::jsonb, '{}'::jsonb, 1, 7, now() AT TIME ZONE 'UTC')
		RETURNING id`).Scan(&otherToolkitID); err != nil {
		t.Fatalf("insert second toolkit: %v", err)
	}

	response := fixture.do(t, http.MethodDelete,
		"/index_meta/prompt_lib/1/"+itoa(otherToolkitID)+"/"+itoa(fixture.indexID), "")
	if response.Code != http.StatusNotFound {
		t.Errorf("cross-toolkit delete answered %d, want 404", response.Code)
	}
	if got := fixture.countIndexRows(t); got != 1 {
		t.Errorf("cross-toolkit delete removed the row: %d rows left, want 1", got)
	}
}

// ── PATCH (schedule) ─────────────────────────────────────────────────────

// The schedule must land at meta.indexes_meta[<name>].schedules[-1] with the
// submitted values, because that is the only path the client reads back.
// Against the stub (`{"ok":true}`, no query) meta stays `{}` and every field
// assertion below fails.
func TestIndexMetaUpdateStoresTheScheduleWhereTheClientReadsIt(t *testing.T) {
	fixture := newIndexWriteFixture(t)

	response := fixture.do(t, http.MethodPatch,
		"/index_meta/prompt_lib/1/"+itoa(fixture.toolkitID)+"/"+fixture.indexName,
		`{"timezone":"Europe/Vilnius","cron":"0 4 * * *","enabled":true,"credentials":"cred-9"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("PATCH status %d, body %s", response.Code, response.Body.String())
	}

	schedule := scheduleFor(fixture.readToolkitMeta(t), fixture.indexName, "-1")
	if schedule == nil {
		t.Fatalf("nothing was written to meta.indexes_meta[%q].schedules[-1]", fixture.indexName)
	}
	if got, _ := schedule["cron"].(string); got != "0 4 * * *" {
		t.Errorf("stored cron = %q, want %q", got, "0 4 * * *")
	}
	if got, _ := schedule["enabled"].(bool); !got {
		t.Errorf("stored enabled = %v, want true", schedule["enabled"])
	}
	if got, _ := schedule["credentials"].(string); got != "cred-9" {
		t.Errorf("stored credentials = %v, want %q", schedule["credentials"], "cred-9")
	}
	if got, _ := schedule["timezone"].(string); got != "Europe/Vilnius" {
		t.Errorf("stored timezone = %v, want %q", schedule["timezone"], "Europe/Vilnius")
	}
}

// The enable/disable toggle submits `{enabled}` and a timezone only. A blind
// overwrite would drop the cron the user configured — the schedule would still
// exist, so a "did it write anything" test would not see it.
func TestIndexMetaUpdatePreservesFieldsTheRequestOmits(t *testing.T) {
	fixture := newIndexWriteFixture(t)
	path := "/index_meta/prompt_lib/1/" + itoa(fixture.toolkitID) + "/" + fixture.indexName

	if code := fixture.do(t, http.MethodPatch, path,
		`{"timezone":"UTC","cron":"0 5 * * *","enabled":true}`).Code; code != http.StatusOK {
		t.Fatalf("first PATCH: status %d", code)
	}
	if code := fixture.do(t, http.MethodPatch, path, `{"timezone":"UTC","enabled":false}`).Code; code != http.StatusOK {
		t.Fatalf("second PATCH: status %d", code)
	}

	schedule := scheduleFor(fixture.readToolkitMeta(t), fixture.indexName, "-1")
	if schedule == nil {
		t.Fatalf("no schedule stored after two PATCHes")
	}
	if got, _ := schedule["cron"].(string); got != "0 5 * * *" {
		t.Errorf("cron = %q after a request that did not mention it, want the stored %q", got, "0 5 * * *")
	}
	if got, _ := schedule["enabled"].(bool); got {
		t.Errorf("enabled = true, want the false the second request submitted")
	}
}

// Scheduling one index must not disturb another index's schedule on the same
// toolkit — they share a single `meta` document, so a whole-document write
// that forgets to merge loses one of them.
func TestIndexMetaUpdateKeepsOtherIndexesSchedules(t *testing.T) {
	fixture := newIndexWriteFixture(t)
	base := "/index_meta/prompt_lib/1/" + itoa(fixture.toolkitID) + "/"

	if code := fixture.do(t, http.MethodPatch, base+"first-index",
		`{"timezone":"UTC","cron":"0 1 * * *","enabled":true}`).Code; code != http.StatusOK {
		t.Fatalf("PATCH first-index: %d", code)
	}
	if code := fixture.do(t, http.MethodPatch, base+"second-index",
		`{"timezone":"UTC","cron":"0 2 * * *","enabled":true}`).Code; code != http.StatusOK {
		t.Fatalf("PATCH second-index: %d", code)
	}

	meta := fixture.readToolkitMeta(t)
	first := scheduleFor(meta, "first-index", "-1")
	if first == nil {
		t.Fatalf("first-index's schedule was lost by the second write")
	}
	if got, _ := first["cron"].(string); got != "0 1 * * *" {
		t.Errorf("first-index cron = %q, want %q", got, "0 1 * * *")
	}
	if second := scheduleFor(meta, "second-index", "-1"); second == nil {
		t.Errorf("second-index's schedule is missing")
	}
}

func TestIndexMetaUpdateReportsNotFoundForAnUnknownToolkit(t *testing.T) {
	fixture := newIndexWriteFixture(t)

	response := fixture.do(t, http.MethodPatch,
		"/index_meta/prompt_lib/1/999999/"+fixture.indexName, `{"timezone":"UTC","enabled":true}`)
	if response.Code != http.StatusNotFound {
		t.Errorf("PATCH against a nonexistent toolkit answered %d, want 404 (body %s)",
			response.Code, response.Body.String())
	}
}

// ── CANCEL ───────────────────────────────────────────────────────────────

// Cancel must durably record the transition, and must refuse a task id the row
// is not running. Against the stub (`{"ok":true}`, no query) the status stays
// `in_progress` and the stale-cancel case is reported as success.
func TestIndexCancelTransitionsARunningRowAndRejectsAStaleTaskID(t *testing.T) {
	fixture := newIndexWriteFixture(t)
	base := "/index_cancel/prompt_lib/1/" + itoa(fixture.toolkitID) + "/" + fixture.indexName + "/"

	// A task id this index is NOT running: the run it belonged to is over and
	// a different one has taken its place. Cancelling it must not touch the
	// current run.
	stale := fixture.do(t, http.MethodDelete, base+"task-from-a-previous-run", "")
	if stale.Code != http.StatusConflict {
		t.Errorf("stale cancel answered %d, want 409 (body %s)", stale.Code, stale.Body.String())
	}
	if got := fixture.readIndexStatus(t); got != "in_progress" {
		t.Errorf("a stale cancel changed the status to %q; the running index was cancelled by a stale button", got)
	}

	// The task the row is actually running.
	response := fixture.do(t, http.MethodDelete, base+"task-abc", "")
	if response.Code != http.StatusNoContent {
		t.Fatalf("cancel answered %d, want 204 (body %s)", response.Code, response.Body.String())
	}

	// THE ASSERTION. 204 is exactly what the stub returned.
	if got := fixture.readIndexStatus(t); got != "cancelled" {
		t.Errorf("status = %q after a successful cancel, want %q — nothing was recorded", got, "cancelled")
	}
}

// Cancelling something already finished is a client mistake, not a silent
// success. It is also the case that distinguishes this handler from one that
// blindly UPDATEs: a blanket update would report 204 here too.
func TestIndexCancelRefusesATerminalRow(t *testing.T) {
	fixture := newIndexWriteFixture(t)
	if _, err := fixture.pool.Exec(context.Background(),
		`UPDATE p_1.index_meta SET status = 'completed' WHERE id = $1`, fixture.indexID); err != nil {
		t.Fatalf("mark the fixture completed: %v", err)
	}

	response := fixture.do(t, http.MethodDelete,
		"/index_cancel/prompt_lib/1/"+itoa(fixture.toolkitID)+"/"+fixture.indexName+"/task-abc", "")
	if response.Code != http.StatusConflict {
		t.Errorf("cancelling a completed index answered %d, want 409 (body %s)", response.Code, response.Body.String())
	}
	if got := fixture.readIndexStatus(t); got != "completed" {
		t.Errorf("status = %q, want the untouched %q", got, "completed")
	}
}

// An index that does not exist cannot be cancelled, and saying otherwise is
// the exact fake-success this issue is about.
func TestIndexCancelReportsNotFoundForAnUnknownIndex(t *testing.T) {
	fixture := newIndexWriteFixture(t)

	response := fixture.do(t, http.MethodDelete,
		"/index_cancel/prompt_lib/1/"+itoa(fixture.toolkitID)+"/no-such-index/task-abc", "")
	if response.Code != http.StatusNotFound {
		t.Errorf("cancelling a nonexistent index answered %d, want 404 (body %s)",
			response.Code, response.Body.String())
	}
}

// The client interpolates a missing task id into the path as the literal
// "null". A row with no stored task id must match it rather than 409.
func TestIndexCancelTreatsTheLiteralNullPathSegmentAsNoTaskID(t *testing.T) {
	fixture := newIndexWriteFixture(t)
	if _, err := fixture.pool.Exec(context.Background(),
		`UPDATE p_1.index_meta SET meta = '{}'::jsonb WHERE id = $1`, fixture.indexID); err != nil {
		t.Fatalf("clear the fixture's task id: %v", err)
	}

	response := fixture.do(t, http.MethodDelete,
		"/index_cancel/prompt_lib/1/"+itoa(fixture.toolkitID)+"/"+fixture.indexName+"/null", "")
	if response.Code != http.StatusNoContent {
		t.Fatalf("cancel with a \"null\" task id answered %d, want 204 (body %s)",
			response.Code, response.Body.String())
	}
	if got := fixture.readIndexStatus(t); got != "cancelled" {
		t.Errorf("status = %q, want %q", got, "cancelled")
	}
}
