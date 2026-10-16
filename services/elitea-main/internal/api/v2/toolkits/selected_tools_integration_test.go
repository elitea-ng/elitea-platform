package toolkits

// Write-then-RE-READ tests for the per-mapping `selected_tools` list (#248).
//
// `p_<project>.entity_tool_mapping.selected_tools` has existed since
// 001_initial.sql and NOTHING wrote it: `updateToolRelation` answered 201 for
// every relation PATCH while its INSERT named four columns, none of them
// `selected_tools`, under an unconditional `ON CONFLICT ... DO NOTHING`. A
// handler test that stops at the status code passes against that code
// unchanged, which is exactly how the gap survived (#128's lesson) — so every
// assertion below reads the STORED ROW, and the first one also round-trips the
// value back out through the read path the editor actually reloads from
// (`applications.Handler.GetVersion` -> `fetchVersionDetails`).
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL); the shared helper
// in create_toolkit_owner_id_test.go creates a throwaway database per test.

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

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/applications"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

// relationFixture is one project schema holding one application, one draft
// version and one toolkit instance, reachable through the same two routes the
// router mounts for this flow: the relation PATCH that owns the mapping row,
// and the version GET the editor reloads from.
type relationFixture struct {
	pool          *pgxpool.Pool
	router        chi.Router
	applicationID int64
	versionID     int64
	toolkitID     int64
}

