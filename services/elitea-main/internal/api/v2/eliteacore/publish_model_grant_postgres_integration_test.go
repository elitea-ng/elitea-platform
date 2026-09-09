package eliteacore_test

// The publish rule for a RESTRICTED platform model, against a real PostgreSQL.
//
// ## The rule
//
// Publishing already refuses a version whose model belongs to a project other
// than the catalogue's. The grant scope splits the catalogue's own models in
// two: a platform model is offered to every project, to none, or to a chosen
// set. A published agent is read by every project that opens the catalogue, so
// it may only name a model granted to ALL of them — otherwise the agent is
// listed for everybody and answers `model_not_found` for everybody but the
// projects on the list, and that failure lands on readers who cannot fix it.
//
// ## Why a database
//
// The scope is a field of a row in another schema. The publish body does not
// carry it and no fake can stand in for it: what is under test is that the
// guard reads the catalogue's row for the model this version names, and that
// it reads the right one — the model name in `llm_settings` is matched against
// both the row title and `data.name`, because the two routes that serve the
// catalogue disagree about which of them a version carries.
//
// Every refusal below also asserts that NOTHING was published. A guard that
// refused after the clone answers the same 400 and leaves a catalogue entry.
//
// The fixtures, the pool and the router come from
// catalog_mirror_postgres_integration_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedCataloguePlatformModel creates the catalogue's `configuration` table (if
// this harness has none) and writes one platform model row with a grant.
//
// The table comes from the canonical projection rather than from a shape
// restated here: it is owned by the platform migrations this service does not
// run, and a second transcription of it would drift from the column set the
// production query reads.
func seedCataloguePlatformModel(
	t *testing.T, pool *pgxpool.Pool, title, modelName, grant string,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	baseline, err := os.ReadFile(filepath.Join(
		"..", "..", "..", "db", "schema", "configuration_baseline.sql"))
	if err != nil {
		t.Fatalf("read configuration_baseline.sql: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"SET LOCAL search_path TO p_1; "+
			strings.Replace(string(baseline), "CREATE TABLE configuration",
				"CREATE TABLE IF NOT EXISTS configuration", 1)); err != nil {
		t.Fatalf("apply the configuration projection into p_1: %v", err)
	}

	data := fmt.Sprintf(`{"name":%q,"ai_credentials":{"elitea_title":"autotest_provider"}%s}`,
		modelName, grant)
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.configuration
    (uuid, project_id, label, elitea_title, type, section, data, meta, shared, status_ok, source)
VALUES (gen_random_uuid(), 1, 'Autotest platform model', $1, 'llm_model', 'llm',
        $2::jsonb, '{}'::jsonb, true, true, 'test')`, title, data); err != nil {
		t.Fatalf("seed the catalogue platform model: %v", err)
	}
}

// TestPublishRefusesAModelThatIsNotGrantedToEveryProject.
//
// The model IS the catalogue's, so the existing owning-project check admits it.
// What refuses it is the grant, and the message has to say so: the author is
// looking at a model the picker offered them, and "llm_not_shared" with no
// words reads as a defect rather than as a choice somebody made.
func TestPublishRefusesAModelThatIsNotGrantedToEveryProject(t *testing.T) {
	for name, grant := range map[string]string{
		"granted to selected projects": `,"share_scope":"projects","shared_with":[2]`,
		"granted to no project":        `,"share_scope":"none"`,
	} {
		t.Run(name, func(t *testing.T) {
			pool := newCatalogMirrorPool(t)
			router := catalogMirrorRouter(eliteacore.NewHandler(pool))
			seedCataloguePlatformModel(t, pool,
				"autotest_restricted_model", "autotest-restricted", grant)
			fixture := seedCatalogMirrorFixture(t, pool, 2, "agent on a restricted platform model")
			setPublishFixtureModel(t, pool, fixture, "autotest-restricted", 1)

			recorder := catalogMirrorPublish(t, router, fixture, map[string]any{
				"version_name": "v-one",
				// The approval token is sent, so the quality gate cannot be
				// what refuses this: the model check runs BEFORE it.
				"validation_token": catalogMirrorToken(t, pool, fixture),
			})
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("publish status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
			}
			var response map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode publish refusal: %v", err)
			}
			// The same CODE as the owning-project refusal, because it is the
			// same finding and the publish dialog already renders it beside the
			// model picker. The `msg` is what distinguishes them.
			if response["error"] != "llm_not_shared" {
				t.Errorf("refusal = %v, want llm_not_shared", response["error"])
			}
			message, _ := response["msg"].(string)
			if !strings.Contains(message, "autotest-restricted") ||
				!strings.Contains(message, "all projects") {
				t.Errorf("msg = %q, want it to name the model and what to change", message)
			}

			// Nothing was cloned, and nothing reached the catalogue.
			if names := publishedVersionNames(t, pool, fixture); len(names) != 0 {
				t.Errorf("the refusal published %v", names)
			}
			if rows := catalogMirrorPublicApplications(t, router); len(rows) != 0 {
				t.Errorf("the refusal reached the catalogue: %v", rows)
			}
		})
	}
}

// TestPublishAdmitsAModelGrantedToEveryProject is the other half, and it is the
// one that makes the refusal meaningful: a guard that refused every catalogue
// model would pass the test above and publish nothing at all.
//
// Both spellings are covered. `share_scope: "all"` is the explicit choice; a
// row with NO scope is every model written before this feature, and it must go
// on publishing exactly as it did.
func TestPublishAdmitsAModelGrantedToEveryProject(t *testing.T) {
	for name, grant := range map[string]string{
		"the explicit all scope": `,"share_scope":"all"`,
		"a row with no scope":    ``,
	} {
		t.Run(name, func(t *testing.T) {
			pool := newCatalogMirrorPool(t)
			router := catalogMirrorRouter(eliteacore.NewHandler(pool))
			seedCataloguePlatformModel(t, pool,
				"autotest_open_model", "autotest-open", grant)
			fixture := seedCatalogMirrorFixture(t, pool, 2, "agent on an open platform model")
			setPublishFixtureModel(t, pool, fixture, "autotest-open", 1)

			recorder := catalogMirrorPublish(t, router, fixture, map[string]any{
				"version_name":     "v-one",
				"validation_token": catalogMirrorToken(t, pool, fixture),
			})
			if recorder.Code != http.StatusOK {
				t.Fatalf("publish status = %d, want 200; body = %s",
					recorder.Code, recorder.Body.String())
			}
		})
	}
}

// TestPublishFindsTheCatalogueRowByItsTitleToo.
//
// A version's `llm_settings.model_name` carries the row TITLE on one of the two
// routes that serve the catalogue and `data.name` on the other. A guard that
// matched one of them alone would find the row on one deployment and silently
// admit the publish on the other — the absence-reads-as-correctness shape.
func TestPublishFindsTheCatalogueRowByItsTitleToo(t *testing.T) {
	pool := newCatalogMirrorPool(t)
	router := catalogMirrorRouter(eliteacore.NewHandler(pool))
	seedCataloguePlatformModel(t, pool,
		"autotest_titled_model", "autotest-wire-name", `,"share_scope":"none"`)
	fixture := seedCatalogMirrorFixture(t, pool, 2, "agent naming the row title")
	// The TITLE, not the wire name.
	setPublishFixtureModel(t, pool, fixture, "autotest_titled_model", 1)

	recorder := catalogMirrorPublish(t, router, fixture, map[string]any{
		"version_name":     "v-one",
		"validation_token": catalogMirrorToken(t, pool, fixture),
	})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("publish status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
	}
}

// TestAModelThisGuardCannotFindDoesNotBlockAPublish pins the DECISION that a
// missing row is not a refusal.
//
// A version can name a model this lookup cannot reconstruct. Refusing on
// absence would turn "I could not find the row" into "you may not publish", and
// the enforcement that matters is elsewhere: a project with no grant cannot see
// the model in its catalogue read and cannot dispatch it through the gateway.
func TestAModelThisGuardCannotFindDoesNotBlockAPublish(t *testing.T) {
	pool := newCatalogMirrorPool(t)
	router := catalogMirrorRouter(eliteacore.NewHandler(pool))
	seedCataloguePlatformModel(t, pool,
		"autotest_other_model", "autotest-other", `,"share_scope":"none"`)
	fixture := seedCatalogMirrorFixture(t, pool, 2, "agent on a model with no catalogue row")
	setPublishFixtureModel(t, pool, fixture, "autotest-not-in-the-catalogue", 1)

	recorder := catalogMirrorPublish(t, router, fixture, map[string]any{
		"version_name":     "v-one",
		"validation_token": catalogMirrorToken(t, pool, fixture),
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("publish status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
}
