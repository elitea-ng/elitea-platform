package repos

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
)

// The evaluation dimension library, exercised against a REAL migrated tenant
// schema and through the REAL routes.
//
// WHY THIS IS NOT A REPOSITORY-METHOD TEST.
//
// The failure this file exists to make impossible is not "the SQL is wrong".
// It is "the write reported success and persisted nothing", and its
// close relative "the write persisted something and the read cannot see it".
// Both of those are invisible to a test that calls Create and asserts on
// Create's own return value, and both have shipped from this repository
// before. So every assertion below goes:
//
//	POST through the handler  ->  GET through the handler  ->  find the row
//
// which is the sequence the browser performs, over the same chi router shape
// internal/api/router.go registers.
//
// It also exercises the constraints that only exist in the database
// (tenant/0130_eval_dimensions.sql). The handler validates the same rules, so
// the interesting question is whether the two AGREE — a handler that accepts
// what the table refuses is a 500 in production, and a table that accepts what
// the handler refuses is a rule with a hole in it for every other writer.
func newEvalDimensionsRouter(t *testing.T) http.Handler {
	t.Helper()
	return newEvalDimensionsRouterWith(t, true)
}

// newEvalDimensionsRouterWith wires the create-permission check a promotion
// asks, answering `mayCreate` for every request.
func newEvalDimensionsRouterWith(t *testing.T, mayCreate bool) http.Handler {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	handler := evaluation.NewHandler(NewEvalDimensionsRepo(pool),
		evaluation.WithCreatePermissionCheck(func(*http.Request) (bool, error) { return mayCreate, nil }))

	r := chi.NewRouter()
	r.Get("/eval_dimensions/prompt_lib/{projectID}", handler.List)
	r.Post("/eval_dimensions/prompt_lib/{projectID}", handler.Create)
	r.Put("/eval_dimension/prompt_lib/{projectID}/{dimensionID}", handler.Update)
	r.Delete("/eval_dimension/prompt_lib/{projectID}/{dimensionID}", handler.Delete)
	return r
}

