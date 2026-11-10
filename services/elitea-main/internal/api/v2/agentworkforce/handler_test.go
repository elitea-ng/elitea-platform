package agentworkforce

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentworkforce"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

type fakeStore struct {
	Store
	proposed  repos.NewWorkforceProposal
	decided   repos.Decision
	appeal    repos.AppealDecision
	reinstate repos.WorkforceActor
	refuse    error
}

func (f *fakeStore) ProposeByPerson(_ context.Context, request repos.NewWorkforceProposal) (repos.WorkforceProposal, error) {
	f.proposed = request
	return repos.WorkforceProposal{ID: 1, Stage: string(request.Input.Stage)}, f.refuse
}

func (f *fakeStore) Approve(_ context.Context, decision repos.Decision) (repos.WorkforceProposal, error) {
	f.decided = decision
	return repos.WorkforceProposal{ID: decision.ProposalID, Status: "approved"}, f.refuse
}

func (f *fakeStore) DecideAppeal(_ context.Context, decision repos.AppealDecision) (repos.WorkforceProposal, error) {
	f.appeal = decision
	return repos.WorkforceProposal{ID: decision.ProposalID}, f.refuse
}

func (f *fakeStore) Reinstate(_ context.Context, _, _ int64, actor repos.WorkforceActor, _ string) (repos.WorkforceAgent, error) {
	f.reinstate = actor
	return repos.WorkforceAgent{State: "active"}, f.refuse
}

func (f *fakeStore) ListProposals(context.Context, int64, repos.WorkforceFilter) ([]repos.WorkforceProposal, int, error) {
	return []repos.WorkforceProposal{}, 0, nil
}

func serve(t *testing.T, store *fakeStore, manager bool, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	handler := NewHandler(store, func(_ *http.Request, projectID, permission string) (bool, error) {
		return manager && projectID == "7" && permission == ManagePermission, nil
	})
	router := chi.NewRouter()
	router.Get(ProposalsPath, handler.List)
	router.Post(ProposalsPath, handler.Propose)
	router.Post(ApprovePath, handler.Approve)
	router.Post(AppealDecidePath, handler.DecideAppeal)
	router.Post(ReinstatePath, handler.Reinstate)
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: "4", UserID: "4", AuthType: "user"}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestProposeCarriesTheManagerFlagAndTheInput(t *testing.T) {
	store := &fakeStore{}
	got := serve(t, store, true, http.MethodPost, "/agent_workforce/prompt_lib/7/proposals",
		`{"application_id":9,"stage":"demote","rationale":"poor","narrowing":{"remove_lead":true}}`)
	if got.Code != http.StatusCreated {
		t.Fatalf("status %d %s", got.Code, got.Body)
	}
	if !store.proposed.Actor.Manager || store.proposed.Actor.UserID != 4 || store.proposed.ApplicationID != 9 ||
		store.proposed.Input.Stage != agentworkforce.StageDemote || !store.proposed.Input.Narrowing.RemoveLead {
		t.Fatalf("proposed %+v", store.proposed)
	}
	if got := serve(t, store, false, http.MethodPost, "/agent_workforce/prompt_lib/7/proposals", `{"application_id":9,"stage":"watch","rationale":"x","extra":1}`); got.Code != http.StatusBadRequest {
		t.Fatalf("an unknown field: %d", got.Code)
	}
	if got := serve(t, store, false, http.MethodPost, "/agent_workforce/prompt_lib/7/proposals", `{"stage":"watch","rationale":"x"}`); got.Code != http.StatusBadRequest {
		t.Fatalf("no application: %d", got.Code)
	}
}

func TestDecisionsMapRefusalsAndAcceptAnEmptyBody(t *testing.T) {
	store := &fakeStore{}
	if got := serve(t, store, false, http.MethodPost, "/agent_workforce/prompt_lib/7/proposals/3/approve", ""); got.Code != http.StatusOK {
		t.Fatalf("empty body: %d %s", got.Code, got.Body)
	}
	if store.decided.ProposalID != 3 || store.decided.Actor.Manager {
		t.Fatalf("decided %+v", store.decided)
	}
	serve(t, store, true, http.MethodPost, "/agent_workforce/prompt_lib/7/proposals/3/approve", `{"confirm_nested":true,"note":"ok"}`)
	if !store.decided.ConfirmNested || store.decided.Note != "ok" || !store.decided.Actor.Manager {
		t.Fatalf("decided %+v", store.decided)
	}
	store.refuse = &agentworkforce.Refusal{Code: agentworkforce.ConfirmNestedCode, Message: "confirm", Detail: map[string]any{"nested_by": []int{1}}}
	got := serve(t, store, true, http.MethodPost, "/agent_workforce/prompt_lib/7/proposals/3/approve", "")
	var body map[string]any
	_ = json.Unmarshal(got.Body.Bytes(), &body)
	if got.Code != http.StatusConflict || body["code"] != agentworkforce.ConfirmNestedCode || body["nested_by"] == nil {
		t.Fatalf("refusal %d %v", got.Code, body)
	}
	store.refuse = agentworkforce.Refuse(agentworkforce.ForbiddenCode, "no")
	if got := serve(t, store, false, http.MethodPost, "/agent_workforce/prompt_lib/7/agents/9/reinstate", ""); got.Code != http.StatusForbidden {
		t.Fatalf("forbidden: %d", got.Code)
	}
}

