package eliteacore_test

// Issue #940 A18 acceptance — the AI step of publish validation (ELITEA-0146,
// "AI Validation Uses Project-Level Low-Tier LLM — Broken Agent LLM Does Not
// Block Validation").
//
// It drives the ROUTE against a real corpus-built database with a FAKE model
// client, because the two halves of the claim are on opposite sides of that
// seam:
//
//   - which model is chosen, and that the version's OWN model is not it, is a
//     database question (the project's low-tier catalogue row);
//   - that a broken model does not block the validation is a client question.
//
// The journeys stack has no model plane at all, so the model-backed path is
// verified here and nowhere else; the API-level journey asserts only the
// response shape and the flag's value on a deployment with no gateway.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

const (
	publishAIProjectID = 7301
	publishAICallerID  = 7302
)

// fakePublishModel records what it was asked and answers what the test set.
type fakePublishModel struct {
	answer  string
	err     error
	calls   int
	lastReq eliteacore.PublishModelRequest
}

func (f *fakePublishModel) Complete(
	_ context.Context, request eliteacore.PublishModelRequest,
) (string, error) {
	f.calls++
	f.lastReq = request
	return f.answer, f.err
}

type publishValidationBody struct {
	Status                string           `json:"status"`
	CriticalIssues        []map[string]any `json:"critical_issues"`
	Warnings              []map[string]any `json:"warnings"`
	Recommendations       []map[string]any `json:"recommendations"`
	AIValidationAvailable bool             `json:"ai_validation_available"`
	ValidationToken       *string          `json:"validation_token"`
}

// publishAIFixtureSQL seeds one project, one agent with one draft version, and
// TWO model catalogue rows.
//
// The two rows are the point. `premium-model` is the version's own model and
// is NOT low-tier; `tiny-model` is the project's low-tier row. A step that
// reached for the version's `llm_settings.model_name` — the model that is
// broken in this case's scenario — passes any test seeded with one model.
//
// Everything else about the fixture exists to make the DETERMINISTIC verdict
// PASS, so that a change in status can only have come from the AI step:
// instructions past the 50-character floor, one conversation starter (absent
// starters are a warning, and a warning is already not PASS), and an
// `llm_settings` with NO `model_project_id` — naming one that is not the
// public project is a critical issue in its own right.
const publishAIFixtureSQL = `
INSERT INTO auth_core__user (id, email, name) VALUES (7302, 'publisher@autotest.local', 'Publisher');
INSERT INTO %[1]s.applications (id, name, description, owner_id)
VALUES (81, 'Payroll Helper', 'Answers payroll questions.', 7301);
INSERT INTO %[1]s.application_versions
  (id, application_id, name, status, instructions, conversation_starters, llm_settings, author_id)
VALUES (91, 81, 'draft-1', 'draft',
        'Answer payroll questions for the finance team, citing the handbook section you used each time.',
        '["How do I read my payslip?"]'::jsonb,
        '{"model_name":"premium-model"}'::jsonb, 7302);
INSERT INTO %[1]s.configuration (project_id, label, elitea_title, type, section, data, status_ok, shared)
VALUES
  (7301, 'Premium', 'model_premium_7301', 'model', 'llm',
   '{"name":"premium-model","low_tier":false}'::jsonb, true, false),
  (7301, 'Tiny',    'model_tiny_7301',    'model', 'llm',
   '{"name":"tiny-model","low_tier":true}'::jsonb,  true, false);
`

func seedPublishAIFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	schema := fmt.Sprintf("p_%d", publishAIProjectID)
	if _, err := pool.Exec(context.Background(),
		fmt.Sprintf(publishAIFixtureSQL, schema)); err != nil {
		t.Fatalf("seed the publish AI validation fixture: %v", err)
	}
}

func publishAIRouter(handler *eliteacore.Handler) chi.Router {
	router := chi.NewRouter()
	router.Post("/elitea_core/validate_for_publish/prompt_lib/{projectID}/{versionID}",
		handler.PublishValidate)
	return router
}

