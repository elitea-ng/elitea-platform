package evaluation_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
)

// recordingRepo is an in-memory library. It records what the handler asked it
// to store, which is the only way a test can tell "the handler validated and
// then persisted" from "the handler answered 200".
type recordingRepo struct {
	stored     []evaluation.Dimension
	lastFilter evaluation.ListFilter
	listCalls  int
}

func (r *recordingRepo) List(_ context.Context, _ string, filter evaluation.ListFilter) ([]evaluation.Dimension, error) {
	r.listCalls++
	r.lastFilter = filter
	return append([]evaluation.Dimension(nil), r.stored...), nil
}

func (r *recordingRepo) Create(_ context.Context, _ string, d evaluation.Dimension) (evaluation.Dimension, error) {
	d.ID = "1"
	r.stored = append(r.stored, d)
	return d, nil
}

func (r *recordingRepo) Update(_ context.Context, _, id string, d evaluation.Dimension) (evaluation.Dimension, error) {
	d.ID = id
	for i := range r.stored {
		if r.stored[i].ID == id {
			// The tier is decided the way the real repository decides it:
			// ResolveTierUpdate against the STORED tier. application_id is
			// never taken from the body; a promotion clears it. This fake
			// must not be more permissive than the real one, or the handler
			// test would pass against a scope-moving bug.
			tier, err := evaluation.ResolveTierUpdate(r.stored[i].Tier, d.Tier)
			if err != nil {
				return evaluation.Dimension{}, err
			}
			d.Tier = tier
			d.ApplicationID = r.stored[i].ApplicationID
			if tier == evaluation.TierProject {
				d.ApplicationID = nil
			}
			r.stored[i] = d
			return d, nil
		}
	}
	r.stored = append(r.stored, d)
	return d, nil
}

func (r *recordingRepo) Delete(_ context.Context, _, id string) error {
	for i := range r.stored {
		if r.stored[i].ID == id {
			r.stored = append(r.stored[:i], r.stored[i+1:]...)
			return nil
		}
	}
	return nil
}

func newTestRouter(repo evaluation.Repository) http.Handler {
	handler := evaluation.NewHandler(repo)
	r := chi.NewRouter()
	r.Get("/eval_dimensions/prompt_lib/{projectID}", handler.List)
	r.Post("/eval_dimensions/prompt_lib/{projectID}", handler.Create)
	r.Put("/eval_dimension/prompt_lib/{projectID}/{dimensionID}", handler.Update)
	r.Delete("/eval_dimension/prompt_lib/{projectID}/{dimensionID}", handler.Delete)
	return r
}

func do(t *testing.T, router http.Handler, method, path, body string) *httptest.ResponseRecorder {
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
	router.ServeHTTP(recorder, request)
	return recorder
}

const validBody = `{
  "name": "Helpfulness",
  "description": "Does the answer help?",
  "tier": "project",
  "allowed_engines": ["ai"],
  "scale_type": "continuous",
  "scale_min": 0,
  "scale_max": 100,
  "polarity": "higher_better",
  "default_weight": 1,
  "default_target": null,
  "default_target_operator": "",
  "code": "",
  "return_contract": ""
}`

