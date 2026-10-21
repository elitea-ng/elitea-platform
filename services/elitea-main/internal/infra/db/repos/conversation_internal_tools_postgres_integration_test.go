package repos

// Issue 1060 (client contract 1.14): updateConversationInternalTools writes
// `meta.internal_tools` alone, and the PUT keeps a stored list its document
// does not state, so the two never clobber each other. Through the real
// handler and repository.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type internalToolsFixture struct {
	repo   *ConversationsRepo
	router chi.Router
	uuid   string
}

func newInternalToolsFixture(t *testing.T) *internalToolsFixture {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	grantPinMembership(t, pool, 1, 7)
	repo := NewConversationsRepo(pool)
	_, uuid, _ := seedConversationWithParticipant(t, repo, "hello")
	if _, err := pool.Exec(context.Background(),
		`UPDATE p_1.chat_conversations SET meta = '{"steps_limit": 9, "internal_tools": ["planner"]}'::jsonb WHERE uuid = $1::uuid`, uuid); err != nil {
		t.Fatalf("seed meta: %v", err)
	}
	handler := conversations.NewHandler(repo).WithPool(pool)
	router := chi.NewRouter()
	router.Put("/conversation/{projectID}/{conversationID}", handler.Update)
	router.Patch("/internal_tools/{projectID}/{conversationID}", handler.UpdateInternalTools)
	return &internalToolsFixture{repo: repo, router: router, uuid: uuid}
}

func (f *internalToolsFixture) do(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{UserID: "7"}))
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.String()
}

func (f *internalToolsFixture) meta(t *testing.T) map[string]any {
	t.Helper()
	conversation, err := f.repo.Get(asUser("7"), "1", f.uuid)
	if err != nil {
		t.Fatalf("get conversation: %v", err)
	}
	return conversation.Meta
}

func metaTools(meta map[string]any) []string {
	raw, _ := meta["internal_tools"].([]any)
	tools := make([]string, 0, len(raw))
	for _, value := range raw {
		name, _ := value.(string)
		tools = append(tools, name)
	}
	return tools
}

func TestInternalToolsPatchWritesOnlyTheList(t *testing.T) {
	f := newInternalToolsFixture(t)

	code, body := f.do(t, http.MethodPatch, "/internal_tools/1/"+f.uuid, `{"internal_tools": ["attachments", "ask_user", "attachments"]}`)
	if code != http.StatusOK {
		t.Fatalf("PATCH answered %d: %s", code, body)
	}
	var answered map[string]any
	if err := json.Unmarshal([]byte(body), &answered); err != nil {
		t.Fatalf("decode: %v", err)
	}
	meta := f.meta(t)
	if got := metaTools(meta); !slices.Equal(got, []string{"attachments", "ask_user"}) {
		t.Fatalf("internal_tools = %v, want [attachments ask_user] (repeats dropped)", got)
	}
	if meta["steps_limit"] != float64(9) {
		t.Fatalf("meta = %v: the PATCH touched another key", meta)
	}
	if answeredMeta, _ := answered["meta"].(map[string]any); !slices.Equal(metaTools(answeredMeta), []string{"attachments", "ask_user"}) {
		t.Fatalf("answer = %s, want the updated conversation", body)
	}

	if code, _ := f.do(t, http.MethodPatch, "/internal_tools/1/"+f.uuid, `{"internal_tools": []}`); code != http.StatusOK {
		t.Fatalf("clearing answered %d", code)
	}
	if got := metaTools(f.meta(t)); len(got) != 0 {
		t.Fatalf("internal_tools = %v, want []", got)
	}
}

