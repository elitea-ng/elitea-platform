package eliteacore_test

// What a WITHDRAWAL is allowed to delete.
//
// Publishing a parent leaves the published version holding TWO application
// references for every sub-agent, not one:
//
//   - Publish copies the source version's `entity_tool_mapping` rows onto the
//     clone verbatim, which carries the AUTHOR's own reference across;
//   - `embedSubAgents` then creates the embedded snapshot and links it beside
//     that copy.
//
// `deleteEmbeddedSubAgents` reads both back through one list and used to hard
// delete the application behind each of them. So withdrawing a parent removed
// the author's own sub-agent — the application row, every version of it, the
// draft they were still editing — and answered 200. The agent was simply gone
// from the project, and nothing said so.
//
// The assertions below therefore read the APPLICATION rows back out of the
// database. The withdrawal's own 200 is exactly what the defect produced.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
)

// cascadeInstructions is long enough to clear the pre-publish check's
// fifty-character rule, so the publish under test fails for its own reasons or
// not at all.
const cascadeInstructions = "You are a release notes assistant. Turn the commits the caller gives you into a summary."

type cascadeAgent struct {
	applicationID int
	versionID     int
}

func cascadeRouter(pool *pgxpool.Pool) chi.Router {
	handler := eliteacore.NewHandler(pool)
	router := chi.NewRouter()
	router.Post("/elitea_core/publish/prompt_lib/{projectID}/{versionID}", handler.Publish)
	router.Post("/elitea_core/unpublish/prompt_lib/{projectID}/{versionID}", handler.Unpublish)
	router.Patch("/elitea_core/application_relation/prompt_lib/{projectID}/{appID}/{versionID}",
		handler.UpdateApplicationRelation)
	return router
}

func seedCascadeAgent(t *testing.T, pool *pgxpool.Pool, name string) cascadeAgent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var agent cascadeAgent
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.applications (name, description, owner_id) VALUES ($1, 'seeded', 1) RETURNING id`,
		name).Scan(&agent.applicationID); err != nil {
		t.Fatalf("seed application %q: %v", name, err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.application_versions (application_id, name, status, author_id, instructions, agent_type)
VALUES ($1, 'base', 'draft', 1, $2, 'agent') RETURNING id`,
		agent.applicationID, cascadeInstructions).Scan(&agent.versionID); err != nil {
		t.Fatalf("seed version for %q: %v", name, err)
	}
	return agent
}

// attachCascadeChild links child as a sub-agent of parentVersionID through the
// route that writes the reference, so the fixture is the shape production makes.
func attachCascadeChild(t *testing.T, router chi.Router, parentVersionID int, child cascadeAgent) {
	t.Helper()
	body := fmt.Sprintf(`{"application_id":%d,"version_id":%d,"has_relation":true}`,
		child.applicationID, parentVersionID)
	target := fmt.Sprintf("/elitea_core/application_relation/prompt_lib/1/%d/%d",
		child.applicationID, child.versionID)
	request := httptest.NewRequest(http.MethodPatch, target, jsonBodyReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("attach the sub-agent: got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestWithdrawingAParentKeepsTheAuthorsOwnSubAgent(t *testing.T) {
	pool := newPublishCopyPool(t)
	router := cascadeRouter(pool)
	parent := seedCascadeAgent(t, pool, "the parent")
	child := seedCascadeAgent(t, pool, "the sub-agent")
	attachCascadeChild(t, router, parent.versionID, child)

	recorder := publishCopyDo(t, router, parent.versionID, map[string]any{
		"version_name":     "v-one",
		"validation_token": publishCopyToken(t, pool, parent.versionID),
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("publish: got %d: %s", recorder.Code, recorder.Body.String())
	}
	var published struct {
		PublicVersionID string `json:"public_version_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &published); err != nil {
		t.Fatalf("decode the publish response %q: %v", recorder.Body.String(), err)
	}
	var cloneID int
	if _, err := fmt.Sscanf(published.PublicVersionID, "%d", &cloneID); err != nil {
		t.Fatalf("the publish named no cloned version: %q", published.PublicVersionID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// The embed really happened: an application other than the two seeded ones
	// now holds an `embedded` version. Without this the withdrawal below would
	// have nothing to delete and would pass for the wrong reason.
	var embeddedApplicationID int
	if err := pool.QueryRow(ctx, `
SELECT application_id FROM p_1.application_versions WHERE status = 'embedded'`).Scan(&embeddedApplicationID); err != nil {
		t.Fatalf("the publish embedded no sub-agent copy: %v", err)
	}
	if embeddedApplicationID == child.applicationID {
		t.Fatalf("the embed reused the author's own application %d", child.applicationID)
	}

	withdrawn := cascadeUnpublish(t, router, cloneID)
	if withdrawn.Code != http.StatusOK {
		t.Fatalf("unpublish: got %d: %s", withdrawn.Code, withdrawn.Body.String())
	}

	// The author's sub-agent survives, with the draft they were editing.
	var childApplications, childVersions int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM p_1.applications WHERE id = $1`, child.applicationID).Scan(&childApplications); err != nil {
		t.Fatalf("read the author's sub-agent: %v", err)
	}
	if childApplications != 1 {
		t.Fatalf("the withdrawal deleted the author's own sub-agent (application %d)", child.applicationID)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM p_1.application_versions WHERE id = $1 AND status = 'draft'`,
		child.versionID).Scan(&childVersions); err != nil {
		t.Fatalf("read the author's sub-agent draft: %v", err)
	}
	if childVersions != 1 {
		t.Errorf("the withdrawal took the sub-agent's draft version with it")
	}

	// The embedded snapshot goes, which is what the withdrawal is FOR.
	var embeddedLeft int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM p_1.applications WHERE id = $1`, embeddedApplicationID).Scan(&embeddedLeft); err != nil {
		t.Fatalf("read the embedded copy: %v", err)
	}
	if embeddedLeft != 0 {
		t.Errorf("the embedded sub-agent copy survived the withdrawal")
	}

	// …and the author's own draft parent still offers the sub-agent it always
	// did. A cleanup that reached the wrong version would take this too.
	var parentReferences int
	if err := pool.QueryRow(ctx, `
SELECT count(*)
FROM p_1.entity_tool_mapping AS mapping
JOIN p_1.elitea_tools AS tool ON tool.id = mapping.tool_id
WHERE mapping.entity_version_id = $1
  AND tool.type = 'application'
  AND tool.settings->>'application_id' = $2`,
		parent.versionID, fmt.Sprint(child.applicationID)).Scan(&parentReferences); err != nil {
		t.Fatalf("read the parent draft's references: %v", err)
	}
	if parentReferences != 1 {
		t.Errorf("the withdrawal detached the sub-agent from the author's own draft")
	}
}

func cascadeUnpublish(t *testing.T, router chi.Router, versionID int) *httptest.ResponseRecorder {
	t.Helper()
	target := fmt.Sprintf("/elitea_core/unpublish/prompt_lib/1/%d", versionID)
	request := httptest.NewRequest(http.MethodPost, target, jsonBodyReader(`{}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// jsonBodyReader wraps a literal body for httptest.NewRequest.
func jsonBodyReader(body string) *bytes.Reader {
	return bytes.NewReader([]byte(body))
}