func callEvalRoute(t *testing.T, router http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// listEvalDimensions reads the library back THROUGH THE ROUTE and decodes the
// `{rows,total}` envelope. Decoding it here rather than querying the table is
// the point: a stored row a client cannot see is not a stored row as far as
// anyone using the product is concerned.
func listEvalDimensions(t *testing.T, router http.Handler, query string) []evaluation.Dimension {
	t.Helper()
	response := callEvalRoute(t, router, http.MethodGet, "/eval_dimensions/prompt_lib/1"+query, "")
	if response.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", response.Code, response.Body.String())
	}
	var page struct {
		Rows  []evaluation.Dimension `json:"rows"`
		Total int                    `json:"total"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode the listing: %v (body %s)", err, response.Body.String())
	}
	if page.Total != len(page.Rows) {
		t.Fatalf("total (%d) disagrees with the row count (%d)", page.Total, len(page.Rows))
	}
	return page.Rows
}

const evalCreateBody = `{
  "name": "Faithfulness",
  "description": "Is the answer grounded in the retrieved context?",
  "tier": "project",
  "allowed_engines": ["ai", "human"],
  "scale_type": "ordinal",
  "scale_min": 1,
  "scale_max": 5,
  "polarity": "higher_better",
  "default_weight": 2,
  "default_target": 4,
  "default_target_operator": ">=",
  "code": "",
  "return_contract": ""
}`

// THE READ-BACK PROOF. A 201 is not evidence of anything.
func TestEvalDimensionCreatedThroughTheRouteIsReadBackThroughTheRoute(t *testing.T) {
	router := newEvalDimensionsRouter(t)

	created := callEvalRoute(t, router, http.MethodPost, "/eval_dimensions/prompt_lib/1", evalCreateBody)
	if created.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", created.Code, created.Body.String())
	}

	rows := listEvalDimensions(t, router, "")
	if len(rows) != 1 {
		t.Fatalf("expected exactly the created dimension in the library, got %d rows", len(rows))
	}
	stored := rows[0]

	// Every authored field, checked. A projection that selects some columns on
	// create and others on list is how a value round-trips on save and
	// disappears on reload, with a 200 at every step.
	if stored.Name != "Faithfulness" {
		t.Errorf("name: got %q", stored.Name)
	}
	if stored.Description != "Is the answer grounded in the retrieved context?" {
		t.Errorf("description: got %q", stored.Description)
	}
	if stored.Tier != evaluation.TierProject {
		t.Errorf("tier: got %q", stored.Tier)
	}
	if len(stored.AllowedEngines) != 2 ||
		stored.AllowedEngines[0] != evaluation.EngineAI ||
		stored.AllowedEngines[1] != evaluation.EngineHuman {
		t.Errorf("allowed_engines: got %v", stored.AllowedEngines)
	}
	if stored.ScaleType != evaluation.ScaleOrdinal {
		t.Errorf("scale_type: got %q", stored.ScaleType)
	}
	if stored.ScaleMin != 1 || stored.ScaleMax != 5 {
		t.Errorf("scale bounds: got %v..%v", stored.ScaleMin, stored.ScaleMax)
	}
	if stored.Polarity != evaluation.PolarityHigherBetter {
		t.Errorf("polarity: got %q", stored.Polarity)
	}
	if stored.DefaultWeight != 2 {
		t.Errorf("default_weight: got %v", stored.DefaultWeight)
	}
	if stored.DefaultTarget == nil || *stored.DefaultTarget != 4 {
		t.Errorf("default_target: got %v", stored.DefaultTarget)
	}
	if stored.DefaultTargetOperator != ">=" {
		t.Errorf("default_target_operator: got %q", stored.DefaultTargetOperator)
	}
	if stored.ID == "" || stored.UUID == "" {
		t.Errorf("the stored row must carry an id and a uuid, got id=%q uuid=%q", stored.ID, stored.UUID)
	}
	if stored.CreatedAt == "" || stored.UpdatedAt == "" {
		t.Errorf("timestamps must be returned, got created=%q updated=%q", stored.CreatedAt, stored.UpdatedAt)
	}
}

// An edit is read back too, and — the part that matters — the edit does NOT
// change the scope. The editor renders scope as a disabled field and sends no
// tier, so a repository that wrote the request's tier would promote an
// agent-scoped rubric into the whole project's library on a rename.
func TestEvalDimensionUpdateIsReadBackAndDoesNotChangeScope(t *testing.T) {
	router := newEvalDimensionsRouter(t)

	adhoc := strings.NewReplacer(
		`"tier": "project"`, `"tier": "agent_adhoc", "application_id": 77`,
		`"name": "Faithfulness"`, `"name": "Tone"`,
	).Replace(evalCreateBody)
	created := callEvalRoute(t, router, http.MethodPost, "/eval_dimensions/prompt_lib/1", adhoc)
	if created.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", created.Code, created.Body.String())
	}
	var createdRow evaluation.Dimension
	if err := json.Unmarshal(created.Body.Bytes(), &createdRow); err != nil {
		t.Fatalf("decode the created dimension: %v", err)
	}

	// The rename, with the tier omitted exactly as the editor omits it.
	renamed := strings.NewReplacer(
		`"tier": "project",`, ``,
		`"name": "Faithfulness"`, `"name": "Tone of voice"`,
	).Replace(evalCreateBody)
	updated := callEvalRoute(t, router, http.MethodPut,
		"/eval_dimension/prompt_lib/1/"+createdRow.ID, renamed)
	if updated.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d: %s", updated.Code, updated.Body.String())
	}

	rows := listEvalDimensions(t, router, "?agent_id=77")
	if len(rows) != 1 {
		t.Fatalf("expected the edited dimension to be listed for its agent, got %d rows", len(rows))
	}
	if rows[0].Name != "Tone of voice" {
		t.Fatalf("the rename was not persisted: got %q", rows[0].Name)
	}
	if rows[0].Tier != evaluation.TierAgentAdhoc {
		t.Fatalf("an update without a tier changed the tier to %q; an absent tier keeps the stored scope", rows[0].Tier)
	}
	if rows[0].ApplicationID == nil || *rows[0].ApplicationID != 77 {
		t.Fatalf("the update dropped the agent scope: %v", rows[0].ApplicationID)
	}

	// And it is NOT in the plain project library, which is the other half of
	// the same claim.
	if plain := listEvalDimensions(t, router, ""); len(plain) != 0 {
		t.Fatalf("an agent-scoped dimension must not appear in the project library, got %d rows", len(plain))
	}
}

// An ad-hoc dimension belongs to ONE agent. Listing for another agent must not
// return it, or every agent's editor grows every other agent's rubrics. (A
// listing scope, not an access boundary: any project editor may edit any agent.)
func TestEvalDimensionAdhocScopeIsPerAgent(t *testing.T) {
	router := newEvalDimensionsRouter(t)

	for _, agent := range []string{"11", "22"} {
		body := strings.NewReplacer(
			`"tier": "project"`, `"tier": "agent_adhoc", "application_id": `+agent,
			`"name": "Faithfulness"`, `"name": "Rubric for `+agent+`"`,
		).Replace(evalCreateBody)
		if code := callEvalRoute(t, router, http.MethodPost, "/eval_dimensions/prompt_lib/1", body).Code; code != http.StatusCreated {
			t.Fatalf("create for agent %s: expected 201, got %d", agent, code)
		}
	}

	rows := listEvalDimensions(t, router, "?agent_id=11")
	if len(rows) != 1 || rows[0].Name != "Rubric for 11" {
		t.Fatalf("agent 11 must see exactly its own rubric, got %+v", rows)
	}
}

// The mutual exclusion, proven at BOTH layers.
//
// The handler refuses it (400, nothing stored). The table refuses it too — and
// that half is checked by writing straight to the pool, because the handler can
// never produce the row and the constraint would otherwise be a claim nobody
// ever tested.
func TestEvalDimensionCodeEngineExclusivityHoldsInTheDatabaseToo(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	handler := evaluation.NewHandler(NewEvalDimensionsRepo(pool))
	router := chi.NewRouter()
	router.Post("/eval_dimensions/prompt_lib/{projectID}", handler.Create)
	router.Get("/eval_dimensions/prompt_lib/{projectID}", handler.List)

	mixed := strings.Replace(evalCreateBody, `"allowed_engines": ["ai", "human"]`,
		`"allowed_engines": ["ai", "code"]`, 1)
	response := callEvalRoute(t, router, http.MethodPost, "/eval_dimensions/prompt_lib/1", mixed)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("the handler must refuse a mixed engine set with 400, got %d: %s",
			response.Code, response.Body.String())
	}
	if rows := listEvalDimensions(t, router, ""); len(rows) != 0 {
		t.Fatalf("a refused create must store nothing, found %d rows", len(rows))
	}

	// The table, bypassing the handler entirely.
	_, err := pool.Exec(t.Context(), `
		INSERT INTO p_1.eval_dimensions
			(name, tier, allowed_engines, scale_type, scale_min, scale_max, polarity, code)
		VALUES ('smuggled', 'project', ARRAY['ai','code']::text[], 'continuous', 0, 100, 'higher_better', 'x')`)
	if err == nil {
		t.Fatal("the table accepted allowed_engines = {ai,code}; the CHECK constraint is not doing its job")
	}
	if !strings.Contains(err.Error(), "eval_dimensions_code_engine_exclusive_check") {
		t.Fatalf("expected the exclusivity constraint to refuse it, got: %v", err)
	}

	// And a code dimension with no code.
	_, err = pool.Exec(t.Context(), `
		INSERT INTO p_1.eval_dimensions
			(name, tier, allowed_engines, scale_type, scale_min, scale_max, polarity)
		VALUES ('codeless', 'project', ARRAY['code']::text[], 'continuous', 0, 100, 'higher_better')`)
	if err == nil {
		t.Fatal("the table accepted a code dimension with no code")
	}
	if !strings.Contains(err.Error(), "eval_dimensions_code_required_check") {
		t.Fatalf("expected the code-required constraint to refuse it, got: %v", err)
	}
}

// A code dimension round-trips its script and its return contract. Storing the
// engine without the script would leave a validation that cannot validate.
func TestEvalDimensionCodeEngineRoundTrips(t *testing.T) {
	router := newEvalDimensionsRouter(t)

	body := strings.NewReplacer(
		`"allowed_engines": ["ai", "human"]`, `"allowed_engines": ["code"]`,
		`"code": ""`, `"code": "def score(output):\n    return len(output) > 0"`,
		`"return_contract": ""`, `"return_contract": "bool"`,
	).Replace(evalCreateBody)

	if code := callEvalRoute(t, router, http.MethodPost, "/eval_dimensions/prompt_lib/1", body).Code; code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", code)
	}

	rows := listEvalDimensions(t, router, "")
	if len(rows) != 1 {
		t.Fatalf("expected one dimension, got %d", len(rows))
	}
	if !rows[0].IsCodeEngine() {
		t.Fatalf("allowed_engines must round-trip as exactly [code], got %v", rows[0].AllowedEngines)
	}
	if !strings.Contains(rows[0].Code, "def score(output)") {
		t.Fatalf("the script must round-trip, got %q", rows[0].Code)
	}
	if rows[0].ReturnContract != evaluation.ReturnContractBool {
		t.Fatalf("return_contract must round-trip, got %q", rows[0].ReturnContract)
	}
}

// Delete removes the row from the library, and a second delete is a 404 rather
// than a second 204. A DELETE that answers success for an id that was never
// there teaches a client to trust a status code that means nothing.
func TestEvalDimensionDeleteIsReadBackAsGone(t *testing.T) {
	router := newEvalDimensionsRouter(t)

	created := callEvalRoute(t, router, http.MethodPost, "/eval_dimensions/prompt_lib/1", evalCreateBody)
	if created.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", created.Code, created.Body.String())
	}
	var row evaluation.Dimension
	if err := json.Unmarshal(created.Body.Bytes(), &row); err != nil {
		t.Fatalf("decode the created dimension: %v", err)
	}

	if code := callEvalRoute(t, router, http.MethodDelete, "/eval_dimension/prompt_lib/1/"+row.ID, "").Code; code != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d", code)
	}
	if rows := listEvalDimensions(t, router, ""); len(rows) != 0 {
		t.Fatalf("the dimension must be gone from the library, got %d rows", len(rows))
	}
	if code := callEvalRoute(t, router, http.MethodDelete, "/eval_dimension/prompt_lib/1/"+row.ID, "").Code; code != http.StatusNotFound {
		t.Fatalf("a second delete must be 404, got %d", code)
	}
}

// The library is ONE project's. A dimension authored in p_1 must not be visible
// through another project's path segment — the schema is derived from that
// segment, so this is really a check that nothing here reads a fixed schema.
func TestEvalDimensionLibraryIsPerProject(t *testing.T) {
	router := newEvalDimensionsRouter(t)

	if code := callEvalRoute(t, router, http.MethodPost, "/eval_dimensions/prompt_lib/1", evalCreateBody).Code; code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", code)
	}

	// p_2 does not exist in this template, so the read must FAIL rather than
	// answer p_1's rows. Failing closed is the requirement; the status is
	// whatever the missing relation produces.
	other := callEvalRoute(t, router, http.MethodGet, "/eval_dimensions/prompt_lib/2", "")
	if other.Code == http.StatusOK && strings.Contains(other.Body.String(), "Faithfulness") {
		t.Fatalf("project 2 was served project 1's library: %s", other.Body.String())
	}
}