func TestInternalToolsPatchRefusesNamesOutsideTheCatalogue(t *testing.T) {
	f := newInternalToolsFixture(t)
	for _, body := range []string{
		`{"internal_tools": ["planner", "rm_rf"]}`,
		`{"internal_tools": [1]}`,
		`{"internal_tools": null}`,
		`{}`,
		`{"internal_tools": ["planner"], "steps_limit": 1}`,
		`[]`,
	} {
		if code, raw := f.do(t, http.MethodPatch, "/internal_tools/1/"+f.uuid, body); code != http.StatusBadRequest {
			t.Errorf("%s: answered %d (%s), want 400", body, code, raw)
		}
	}
	if got := metaTools(f.meta(t)); !slices.Equal(got, []string{"planner"}) {
		t.Fatalf("internal_tools = %v after refused writes, want [planner]", got)
	}
}

func TestInternalToolsPatchHidesAConversationTheCallerCannotSee(t *testing.T) {
	f := newInternalToolsFixture(t)
	request := httptest.NewRequest(http.MethodPatch, "/internal_tools/1/"+f.uuid, strings.NewReader(`{"internal_tools": []}`))
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{UserID: "8"}))
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("an outsider's PATCH answered %d, want 404", recorder.Code)
	}
	if got := metaTools(f.meta(t)); !slices.Equal(got, []string{"planner"}) {
		t.Fatalf("internal_tools = %v, want it untouched", got)
	}
}

// The PUT keeps a stored list its document does not state, and still
// replaces it when the document states one.
func TestConversationPutKeepsInternalToolsItDoesNotState(t *testing.T) {
	f := newInternalToolsFixture(t)
	if code, raw := f.do(t, http.MethodPut, "/conversation/1/"+f.uuid, `{"meta": {"steps_limit": 3}}`); code != http.StatusOK {
		t.Fatalf("PUT answered %d: %s", code, raw)
	}
	meta := f.meta(t)
	if meta["steps_limit"] != float64(3) || !slices.Equal(metaTools(meta), []string{"planner"}) {
		t.Fatalf("meta = %v, want steps_limit 3 and the stored internal_tools kept", meta)
	}
	if code, raw := f.do(t, http.MethodPut, "/conversation/1/"+f.uuid, `{"meta": {"steps_limit": 3, "internal_tools": []}}`); code != http.StatusOK {
		t.Fatalf("PUT answered %d: %s", code, raw)
	}
	if got := metaTools(f.meta(t)); len(got) != 0 {
		t.Fatalf("internal_tools = %v, want the stated [] to replace it", got)
	}
}

// Concurrent PUTs of other settings and PATCHes of the list: whatever the
// interleaving, the last PATCH's list and the last PUT's settings both
// survive.
func TestConcurrentPutAndInternalToolsPatchDoNotClobberEachOther(t *testing.T) {
	f := newInternalToolsFixture(t)
	const rounds = 20
	var wg sync.WaitGroup
	errs := make(chan string, 2*rounds)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range rounds {
			if code, raw := f.do(t, http.MethodPut, "/conversation/1/"+f.uuid, fmt.Sprintf(`{"meta": {"steps_limit": %d}}`, i)); code != http.StatusOK {
				errs <- fmt.Sprintf("PUT %d answered %d: %s", i, code, raw)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := range rounds {
			tools := `["attachments"]`
			if i%2 == 1 {
				tools = `["attachments", "planner"]`
			}
			if code, raw := f.do(t, http.MethodPatch, "/internal_tools/1/"+f.uuid, `{"internal_tools": `+tools+`}`); code != http.StatusOK {
				errs <- fmt.Sprintf("PATCH %d answered %d: %s", i, code, raw)
			}
		}
	}()
	wg.Wait()
	close(errs)
	for message := range errs {
		t.Error(message)
	}
	meta := f.meta(t)
	if meta["steps_limit"] != float64(rounds-1) {
		t.Fatalf("steps_limit = %v, want the last PUT's %d", meta["steps_limit"], rounds-1)
	}
	if got := metaTools(meta); !slices.Equal(got, []string{"attachments", "planner"}) {
		t.Fatalf("internal_tools = %v, want the last PATCH's [attachments planner]", got)
	}
}
