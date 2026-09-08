package eliteacore_test

// Real-PostgreSQL coverage for the three publish rules that decide WHETHER a
// version may be published at all, as opposed to what happens once it is:
//
//   - the model a published agent names must belong to the public project;
//   - the pre-publish quality gate has no exemption for the public project,
//     so a moderator publishing in place is checked like everybody else;
//   - a withdrawn release name is FREE again, because the withdrawal renames
//     the clone it reverts to a draft.
//
// The first was gated in handler.go with no test of any kind, so `llm_not_shared`
// was reachable only by reading the source. The second is a place where this
// platform answers differently from the one it replaces — the reference let a
// publish issued from inside the public project skip the check — and is pinned
// here so a future change to it is a deliberate one. The third is the fix for
// issue 854: a name spent for good by a withdrawal was this platform's own
// defect, not a decision, and the tests below now pin the recovery.
//
// The fixtures, the pool and the router come from
// catalog_mirror_postgres_integration_test.go: the same two tenants (p_1, the
// public project, and p_2, an ordinary user project) and the same production
// migration chain.

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

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// setPublishFixtureModel points a fixture's version at one model in one project,
// which is the only part of `llm_settings` the publish guard reads.
func setPublishFixtureModel(
	t *testing.T, pool *pgxpool.Pool, fixture catalogMirrorFixture, modelName string, modelProjectID int,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	settings, err := json.Marshal(map[string]any{
		"model_name":       modelName,
		"model_project_id": modelProjectID,
	})
	if err != nil {
		t.Fatalf("marshal llm_settings: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`UPDATE p_%d.application_versions SET llm_settings = $2::jsonb WHERE id = $1`, fixture.projectID),
		fixture.versionID, string(settings)); err != nil {
		t.Fatalf("set llm_settings: %v", err)
	}
}

// publishedVersionNames answers the names of every published version of one
// agent, which is what says whether a refusal had already cloned the row.
func publishedVersionNames(
	t *testing.T, pool *pgxpool.Pool, fixture catalogMirrorFixture,
) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	rows, err := pool.Query(ctx, fmt.Sprintf(
		`SELECT name FROM p_%d.application_versions WHERE application_id = $1 AND status = 'published' ORDER BY name`,
		fixture.projectID), fixture.appID)
	if err != nil {
		t.Fatalf("read published versions: %v", err)
	}
	defer rows.Close()

	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan published version: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read published versions: %v", err)
	}
	return names
}

// TestPublishRefusesAModelFromAnotherProject is the guard's only test.
//
// The model lives in the AUTHOR's project, which is the ordinary shape of a
// project-private model: it is visible to the project that owns it and to
// nobody else. A published agent carrying it would be listed in the catalogue
// and unusable by every reader of that catalogue.
func TestPublishRefusesAModelFromAnotherProject(t *testing.T) {
	pool := newCatalogMirrorPool(t)
	router := catalogMirrorRouter(eliteacore.NewHandler(pool))
	fixture := seedCatalogMirrorFixture(t, pool, 2, "agent on a private model")
	setPublishFixtureModel(t, pool, fixture, "private-model", 2)

	recorder := catalogMirrorPublish(t, router, fixture, map[string]any{
		"version_name": "v-one",
		// The approval token is sent, so the quality gate cannot be what
		// refuses this: the model check runs BEFORE it and must be what
		// answers.
		"validation_token": catalogMirrorToken(t, pool, fixture),
	})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("publish status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
	}

	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode publish refusal: %v", err)
	}
	// The FINDING, not only the status. This refusal is the one the publish
	// dialog renders beside the model picker, and the four other 400s the route
	// can answer belong beside something else.
	if response["error"] != "llm_not_shared" {
		t.Errorf("refusal = %v, want llm_not_shared", response["error"])
	}

	// Nothing was cloned. A guard that refused after the clone answers the same
	// 400 and leaves a published version behind it.
	if names := publishedVersionNames(t, pool, fixture); len(names) != 0 {
		t.Errorf("the refusal published %v", names)
	}
	if rows := catalogMirrorPublicApplications(t, router); len(rows) != 0 {
		t.Errorf("the refusal reached the catalogue: %v", rows)
	}
}

