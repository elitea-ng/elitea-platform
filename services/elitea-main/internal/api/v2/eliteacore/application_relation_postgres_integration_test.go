package eliteacore_test

// Real-PostgreSQL coverage for the agent-as-tool relation route
// (application_relation.go). Four defect classes from one review round, each
// pinned by reading the rows back rather than trusting the status code:
//
//   - the detach deleted the shared `elitea_tools` ROW, and because publish
//     CLONES mappings reusing the same tool_id while the mapping cascades on
//     tool deletion, detaching a child from a DRAFT silently stripped it from
//     every published clone as well;
//   - an attach body missing `application_id` fell through to the detach
//     branch, deleted the relation, and answered success;
//   - numeric ids travelled through fmt.Sprintf("%v", float64), which renders
//     1234567 as "1.234567e+06" and a missing id as "<nil>" — both refused by
//     Postgres, both surfacing as 500s (the typed body makes those inputs
//     either work or answer 400);
//   - a pylon-migrated tenant holds the same reference in the legacy
//     `application_tools` table, which the duplicate check and the detach
//     must both see (the union read).
//
// The schema comes from the real migration corpus via newPublishCopyPool.

import (
	"bytes"
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

type relationFixture struct {
	parentAppID     int
	parentVersionID int
	childAppID      int
	childVersionID  int
}

func seedRelationFixture(t *testing.T, pool *pgxpool.Pool) relationFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var fixture relationFixture
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.applications (name, description, owner_id) VALUES ('relation parent', 'seeded', 1) RETURNING id`).
		Scan(&fixture.parentAppID); err != nil {
		t.Fatalf("seed parent application: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.application_versions (application_id, name, status, author_id, instructions, agent_type)
VALUES ($1, 'latest', 'draft', 1, 'delegate', 'agent') RETURNING id`, fixture.parentAppID).
		Scan(&fixture.parentVersionID); err != nil {
		t.Fatalf("seed parent version: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.applications (name, description, owner_id) VALUES ('relation child', 'seeded', 1) RETURNING id`).
		Scan(&fixture.childAppID); err != nil {
		t.Fatalf("seed child application: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.application_versions (application_id, name, status, author_id, instructions, agent_type)
VALUES ($1, 'latest', 'draft', 1, 'do the child thing', 'agent') RETURNING id`, fixture.childAppID).
		Scan(&fixture.childVersionID); err != nil {
		t.Fatalf("seed child version: %v", err)
	}
	return fixture
}

func relationRouter(handler *eliteacore.Handler) chi.Router {
	router := chi.NewRouter()
	router.Patch("/elitea_core/application_relation/prompt_lib/{projectID}/{appID}/{versionID}", handler.UpdateApplicationRelation)
	return router
}

func relationDo(t *testing.T, router chi.Router, fixture relationFixture, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	target := fmt.Sprintf("/elitea_core/application_relation/prompt_lib/1/%d/%d",
		fixture.childAppID, fixture.childVersionID)
	request := httptest.NewRequest(http.MethodPatch, target, bytes.NewReader(encoded))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func countRelationRows(t *testing.T, pool *pgxpool.Pool, parentVersionID int) (mappings int, tools int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := pool.QueryRow(ctx, `
SELECT COUNT(*) FROM p_1.entity_tool_mapping AS mapping
JOIN p_1.elitea_tools AS tool ON tool.id = mapping.tool_id
WHERE mapping.entity_version_id = $1 AND mapping.entity_type = 'agent' AND tool.type = 'application'`,
		parentVersionID).Scan(&mappings); err != nil {
		t.Fatalf("count mappings: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM p_1.elitea_tools WHERE type = 'application'`).Scan(&tools); err != nil {
		t.Fatalf("count tool rows: %v", err)
	}
	return mappings, tools
}

func TestApplicationRelationAttachWritesTheRealPairAndRefusesADuplicate(t *testing.T) {
	pool := newPublishCopyPool(t)
	fixture := seedRelationFixture(t, pool)
	router := relationRouter(eliteacore.NewHandler(pool))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	attach := map[string]any{
		"application_id": fixture.parentAppID,
		"version_id":     fixture.parentVersionID,
		"has_relation":   true,
	}
	if recorder := relationDo(t, router, fixture, attach); recorder.Code != http.StatusCreated {
		t.Fatalf("attach status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// The settings pair must come back as NUMBERS: the freeze
	// (freezeCurrentStoredApplicationReference) refuses strings there.
	var settingsRaw []byte
	if err := pool.QueryRow(ctx, `
SELECT tool.settings FROM p_1.entity_tool_mapping AS mapping
JOIN p_1.elitea_tools AS tool ON tool.id = mapping.tool_id
WHERE mapping.entity_version_id = $1 AND mapping.entity_type = 'agent' AND tool.type = 'application'`,
		fixture.parentVersionID).Scan(&settingsRaw); err != nil {
		t.Fatalf("read back the attached relation: %v", err)
	}
	var settings struct {
		ApplicationID        int `json:"application_id"`
		ApplicationVersionID int `json:"application_version_id"`
	}
	if err := json.Unmarshal(settingsRaw, &settings); err != nil {
		t.Fatalf("settings %s do not carry the numeric pair: %v", settingsRaw, err)
	}
	if settings.ApplicationID != fixture.childAppID || settings.ApplicationVersionID != fixture.childVersionID {
		t.Fatalf("settings = %s, want the child pair (%d, %d)", settingsRaw, fixture.childAppID, fixture.childVersionID)
	}

	if recorder := relationDo(t, router, fixture, attach); recorder.Code != http.StatusBadRequest {
		t.Fatalf("duplicate attach status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestApplicationRelationAttachWithoutApplicationIDIsRefusedNotTreatedAsDetach(t *testing.T) {
	pool := newPublishCopyPool(t)
	fixture := seedRelationFixture(t, pool)
	router := relationRouter(eliteacore.NewHandler(pool))

	if recorder := relationDo(t, router, fixture, map[string]any{
		"application_id": fixture.parentAppID,
		"version_id":     fixture.parentVersionID,
		"has_relation":   true,
	}); recorder.Code != http.StatusCreated {
		t.Fatalf("seed attach status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// The old guard sent this body down the DETACH branch, which deleted the
	// relation and answered success.
	recorder := relationDo(t, router, fixture, map[string]any{
		"version_id":   fixture.parentVersionID,
		"has_relation": true,
	})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("incomplete attach status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
	}
	if mappings, _ := countRelationRows(t, pool, fixture.parentVersionID); mappings != 1 {
		t.Fatalf("the refused attach removed the existing relation: %d mappings left, want 1", mappings)
	}
}

func TestApplicationRelationDetachKeepsThePublishedClone(t *testing.T) {
	pool := newPublishCopyPool(t)
	fixture := seedRelationFixture(t, pool)
	router := relationRouter(eliteacore.NewHandler(pool))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if recorder := relationDo(t, router, fixture, map[string]any{
		"application_id": fixture.parentAppID,
		"version_id":     fixture.parentVersionID,
		"has_relation":   true,
	}); recorder.Code != http.StatusCreated {
		t.Fatalf("attach status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// The clone publish performs: a second version whose mapping reuses the
	// SAME tool_id (handler.go, the entity_tool_mapping copy in Publish).
	var publishedVersionID int
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.application_versions (application_id, name, status, author_id, instructions, agent_type)
VALUES ($1, 'v-one', 'published', 1, 'delegate', 'agent') RETURNING id`, fixture.parentAppID).
		Scan(&publishedVersionID); err != nil {
		t.Fatalf("seed published version: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.entity_tool_mapping (entity_version_id, entity_id, entity_type, tool_id, selected_tools)
SELECT $2, entity_id, entity_type, tool_id, selected_tools
FROM p_1.entity_tool_mapping WHERE entity_version_id = $1`,
		fixture.parentVersionID, publishedVersionID); err != nil {
		t.Fatalf("clone mapping onto the published version: %v", err)
	}

	if recorder := relationDo(t, router, fixture, map[string]any{
		"application_id": fixture.parentAppID,
		"version_id":     fixture.parentVersionID,
		"has_relation":   false,
	}); recorder.Code != http.StatusCreated {
		t.Fatalf("detach status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	draftMappings, toolRows := countRelationRows(t, pool, fixture.parentVersionID)
	if draftMappings != 0 {
		t.Fatalf("detach left %d draft mappings, want 0", draftMappings)
	}
	publishedMappings, _ := countRelationRows(t, pool, publishedVersionID)
	if publishedMappings != 1 {
		t.Fatalf("detaching from the DRAFT stripped the published clone: %d mappings, want 1", publishedMappings)
	}
	if toolRows != 1 {
		t.Fatalf("the shared tool row must survive while the published clone references it: %d rows, want 1", toolRows)
	}

	// A detach of a relation that does not exist answers 404 now — the old
	// zero-row detach answered success and the row reappeared on reload.
	if recorder := relationDo(t, router, fixture, map[string]any{
		"application_id": fixture.parentAppID,
		"version_id":     fixture.parentVersionID,
		"has_relation":   false,
	}); recorder.Code != http.StatusNotFound {
		t.Fatalf("second detach status = %d, want 404; body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestApplicationRelationSeesAndClearsTheLegacyTable(t *testing.T) {
	pool := newPublishCopyPool(t)
	fixture := seedRelationFixture(t, pool)
	router := relationRouter(eliteacore.NewHandler(pool))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// The pylon table, in the live legacy shape (see
	// embed_skill_copy_postgres_integration_test.go's createLegacyApplicationTools),
	// holding the same reference under the legacy settings key `version_id`.
	if _, err := pool.Exec(ctx, `
CREATE TABLE p_1.application_tools (
    id SERIAL PRIMARY KEY,
    application_version_id INTEGER NOT NULL REFERENCES p_1.application_versions(id),
    created_at TIMESTAMP NOT NULL DEFAULT now(),
    updated_at TIMESTAMP,
    type VARCHAR NOT NULL,
    name VARCHAR(128) NOT NULL,
    description VARCHAR(1024),
    settings JSONB NOT NULL
)`); err != nil {
		t.Fatalf("create the legacy application_tools table: %v", err)
	}
	legacySettings, err := json.Marshal(map[string]any{
		"application_id": fmt.Sprintf("%d", fixture.childAppID),
		"version_id":     fmt.Sprintf("%d", fixture.childVersionID),
	})
	if err != nil {
		t.Fatalf("marshal legacy settings: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.application_tools (application_version_id, name, type, settings)
VALUES ($1, 'relation child', 'application', $2)`, fixture.parentVersionID, legacySettings); err != nil {
		t.Fatalf("seed the legacy reference: %v", err)
	}

	// The duplicate check must see the legacy row.
	if recorder := relationDo(t, router, fixture, map[string]any{
		"application_id": fixture.parentAppID,
		"version_id":     fixture.parentVersionID,
		"has_relation":   true,
	}); recorder.Code != http.StatusBadRequest {
		t.Fatalf("attach over a legacy row status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
	}

	// The detach must clear it, or the union read resurrects the relation.
	if recorder := relationDo(t, router, fixture, map[string]any{
		"application_id": fixture.parentAppID,
		"version_id":     fixture.parentVersionID,
		"has_relation":   false,
	}); recorder.Code != http.StatusCreated {
		t.Fatalf("legacy detach status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var legacyLeft int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM p_1.application_tools WHERE application_version_id = $1`,
		fixture.parentVersionID).Scan(&legacyLeft); err != nil {
		t.Fatalf("count legacy rows: %v", err)
	}
	if legacyLeft != 0 {
		t.Fatalf("the legacy reference survived the detach: %d rows left", legacyLeft)
	}
}

// TestApplicationRelationAttachAcceptsNumericStringIDs pins the legacy body
// form. The contract and pylon accept "42" as well as 42 for both ids; the
// typed *int64 body refused the string form with 400.
func TestApplicationRelationAttachAcceptsNumericStringIDs(t *testing.T) {
	pool := newPublishCopyPool(t)
	fixture := seedRelationFixture(t, pool)
	router := relationRouter(eliteacore.NewHandler(pool))

	attach := map[string]any{
		"application_id": fmt.Sprintf("%d", fixture.parentAppID),
		"version_id":     fmt.Sprintf("%d", fixture.parentVersionID),
		"has_relation":   true,
	}
	if recorder := relationDo(t, router, fixture, attach); recorder.Code != http.StatusCreated {
		t.Fatalf("string-id attach status = %d, want 201; body = %s", recorder.Code, recorder.Body.String())
	}
	if mappings, _ := countRelationRows(t, pool, fixture.parentVersionID); mappings != 1 {
		t.Fatalf("mappings after string-id attach = %d, want 1", mappings)
	}

	detach := map[string]any{
		"application_id": fmt.Sprintf("%d", fixture.parentAppID),
		"version_id":     fmt.Sprintf("%d", fixture.parentVersionID),
		"has_relation":   false,
	}
	if recorder := relationDo(t, router, fixture, detach); recorder.Code >= 300 {
		t.Fatalf("string-id detach status = %d; body = %s", recorder.Code, recorder.Body.String())
	}
	if mappings, _ := countRelationRows(t, pool, fixture.parentVersionID); mappings != 0 {
		t.Fatalf("mappings after string-id detach = %d, want 0", mappings)
	}
}

// TestApplicationRelationRefusesAVersionOfAnotherApplication pins the
// application_id check. The parent row was locked by version_id alone, so a
// body with a wrong application_id edited a version of another agent.
func TestApplicationRelationRefusesAVersionOfAnotherApplication(t *testing.T) {
	pool := newPublishCopyPool(t)
	fixture := seedRelationFixture(t, pool)
	router := relationRouter(eliteacore.NewHandler(pool))

	if recorder := relationDo(t, router, fixture, map[string]any{
		"application_id": fixture.parentAppID,
		"version_id":     fixture.parentVersionID,
		"has_relation":   true,
	}); recorder.Code != http.StatusCreated {
		t.Fatalf("seed attach status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	// The child application does not own the parent version.
	for _, hasRelation := range []bool{false, true} {
		recorder := relationDo(t, router, fixture, map[string]any{
			"application_id": fixture.childAppID,
			"version_id":     fixture.parentVersionID,
			"has_relation":   hasRelation,
		})
		if recorder.Code != http.StatusBadRequest ||
			!strings.Contains(recorder.Body.String(), "does not belong to application_id") {
			t.Fatalf("mismatched application_id (has_relation=%v) = %d %s, want 400",
				hasRelation, recorder.Code, recorder.Body.String())
		}
	}
	if mappings, _ := countRelationRows(t, pool, fixture.parentVersionID); mappings != 1 {
		t.Fatalf("a refused mismatched body changed the relation: %d mappings, want 1", mappings)
	}
}