func TestAppealDecisionAndListValidation(t *testing.T) {
	store := &fakeStore{}
	if got := serve(t, store, true, http.MethodPost, "/agent_workforce/prompt_lib/7/proposals/3/appeal/decide", `{"grant":true}`); got.Code != http.StatusOK || !store.appeal.Grant {
		t.Fatalf("appeal %d %+v", got.Code, store.appeal)
	}
	if got := serve(t, store, true, http.MethodGet, "/agent_workforce/prompt_lib/7/proposals?status=open", ""); got.Code != http.StatusBadRequest {
		t.Fatalf("status=open: %d", got.Code)
	}
	if got := serve(t, store, true, http.MethodGet, "/agent_workforce/prompt_lib/7/proposals?status=all&application_id=9", ""); got.Code != http.StatusOK {
		t.Fatalf("status=all: %d", got.Code)
	}
	if got := serve(t, store, true, http.MethodGet, "/agent_workforce/prompt_lib/x/proposals", ""); got.Code != http.StatusBadRequest {
		t.Fatalf("bad project: %d", got.Code)
	}
}

// readStore answers an agent owned by person ownerID and one proposal for it.
type readStore struct {
	fakeStore
	ownerID int64
	read    []string
}

func (f *readStore) Agent(_ context.Context, _, applicationID int64) (repos.WorkforceAgent, error) {
	return repos.WorkforceAgent{ApplicationID: applicationID, BusinessOwnerID: f.ownerID, State: "active"}, nil
}

func (f *readStore) Scorecards(context.Context, int64, int64, int) ([]agentworkforce.Scorecard, error) {
	f.read = append(f.read, "scorecards")
	return []agentworkforce.Scorecard{{}}, nil
}

func (f *readStore) Impact(context.Context, int64, int64) (repos.WorkforceImpact, error) {
	f.read = append(f.read, "impact")
	return repos.WorkforceImpact{}, nil
}

func (f *readStore) GetProposal(_ context.Context, _, proposalID int64) (repos.WorkforceProposal, error) {
	return repos.WorkforceProposal{ID: proposalID, ApplicationID: 9, Rationale: "secret"}, nil
}

func (f *readStore) ListProposals(context.Context, int64, repos.WorkforceFilter) ([]repos.WorkforceProposal, int, error) {
	f.read = append(f.read, "list")
	return []repos.WorkforceProposal{}, 0, nil
}

func serveRead(t *testing.T, store *readStore, manager bool, path string) *httptest.ResponseRecorder {
	t.Helper()
	handler := NewHandler(store, func(_ *http.Request, _, permission string) (bool, error) {
		return manager && permission == ManagePermission, nil
	})
	router := chi.NewRouter()
	router.Get(AgentPath, handler.GetAgent)
	router.Get(ScorecardsPath, handler.ListScorecards)
	router.Get(ImpactPath, handler.GetImpact)
	router.Get(ProposalsPath, handler.List)
	router.Get(ProposalPath, handler.Get)
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: "4", UserID: "4", AuthType: "user"}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// ADR-0050 5h: scorecards, the impact preview and the proposals (with
// their rationales, owner comments and appeal reasons) are read by agent
// managers and the agent's owner, not by everyone who can list agents.
func TestReadsNeedAManagerOrTheOwner(t *testing.T) {
	reads := []string{
		"/agent_workforce/prompt_lib/7/agents/9/scorecards",
		"/agent_workforce/prompt_lib/7/agents/9/impact",
		"/agent_workforce/prompt_lib/7/proposals/3",
		"/agent_workforce/prompt_lib/7/proposals?application_id=9",
		"/agent_workforce/prompt_lib/7/proposals",
	}
	for _, path := range reads {
		stranger := &readStore{ownerID: 5}
		if got := serveRead(t, stranger, false, path); got.Code != http.StatusForbidden || strings.Contains(got.Body.String(), "secret") {
			t.Errorf("a non-owner read %s: %d %s", path, got.Code, got.Body)
		}
		if len(stranger.read) != 0 {
			t.Errorf("%s reached the store for a non-owner: %v", path, stranger.read)
		}
		if got := serveRead(t, &readStore{ownerID: 5}, true, path); got.Code != http.StatusOK {
			t.Errorf("a manager read %s: %d %s", path, got.Code, got.Body)
		}
	}
	// The owner reads their own agent's, not the project-wide inbox.
	for _, path := range reads[:4] {
		if got := serveRead(t, &readStore{ownerID: 4}, false, path); got.Code != http.StatusOK {
			t.Errorf("the owner read %s: %d %s", path, got.Code, got.Body)
		}
	}
	if got := serveRead(t, &readStore{ownerID: 4}, false, reads[4]); got.Code != http.StatusForbidden {
		t.Errorf("the owner read the project inbox: %d", got.Code)
	}
	// The ladder state stays visible; the scorecard does not.
	stranger := &readStore{ownerID: 5}
	got := serveRead(t, stranger, false, "/agent_workforce/prompt_lib/7/agents/9")
	var view map[string]any
	_ = json.Unmarshal(got.Body.Bytes(), &view)
	if got.Code != http.StatusOK || view["scorecard"] != nil || len(stranger.read) != 0 {
		t.Fatalf("a non-owner's agent view: %d %v %v", got.Code, view, stranger.read)
	}
}
