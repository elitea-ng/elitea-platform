package agentproposals

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentproposal"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// fakeStore answers like repos.AgentProposals for proposal 5 of agent 11,
// with two seeds.
type fakeStore struct {
	dismissed bool
	created   int64
	seeded    []repos.CopiedMemory
	seedTo    [2]int64
	copyFrom  [2]int64
	decideErr error
	notFork   bool
}

func (f *fakeStore) ListProposals(context.Context, int64, repos.AgentProposalFilter) ([]repos.AgentProposal, int, error) {
	return []repos.AgentProposal{{ID: 5}}, 1, nil
}

func (f *fakeStore) GetProposal(_ context.Context, _, proposalID int64) (repos.AgentProposal, error) {
	if proposalID != 5 {
		return repos.AgentProposal{}, repos.ErrProposalNotFound
	}
	return repos.AgentProposal{ID: 5, ProposerApplicationID: 11, MemorySeeds: []agentproposal.Seed{
		{Kind: "fact", Content: "uses Go"}, {Kind: "preference", Content: "short answers"},
	}}, nil
}

func (f *fakeStore) DismissProposal(context.Context, int64, int64, int64) error {
	f.dismissed = true
	return f.decideErr
}

func (f *fakeStore) MarkProposalCreated(_ context.Context, _, _, _, applicationID int64) error {
	f.created = applicationID
	return f.decideErr
}

func (f *fakeStore) SeedAgentMemory(_ context.Context, projectID, applicationID, _ int64, entries []repos.CopiedMemory, _, _ string) (int, error) {
	f.seedTo, f.seeded = [2]int64{projectID, applicationID}, entries
	return len(entries), nil
}

func (f *fakeStore) IsForkOfBy(context.Context, int64, int64, int64, int64, int64) (bool, error) {
	return !f.notFork, nil
}

func (f *fakeStore) MemoryEntriesForCopy(_ context.Context, projectID, applicationID int64, _ []int64) ([]repos.CopiedMemory, error) {
	f.copyFrom = [2]int64{projectID, applicationID}
	return []repos.CopiedMemory{{Kind: "fact", Content: "uses Go"}}, nil
}

type fakeOwners bool

func (f fakeOwners) IsAgentOwner(context.Context, int64, int64, int64) (bool, error) {
	return bool(f), nil
}

type personalProject int

func (p personalProject) PersonalProjectID(context.Context, string) (int, error) { return int(p), nil }

// held is "projectID:permission".
func serve(t *testing.T, store *fakeStore, owner bool, held map[string]bool, personal int, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	handler := NewHandler(store, fakeOwners(owner), func(_ *http.Request, projectID, permission string) (bool, error) {
		return held[projectID+":"+permission], nil
	}, personalProject(personal))
	router := chi.NewRouter()
	router.Get(ProposalsPath, handler.List)
	router.Get(ProposalPath, handler.Get)
	router.Post(DismissPath, handler.Dismiss)
	router.Post(CreatedPath, handler.MarkCreated)
	router.Post(CopyMemoryPath, handler.CopyMemory)
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: "4", UserID: "4", AuthType: "user"}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestListRefusesAnUnknownStatus(t *testing.T) {
	if got := serve(t, &fakeStore{}, false, nil, 0, http.MethodGet, "/agent_proposals/prompt_lib/7?status=open", ""); got.Code != http.StatusBadRequest {
		t.Fatalf("status=open: %d", got.Code)
	}
	if got := serve(t, &fakeStore{}, false, nil, 0, http.MethodGet, "/agent_proposals/prompt_lib/7?status=all", ""); got.Code != http.StatusOK {
		t.Fatalf("status=all: %d", got.Code)
	}
}

func TestDismissTakesCreateOrOwnershipOfTheProposer(t *testing.T) {
	store := &fakeStore{}
	if got := serve(t, store, false, nil, 0, http.MethodPost, "/agent_proposals/prompt_lib/7/5/dismiss", ""); got.Code != http.StatusForbidden || store.dismissed {
		t.Fatalf("a reader dismissing: %d", got.Code)
	}
	store = &fakeStore{}
	if got := serve(t, store, true, nil, 0, http.MethodPost, "/agent_proposals/prompt_lib/7/5/dismiss", ""); got.Code != http.StatusOK || !store.dismissed {
		t.Fatalf("the owner dismissing: %d", got.Code)
	}
	store = &fakeStore{}
	if got := serve(t, store, false, map[string]bool{"7:" + CreatePermission: true}, 0, http.MethodPost, "/agent_proposals/prompt_lib/7/5/dismiss", ""); got.Code != http.StatusOK {
		t.Fatalf("a creator dismissing: %d", got.Code)
	}
	store = &fakeStore{decideErr: repos.ErrProposalDecided}
	if got := serve(t, store, true, nil, 0, http.MethodPost, "/agent_proposals/prompt_lib/7/5/dismiss", ""); got.Code != http.StatusConflict {
		t.Fatalf("dismissing a decided proposal: %d", got.Code)
	}
	if got := serve(t, &fakeStore{}, true, nil, 0, http.MethodPost, "/agent_proposals/prompt_lib/7/6/dismiss", ""); got.Code != http.StatusNotFound {
		t.Fatalf("an unknown proposal: %d", got.Code)
	}
}

