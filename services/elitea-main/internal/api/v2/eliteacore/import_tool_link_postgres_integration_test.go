package eliteacore_test

// Real-PostgreSQL coverage for the tool-link INSERT in the IMPORT path
// (handler.go, Handler.ExportImportPost) — issue #420.
//
// The link ran as `_, _ = h.pool.Exec(...)` under a comment that called it a
// "best-effort link". A failed link therefore left the agent with no toolkit,
// the response still named the toolkit in `version_details.tools`, and the
// caller got 201. The import path already had a partial-success channel —
// `errors.agents` plus 207 — and no branch put the insert error into it.
//
// So no test here asserts on the status code alone. A 201 from the import is
// exactly what the defect produced. Every case reads the mapping rows back out
// of the database, and the failure cases read the response body as well.
//
// SCHEMA SHAPE, PER CASE
//
// All cases build the schema from the REAL migration corpus — db.RunMigrations
// followed by migrate.Runner.ApplyShared/ApplyTenant, the production chain — and
// then make ONE recorded adjustment, in `relaxImportLinkToolkitOwner`:
// `p_1.elitea_tools.owner_id` gets its NOT NULL dropped.
//
// That adjustment is not convenience. The bootstrap table declares
// `owner_id INTEGER NOT NULL` (internal/infra/db/migrations/001_initial.sql:434-447)
// and the DEPLOYED pylon table has no `owner_id` column at all — see
// internal/db/schema/toolkit_baseline.sql:8-20, the sqlc projection of the shape
// production runs. The import path's own toolkit INSERT
// (handler.go, phase 2) names neither `owner_id` nor a value for it, so on a
// corpus-built schema every toolkit import raises 23502 and the tool-link INSERT
// this file is about is never reached. Dropping the NOT NULL is the closest a
// corpus-built schema can come to the shape production serves, and it is the
// minimum change that lets the link statement run at all. The divergence itself
// is a separate defect of the same family as the `entity_id` column that
// migration 0125 repaired, and it is reported on #420 rather than repaired here.
//
// `entity_id` needs no adjustment: tenant migration 0125 adds it NOT NULL, and
// `assertImportLinkColumns` guards that, because a corpus that stopped
// delivering the column would let these tests pass against a schema that cannot
// carry a link row at all.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// importLinkResponse is the part of the import answer these tests read.
type importLinkResponse struct {
	Result struct {
		Agents []struct {
			ID             string          `json:"id"`
			Name           string          `json:"name"`
			VersionDetails *importLinkVerD `json:"version_details"`
		} `json:"agents"`
		Toolkits []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"toolkits"`
	} `json:"result"`
	Errors struct {
		Agents []struct {
			Index int    `json:"index"`
			Name  string `json:"name"`
			Msg   string `json:"msg"`
		} `json:"agents"`
		Toolkits []struct {
			Index int    `json:"index"`
			Name  string `json:"name"`
			Msg   string `json:"msg"`
		} `json:"toolkits"`
	} `json:"errors"`
}

type importLinkVerD struct {
	ID    string `json:"id"`
	Tools []struct {
		ID string `json:"id"`
		// The two keys the answer used to fill with a literal `"custom"` and a
		// literal `""`. They are decoded here so the case below can read what
		// the import says the version now carries.
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"tools"`
}

// importLinkBody is one toolkit and one agent whose single version references
// that toolkit. It is the smallest export file that exercises the link.
func importLinkBody(toolRefs []map[string]any, versionName string) []map[string]any {
	return []map[string]any{
		{
			"entity":      "toolkits",
			"import_uuid": "tk-1",
			"name":        "fixture toolkit",
			"type":        "github",
			"description": "seeded toolkit",
			"settings":    map[string]any{"url": "https://example.invalid"},
		},
		{
			"entity":      "agents",
			"import_uuid": "ag-1",
			"name":        "fixture agent",
			"description": "seeded agent",
			"versions": []map[string]any{
				{
					"name":                versionName,
					"agent_type":          "openai",
					"instructions":        "do the thing",
					"import_version_uuid": "ver-1",
					"tools":               toolRefs,
				},
			},
		},
	}
}

func importLinkToolRef() map[string]any {
	return map[string]any{
		"import_uuid":    "tk-1",
		"selected_tools": map[string]any{"get_issue": true},
	}
}

// TestImportWritesToolLinkRows is the guard on the status change. A working
// import must still answer 201, and it must still write one row for each tool
// link. Without this case, routing the insert error into `errors.agents` could
// turn every import into a 207 and nothing would notice.
func TestImportWritesToolLinkRows(t *testing.T) {
	pool := newImportLinkPool(t)
	router := importLinkRouter(eliteacore.NewHandler(pool))

	recorder := importLinkDo(t, router, importLinkBody([]map[string]any{importLinkToolRef()}, "latest"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	answer := decodeImportLink(t, recorder)
	if len(answer.Errors.Agents) != 0 || len(answer.Errors.Toolkits) != 0 {
		t.Fatalf("working import reported errors: %s", recorder.Body.String())
	}
	if len(answer.Result.Agents) != 1 || len(answer.Result.Toolkits) != 1 {
		t.Fatalf("working import result = %s", recorder.Body.String())
	}

	appID := importLinkAtoi(t, answer.Result.Agents[0].ID)
	toolID := importLinkAtoi(t, answer.Result.Toolkits[0].ID)
	details := answer.Result.Agents[0].VersionDetails
	if details == nil {
		t.Fatalf("working import returned no version_details: %s", recorder.Body.String())
	}
	if len(details.Tools) != 1 || details.Tools[0].ID != answer.Result.Toolkits[0].ID {
		t.Errorf("version_details.tools = %+v, want the imported toolkit %s", details.Tools, answer.Result.Toolkits[0].ID)
	}
	versionID := importLinkAtoi(t, details.ID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var rowVersionID, rowEntityID, rowToolID int
	var rowEntityType, rowSelected string
	if err := pool.QueryRow(ctx, `
SELECT entity_version_id, entity_id, entity_type, tool_id, COALESCE(selected_tools::text, '')
FROM p_1.entity_tool_mapping`).
		Scan(&rowVersionID, &rowEntityID, &rowEntityType, &rowToolID, &rowSelected); err != nil {
		t.Fatalf("the import wrote no tool link: %v", err)
	}
	if rowVersionID != versionID {
		t.Errorf("link entity_version_id = %d, want %d", rowVersionID, versionID)
	}
	// The chat read joins entity_id to the owning application
	// (internal/db/queries/agent_chat.sql:107), so a link that names the wrong
	// agent resolves to no toolkit at run time.
	if rowEntityID != appID {
		t.Errorf("link entity_id = %d, want the owning application %d", rowEntityID, appID)
	}
	if rowEntityType != "application" {
		t.Errorf("link entity_type = %q, want %q", rowEntityType, "application")
	}
	if rowToolID != toolID {
		t.Errorf("link tool_id = %d, want %d", rowToolID, toolID)
	}
	if rowSelected != `{"get_issue": true}` {
		t.Errorf("link selected_tools = %q, want the requested selection", rowSelected)
	}

	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.entity_tool_mapping`); count != 1 {
		t.Errorf("tool link rows = %d, want 1", count)
	}

	// The author columns carry the caller, not user 1 (#505). The owner column
	// carries the DESTINATION PROJECT, and the destination schema is p_1
	// (#533): `applications.owner_id` is the project, as it is on `skills` and
	// on `elitea_tools`.
	var applicationOwner, versionAuthor, toolkitAuthor int
	if err := pool.QueryRow(ctx, `
SELECT a.owner_id, v.author_id, t.author_id
FROM p_1.applications a
JOIN p_1.application_versions v ON v.application_id = a.id
CROSS JOIN p_1.elitea_tools t`).Scan(&applicationOwner, &versionAuthor, &toolkitAuthor); err != nil {
		t.Fatalf("read the imported owners: %v", err)
	}
	for _, author := range []struct {
		column string
		value  int
	}{
		{"application_versions.author_id", versionAuthor},
		{"elitea_tools.author_id", toolkitAuthor},
	} {
		if author.value != importLinkPrincipal {
			t.Errorf("%s = %d, want the caller %d", author.column, author.value, importLinkPrincipal)
		}
	}
	if applicationOwner != 1 {
		t.Errorf("applications.owner_id = %d, want the destination project 1", applicationOwner)
	}
	if applicationOwner == importLinkPrincipal {
		t.Error("applications.owner_id holds the caller; the fixture picks a principal that is not project 1 precisely so this is visible")
	}
}

// TestImportNamesTheLinkedToolkit is the acceptance test for the import answer
// that could not say WHICH toolkit it had attached.
//
// The link phase wrote `{"id": <row>, "type": "custom", "name": ""}` into the
// version's tool summary, whatever the toolkit was. `result.toolkits` — built
// one phase earlier from the same two values — named it correctly, so one
// response carried both the true name and a placeholder for it, and the wizard
// shows the version summary. An import of a `github` toolkit reported an
// unnamed `custom` one on the agent it had just attached it to.
//
// Both channels are read, and they are compared with each other: an answer
// that named the toolkit in only one of them is the defect itself.
func TestImportNamesTheLinkedToolkit(t *testing.T) {
	pool := newImportLinkPool(t)
	router := importLinkRouter(eliteacore.NewHandler(pool))

	recorder := importLinkDo(t, router, importLinkBody([]map[string]any{importLinkToolRef()}, "latest"))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	answer := decodeImportLink(t, recorder)
	if len(answer.Result.Toolkits) != 1 {
		t.Fatalf("result.toolkits = %+v, want the imported toolkit", answer.Result.Toolkits)
	}
	imported := answer.Result.Toolkits[0]
	details := answer.Result.Agents[0].VersionDetails
	if details == nil || len(details.Tools) != 1 {
		t.Fatalf("version_details.tools = %+v, want the one link that was written", details)
	}
	attached := details.Tools[0]
	if attached.Name != "fixture toolkit" {
		t.Errorf("version_details.tools[0].name = %q, want the toolkit's own name %q",
			attached.Name, "fixture toolkit")
	}
	if attached.Type != "github" {
		t.Errorf("version_details.tools[0].type = %q, want the toolkit's own type %q", attached.Type, "github")
	}
	if attached.Name != imported.Name || attached.Type != imported.Type || attached.ID != imported.ID {
		t.Errorf("version_details.tools[0] = %+v, want the same toolkit result.toolkits reports (%+v)",
			attached, imported)
	}

	// The stored row is the authority on both values, so a summary built from
	// anything else — the request, a default — is visible here.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var storedName, storedType string
	if err := pool.QueryRow(ctx, `
SELECT t.name, t.type FROM p_1.elitea_tools t
JOIN p_1.entity_tool_mapping m ON m.tool_id = t.id`).Scan(&storedName, &storedType); err != nil {
		t.Fatalf("read the linked toolkit row: %v", err)
	}
	if attached.Name != storedName || attached.Type != storedType {
		t.Errorf("version_details.tools[0] = {name %q, type %q}, want the stored row {%q, %q}",
			attached.Name, attached.Type, storedName, storedType)
	}
}

// TestImportNamesAToolkitWithNoDeclaredType guards the other half of the same
// answer. `custom` is a real value here: it is what phase 2 STORES for an entry
// that declares no type. The summary must report that stored default, and it
// must report the entry's name beside it — which is what tells this case apart
// from the literal it replaces.
func TestImportNamesAToolkitWithNoDeclaredType(t *testing.T) {
	pool := newImportLinkPool(t)
	router := importLinkRouter(eliteacore.NewHandler(pool))

	body := importLinkBody([]map[string]any{importLinkToolRef()}, "latest")
	delete(body[0], "type")
	body[0]["name"] = "untyped fixture toolkit"

	recorder := importLinkDo(t, router, body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	answer := decodeImportLink(t, recorder)
	details := answer.Result.Agents[0].VersionDetails
	if details == nil || len(details.Tools) != 1 {
		t.Fatalf("version_details.tools = %+v, want the one link that was written", details)
	}
	attached := details.Tools[0]
	if attached.Type != "custom" {
		t.Errorf("version_details.tools[0].type = %q, want the stored default %q", attached.Type, "custom")
	}
	if attached.Name != "untyped fixture toolkit" {
		t.Errorf("version_details.tools[0].name = %q, want the entry's own name", attached.Name)
	}
}

// TestImportRefusesWithNoPrincipal proves the other half of the same repair.
// With no principal the import used to write every row as user 1 and answer
// 201. It must now write nothing and say why.
func TestImportRefusesWithNoPrincipal(t *testing.T) {
	pool := newImportLinkPool(t)
	router := importLinkRouterWithoutPrincipal(eliteacore.NewHandler(pool))

	recorder := importLinkDo(t, router, importLinkBody([]map[string]any{importLinkToolRef()}, "latest"))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusUnauthorized, recorder.Body.String())
	}
	for _, table := range []string{"applications", "application_versions", "elitea_tools", "entity_tool_mapping"} {
		if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.`+table); count != 0 {
			t.Errorf("%s rows = %d, want 0 — a refused import must write nothing", table, count)
		}
	}
}

// TestImportReportsAFailedToolLink is the acceptance test #420 asks for. The
// insert is broken on purpose, so the toolkit imports and the row that joins it
// to the agent cannot be written. The caller must see that, and the response
// must not name a link that has no row.
func TestImportReportsAFailedToolLink(t *testing.T) {
	pool := newImportLinkPool(t)
	router := importLinkRouter(eliteacore.NewHandler(pool))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Break the link statement and nothing before it. Every row the import can
	// write carries a positive entity_version_id and cannot satisfy this.
	if _, err := pool.Exec(ctx, `
ALTER TABLE p_1.entity_tool_mapping ADD CONSTRAINT link_must_fail CHECK (entity_version_id < 0)`); err != nil {
		t.Fatalf("install failing constraint: %v", err)
	}

	recorder := importLinkDo(t, router, importLinkBody([]map[string]any{importLinkToolRef()}, "latest"))
	if recorder.Code == http.StatusCreated {
		t.Fatalf("import reported success while the tool link failed: %s", recorder.Body.String())
	}
	// 207: the agent and the toolkit did import. Only the link between them
	// failed, so the answer is partial success and not a plain refusal.
	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusMultiStatus, recorder.Body.String())
	}

	answer := decodeImportLink(t, recorder)
	if len(answer.Errors.Agents) != 1 {
		t.Fatalf("errors.agents = %+v, want exactly the failed link", answer.Errors.Agents)
	}
	reported := answer.Errors.Agents[0]
	if reported.Name != "fixture agent" {
		t.Errorf("reported name = %q, want %q", reported.Name, "fixture agent")
	}
	// The import wizard maps this index back to the submitted entity to mark it
	// red (apps/elitea-ui .../import-wizard/lib/helpers/importWizardForkImport.helpers.js:12).
	// The agent is the second entity in the body.
	if reported.Index != 1 {
		t.Errorf("reported index = %d, want the agent's position 1", reported.Index)
	}
	// The pre-existing message describes an unresolved reference. This fault is
	// a failed write, so it must not borrow that text.
	if !bytes.Contains([]byte(reported.Msg), []byte("unable to link toolkit")) {
		t.Errorf("reported msg = %q, want the failed-insert message", reported.Msg)
	}

	if len(answer.Result.Agents) != 1 {
		t.Fatalf("result.agents = %+v, want the imported agent", answer.Result.Agents)
	}
	details := answer.Result.Agents[0].VersionDetails
	if details == nil {
		t.Fatalf("import returned no version_details: %s", recorder.Body.String())
	}
	// The response must not name a tool link that has no row.
	if len(details.Tools) != 0 {
		t.Errorf("version_details.tools = %+v, want empty after the link failed", details.Tools)
	}

	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.entity_tool_mapping`); count != 0 {
		t.Errorf("tool link rows = %d, want 0", count)
	}
	// The toolkit itself imported. The report must be about the link only.
	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.elitea_tools`); count != 1 {
		t.Errorf("toolkit rows = %d, want 1", count)
	}
}