// THE READ-BACK. A 201 proves the handler returned; it does not prove anything
// was stored. This repository has shipped write paths that reported success and
// persisted nothing, so the assertion is: create, then LIST through the same
// route the UI lists with, and find the row there.
func TestCreatedDimensionIsReadableBackThroughTheListRoute(t *testing.T) {
	t.Parallel()

	repo := &recordingRepo{}
	router := newTestRouter(repo)

	created := do(t, router, http.MethodPost, "/eval_dimensions/prompt_lib/1", validBody)
	if created.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", created.Code, created.Body.String())
	}

	listed := do(t, router, http.MethodGet, "/eval_dimensions/prompt_lib/1", "")
	if listed.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", listed.Code, listed.Body.String())
	}

	var page struct {
		Rows  []evaluation.Dimension `json:"rows"`
		Total int                    `json:"total"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode the listing: %v (body %s)", err, listed.Body.String())
	}
	if len(page.Rows) != 1 {
		t.Fatalf("expected the created dimension to be listed, got %d rows: %s", len(page.Rows), listed.Body.String())
	}
	if page.Rows[0].Name != "Helpfulness" {
		t.Fatalf("the listed dimension is not the created one: %+v", page.Rows[0])
	}
	if page.Total != 1 {
		t.Fatalf("expected total 1, got %d", page.Total)
	}
}

// The envelope key, asserted on the RAW BODY.
//
// Decoding into a struct cannot see this: a body keyed `items` decodes into a
// `rows`-tagged struct as an empty slice, with no error. That is the exact
// shape of the defect this pins — a 200, an empty screen, nothing in the
// console (issue #132).
func TestListAnswersRowsEnvelope(t *testing.T) {
	t.Parallel()

	repo := &recordingRepo{}
	router := newTestRouter(repo)
	if code := do(t, router, http.MethodPost, "/eval_dimensions/prompt_lib/1", validBody).Code; code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", code)
	}

	var raw map[string]json.RawMessage
	body := do(t, router, http.MethodGet, "/eval_dimensions/prompt_lib/1", "").Body.Bytes()
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode the listing as a raw object: %v", err)
	}
	if _, ok := raw["rows"]; !ok {
		t.Fatalf("the listing must be keyed `rows`; got keys %v", keysOf(raw))
	}
	if _, ok := raw["items"]; ok {
		t.Fatalf("the listing must NOT also carry `items`; got keys %v", keysOf(raw))
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}

// An empty library is `{"rows": [], "total": 0}` — never `{"rows": null}`. A
// null there is `undefined.length` at the call site of any client that trusts
// the type.
func TestEmptyLibraryIsAnEmptyArray(t *testing.T) {
	t.Parallel()

	router := newTestRouter(&recordingRepo{})
	body := do(t, router, http.MethodGet, "/eval_dimensions/prompt_lib/1", "").Body.String()
	if !strings.Contains(body, `"rows":[]`) {
		t.Fatalf("expected an empty array, got %s", body)
	}
}

// The handler must refuse a mixed engine set BEFORE it reaches the repository.
// If it did not, the table's CHECK would catch it — but as a 500-shaped
// surprise rather than a message the author can act on, and only where the
// constraint exists.
func TestMixedEngineSetIsRefusedBeforeStorage(t *testing.T) {
	t.Parallel()

	repo := &recordingRepo{}
	router := newTestRouter(repo)

	mixed := strings.Replace(validBody, `"allowed_engines": ["ai"]`, `"allowed_engines": ["ai", "code"]`, 1)
	response := do(t, router, http.MethodPost, "/eval_dimensions/prompt_lib/1", mixed)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a mixed engine set, got %d: %s", response.Code, response.Body.String())
	}
	if len(repo.stored) != 0 {
		t.Fatalf("nothing may be stored for a refused body, got %d rows", len(repo.stored))
	}
}

// `agent_id` is the query parameter the library listing narrows on. Without it
// the listing is the project library alone — NOT every ad-hoc dimension in the
// project, which would put each agent's rubrics in every other agent's editor.
// It is a listing scope, not an access check (see Handler.List).
func TestListPassesTheAgentFilterThrough(t *testing.T) {
	t.Parallel()

	repo := &recordingRepo{}
	router := newTestRouter(repo)

	do(t, router, http.MethodGet, "/eval_dimensions/prompt_lib/1?agent_id=42&include_platform=false", "")
	if repo.lastFilter.ApplicationID == nil || *repo.lastFilter.ApplicationID != 42 {
		t.Fatalf("agent_id must reach the repository, got %+v", repo.lastFilter)
	}
	if repo.lastFilter.IncludePlatform {
		t.Fatalf("include_platform=false must reach the repository, got %+v", repo.lastFilter)
	}

	do(t, router, http.MethodGet, "/eval_dimensions/prompt_lib/1", "")
	if repo.lastFilter.ApplicationID != nil {
		t.Fatalf("an absent agent_id must not become an agent filter, got %+v", repo.lastFilter)
	}
	if !repo.lastFilter.IncludePlatform {
		t.Fatalf("include_platform defaults to true, got %+v", repo.lastFilter)
	}

	bad := do(t, router, http.MethodGet, "/eval_dimensions/prompt_lib/1?agent_id=all", "")
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("a non-integer agent_id must be refused, got %d", bad.Code)
	}
}

// A body carrying a field this slice does not implement is REFUSED, not
// silently dropped. `evidence_scope` belongs to a BINDING; accepting it would
// report success for a setting nothing stored.
func TestUnknownFieldIsRefused(t *testing.T) {
	t.Parallel()

	repo := &recordingRepo{}
	router := newTestRouter(repo)

	withBinding := strings.Replace(validBody, `"name": "Helpfulness"`,
		`"name": "Helpfulness", "evidence_scope": {"output": true}`, 1)
	response := do(t, router, http.MethodPost, "/eval_dimensions/prompt_lib/1", withBinding)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unimplemented field, got %d: %s", response.Code, response.Body.String())
	}
	if len(repo.stored) != 0 {
		t.Fatalf("nothing may be stored for a refused body, got %d rows", len(repo.stored))
	}
}

func TestDeleteAnswersNoContent(t *testing.T) {
	t.Parallel()

	repo := &recordingRepo{}
	router := newTestRouter(repo)
	do(t, router, http.MethodPost, "/eval_dimensions/prompt_lib/1", validBody)

	response := do(t, router, http.MethodDelete, "/eval_dimension/prompt_lib/1/1", "")
	if response.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", response.Code, response.Body.String())
	}
	if len(repo.stored) != 0 {
		t.Fatalf("the dimension must be gone, got %d rows", len(repo.stored))
	}
}