func validateForPublish(t *testing.T, router chi.Router, versionName string) publishValidationBody {
	t.Helper()
	url := fmt.Sprintf("/elitea_core/validate_for_publish/prompt_lib/%d/91", publishAIProjectID)
	request := httptest.NewRequest(http.MethodPost, url,
		strings.NewReader(fmt.Sprintf(`{"version_name":%q}`, versionName)))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(auth.ContextWithUser(request.Context(),
		auth.User{ID: fmt.Sprintf("%d", publishAICallerID)}))

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK && recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("validate status = %d, want 200 or 422 (body %s)", recorder.Code, recorder.Body.String())
	}
	var body publishValidationBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode validation body %q: %v", recorder.Body.String(), err)
	}
	return body
}

func newPublishAIHandler(pool *pgxpool.Pool, model eliteacore.PublishModelClient) *eliteacore.Handler {
	return eliteacore.NewHandler(pool, eliteacore.WithPublishAIValidation(model))
}

// TestPublishValidationRunsTheAIStepOnTheProjectLowTierModel is ELITEA-0146's
// first half: the step runs, it names the PROJECT's low-tier model rather than
// the version's own, and its findings come back in the existing shape with the
// flag true.
func TestPublishValidationRunsTheAIStepOnTheProjectLowTierModel(t *testing.T) {
	pool := newImportCorpusPool(t)
	provisionTenantProject(t, pool, publishAIProjectID)
	seedPublishAIFixture(t, pool)

	model := &fakePublishModel{answer: `{"findings":[
		{"field":"description","issue":"the description does not say which payroll system","suggestion":"name the system"},
		{"field":"name","issue":"\"Helper\" says nothing about what it does"}
	]}`}
	body := validateForPublish(t, publishAIRouter(newPublishAIHandler(pool, model)), "release-1")

	if !body.AIValidationAvailable {
		t.Fatalf("ai_validation_available = false with a working model: %+v", body)
	}
	if model.calls != 1 {
		t.Fatalf("the model was called %d times, want 1", model.calls)
	}
	// The PROJECT's low-tier row, not the version's `premium-model`.
	if model.lastReq.Model != "tiny-model" {
		t.Errorf("validated on model %q, want the project's low-tier tiny-model", model.lastReq.Model)
	}
	// Billed to the caller who asked, and to their project.
	if model.lastReq.UserID != fmt.Sprintf("%d", publishAICallerID) {
		t.Errorf("turn attributed to user %q, want the calling principal", model.lastReq.UserID)
	}
	if model.lastReq.ProjectID != fmt.Sprintf("%d", publishAIProjectID) {
		t.Errorf("turn attributed to project %q, want %d", model.lastReq.ProjectID, publishAIProjectID)
	}
	// The version's own text reached the prompt — otherwise the model would be
	// reviewing nothing and the findings would be about nothing.
	if !strings.Contains(model.lastReq.User, "Payroll Helper") ||
		!strings.Contains(model.lastReq.User, "payroll questions for the finance team") {
		t.Errorf("the prompt does not carry the version's own text: %q", model.lastReq.User)
	}

	aiFindings := 0
	for _, entry := range body.Recommendations {
		if entry["source"] == "ai" {
			aiFindings++
		}
	}
	if aiFindings != 2 {
		t.Errorf("%d AI recommendations, want 2: %+v", aiFindings, body.Recommendations)
	}
}