// TestImportReportsADuplicateToolLink drives the same failure from the request
// body alone, with no constraint of the test's own making. `_entity_tool_unique`
// (001_initial.sql:451-460) rejects a second reference to one toolkit in one
// version with 23505. The first link is written, the second is not, and the
// response must name one tool and one error.
func TestImportReportsADuplicateToolLink(t *testing.T) {
	pool := newImportLinkPool(t)
	router := importLinkRouter(eliteacore.NewHandler(pool))

	recorder := importLinkDo(t, router, importLinkBody(
		[]map[string]any{importLinkToolRef(), importLinkToolRef()}, "latest"))
	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusMultiStatus, recorder.Body.String())
	}

	answer := decodeImportLink(t, recorder)
	if len(answer.Errors.Agents) != 1 {
		t.Fatalf("errors.agents = %+v, want exactly the rejected duplicate", answer.Errors.Agents)
	}
	if !bytes.Contains([]byte(answer.Errors.Agents[0].Msg), []byte("unable to link toolkit")) {
		t.Errorf("reported msg = %q, want the failed-insert message", answer.Errors.Agents[0].Msg)
	}

	details := answer.Result.Agents[0].VersionDetails
	if details == nil {
		t.Fatalf("import returned no version_details: %s", recorder.Body.String())
	}
	if len(details.Tools) != 1 {
		t.Errorf("version_details.tools = %+v, want the one link that was written", details.Tools)
	}
	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.entity_tool_mapping`); count != 1 {
		t.Errorf("tool link rows = %d, want 1", count)
	}
}

// TestImportReportsAFailedVersionInsert covers the sibling this repair found in
// the same function. The version INSERT answered a failure with a bare
// `continue`, so the agent row was written, the version was lost, and the caller
// got 201. It also proves the nil-map guard on `version_details`: before the
// repair this request panicked with "assignment to entry in nil map", because an
// agent whose every version failed stored a nil map in that key.
func TestImportReportsAFailedVersionInsert(t *testing.T) {
	pool := newImportLinkPool(t)
	router := importLinkRouter(eliteacore.NewHandler(pool))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
ALTER TABLE p_1.application_versions ADD CONSTRAINT version_must_fail CHECK (name <> 'boom')`); err != nil {
		t.Fatalf("install failing constraint: %v", err)
	}

	recorder := importLinkDo(t, router, importLinkBody([]map[string]any{importLinkToolRef()}, "boom"))
	if recorder.Code == http.StatusCreated {
		t.Fatalf("import reported success while the version insert failed: %s", recorder.Body.String())
	}
	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("import status = %d, want %d, body = %s", recorder.Code, http.StatusMultiStatus, recorder.Body.String())
	}

	answer := decodeImportLink(t, recorder)
	if len(answer.Errors.Agents) != 1 {
		t.Fatalf("errors.agents = %+v, want exactly the failed version", answer.Errors.Agents)
	}
	if !bytes.Contains([]byte(answer.Errors.Agents[0].Msg), []byte("unable to import version boom")) {
		t.Errorf("reported msg = %q, want the failed-version message", answer.Errors.Agents[0].Msg)
	}
	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.application_versions`); count != 0 {
		t.Errorf("application_versions rows = %d, want 0", count)
	}
}

/* ── helpers ───────────────────────────────────────────────────────────── */

// importLinkPrincipal is the user every request in this file is made by.
//
// It is not user 1. The import used to read the principal with
// `fmt.Sscanf(user.ID, "%d", &userID)` over a `userID := 1` default, so an
// unauthenticated request and a request whose identifier could not be read both
// wrote rows owned by user 1 (#505). These cases used to send no principal at
// all and could not have seen that. They now send one, and
// TestImportWritesToolLinkRows reads the owner column back.
const importLinkPrincipal = 4242

func importLinkRouter(handler *eliteacore.Handler) chi.Router {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			user := auth.User{ID: strconv.Itoa(importLinkPrincipal)}
			next.ServeHTTP(w, request.WithContext(auth.ContextWithUser(request.Context(), user)))
		})
	})
	router.Post("/elitea_core/import_wizard/prompt_lib/{projectID}", handler.ExportImportPost)
	return router
}

// importLinkRouterWithoutPrincipal serves the same route with no principal in
// the request context.
func importLinkRouterWithoutPrincipal(handler *eliteacore.Handler) chi.Router {
	router := chi.NewRouter()
	router.Post("/elitea_core/import_wizard/prompt_lib/{projectID}", handler.ExportImportPost)
	return router
}

func importLinkDo(t *testing.T, router chi.Router, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/elitea_core/import_wizard/prompt_lib/1", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func decodeImportLink(t *testing.T, recorder *httptest.ResponseRecorder) importLinkResponse {
	t.Helper()
	var answer importLinkResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode import response %q: %v", recorder.Body.String(), err)
	}
	return answer
}

func importLinkAtoi(t *testing.T, value string) int {
	t.Helper()
	var parsed int
	if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil {
		t.Fatalf("import returned identifier %q: %v", value, err)
	}
	return parsed
}

func importLinkCount(t *testing.T, pool *pgxpool.Pool, query string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var count int
	if err := pool.QueryRow(ctx, query).Scan(&count); err != nil {
		t.Fatalf("count with %q: %v", query, err)
	}
	return count
}

// newImportLinkPool opens an isolated database on the server named by
// ELITEA_TEST_DATABASE_URL and applies the production migration chain to it.
func newImportLinkPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL import tool-link integration test", environment)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 4
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_imptool_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// 120 s, not 20 s to 30 s: this DROP queues behind the CREATE DATABASE
		// calls of every package that `go test ./...` runs at the same time, so
		// the wait is server load and not a hang (#409).
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})

	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("apply bootstrap migrations: %v", err)
	}
	runner := migrate.New(pool, platformmigrations.Files)
	if err := runner.ApplyShared(ctx); err != nil {
		t.Fatalf("apply embedded shared migrations: %v", err)
	}
	if err := runner.ApplyTenant(ctx, 1); err != nil {
		t.Fatalf("apply embedded tenant migrations: %v", err)
	}
	assertImportLinkColumns(t, pool)
	relaxImportLinkToolkitOwner(t, pool)
	return pool
}

// assertImportLinkColumns guards the point of the exercise. Tenant migration
// 0125 gives `entity_tool_mapping` the `entity_id` column the import names. If
// the corpus stopped delivering it, every case here would exercise a statement
// that cannot run and the tests would report the wrong reason.
func assertImportLinkColumns(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, column := range []string{"entity_id", "entity_type"} {
		var notNull bool
		if err := pool.QueryRow(ctx, `
