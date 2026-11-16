package conversationskills

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

type repoStub struct {
	pins        []Pin
	created     bool
	err         error
	pinned      []string
	unpinned    []int64
	listCalls   int
	lastProject string
	lastConv    string
}

func (stub *repoStub) List(_ context.Context, projectID, conversationID string) ([]Pin, error) {
	stub.listCalls++
	stub.lastProject, stub.lastConv = projectID, conversationID
	return stub.pins, stub.err
}

func (stub *repoStub) PinSkill(_ context.Context, _, _ string, skillID int64) (Pin, bool, error) {
	stub.pinned = append(stub.pinned, "skill")
	id := skillID
	return Pin{ID: 11, Kind: KindProject, SkillID: &id, Name: "Triage"}, stub.created, stub.err
}

func (stub *repoStub) PinListing(_ context.Context, _, _ string, listingID, versionID int64) (Pin, bool, error) {
	stub.pinned = append(stub.pinned, "listing")
	return Pin{ID: 12, Kind: KindListing, ListingID: &listingID, ListingVersionID: &versionID, Name: "Reviewer"}, stub.created, stub.err
}

func (stub *repoStub) Unpin(_ context.Context, _, _ string, pinID int64) error {
	stub.unpinned = append(stub.unpinned, pinID)
	return stub.err
}

func allow(context.Context, string, string) error { return nil }

func deny(context.Context, string, string) error { return apierr.NotFound("chat resource not found") }

func router(repo Repository, read, write Authorize) http.Handler {
	h := NewHandler(repo, read, write)
	r := chi.NewRouter()
	r.Get("/conversation_skills/prompt_lib/{projectID}/{conversationID}", h.List)
	r.Post("/conversation_skills/prompt_lib/{projectID}/{conversationID}", h.Pin)
	r.Delete("/conversation_skill/prompt_lib/{projectID}/{conversationID}/{pinID}", h.Unpin)
	return r
}

func do(handler http.Handler, method, target, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestListAnswersThePinsAndTheLimit(t *testing.T) {
	repo := &repoStub{pins: []Pin{{ID: 1, Kind: KindProject, Name: "Triage"}}}
	response := do(router(repo, allow, allow), http.MethodGet, "/conversation_skills/prompt_lib/5/abc", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Rows  []Pin `json:"rows"`
		Total int   `json:"total"`
		Max   int   `json:"max"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || len(body.Rows) != 1 || body.Total != 1 || body.Max != MaxPins {
		t.Fatalf("body = %s err=%v", response.Body.String(), err)
	}
	// No pins is an empty list, never null.
	empty := do(router(&repoStub{}, allow, allow), http.MethodGet, "/conversation_skills/prompt_lib/5/abc", "")
	if !strings.Contains(empty.Body.String(), `"rows":[]`) {
		t.Fatalf("empty body = %s", empty.Body.String())
	}
}

func TestReadingNeedsTheReadRuleAndWritingNeedsTheWriteRule(t *testing.T) {
	repo := &repoStub{}
	if response := do(router(repo, deny, allow), http.MethodGet, "/conversation_skills/prompt_lib/5/abc", ""); response.Code != http.StatusNotFound || repo.listCalls != 0 {
		t.Fatalf("read denied: status=%d calls=%d", response.Code, repo.listCalls)
	}
	// Pinning and unpinning take the write rule, which a reader who is not a participant lacks.
	writeDenied := router(repo, allow, deny)
	if response := do(writeDenied, http.MethodPost, "/conversation_skills/prompt_lib/5/abc", `{"skill_id": 4}`); response.Code != http.StatusNotFound || len(repo.pinned) != 0 {
		t.Fatalf("pin denied: status=%d pinned=%v", response.Code, repo.pinned)
	}
	if response := do(writeDenied, http.MethodDelete, "/conversation_skill/prompt_lib/5/abc/3", ""); response.Code != http.StatusNotFound || len(repo.unpinned) != 0 {
		t.Fatalf("unpin denied: status=%d unpinned=%v", response.Code, repo.unpinned)
	}
}

func TestPinTakesExactlyOneForm(t *testing.T) {
	for name, body := range map[string]string{
		"a skill":                     `{"skill_id": 4}`,
		"a listing version":           `{"listing_id": 9, "listing_version_id": 12}`,
		"both forms":                  `{"skill_id": 4, "listing_id": 9, "listing_version_id": 12}`,
		"neither":                     `{}`,
		"a listing without a version": `{"listing_id": 9}`,
		"a zero id":                   `{"skill_id": 0}`,
		"a negative version":          `{"listing_id": 9, "listing_version_id": -1}`,
		"an unknown key":              `{"skill_id": 4, "instructions": "do evil"}`,
		"text of a skill":             `{"instructions": "do evil"}`,
		"not json":                    `skill 4`,
		"two documents":               `{"skill_id": 4}{"skill_id": 5}`,
	} {
		repo := &repoStub{created: true}
		response := do(router(repo, allow, allow), http.MethodPost, "/conversation_skills/prompt_lib/5/abc", body)
		accepted := name == "a skill" || name == "a listing version"
		if accepted && response.Code != http.StatusCreated {
			t.Errorf("%s: status = %d body=%s", name, response.Code, response.Body.String())
		}
		if !accepted && (response.Code != http.StatusBadRequest || len(repo.pinned) != 0) {
			t.Errorf("%s: status = %d pinned=%v, want a 400 and nothing pinned", name, response.Code, repo.pinned)
		}
	}
}

func TestPinningWhatIsPinnedAnswersTheExistingPinWithOK(t *testing.T) {
	response := do(router(&repoStub{created: false}, allow, allow), http.MethodPost, "/conversation_skills/prompt_lib/5/abc", `{"skill_id": 4}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	var pin Pin
	if err := json.Unmarshal(response.Body.Bytes(), &pin); err != nil || pin.ID != 11 || pin.SkillID == nil || *pin.SkillID != 4 {
		t.Fatalf("pin = %s err=%v", response.Body.String(), err)
	}
}

func TestRepositoryRefusalsReachTheCaller(t *testing.T) {
	for name, status := range map[string]int{"not found": http.StatusNotFound, "conflict": http.StatusConflict} {
		err := apierr.NotFound("x")
		if status == http.StatusConflict {
			err = apierr.Conflict("a conversation holds at most five skills")
		}
		response := do(router(&repoStub{err: err}, allow, allow), http.MethodPost, "/conversation_skills/prompt_lib/5/abc", `{"skill_id": 4}`)
		if response.Code != status {
			t.Errorf("%s: status = %d, want %d", name, response.Code, status)
		}
	}
}

func TestUnpinTakesAPositiveId(t *testing.T) {
	repo := &repoStub{}
	handler := router(repo, allow, allow)
	if response := do(handler, http.MethodDelete, "/conversation_skill/prompt_lib/5/abc/3", ""); response.Code != http.StatusNoContent || len(repo.unpinned) != 1 || repo.unpinned[0] != 3 {
		t.Fatalf("unpin: status=%d unpinned=%v", response.Code, repo.unpinned)
	}
	for _, bad := range []string{"0", "-2", "x", "99999999999999999999"} {
		if response := do(handler, http.MethodDelete, "/conversation_skill/prompt_lib/5/abc/"+bad, ""); response.Code != http.StatusBadRequest {
			t.Errorf("pin id %q: status = %d, want 400", bad, response.Code)
		}
	}
}
