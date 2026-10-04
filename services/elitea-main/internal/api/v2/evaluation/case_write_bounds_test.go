package evaluation_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// caseRepo is an in-memory dataset store holding ONE case. It records which
// write path the handler took, because the point of the flag-only update is
// that it does NOT go through the path that rewrites the case text.
type caseRepo struct {
	stored          evaluation.DatasetCase
	updateCaseCalls int
	setFlagCalls    int
	addCaseCalls    int
	createCalls     int
}

func newCaseRepo() *caseRepo {
	expected := "the reference answer, corrected by someone else"
	return &caseRepo{stored: evaluation.DatasetCase{
		ID: "11", DatasetID: "5", Input: "newer text written in another tab",
		Variables: map[string]any{}, ExpectedOutput: &expected, SourceType: evaluation.CaseSourceManual,
	}}
}

func (r *caseRepo) ListDatasets(context.Context, string, evaluation.DatasetListFilter) ([]evaluation.Dataset, error) {
	return nil, nil
}

func (r *caseRepo) GetDataset(context.Context, string, string, evaluation.CasePage) (evaluation.DatasetDetail, error) {
	return evaluation.DatasetDetail{}, nil
}

func (r *caseRepo) CreateDataset(_ context.Context, _ string, input evaluation.DatasetWriteInput) (evaluation.Dataset, error) {
	r.createCalls++
	return evaluation.Dataset{ID: "5", Name: input.Name}, nil
}

func (r *caseRepo) UpdateDataset(context.Context, string, string, evaluation.DatasetWriteInput) (evaluation.Dataset, error) {
	return evaluation.Dataset{}, nil
}

func (r *caseRepo) DeleteDataset(context.Context, string, string) error { return nil }

func (r *caseRepo) AddCase(_ context.Context, _, _ string, input evaluation.CaseWriteInput) (evaluation.DatasetCase, error) {
	r.addCaseCalls++
	return evaluation.DatasetCase{ID: "12", Input: input.Input, Variables: input.Variables}, nil
}

func (r *caseRepo) UpdateCase(_ context.Context, _, _, caseID string, input evaluation.CaseWriteInput) (evaluation.DatasetCase, error) {
	r.updateCaseCalls++
	if caseID != r.stored.ID {
		return evaluation.DatasetCase{}, apierr.NotFound("case not found in this dataset")
	}
	r.stored.Input = input.Input
	r.stored.Variables = input.Variables
	r.stored.ExpectedOutput = input.ExpectedOutput
	if input.Excluded != nil {
		r.stored.Excluded = *input.Excluded
	}
	return r.stored, nil
}

func (r *caseRepo) SetCaseExcluded(_ context.Context, _, _, caseID string, excluded bool) (evaluation.DatasetCase, error) {
	r.setFlagCalls++
	if caseID != r.stored.ID {
		return evaluation.DatasetCase{}, apierr.NotFound("case not found in this dataset")
	}
	r.stored.Excluded = excluded
	return r.stored, nil
}

func (r *caseRepo) DeleteCase(context.Context, string, string, string) error { return nil }

func newDatasetTestRouter(repo evaluation.DatasetRepository) http.Handler {
	handler := evaluation.NewDatasetHandler(repo)
	r := chi.NewRouter()
	r.Post("/eval_datasets/prompt_lib/{projectID}", handler.Create)
	r.Post("/eval_dataset_cases/prompt_lib/{projectID}/{datasetID}", handler.AddCase)
	r.Put("/eval_dataset_case/prompt_lib/{projectID}/{datasetID}/{caseID}", handler.UpdateCase)
	return r
}

const casePath = "/eval_dataset_case/prompt_lib/1/5/11"

// A body holding ONLY `excluded` writes the flag and leaves the text alone.
// Before, the checkbox sent the client's cached text back with the flag, and a
// toggle from a stale view reverted an edit made in another tab.
func TestExclusionOnlyUpdateLeavesTheCaseTextAlone(t *testing.T) {
	t.Parallel()

	repo := newCaseRepo()
	before := repo.stored
	response := do(t, newDatasetTestRouter(repo), http.MethodPut, casePath, `{"excluded":true}`)
	if response.Code != http.StatusOK {
		t.Fatalf("toggle: expected 200, got %d: %s", response.Code, response.Body.String())
	}
	if repo.setFlagCalls != 1 || repo.updateCaseCalls != 0 {
		t.Fatalf("flag-only body went through UpdateCase %d times and SetCaseExcluded %d times, want 0 and 1",
			repo.updateCaseCalls, repo.setFlagCalls)
	}
	if !repo.stored.Excluded {
		t.Fatal("the case was not excluded")
	}
	if repo.stored.Input != before.Input || *repo.stored.ExpectedOutput != *before.ExpectedOutput {
		t.Fatalf("the toggle changed the case text: %+v", repo.stored)
	}
	var answered evaluation.DatasetCase
	if err := json.Unmarshal(response.Body.Bytes(), &answered); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !answered.Excluded || answered.Input != before.Input {
		t.Fatalf("answered %+v, want the stored case with the flag set", answered)
	}

	if code := do(t, newDatasetTestRouter(repo), http.MethodPut, casePath, `{"excluded":false}`).Code; code != http.StatusOK {
		t.Fatalf("include: expected 200, got %d", code)
	}
	if repo.stored.Excluded {
		t.Fatal("the case was not included again")
	}
}