// TestPublishValidationSurvivesABrokenModel is the case's own title. A model
// that fails must leave the validation exactly as it was, with the flag false
// — not a 500, and not a refusal to publish.
func TestPublishValidationSurvivesABrokenModel(t *testing.T) {
	pool := newImportCorpusPool(t)
	provisionTenantProject(t, pool, publishAIProjectID)
	seedPublishAIFixture(t, pool)

	working := validateForPublish(t,
		publishAIRouter(newPublishAIHandler(pool, &fakePublishModel{answer: `{"findings":[]}`})), "release-1")
	broken := validateForPublish(t,
		publishAIRouter(newPublishAIHandler(pool, &fakePublishModel{err: errors.New("upstream refused")})), "release-1")

	if broken.AIValidationAvailable {
		t.Errorf("ai_validation_available = true after the model failed")
	}
	// The deterministic verdict is untouched — same status, and a token is
	// still issued so the author can still publish.
	if broken.Status != working.Status {
		t.Errorf("status = %q with a broken model, want %q", broken.Status, working.Status)
	}
	if broken.Status != "PASS" {
		t.Errorf("status = %q, want PASS: the deterministic checks all hold on this fixture", broken.Status)
	}
	if broken.ValidationToken == nil || *broken.ValidationToken == "" {
		t.Error("no validation token was issued, so a broken review model blocked the publish")
	}
	if len(broken.CriticalIssues) != 0 {
		t.Errorf("a broken model produced critical issues: %+v", broken.CriticalIssues)
	}
}

// TestPublishValidationSkipsTheAIStepWithNoLowTierModel: a project that
// configures no low-tier model has nothing to validate WITH, and says so.
func TestPublishValidationSkipsTheAIStepWithNoLowTierModel(t *testing.T) {
	pool := newImportCorpusPool(t)
	provisionTenantProject(t, pool, publishAIProjectID)
	seedPublishAIFixture(t, pool)
	if _, err := pool.Exec(context.Background(), fmt.Sprintf(
		`UPDATE p_%d.configuration SET data = jsonb_set(data, '{low_tier}', 'false')`, publishAIProjectID)); err != nil {
		t.Fatalf("clear the low-tier flag: %v", err)
	}

	model := &fakePublishModel{answer: `{"findings":[]}`}
	body := validateForPublish(t, publishAIRouter(newPublishAIHandler(pool, model)), "release-1")

	if body.AIValidationAvailable {
		t.Error("ai_validation_available = true with no low-tier model configured")
	}
	// And the model was never called: a step with no model must not fall back
	// to whichever model it can find, least of all the version's own.
	if model.calls != 0 {
		t.Errorf("the model was called %d times with no low-tier row, want 0", model.calls)
	}
}

// TestPublishValidationWithNoModelClientKeepsTheOldAnswer pins the
// unconfigured deployment — every one before #940 A18, and every one with no
// LLM_GATEWAY_URL today.
func TestPublishValidationWithNoModelClientKeepsTheOldAnswer(t *testing.T) {
	pool := newImportCorpusPool(t)
	provisionTenantProject(t, pool, publishAIProjectID)
	seedPublishAIFixture(t, pool)

	body := validateForPublish(t, publishAIRouter(eliteacore.NewHandler(pool)), "release-1")
	if body.AIValidationAvailable {
		t.Error("ai_validation_available = true on a deployment with no model client")
	}
	if body.Status != "PASS" {
		t.Errorf("status = %q, want PASS", body.Status)
	}
	for _, entry := range body.Recommendations {
		if entry["source"] == "ai" {
			t.Errorf("an AI recommendation appeared with no model client: %+v", entry)
		}
	}
}