func TestMarkCreatedSeedsOnlyTheKeptSeedsIntoTheNewAgent(t *testing.T) {
	store := &fakeStore{}
	got := serve(t, store, false, nil, 0, http.MethodPost, "/agent_proposals/prompt_lib/7/5/created", `{"application_id":42,"memory_seeds":[1,1]}`)
	if got.Code != http.StatusOK || store.created != 42 {
		t.Fatalf("mark created: %d %d", got.Code, store.created)
	}
	if store.seedTo != [2]int64{7, 42} || len(store.seeded) != 1 || store.seeded[0].Content != "short answers" {
		t.Fatalf("seeded %v into %v", store.seeded, store.seedTo)
	}
	for _, body := range []string{`{}`, `{"application_id":42,"memory_seeds":[2]}`, `{"application_id":42,"extra":1}`} {
		if got := serve(t, &fakeStore{}, false, nil, 0, http.MethodPost, "/agent_proposals/prompt_lib/7/5/created", body); got.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", body, got.Code)
		}
	}
}

func TestCopyMemoryOnlyFromTheCallersPersonalAgent(t *testing.T) {
	body := `{"source_project_id":3,"source_application_id":9,"entry_ids":[1]}`
	path := "/agent_copy_memory/prompt_lib/7/42"
	if got := serve(t, &fakeStore{}, true, nil, 5, http.MethodPost, path, body); got.Code != http.StatusForbidden {
		t.Fatalf("a source that is not the caller's personal project: %d", got.Code)
	}
	if got := serve(t, &fakeStore{}, false, nil, 3, http.MethodPost, path, body); got.Code != http.StatusForbidden {
		t.Fatalf("a personal agent whose memory the caller cannot read: %d", got.Code)
	}
	store := &fakeStore{}
	got := serve(t, store, true, nil, 3, http.MethodPost, path, body)
	if got.Code != http.StatusOK || store.copyFrom != [2]int64{3, 9} || store.seedTo != [2]int64{7, 42} {
		t.Fatalf("copy by the owner: %d from %v to %v", got.Code, store.copyFrom, store.seedTo)
	}
	store = &fakeStore{}
	if got := serve(t, store, false, map[string]bool{"3:" + MemoryListPermission: true}, 3, http.MethodPost, path, body); got.Code != http.StatusOK {
		t.Fatalf("copy by a memory reader: %d", got.Code)
	}
	if got := serve(t, &fakeStore{}, true, nil, 7, http.MethodPost, path, `{"source_project_id":7,"source_application_id":9}`); got.Code != http.StatusBadRequest {
		t.Fatalf("a copy into the same project: %d", got.Code)
	}
	// The target must be the caller's own fork of the source: any other
	// agent of the project gets no person-written memory.
	store = &fakeStore{notFork: true}
	if got := serve(t, store, true, nil, 3, http.MethodPost, path, body); got.Code != http.StatusForbidden || store.seedTo != [2]int64{} {
		t.Fatalf("a copy into an agent that is not the caller's fork: %d into %v", got.Code, store.seedTo)
	}
}

// Create may only record (and seed) an agent the caller just made; the
// store refuses any other and the route answers 403, seeding nothing.
func TestMarkCreatedRefusesAnAgentTheCallerDidNotJustMake(t *testing.T) {
	store := &fakeStore{decideErr: repos.ErrNotYourNewAgent}
	got := serve(t, store, false, nil, 0, http.MethodPost, "/agent_proposals/prompt_lib/7/5/created", `{"application_id":42,"memory_seeds":[0]}`)
	if got.Code != http.StatusForbidden || store.seeded != nil {
		t.Fatalf("someone else's agent: %d, seeded %v", got.Code, store.seeded)
	}
}

func TestANilStoreAnswers503(t *testing.T) {
	handler := NewHandler((*repos.AgentProposals)(nil), nil, nil, nil)
	recorder := httptest.NewRecorder()
	handler.List(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d", recorder.Code)
	}
}
