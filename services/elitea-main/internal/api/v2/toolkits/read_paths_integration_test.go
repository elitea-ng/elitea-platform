package toolkits

// The three toolkit READ paths that build their own statement text: the
// export, the index list and the single index read.
//
// None of them had a test. All three interpolate a tenant schema name into
// their SQL and then scan a fixed column list, so a migration that renames a
// column, or a schema guard that stops running, breaks them in a way no
// stubbed handler test can see. They also decide what a caller may READ: the
// export redacts the stored settings, and a test that stops at the status code
// cannot tell a redacted secret from a served one.
//
// One database per test is expensive here (a fresh database plus the baseline
// migrations), so this file builds ONE fixture and drives the three paths
// across it.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL), which ci-go.yml
// provides.

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

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
)

func TestToolkitReadPathsServeTheStoredRows(t *testing.T) {
	pool := newToolkitsIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run baseline migrations: %v", err)
	}

	var toolkitID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.elitea_tools (name, type, description, settings, meta, owner_id, author_id)
		VALUES ('read-paths-fixture', 'github', 'a description',
			'{"repository":"acme/widgets","access_token":"ghp_supersecret"}'::jsonb,
			'{}'::jsonb, 1, 7)
		RETURNING id`).Scan(&toolkitID); err != nil {
		t.Fatalf("insert fixture toolkit: %v", err)
	}
	const indexName = "read-paths-collection"
	var indexID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.index_meta (toolkit_id, name, status, progress, meta)
		VALUES ($1, $2, 'completed', 100, '{"task_id":"task-read"}'::jsonb)
		RETURNING id`, toolkitID, indexName).Scan(&indexID); err != nil {
		t.Fatalf("insert fixture index_meta: %v", err)
	}

	handler := NewHandler(pool)
	router := chi.NewRouter()
	router.Get("/toolkit_export/prompt_lib/{projectID}/{toolkitID}", handler.ExportToolkit)
	router.Get("/index_meta/prompt_lib/{projectID}/{toolkitID}", handler.IndexMeta)
	// The single read keys on the index ROW ID, not on the index name: that is
	// the parameter the router mounts, {indexMetaID}.
	router.Get("/index_meta/prompt_lib/{projectID}/{toolkitID}/{indexMetaID}", handler.IndexMetaGet)

	id := strconv.FormatInt(toolkitID, 10)
	get := func(path string) (int, map[string]any) {
		t.Helper()
		recorder := doToolkitRequest(t, router, http.MethodGet, path)
		var body map[string]any
		if recorder.Body.Len() > 0 {
			_ = json.Unmarshal(recorder.Body.Bytes(), &body)
		}
		return recorder.Code, body
	}
	// The index list answers a BARE ARRAY. The single reads answer an object.
	// Two decoders, because a wrong one here would read as an empty list.
	getList := func(path string) (int, []any) {
		t.Helper()
		recorder := doToolkitRequest(t, router, http.MethodGet, path)
		var body []any
		if recorder.Body.Len() > 0 {
			_ = json.Unmarshal(recorder.Body.Bytes(), &body)
		}
		return recorder.Code, body
	}

	t.Run("the export serves the row and redacts the secret", func(t *testing.T) {
		status, body := get("/toolkit_export/prompt_lib/1/" + id)
		if status != http.StatusOK {
			t.Fatalf("status=%d", status)
		}
		if body["name"] != "read-paths-fixture" || body["type"] != "github" ||
			body["description"] != "a description" {
			t.Errorf("body=%#v", body)
		}
		if body["forked"] != false {
			t.Errorf("forked=%#v, want false without ?fork=true", body["forked"])
		}
		settings, _ := body["settings"].(map[string]any)
		if settings["repository"] != "acme/widgets" {
			t.Errorf("settings=%#v", settings)
		}
		// The export is a download. A stored credential must not ride out in
		// it, whatever the caller may read of the toolkit itself.
		if strings.Contains(strings.ToLower(mustEncode(t, body)), "ghp_supersecret") {
			t.Errorf("the export carries the stored access token: %s", mustEncode(t, body))
		}
	})

	t.Run("the export marks a fork", func(t *testing.T) {
		status, body := get("/toolkit_export/prompt_lib/1/" + id + "?fork=true")
		if status != http.StatusOK || body["forked"] != true {
			t.Errorf("status=%d forked=%#v", status, body["forked"])
		}
	})

	t.Run("an unknown toolkit is not found", func(t *testing.T) {
		status, body := get("/toolkit_export/prompt_lib/1/999999")
		if status != http.StatusNotFound {
			t.Fatalf("status=%d, want 404", status)
		}
		// The driver message names the schema and the column list. The caller
		// gets neither.
		if encoded := mustEncode(t, body); strings.Contains(encoded, "elitea_tools") {
			t.Errorf("the not-found body names the table: %s", encoded)
		}
	})

	t.Run("the index list serves the stored index as a bare array", func(t *testing.T) {
		// The shape is asserted, not assumed. The web client reads this
		// endpoint with .map, and the handler's own comment records that its
		// failure path once answered an object instead — which reached the
		// browser as an `e.map is not a function` error boundary (#149).
		status, rows := getList("/index_meta/prompt_lib/1/" + id)
		if status != http.StatusOK {
			t.Fatalf("status=%d", status)
		}
		if len(rows) != 1 {
			t.Fatalf("rows=%#v", rows)
		}
		row, _ := rows[0].(map[string]any)
		if row["name"] != indexName || row["status"] != "completed" {
			t.Errorf("row=%#v", row)
		}
	})

	t.Run("the index list is empty for a toolkit with no index", func(t *testing.T) {
		var otherID int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO p_1.elitea_tools (name, type, settings, meta, owner_id, author_id)
			VALUES ('read-paths-empty', 'custom', '{}'::jsonb, '{}'::jsonb, 1, 7)
			RETURNING id`).Scan(&otherID); err != nil {
			t.Fatalf("insert second toolkit: %v", err)
		}
		status, rows := getList("/index_meta/prompt_lib/1/" + strconv.FormatInt(otherID, 10))
		if status != http.StatusOK {
			t.Fatalf("status=%d", status)
		}
		if rows == nil || len(rows) != 0 {
			t.Errorf("rows=%#v, want an empty array", rows)
		}
	})

	t.Run("the single index read serves it by row id", func(t *testing.T) {
		status, body := get("/index_meta/prompt_lib/1/" + id + "/" +
			strconv.FormatInt(indexID, 10))
		if status != http.StatusOK {
			t.Fatalf("status=%d: %#v", status, body)
		}
		if body["name"] != indexName || body["status"] != "completed" {
			t.Errorf("body=%#v", body)
		}
	})

	t.Run("an unknown index is not found", func(t *testing.T) {
		status, _ := get("/index_meta/prompt_lib/1/" + id + "/999999")
		if status != http.StatusNotFound {
			t.Errorf("status=%d, want 404", status)
		}
	})
}

func doToolkitRequest(
	t *testing.T, router chi.Router, method, path string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(""))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func mustEncode(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(encoded)
}