func newRelationFixture(t *testing.T) *relationFixture {
	t.Helper()
	pool := newToolkitsIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run baseline migrations: %v", err)
	}

	// The REAL ledgered corpus, on top of the bootstrap schema — not a
	// hand-written ALTER bringing the fixture up to the shape production has.
	// `entity_tool_mapping.entity_id` is missing from 001_initial.sql:451-460
	// and is added by tenant/0125; an earlier revision of this fixture carried
	// its own `ADD COLUMN IF NOT EXISTS entity_id INTEGER` instead, which meant
	// these tests would have gone on passing if 0125 were dropped. Applying the
	// corpus is what makes the attach below discriminate.
	runner := migrate.New(pool, platformmigrations.Files)
	if err := runner.ApplyShared(ctx); err != nil {
		t.Fatalf("apply shared migrations: %v", err)
	}
	if err := runner.ApplyTenant(ctx, 1); err != nil {
		t.Fatalf("apply tenant migrations to p_1: %v", err)
	}

	var applicationID, versionID, toolkitID int64
	// A decoy application, so the real one below does not get id 1 and collide
	// with its own version's id. `entity_id` and `entity_version_id` are
	// different things and a test that cannot tell them apart cannot catch a
	// writer that confuses them.
	if _, err := pool.Exec(ctx, `
		INSERT INTO p_1.applications (name, description, owner_id, created_at, updated_at)
		VALUES ('selected-tools-decoy', '', 1, now() AT TIME ZONE 'UTC', now() AT TIME ZONE 'UTC')`); err != nil {
		t.Fatalf("insert decoy application: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.applications (name, description, owner_id, created_at, updated_at)
		VALUES ('selected-tools-fixture', '', 1, now() AT TIME ZONE 'UTC', now() AT TIME ZONE 'UTC') RETURNING id`).Scan(&applicationID); err != nil {
		t.Fatalf("insert fixture application: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.application_versions (application_id, name, status, author_id, created_at)
		VALUES ($1, 'base', 'draft', 7, now() AT TIME ZONE 'UTC') RETURNING id`, applicationID).Scan(&versionID); err != nil {
		t.Fatalf("insert fixture version: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO p_1.elitea_tools (name, type, description, settings, meta, owner_id, author_id, created_at)
		VALUES ('selected-tools-fixture', 'github', '', '{}'::jsonb, '{}'::jsonb, 1, 7, now() AT TIME ZONE 'UTC')
		RETURNING id`).Scan(&toolkitID); err != nil {
		t.Fatalf("insert fixture toolkit: %v", err)
	}

	router := chi.NewRouter()
	router.Patch("/tool/prompt_lib/{projectID}/{toolkitID}", NewHandler(pool).Update)
	// repo is nil on purpose: fetchVersionDetails takes its pool-backed branch
	// whenever a pool is supplied, which is the branch this test is about.
	router.Get("/version/prompt_lib/{projectID}/{applicationID}/{versionID}", applications.NewHandler(nil, pool).GetVersion)

	return &relationFixture{pool: pool, router: router, applicationID: applicationID, versionID: versionID, toolkitID: toolkitID}
}

func (f *relationFixture) patchRelation(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/tool/prompt_lib/1/" + strconv.FormatInt(f.toolkitID, 10)
	request := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, request)
	return recorder
}

// attachBody is the ATTACH request `ToolMenu` sends: no `selected_tools` key.
func (f *relationFixture) attachBody() string {
	return `{"entity_version_id": ` + strconv.FormatInt(f.versionID, 10) +
		`, "entity_id": ` + strconv.FormatInt(f.applicationID, 10) +
		`, "entity_type": "agent", "has_relation": true}`
}

// selectionBody is the SELECTION EDIT request the tool card sends: the same
// relation PATCH, plus the full resulting list.
func (f *relationFixture) selectionBody(selectedTools string) string {
	return `{"entity_version_id": ` + strconv.FormatInt(f.versionID, 10) +
		`, "entity_id": ` + strconv.FormatInt(f.applicationID, 10) +
		`, "entity_type": "agent", "has_relation": true, "selected_tools": ` + selectedTools + `}`
}

// storedSelectedTools re-reads the column the write claims to have set.
func (f *relationFixture) storedSelectedTools(t *testing.T) string {
	t.Helper()
	var stored string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT COALESCE(selected_tools::text, 'null') FROM p_1.entity_tool_mapping
		 WHERE entity_version_id = $1 AND tool_id = $2 AND entity_type = 'agent'`,
		f.versionID, f.toolkitID,
	).Scan(&stored); err != nil {
		t.Fatalf("re-read the mapping row: %v", err)
	}
	return stored
}

// readBackSelectedTools goes the whole way round: the version GET the editor
// reloads from, down to the tool row's own `selected_tools`.
func (f *relationFixture) readBackSelectedTools(t *testing.T) []any {
	t.Helper()
	path := "/version/prompt_lib/1/" + strconv.FormatInt(f.applicationID, 10) + "/" + strconv.FormatInt(f.versionID, 10)
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET version = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	var details struct {
		Tools []struct {
			ToolID        int   `json:"tool_id"`
			SelectedTools []any `json:"selected_tools"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &details); err != nil {
		t.Fatalf("decode version details: %v\n%s", err, recorder.Body.String())
	}
	for _, tool := range details.Tools {
		if int64(tool.ToolID) == f.toolkitID {
			return tool.SelectedTools
		}
	}
	t.Fatalf("the version's tools[] carries no row for tool_id %d: %s", f.toolkitID, recorder.Body.String())
	return nil
}

// The defect itself: the checkbox list the card sends must reach the column and
// come back out again. Fails before the fix at the stored-row assertion — the
// row exists (attach worked all along) with selected_tools still '[]'.
func TestRelationPatchPersistsSelectedToolsAndReadsThemBack(t *testing.T) {
	fixture := newRelationFixture(t)

	if code := fixture.patchRelation(t, fixture.attachBody()).Code; code != http.StatusCreated {
		t.Fatalf("attach = %d, want 201", code)
	}
	if code := fixture.patchRelation(t, fixture.selectionBody(`["create_issue","get_issue"]`)).Code; code != http.StatusCreated {
		t.Fatalf("selection edit = %d, want 201", code)
	}

	if stored := fixture.storedSelectedTools(t); stored != `["create_issue", "get_issue"]` {
		t.Errorf("stored selected_tools = %s, want [\"create_issue\", \"get_issue\"]", stored)
	}
	readBack := fixture.readBackSelectedTools(t)
	if len(readBack) != 2 || readBack[0] != "create_issue" || readBack[1] != "get_issue" {
		t.Errorf("version_details.tools[].selected_tools = %#v, want [create_issue get_issue]", readBack)
	}
}

// The attach itself, against the schema the migration corpus actually builds.
//
// `entity_tool_mapping.entity_id` is in the deployed pylon shape
// (internal/db/schema/agent_chat_baseline.sql:104-113) and in every writer, but
// was in no migration until tenant/0125. Revert 0125 and this test fails at the
// first line with the production error, verbatim:
//
//	attach = 500, want 201: {"error":"ERROR: column \"entity_id\" of relation
//	\"entity_tool_mapping\" does not exist (SQLSTATE 42703)"}
//
// which is exactly what a browser got from PATCH /api/v2/elitea_core/tool/
// prompt_lib/{projectId}/{toolkitId} on any database this repository built for
// itself.
//
// It asserts the VALUE, not merely that the row landed: `entity_id` must be the
// owning APPLICATION, while `entity_version_id` is the version. A migration
// that added the column and a handler that filled it with the version id would
// satisfy NOT NULL and then join to nothing — the fixture deliberately has
// distinct ids for the two so that confusion is visible here.
//
// The second half runs the chat read's own join predicate
// (internal/db/queries/agent_chat.sql:106-108, generated twin
// sqlcgen/agent_chat.sql.go:1233-1235) against the row the attach just wrote.
// That is the other half of the same defect: fixing attach while leaving the
// tool-resolution subquery unable to compile would be a half-fix, and the chat
// query is far too large to call through from here.
func TestRelationPatchStoresTheOwningApplicationAsEntityIDAndTheChatJoinFindsIt(t *testing.T) {
	fixture := newRelationFixture(t)

	recorder := fixture.patchRelation(t, fixture.attachBody())
	if recorder.Code != http.StatusCreated {
		t.Fatalf("attach = %d, want 201: %s", recorder.Code, recorder.Body.String())
	}

	var storedEntityID, storedVersionID int64
	if err := fixture.pool.QueryRow(context.Background(),
		`SELECT entity_id, entity_version_id FROM p_1.entity_tool_mapping
		 WHERE tool_id = $1 AND entity_type = 'agent'`, fixture.toolkitID,
	).Scan(&storedEntityID, &storedVersionID); err != nil {
		t.Fatalf("re-read the mapping row: %v", err)
	}
	if storedEntityID != fixture.applicationID {
		t.Errorf("entity_id = %d, want %d (the owning application)", storedEntityID, fixture.applicationID)
	}
	if storedVersionID != fixture.versionID {
		t.Errorf("entity_version_id = %d, want %d", storedVersionID, fixture.versionID)
	}
	if fixture.applicationID == fixture.versionID {
		t.Fatalf("fixture degenerate: application and version share id %d, so this test cannot tell them apart", fixture.applicationID)
	}

	var joined int
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT count(*)
		FROM p_1.application_versions AS application_version
		JOIN p_1.entity_tool_mapping AS application_tool_mapping
		  ON application_tool_mapping.entity_version_id = application_version.id
		 AND application_tool_mapping.entity_id = application_version.application_id
		 AND application_tool_mapping.entity_type = 'agent'
		JOIN p_1.elitea_tools AS tool
		  ON tool.id = application_tool_mapping.tool_id
		WHERE application_version.id = $1`, fixture.versionID,
	).Scan(&joined); err != nil {
		t.Fatalf("the chat tool-resolution join does not run against the migrated schema: %v", err)
	}
	if joined != 1 {
		t.Errorf("the chat tool-resolution join matched %d rows, want 1 — an attached toolkit that the agent turn cannot see", joined)
	}
}

// The ABSENT-vs-EMPTY rule, asserted directly. An attach carries no
// `selected_tools` key; it must leave a previously saved selection alone. An
// explicit `[]` is a real edit ("I unchecked the last one") and must land.
func TestRelationPatchDistinguishesAnAbsentSelectionFromAnEmptyOne(t *testing.T) {
	fixture := newRelationFixture(t)

	fixture.patchRelation(t, fixture.selectionBody(`["create_issue"]`))
	if stored := fixture.storedSelectedTools(t); stored != `["create_issue"]` {
		t.Fatalf("setup: stored selected_tools = %s, want [\"create_issue\"]", stored)
	}

	// Absent key: preserve.
	if code := fixture.patchRelation(t, fixture.attachBody()).Code; code != http.StatusCreated {
		t.Fatalf("re-attach = %d, want 201", code)
	}
	if stored := fixture.storedSelectedTools(t); stored != `["create_issue"]` {
		t.Errorf("a request with no selected_tools key changed the stored selection to %s — an attach must not wipe a saved selection", stored)
	}

	// Explicit empty list: write it.
	if code := fixture.patchRelation(t, fixture.selectionBody(`[]`)).Code; code != http.StatusCreated {
		t.Fatalf("empty selection edit = %d, want 201", code)
	}
	if stored := fixture.storedSelectedTools(t); stored != `[]` {
		t.Errorf("stored selected_tools = %s, want [] — unchecking the last tool is a real edit", stored)
	}
}

// A malformed list is rejected rather than stored: the column is read straight
// back into `version_details.tools[].selected_tools`, which the UI indexes as a
// list of strings.
func TestRelationPatchRejectsANonStringSelectedToolsList(t *testing.T) {
	fixture := newRelationFixture(t)

	fixture.patchRelation(t, fixture.selectionBody(`["create_issue"]`))
	if code := fixture.patchRelation(t, fixture.selectionBody(`[{"name":"create_issue"}]`)).Code; code != http.StatusBadRequest {
		t.Errorf("selection edit with object entries = %d, want 400", code)
	}
	if code := fixture.patchRelation(t, fixture.selectionBody(`"create_issue"`)).Code; code != http.StatusBadRequest {
		t.Errorf("selection edit with a bare string = %d, want 400", code)
	}
	if stored := fixture.storedSelectedTools(t); stored != `["create_issue"]` {
		t.Errorf("a rejected request changed the stored selection to %s", stored)
	}
}
