package eliteacore_test

// What an unpublish is allowed to delete.
//
// A published version carries TWO sub-agent references for every sub-agent its
// draft had: the mapping the publish copied, which still names the AUTHOR'S OWN
// child agent, and the link to the private copy `embedSubAgents` then made.
// `deleteEmbeddedSubAgents` walked both and deleted every application either of
// them named — so unpublishing a parent (or deleting it, which runs the same
// cleanup) destroyed the child agent the author still had in their list.
//
// Nothing reported it. Those deletes are best-effort by design, the route still
// answered 200, and the author's next read of that agent was a plain 404. The
// E2E journey that found it did not fail on an assertion either: both cases
// passed and then their own teardown could not find the child any more.
//
// AGAINST A REAL DATABASE, through the same corpus-built pool the embed tests
// use: the fact under test is which rows survive one route, and a hand-cut
// fixture that let `application_versions.status` be anything would not
// reproduce a defect that turns on that column's value.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// subAgentFixture is a parent agent whose DRAFT references one sub-agent
// through the pair of tables the attach route writes
// (`elitea_tools` + `entity_tool_mapping`, application_relation.go).
//
// That pairing is what makes this fixture reproduce the defect, and it is why
// it does not reuse the embed tests' own fixture: theirs seeds the legacy
// `application_tools` table instead, whose rows the publish does not copy onto
// the clone. With only the embed's own link on the published version there is
// no second reference to mistake for a copy, and the deletion below has nothing
// to get wrong.
type subAgentFixture struct {
	parentAppID     int
	parentVersionID int
	subAppID        int
	subVersionID    int
}