// TestPublishKeepsAPublicProjectModelOnTheCatalogueTwin is the other half: the
// model the guard admits must still be there after the copy.
func TestPublishKeepsAPublicProjectModelOnTheCatalogueTwin(t *testing.T) {
	pool := newCatalogMirrorPool(t)
	router := catalogMirrorRouter(eliteacore.NewHandler(pool))
	fixture := seedCatalogMirrorFixture(t, pool, 2, "agent on a shared model")
	setPublishFixtureModel(t, pool, fixture, "shared-model", 1)

	recorder := catalogMirrorPublish(t, router, fixture, map[string]any{
		"version_name":     "v-one",
		"validation_token": catalogMirrorToken(t, pool, fixture),
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("publish status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode publish response: %v", err)
	}
	twinVersionID, present := response["catalog_version_id"]
	if !present {
		t.Fatalf("no catalogue twin in %v", response)
	}

	// Read the TWIN, in the public schema. The author's own clone keeping the
	// model proves nothing about the copy the catalogue serves, and the twin is
	// the only row a reader of ELITEA Catalog can open.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var settings string
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(llm_settings::text, '{}') FROM p_1.application_versions WHERE id = $1`,
		twinVersionID).Scan(&settings); err != nil {
		t.Fatalf("read the twin: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(settings), &decoded); err != nil {
		t.Fatalf("decode the twin's llm_settings %q: %v", settings, err)
	}
	if decoded["model_name"] != "shared-model" {
		t.Errorf("the twin names model %v, want shared-model", decoded["model_name"])
	}
	if fmt.Sprintf("%v", decoded["model_project_id"]) != "1" {
		t.Errorf("the twin's model project = %v, want the public project", decoded["model_project_id"])
	}
}

// TestPublishInThePublicProjectStillRunsTheQualityGate pins a DIVERGENCE from
// the platform this replaces.
//
// The reference skipped the pre-publish check for a publish issued from inside
// the public project, on the reasoning that whoever stands there is a
// moderator. `Publish` has no such branch: the check runs on the version, not
// on who is publishing it. The fixture's instructions are under 50 characters,
// which is the one CRITICAL finding an otherwise empty agent raises.
func TestPublishInThePublicProjectStillRunsTheQualityGate(t *testing.T) {
	pool := newCatalogMirrorPool(t)
	router := catalogMirrorRouter(eliteacore.NewHandler(pool))
	fixture := seedCatalogMirrorFixture(t, pool, 1, "sparse agent in the catalogue")

	// NO approval token: this is the one-step publish the dialog makes when it
	// has not run the check separately, and it is the path that runs it inline.
	recorder := catalogMirrorPublish(t, router, fixture, map[string]any{"version_name": "v-one"})
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("publish status = %d, want 422; body = %s", recorder.Code, recorder.Body.String())
	}

	var response struct {
		Error            string `json:"error"`
		ValidationResult struct {
			Status         string           `json:"status"`
			CriticalIssues []map[string]any `json:"critical_issues"`
		} `json:"validation_result"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode publish refusal: %v", err)
	}
	if response.Error != "validation_failed" || response.ValidationResult.Status != "FAIL" {
		t.Fatalf("refusal = %s / %s, want validation_failed / FAIL",
			response.Error, response.ValidationResult.Status)
	}
	found := false
	for _, issue := range response.ValidationResult.CriticalIssues {
		if issue["field"] == "instructions" {
			found = true
		}
	}
	if !found {
		t.Errorf("critical issues = %v, want one about the instructions",
			response.ValidationResult.CriticalIssues)
	}
	if names := publishedVersionNames(t, pool, fixture); len(names) != 0 {
		t.Errorf("the gate published %v anyway", names)
	}
}