// TestPublishValidationAIFindingsNeverChangeTheVerdict is rule 1 of the
// module's header, measured: an advisory pass must not be able to turn a
// publishable version into a refused one, however many problems it reports.
func TestPublishValidationAIFindingsNeverChangeTheVerdict(t *testing.T) {
	pool := newImportCorpusPool(t)
	provisionTenantProject(t, pool, publishAIProjectID)
	seedPublishAIFixture(t, pool)

	// Ten findings, which is more than the cap, and every one of them phrased
	// as a blocker.
	findings := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		findings = append(findings, fmt.Sprintf(`{"field":"instructions","issue":"blocker %d"}`, i))
	}
	model := &fakePublishModel{answer: `{"findings":[` + strings.Join(findings, ",") + `]}`}
	body := validateForPublish(t, publishAIRouter(newPublishAIHandler(pool, model)), "release-1")

	if body.Status != "PASS" {
		t.Errorf("status = %q after ten AI findings, want PASS", body.Status)
	}
	if len(body.CriticalIssues) != 0 || len(body.Warnings) != 0 {
		t.Errorf("AI findings reached critical_issues/warnings: %+v / %+v", body.CriticalIssues, body.Warnings)
	}
	if body.ValidationToken == nil || *body.ValidationToken == "" {
		t.Error("the AI step withheld the validation token")
	}
	// Capped, so one talkative answer cannot flood the list the SPA renders.
	// Counted by SOURCE: the deterministic tag recommendation is in this list
	// too, and counting the whole list would measure the fixture.
	aiFindings := 0
	for _, entry := range body.Recommendations {
		if entry["source"] == "ai" {
			aiFindings++
		}
	}
	if aiFindings != 5 {
		t.Errorf("%d AI recommendations from a ten-finding answer, want the 5-finding cap", aiFindings)
	}
}

// TestPublishValidationRejectsAnUnusableModelAnswer: prose instead of JSON is
// "unavailable", never "no findings". Those are different claims and an author
// acts on them differently.
func TestPublishValidationRejectsAnUnusableModelAnswer(t *testing.T) {
	pool := newImportCorpusPool(t)
	provisionTenantProject(t, pool, publishAIProjectID)
	seedPublishAIFixture(t, pool)

	for name, answer := range map[string]string{
		"prose":          "I had a look and it seems fine to me!",
		"empty":          "",
		"a refusal":      "I'm sorry, I can't help with that.",
		"half an object": `{"findings":[{"field":"name",`,
	} {
		t.Run(name, func(t *testing.T) {
			body := validateForPublish(t,
				publishAIRouter(newPublishAIHandler(pool, &fakePublishModel{answer: answer})), "release-1")
			if body.AIValidationAvailable {
				t.Errorf("ai_validation_available = true for an unusable answer %q", answer)
			}
		})
	}
}

// TestPublishValidationReadsAFencedJSONAnswer: models fence their JSON, and a
// step that refused a fenced object would report "unavailable" for a model
// that answered perfectly well.
func TestPublishValidationReadsAFencedJSONAnswer(t *testing.T) {
	pool := newImportCorpusPool(t)
	provisionTenantProject(t, pool, publishAIProjectID)
	seedPublishAIFixture(t, pool)

	model := &fakePublishModel{answer: "```json\n{\"findings\":[{\"field\":\"name\",\"issue\":\"too vague\"}]}\n```"}
	body := validateForPublish(t, publishAIRouter(newPublishAIHandler(pool, model)), "release-1")

	if !body.AIValidationAvailable {
		t.Fatal("a fenced JSON answer was reported as unavailable")
	}
	found := false
	for _, entry := range body.Recommendations {
		if entry["source"] == "ai" && entry["issue"] == "too vague" {
			found = true
		}
	}
	if !found {
		t.Errorf("the fenced answer's finding is missing: %+v", body.Recommendations)
	}
}

// A guard on the harness itself: the fixture must really carry two models, or
// "it used the low-tier one" is not a measurement.
func TestPublishAIFixtureHoldsTwoModels(t *testing.T) {
	pool := newImportCorpusPool(t)
	provisionTenantProject(t, pool, publishAIProjectID)
	seedPublishAIFixture(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var count int
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT COUNT(*) FROM p_%d.configuration WHERE section = 'llm'`, publishAIProjectID)).Scan(&count); err != nil {
		t.Fatalf("count the seeded models: %v", err)
	}
	if count != 2 {
		t.Fatalf("the fixture holds %d llm rows, want 2", count)
	}
}
