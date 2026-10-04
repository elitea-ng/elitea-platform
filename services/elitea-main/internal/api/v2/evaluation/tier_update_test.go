package evaluation_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

func statusOf(err error) int {
	var apiErr *apierr.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status
	}
	return 0
}

func adhocRepo() *recordingRepo {
	agent := 7
	return &recordingRepo{stored: []evaluation.Dimension{{
		ID: "1", Name: "Helpfulness", Tier: evaluation.TierAgentAdhoc, ApplicationID: &agent,
		AllowedEngines: []string{evaluation.EngineAI}, ScaleType: evaluation.ScaleContinuous,
		ScaleMin: 0, ScaleMax: 100, Polarity: evaluation.PolarityHigherBetter, DefaultWeight: 1,
	}}}
}

func bodyWithTier(tier string) string {
	if tier == "" {
		return strings.Replace(validBody, `"tier": "project",`, "", 1)
	}
	return strings.Replace(validBody, `"tier": "project"`, `"tier": "`+tier+`"`, 1)
}

// Legacy issue 6669: the owner of a private project could not make an agent
// dimension available across the project. A PUT with `tier: project` on an
// `agent_adhoc` row now promotes it and clears the agent.
func TestUpdatePromotesAnAgentDimensionToTheProject(t *testing.T) {
	t.Parallel()

	repo := adhocRepo()
	response := do(t, newTestRouter(repo), http.MethodPut, "/eval_dimension/prompt_lib/1/1", bodyWithTier("project"))
	if response.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d: %s", response.Code, response.Body.String())
	}
	var got evaluation.Dimension
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Tier != evaluation.TierProject || got.ApplicationID != nil {
		t.Fatalf("answered tier %q application %v, want project and no agent", got.Tier, got.ApplicationID)
	}
	if repo.stored[0].Tier != evaluation.TierProject || repo.stored[0].ApplicationID != nil {
		t.Fatalf("stored tier %q application %v, want project and no agent", repo.stored[0].Tier, repo.stored[0].ApplicationID)
	}
}

// Demotion is refused with 409 and the stored row does not change.
func TestUpdateRefusesToDemoteAProjectDimension(t *testing.T) {
	t.Parallel()

	repo := &recordingRepo{}
	router := newTestRouter(repo)
	if created := do(t, router, http.MethodPost, "/eval_dimensions/prompt_lib/1", validBody); created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	body := strings.Replace(bodyWithTier("agent_adhoc"), `"code": ""`, `"application_id": 7, "code": ""`, 1)
	response := do(t, router, http.MethodPut, "/eval_dimension/prompt_lib/1/1", body)
	if response.Code != http.StatusConflict {
		t.Fatalf("update: expected 409, got %d: %s", response.Code, response.Body.String())
	}
	if repo.stored[0].Tier != evaluation.TierProject {
		t.Fatalf("stored tier = %q, want project", repo.stored[0].Tier)
	}
}

// A body WITHOUT a tier keeps the stored scope. Normalize would otherwise
// default it to `project` and promote the dimension without a request.
func TestUpdateWithoutATierKeepsTheStoredScope(t *testing.T) {
	t.Parallel()

	repo := adhocRepo()
	response := do(t, newTestRouter(repo), http.MethodPut, "/eval_dimension/prompt_lib/1/1", bodyWithTier(""))
	if response.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d: %s", response.Code, response.Body.String())
	}
	if repo.stored[0].Tier != evaluation.TierAgentAdhoc || repo.stored[0].ApplicationID == nil {
		t.Fatalf("stored tier %q application %v, want the agent scope kept", repo.stored[0].Tier, repo.stored[0].ApplicationID)
	}
}

// An update body's `application_id` cannot re-bind an agent dimension to
// another agent, with no tier or with the stored `agent_adhoc` tier. The
// handler must hand the repository NO application id at all: the repository
// keeps the stored one, so a refactor that forwarded the body's value would
// move a rubric between agents silently.
func TestUpdateCannotRebindAnAgentDimensionToAnotherAgent(t *testing.T) {
	t.Parallel()

	for _, tier := range []string{"", "agent_adhoc"} {
		repo := &bindingRecordingRepo{recordingRepo: adhocRepo()}
		body := strings.Replace(bodyWithTier(tier), `"code": ""`, `"application_id": 2, "code": ""`, 1)
		response := do(t, newTestRouter(repo), http.MethodPut, "/eval_dimension/prompt_lib/1/1", body)
		if response.Code != http.StatusOK {
			t.Fatalf("tier %q: expected 200, got %d: %s", tier, response.Code, response.Body.String())
		}
		if repo.sawApplicationID != nil {
			t.Errorf("tier %q: the handler forwarded application_id %d to the repository", tier, *repo.sawApplicationID)
		}
		stored := repo.stored[0]
		if stored.Tier != evaluation.TierAgentAdhoc || stored.ApplicationID == nil || *stored.ApplicationID != 7 {
			t.Errorf("tier %q: stored tier %q application %v, want agent_adhoc bound to agent 7", tier, stored.Tier, stored.ApplicationID)
		}
	}
}

// bindingRecordingRepo records the application id the handler handed to Update.
type bindingRecordingRepo struct {
	*recordingRepo
	sawApplicationID *int
}

func (r *bindingRecordingRepo) Update(ctx context.Context, projectID, id string, d evaluation.Dimension) (evaluation.Dimension, error) {
	r.sawApplicationID = d.ApplicationID
	return r.recordingRepo.Update(ctx, projectID, id, d)
}

// The update body may not ask for the platform tier or an unknown one.
func TestUpdateRefusesAnUnauthorableTier(t *testing.T) {
	t.Parallel()

	for _, tier := range []string{"platform", "galaxy"} {
		repo := adhocRepo()
		response := do(t, newTestRouter(repo), http.MethodPut, "/eval_dimension/prompt_lib/1/1", bodyWithTier(tier))
		if response.Code != http.StatusBadRequest {
			t.Errorf("tier %q: expected 400, got %d: %s", tier, response.Code, response.Body.String())
		}
		if repo.stored[0].Tier != evaluation.TierAgentAdhoc {
			t.Errorf("tier %q: stored tier changed to %q", tier, repo.stored[0].Tier)
		}
	}
}

func TestResolveTierUpdate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		stored, requested, want string
		status                  int
	}{
		{evaluation.TierAgentAdhoc, "", evaluation.TierAgentAdhoc, 0},
		{evaluation.TierProject, "", evaluation.TierProject, 0},
		{evaluation.TierProject, evaluation.TierProject, evaluation.TierProject, 0},
		{evaluation.TierAgentAdhoc, evaluation.TierAgentAdhoc, evaluation.TierAgentAdhoc, 0},
		{evaluation.TierAgentAdhoc, evaluation.TierProject, evaluation.TierProject, 0},
		{evaluation.TierProject, evaluation.TierAgentAdhoc, "", http.StatusConflict},
		{evaluation.TierProject, evaluation.TierPlatform, "", http.StatusBadRequest},
	}
	for _, tc := range cases {
		got, err := evaluation.ResolveTierUpdate(tc.stored, tc.requested)
		if tc.status == 0 {
			if err != nil || got != tc.want {
				t.Errorf("%s -> %q: got %q %v, want %q", tc.stored, tc.requested, got, err, tc.want)
			}
			continue
		}
		recorder := statusOf(err)
		if recorder != tc.status {
			t.Errorf("%s -> %q: status %d, want %d", tc.stored, tc.requested, recorder, tc.status)
		}
	}
}