func seedSubAgentFixture(t *testing.T, pool *pgxpool.Pool) subAgentFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var fixture subAgentFixture
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.applications (name, description, owner_id) VALUES ('survival sub-agent', 'seeded', 1) RETURNING id`).
		Scan(&fixture.subAppID); err != nil {
		t.Fatalf("seed sub-agent application: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.application_versions (application_id, name, status, author_id, instructions, agent_type)
VALUES ($1, 'latest', 'draft', 1, 'do the sub thing', 'agent') RETURNING id`, fixture.subAppID).
		Scan(&fixture.subVersionID); err != nil {
		t.Fatalf("seed sub-agent version: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.applications (name, description, owner_id) VALUES ('survival parent agent', 'seeded', 1) RETURNING id`).
		Scan(&fixture.parentAppID); err != nil {
		t.Fatalf("seed parent application: %v", err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.application_versions (application_id, name, status, author_id, instructions, agent_type)
VALUES ($1, 'latest', 'draft', 1, 'delegate the thing', 'agent') RETURNING id`, fixture.parentAppID).
		Scan(&fixture.parentVersionID); err != nil {
		t.Fatalf("seed parent version: %v", err)
	}

	// The attach, in the shape `insertApplicationToolReference` writes it: a
	// tool row of type `application` whose settings name the child, and a
	// mapping from the parent's version to it.
	settings, err := json.Marshal(map[string]any{
		"application_id":         strconv.Itoa(fixture.subAppID),
		"application_version_id": strconv.Itoa(fixture.subVersionID),
	})
	if err != nil {
		t.Fatalf("marshal sub-agent tool settings: %v", err)
	}
	var toolID int
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.elitea_tools (name, type, description, settings, meta, owner_id, author_id)
VALUES ('survival sub-agent', 'application', '', $1, '{}'::jsonb, 1, 1) RETURNING id`, settings).
		Scan(&toolID); err != nil {
		t.Fatalf("seed sub-agent tool row: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.entity_tool_mapping (entity_version_id, entity_id, entity_type, tool_id)
VALUES ($1, $2, 'agent', $3)`, fixture.parentVersionID, fixture.parentAppID, toolID); err != nil {
		t.Fatalf("seed sub-agent mapping: %v", err)
	}
	return fixture
}

// unpublishRouter mounts the publish/unpublish pair on the paths the real
// router gives them (internal/api/router.go).
func unpublishRouter(handler *eliteacore.Handler) chi.Router {
	router := chi.NewRouter()
	router.Post("/elitea_core/publish/prompt_lib/{projectID}/{versionID}", handler.Publish)
	router.Post("/elitea_core/unpublish/prompt_lib/{projectID}/{versionID}", handler.Unpublish)
	return router
}

func unpublishDo(t *testing.T, router chi.Router, versionID int) *httptest.ResponseRecorder {
	t.Helper()
	target := fmt.Sprintf("/elitea_core/unpublish/prompt_lib/1/%d", versionID)
	request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader([]byte(`{}`)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// applicationExists reports whether an application row is still there.
func applicationExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, applicationID int) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM p_1.applications WHERE id = $1)`, applicationID).Scan(&exists); err != nil {
		t.Fatalf("read application %d: %v", applicationID, err)
	}
	return exists
}

// versionExists reports whether a version row is still there.
func versionExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, versionID int) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM p_1.application_versions WHERE id = $1)`, versionID).Scan(&exists); err != nil {
		t.Fatalf("read version %d: %v", versionID, err)
	}
	return exists
}

// TestUnpublishRemovesTheEmbeddedCopyAndKeepsTheAuthorsSubAgent is the
// acceptance test for the defect: after publishing a parent that references a
// sub-agent and then unpublishing it, the private embedded copy is gone and the
// author's own sub-agent is untouched.
func TestUnpublishRemovesTheEmbeddedCopyAndKeepsTheAuthorsSubAgent(t *testing.T) {
	pool := newPublishCopyPool(t)
	fixture := seedSubAgentFixture(t, pool)
	router := unpublishRouter(eliteacore.NewHandler(pool))

	published := publishCopyDo(t, router, fixture.parentVersionID, map[string]any{
		"version_name":     "v-one",
		"validation_token": publishCopyToken(t, pool, fixture.parentVersionID),
	})
	if published.Code != http.StatusOK {
		t.Fatalf("publish status = %d, body = %s", published.Code, published.Body.String())
	}
	var answer struct {
		PublicVersionID string `json:"public_version_id"`
	}
	if err := json.Unmarshal(published.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the publish answer: %v", err)
	}
	var publishedVersionID int
	if _, err := fmt.Sscanf(answer.PublicVersionID, "%d", &publishedVersionID); err != nil {
		t.Fatalf("the publish answer names no published version: %s", published.Body.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// The copy the publish made, and the application it belongs to.
	embeddedVersionID := embeddedVersionIDOf(t, ctx, pool, fixture.subVersionID)
	var embeddedAppID int
	if err := pool.QueryRow(ctx,
		`SELECT application_id FROM p_1.application_versions WHERE id = $1`, embeddedVersionID).
		Scan(&embeddedAppID); err != nil {
		t.Fatalf("read the embedded copy's application: %v", err)
	}
	// The precondition that makes the assertions below mean something: the copy
	// is a DIFFERENT application from the author's sub-agent. If a future embed
	// stopped cloning the application row, deleting "the embedded copy" would be
	// deleting the author's agent and both assertions would be about one row.
	if embeddedAppID == fixture.subAppID {
		t.Fatalf("the embedded copy shares application %d with the author's sub-agent", embeddedAppID)
	}

	unpublished := unpublishDo(t, router, publishedVersionID)
	if unpublished.Code != http.StatusOK {
		t.Fatalf("unpublish status = %d, body = %s", unpublished.Code, unpublished.Body.String())
	}

	// The copy is gone. It exists only to serve the published version, and a
	// withdrawn publication has nothing to serve.
	if applicationExists(t, ctx, pool, embeddedAppID) {
		t.Errorf("the embedded copy's application %d survived the unpublish", embeddedAppID)
	}

	// THE DEFECT. The author's own sub-agent, and the draft version they keep
	// editing, must both still be there.
	if !applicationExists(t, ctx, pool, fixture.subAppID) {
		t.Errorf("the unpublish deleted the author's own sub-agent application %d", fixture.subAppID)
	}
	if !versionExists(t, ctx, pool, fixture.subVersionID) {
		t.Errorf("the unpublish deleted the author's own sub-agent version %d", fixture.subVersionID)
	}
	// …and so must the parent the author published from.
	if !applicationExists(t, ctx, pool, fixture.parentAppID) {
		t.Errorf("the unpublish deleted the parent application %d", fixture.parentAppID)
	}
	if !versionExists(t, ctx, pool, fixture.parentVersionID) {
		t.Errorf("the unpublish deleted the parent's draft version %d", fixture.parentVersionID)
	}
}