// `{"excluded": null}` names the flag and gives it no value; it is refused
// rather than read as either value.
func TestExclusionOnlyUpdateRefusesANullFlag(t *testing.T) {
	t.Parallel()

	repo := newCaseRepo()
	response := do(t, newDatasetTestRouter(repo), http.MethodPut, casePath, `{"excluded":null}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", response.Code, response.Body.String())
	}
	if repo.setFlagCalls != 0 || repo.updateCaseCalls != 0 {
		t.Fatal("a refused body reached the repository")
	}
}

// Any body other than the flag-only form is a full rewrite, and a full
// rewrite still needs its input: `excluded` plus another key is not a toggle.
func TestFullCaseUpdateStillRequiresTheInput(t *testing.T) {
	t.Parallel()

	repo := newCaseRepo()
	response := do(t, newDatasetTestRouter(repo), http.MethodPut, casePath, `{"variables":{},"excluded":true}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", response.Code, response.Body.String())
	}
	if repo.setFlagCalls != 0 || repo.updateCaseCalls != 0 {
		t.Fatal("a refused body reached the repository")
	}

	full := do(t, newDatasetTestRouter(repo), http.MethodPut, casePath,
		`{"input":"rewritten","variables":{},"expected_output":null,"excluded":true}`)
	if full.Code != http.StatusOK || repo.updateCaseCalls != 1 || repo.stored.Input != "rewritten" {
		t.Fatalf("full rewrite: %d, UpdateCase calls %d, stored %+v", full.Code, repo.updateCaseCalls, repo.stored)
	}
}

// An oversized body is refused with 413 BEFORE it is decoded, on every
// evaluation write that takes a body.
func TestEvaluationWritesRefuseAnOversizedBody(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("x", evaluation.MaxWriteBodyBytes+1)

	cases := []struct {
		name, method, path, body string
	}{
		{"case create", http.MethodPost, "/eval_dataset_cases/prompt_lib/1/5", `{"input":"` + huge + `"}`},
		{"case update", http.MethodPut, casePath, `{"input":"` + huge + `"}`},
		{"dataset create", http.MethodPost, "/eval_datasets/prompt_lib/1", `{"name":"` + huge + `"}`},
	}
	for _, tc := range cases {
		repo := newCaseRepo()
		response := do(t, newDatasetTestRouter(repo), tc.method, tc.path, tc.body)
		if response.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: expected 413, got %d", tc.name, response.Code)
		}
		if repo.addCaseCalls+repo.updateCaseCalls+repo.setFlagCalls+repo.createCalls != 0 {
			t.Errorf("%s: an oversized body reached the repository", tc.name)
		}
	}

	dimensions := &recordingRepo{}
	body := strings.Replace(validBody, `"Does the answer help?"`, `"`+huge+`"`, 1)
	if code := do(t, newTestRouter(dimensions), http.MethodPost, "/eval_dimensions/prompt_lib/1", body).Code; code != http.StatusRequestEntityTooLarge {
		t.Errorf("dimension create: expected 413, got %d", code)
	}
	if len(dimensions.stored) != 0 {
		t.Error("dimension create: an oversized body was stored")
	}
}

// The per-field caps hold for bodies under the transport bound: case
// variables, and a dimension's rubric and code, each sent to a model on every
// run that uses them.
func TestEvaluationFieldCaps(t *testing.T) {
	t.Parallel()

	bigVariables := `{"input":"q","variables":{"v":"` + strings.Repeat("x", evaluation.MaxCaseVariablesBytes) + `"}}`
	repo := newCaseRepo()
	if code := do(t, newDatasetTestRouter(repo), http.MethodPost, "/eval_dataset_cases/prompt_lib/1/5", bigVariables).Code; code != http.StatusBadRequest {
		t.Errorf("case variables over the cap: expected 400, got %d", code)
	}
	if repo.addCaseCalls != 0 {
		t.Error("an over-cap case reached the repository")
	}

	longRubric := strings.Replace(validBody, `"Does the answer help?"`,
		`"`+strings.Repeat("x", evaluation.MaxDimensionDescriptionBytes+1)+`"`, 1)
	dimensions := &recordingRepo{}
	if code := do(t, newTestRouter(dimensions), http.MethodPost, "/eval_dimensions/prompt_lib/1", longRubric).Code; code != http.StatusBadRequest {
		t.Errorf("rubric over the cap: expected 400, got %d", code)
	}

	longCode := strings.NewReplacer(
		`"allowed_engines": ["ai"]`, `"allowed_engines": ["code"]`,
		`"code": ""`, `"code": "`+strings.Repeat("x", evaluation.MaxDimensionCodeBytes+1)+`"`,
		`"return_contract": ""`, `"return_contract": "bool"`,
	).Replace(validBody)
	if code := do(t, newTestRouter(dimensions), http.MethodPost, "/eval_dimensions/prompt_lib/1", longCode).Code; code != http.StatusBadRequest {
		t.Errorf("code over the cap: expected 400, got %d", code)
	}
	if len(dimensions.stored) != 0 {
		t.Error("an over-cap dimension was stored")
	}
}
