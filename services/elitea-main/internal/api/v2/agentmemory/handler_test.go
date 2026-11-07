package agentmemory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// fakeStore answers like repos.AgentMemory for one entry about user 9.
type fakeStore struct {
	owner     bool
	updated   *repos.AgentMemoryUpdate
	approver  bool
	viewers   []repos.AgentMemoryViewer
	cleared   bool
	deleteErr error
}

func (f *fakeStore) ListMemory(_ context.Context, _, _ int64, viewer repos.AgentMemoryViewer, _ repos.AgentMemoryFilter) ([]repos.AgentMemoryEntry, int, error) {
	f.viewers = append(f.viewers, viewer)
	return []repos.AgentMemoryEntry{}, 0, nil
}

func (f *fakeStore) GetMemory(_ context.Context, _, _, _ int64, viewer repos.AgentMemoryViewer) (repos.AgentMemoryEntry, error) {
	f.viewers = append(f.viewers, viewer)
	if !viewer.Manager && viewer.UserID != 9 {
		return repos.AgentMemoryEntry{}, repos.ErrAgentMemoryNotFound
	}
	return repos.AgentMemoryEntry{}, nil
}

func (f *fakeStore) UpdateMemory(_ context.Context, _, _, _ int64, viewer repos.AgentMemoryViewer, approver bool, update repos.AgentMemoryUpdate) (repos.AgentMemoryEntry, error) {
	f.viewers = append(f.viewers, viewer)
	f.approver, f.updated = approver, &update
	return repos.AgentMemoryEntry{}, nil
}

func (f *fakeStore) DeleteMemory(_ context.Context, _, _, _ int64, viewer repos.AgentMemoryViewer) error {
	f.viewers = append(f.viewers, viewer)
	return f.deleteErr
}

func (f *fakeStore) ClearMemory(context.Context, int64, int64, int64) (int64, error) {
	f.cleared = true
	return 3, nil
}

func (f *fakeStore) SetSubjectOptOut(context.Context, int64, int64, int64, bool) error { return nil }

func (f *fakeStore) SubjectOptedOut(context.Context, int64, int64, int64) (bool, error) {
	return false, nil
}

func (f *fakeStore) IsAgentOwner(context.Context, int64, int64, int64) (bool, error) {
	return f.owner, nil
}

func serve(t *testing.T, store *fakeStore, held map[string]bool, userID, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	handler := NewHandler(store, func(_ *http.Request, permission string) (bool, error) { return held[permission], nil })
	router := chi.NewRouter()
	router.Get(EntriesPath, handler.List)
	router.Delete(EntriesPath, handler.Clear)
	router.Get(EntryPath, handler.Get)
	router.Patch(EntryPath, handler.Update)
	router.Delete(EntryPath, handler.Delete)
	router.Put(OptOutPath, handler.SetOptOut)
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: userID, UserID: userID, AuthType: "user"}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestAnEntryAboutSomeoneElseIsHidden(t *testing.T) {
	store := &fakeStore{}
	if got := serve(t, store, nil, "4", http.MethodGet, "/agent_memory/prompt_lib/7/11/5", ""); got.Code != http.StatusNotFound {
		t.Fatalf("member reading another's entry: %d", got.Code)
	}
	if got := serve(t, store, nil, "9", http.MethodGet, "/agent_memory/prompt_lib/7/11/5", ""); got.Code != http.StatusOK {
		t.Fatalf("the subject reading their entry: %d", got.Code)
	}
	if got := serve(t, store, map[string]bool{UpdatePermission: true}, "4", http.MethodGet, "/agent_memory/prompt_lib/7/11/5", ""); got.Code != http.StatusOK {
		t.Fatalf("a manager reading it: %d", got.Code)
	}
}

func TestClearNeedsDeleteOrOwnership(t *testing.T) {
	store := &fakeStore{}
	if got := serve(t, store, map[string]bool{UpdatePermission: true}, "4", http.MethodDelete, "/agent_memory/prompt_lib/7/11", ""); got.Code != http.StatusForbidden || store.cleared {
		t.Fatalf("clear by an editor: %d", got.Code)
	}
	if got := serve(t, &fakeStore{owner: true}, nil, "4", http.MethodDelete, "/agent_memory/prompt_lib/7/11", ""); got.Code != http.StatusOK {
		t.Fatalf("clear by the owner: %d", got.Code)
	}
}

func TestDeleteAndApproveUseTheirOwnPermission(t *testing.T) {
	store := &fakeStore{}
	serve(t, store, map[string]bool{UpdatePermission: true}, "4", http.MethodDelete, "/agent_memory/prompt_lib/7/11/5", "")
	if store.viewers[0].Manager {
		t.Fatal("memory.update alone must not delete other people's entries")
	}
	store = &fakeStore{}
	got := serve(t, store, map[string]bool{UpdatePermission: true}, "4", http.MethodPatch, "/agent_memory/prompt_lib/7/11/5", `{"status":"active"}`)
	if got.Code != http.StatusOK || store.approver || !store.viewers[0].Manager {
		t.Fatalf("approve without memory.approve: %d approver=%v", got.Code, store.approver)
	}
	store = &fakeStore{}
	serve(t, store, map[string]bool{ApprovePermission: true}, "4", http.MethodPatch, "/agent_memory/prompt_lib/7/11/5", `{"status":"active"}`)
	if !store.approver {
		t.Fatal("memory.approve approves")
	}
}

func TestUpdateRefusesBadBodies(t *testing.T) {
	for _, body := range []string{`{}`, `{"secret":1}`, `not json`} {
		if got := serve(t, &fakeStore{}, nil, "9", http.MethodPatch, "/agent_memory/prompt_lib/7/11/5", body); got.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", body, got.Code)
		}
	}
	if got := serve(t, &fakeStore{}, nil, "9", http.MethodPut, "/agent_memory_opt_out/prompt_lib/7/11", `{}`); got.Code != http.StatusBadRequest {
		t.Fatalf("opt out without a value: %d", got.Code)
	}
	if got := serve(t, &fakeStore{}, nil, "9", http.MethodGet, "/agent_memory/prompt_lib/7/11?kind=secret", ""); got.Code != http.StatusBadRequest {
		t.Fatalf("bad kind: %d", got.Code)
	}
}