// TestAWithdrawnReleaseNameCanBePublishedAgain pins what a withdrawal means
// for the release name it took.
//
// Withdrawal reverts the published clone to a draft rather than deleting it, so
// the author keeps the version that was published. The reverted row used to
// keep the RELEASE NAME too, and the name is unique per agent, so publish →
// withdraw → publish again under the same name was refused forever with
// `version_name_exists_in_source` and the name could never be recovered (issue
// 854). The withdrawal now RENAMES the reverted clone, which gives the name
// back: the row survives, under a name that says what it is, and the original
// name is free for the next release.
func TestAWithdrawnReleaseNameCanBePublishedAgain(t *testing.T) {
	pool := newCatalogMirrorPool(t)
	router := catalogMirrorRouter(eliteacore.NewHandler(pool))
	fixture := seedCatalogMirrorFixture(t, pool, 1, "agent republished in place")

	published := catalogMirrorPublish(t, router, fixture, map[string]any{
		"version_name":     "v-one",
		"validation_token": catalogMirrorToken(t, pool, fixture),
	})
	if published.Code != http.StatusOK {
		t.Fatalf("publish status = %d, body = %s", published.Code, published.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(published.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode publish response: %v", err)
	}
	firstCloneID := fmt.Sprintf("%v", response["public_version_id"])

	catalogMirrorUnpublish(t, router, fixture.projectID, firstCloneID)

	// The name is free, and the SAME name goes through again.
	again := catalogMirrorPublish(t, router, fixture, map[string]any{
		"version_name":     "v-one",
		"validation_token": catalogMirrorToken(t, pool, fixture),
	})
	if again.Code != http.StatusOK {
		t.Fatalf("re-publish status = %d, want 200; body = %s", again.Code, again.Body.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// The withdrawn clone SURVIVES. It is a draft, it no longer holds the
	// release name, and it says which release it was — a withdrawal that
	// deleted the row would take the author's published version with it.
	var withdrawnName, withdrawnStatus, withdrawnFrom string
	if err := pool.QueryRow(ctx, `
SELECT name, status, COALESCE(meta->>'withdrawn_from_name', '')
FROM p_1.application_versions WHERE id = $1`, firstCloneID).
		Scan(&withdrawnName, &withdrawnStatus, &withdrawnFrom); err != nil {
		t.Fatalf("read the withdrawn clone: %v", err)
	}
	if withdrawnStatus != "draft" {
		t.Errorf("the withdrawn clone is %q, want draft", withdrawnStatus)
	}
	if !strings.HasPrefix(withdrawnName, "v-one-withdrawn-") {
		t.Errorf("the withdrawn clone is named %q, want the released name to carry the withdrawn marker", withdrawnName)
	}
	if withdrawnFrom != "v-one" {
		t.Errorf("meta.withdrawn_from_name = %q, want v-one", withdrawnFrom)
	}

	// …and exactly ONE row of this agent carries the release name now: the one
	// the second publish created, and it is the live one.
	rows, err := pool.Query(ctx, `
SELECT id, status FROM p_1.application_versions WHERE application_id = $1 AND name = 'v-one'`, fixture.appID)
	if err != nil {
		t.Fatalf("read the republished version: %v", err)
	}
	defer rows.Close()
	holders := map[string]string{}
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			t.Fatalf("scan the republished version: %v", err)
		}
		holders[id] = status
	}
	if len(holders) != 1 {
		t.Fatalf("rows named v-one = %v, want exactly one", holders)
	}
	for id, status := range holders {
		if id == firstCloneID {
			t.Errorf("the withdrawn clone %s still holds the release name", id)
		}
		if status != "published" {
			t.Errorf("the row named v-one is %q, want published", status)
		}
	}
}

// TestASecondWithdrawalOfTheSameNameGetsItsOwnName publishes, withdraws,
// publishes and withdraws again under one name, which is the case the counter
// in the replacement name exists for: two withdrawn clones of one agent cannot
// both be called `v-one-withdrawn-1`, and a collision there would leave the
// second withdrawal holding the release name again.
func TestASecondWithdrawalOfTheSameNameGetsItsOwnName(t *testing.T) {
	pool := newCatalogMirrorPool(t)
	router := catalogMirrorRouter(eliteacore.NewHandler(pool))
	fixture := seedCatalogMirrorFixture(t, pool, 1, "agent republished twice")

	var cloneIDs []string
	for round := 0; round < 2; round++ {
		published := catalogMirrorPublish(t, router, fixture, map[string]any{
			"version_name":     "v-one",
			"validation_token": catalogMirrorToken(t, pool, fixture),
		})
		if published.Code != http.StatusOK {
			t.Fatalf("publish %d status = %d, body = %s", round, published.Code, published.Body.String())
		}
		var response map[string]any
		if err := json.Unmarshal(published.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode publish response: %v", err)
		}
		cloneID := fmt.Sprintf("%v", response["public_version_id"])
		cloneIDs = append(cloneIDs, cloneID)
		catalogMirrorUnpublish(t, router, fixture.projectID, cloneID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	names := map[string]string{}
	for _, cloneID := range cloneIDs {
		var name string
		if err := pool.QueryRow(ctx,
			`SELECT name FROM p_1.application_versions WHERE id = $1`, cloneID).Scan(&name); err != nil {
			t.Fatalf("read clone %s: %v", cloneID, err)
		}
		if !strings.HasPrefix(name, "v-one-withdrawn-") {
			t.Errorf("clone %s is named %q, want the withdrawn marker", cloneID, name)
		}
		if other, clash := names[name]; clash {
			t.Errorf("clones %s and %s are both named %q", other, cloneID, name)
		}
		names[name] = cloneID
	}
	var stillTaken bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM p_1.application_versions WHERE application_id = $1 AND name = 'v-one')`,
		fixture.appID).Scan(&stillTaken); err != nil {
		t.Fatalf("read the release name: %v", err)
	}
	if stillTaken {
		t.Error("the release name is still held after both withdrawals")
	}
}

// catalogMirrorUnpublish withdraws one published version through the route the
// publish dialog calls.
func catalogMirrorUnpublish(t *testing.T, router chi.Router, projectID int, versionID string) {
	t.Helper()
	target := fmt.Sprintf("/elitea_core/unpublish/prompt_lib/%d/%s", projectID, versionID)
	request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader([]byte(`{}`)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("unpublish status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}