SELECT attnotnull FROM pg_attribute
WHERE attrelid = 'p_1.entity_tool_mapping'::regclass AND attname = $1 AND NOT attisdropped`,
			column).Scan(&notNull); err != nil {
			t.Fatalf("read p_1.entity_tool_mapping.%s: %v", column, err)
		}
		if !notNull {
			t.Fatalf("p_1.entity_tool_mapping.%s is not NOT NULL — the migration corpus no longer carries the deployed shape", column)
		}
	}
}

// relaxImportLinkToolkitOwner brings the corpus-built toolkit table to the
// deployed shape in the one respect that blocks this path. See the file header:
// the bootstrap declares `owner_id INTEGER NOT NULL`, the deployed pylon table
// has no such column, and the import's toolkit INSERT names no value for it. It
// is written to tolerate a future migration that removes the divergence, so that
// the repair of that separate defect does not fail these tests.
func relaxImportLinkToolkitOwner(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var notNull bool
	err := pool.QueryRow(ctx, `
SELECT attnotnull FROM pg_attribute
WHERE attrelid = 'p_1.elitea_tools'::regclass AND attname = 'owner_id' AND NOT attisdropped`).Scan(&notNull)
	if err != nil {
		t.Logf("p_1.elitea_tools has no owner_id column; the corpus now matches the deployed shape (%v)", err)
		return
	}
	if !notNull {
		return
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE p_1.elitea_tools ALTER COLUMN owner_id DROP NOT NULL`); err != nil {
		t.Fatalf("relax p_1.elitea_tools.owner_id: %v", err)
	}
}
