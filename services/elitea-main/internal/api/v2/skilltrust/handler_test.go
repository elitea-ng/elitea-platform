package skilltrust

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
	status    Status
	changed   bool
	revoked   int
	err       error
	trusted   []string
	revokeFor []int64
	getFor    []int64
}

func (stub *repoStub) Get(_ context.Context, _ string, skillID int64) (Status, error) {
	stub.getFor = append(stub.getFor, skillID)
	return stub.status, stub.err
}

func (stub *repoStub) Trust(_ context.Context, _ string, _ int64, revision string) (Status, bool, error) {
	stub.trusted = append(stub.trusted, revision)
	return stub.status, stub.changed, stub.err
}

func (stub *repoStub) Revoke(_ context.Context, _ string, skillID int64) (Status, int, error) {
	stub.revokeFor = append(stub.revokeFor, skillID)
	return stub.status, stub.revoked, stub.err
}

func router(repo Repository) http.Handler {
	h := NewHandler(repo)
	r := chi.NewRouter()
	r.Get("/skill_trust/prompt_lib/{projectID}/{skillID}", h.Get)
	r.Post("/skill_trust/prompt_lib/{projectID}/{skillID}", h.Trust)
	r.Delete("/skill_trust/prompt_lib/{projectID}/{skillID}", h.Revoke)
	return r
}

func do(handler http.Handler, method, target, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(method, target, strings.NewReader(body)))
	return recorder
}

var revision = strings.Repeat("ab", 32)

func TestGetAnswersTheStatusAndNeverNullHistory(t *testing.T) {
	repo := &repoStub{status: Status{SkillID: 4, Name: "Triage", Foreign: true, Revision: revision, State: "untrusted", History: []Record{}}}
	response := do(router(repo), http.MethodGet, "/skill_trust/prompt_lib/5/4", "")
	if response.Code != http.StatusOK || len(repo.getFor) != 1 || repo.getFor[0] != 4 {
		t.Fatalf("status=%d getFor=%v", response.Code, repo.getFor)
	}
	var body Status
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.State != "untrusted" || body.Revision != revision || !strings.Contains(response.Body.String(), `"history":[]`) {
		t.Fatalf("body = %s err=%v", response.Body.String(), err)
	}
}

func TestTrustNeedsTheDigestOfTheTextAndNothingElse(t *testing.T) {
	for name, body := range map[string]string{
		"a short revision": `{"revision": "abc"}`,
		"no revision":      `{}`,
		"an unknown key":   `{"revision": "` + revision + `", "state": "trusted"}`,
		"not json":         `trust`,
		"two documents":    `{"revision": "` + revision + `"}{}`,
		"a long revision":  `{"revision": "` + revision + `00"}`,
	} {
		repo := &repoStub{}
		if response := do(router(repo), http.MethodPost, "/skill_trust/prompt_lib/5/4", body); response.Code != http.StatusBadRequest || len(repo.trusted) != 0 {
			t.Errorf("%s: status = %d trusted=%v, want a 400 and nothing trusted", name, response.Code, repo.trusted)
		}
	}
	repo := &repoStub{status: Status{SkillID: 4, Name: "Triage", State: "trusted"}, changed: true}
	if response := do(router(repo), http.MethodPost, "/skill_trust/prompt_lib/5/4", `{"revision": "`+revision+`"}`); response.Code != http.StatusOK || len(repo.trusted) != 1 || repo.trusted[0] != revision {
		t.Fatalf("trust: status=%d trusted=%v", response.Code, repo.trusted)
	}
}

func TestAStaleRevisionAndOtherRefusalsReachTheCaller(t *testing.T) {
	for name, err := range map[string]error{
		"a stale revision":    apierr.Conflict("the skill's text changed since it was read; read it again before trusting it"),
		"a skill not foreign": apierr.BadRequest("this skill was written in this project and needs no trust"),
		"an unreadable skill": apierr.NotFound("skill not found"),
	} {
		response := do(router(&repoStub{err: err}), http.MethodPost, "/skill_trust/prompt_lib/5/4", `{"revision": "`+revision+`"}`)
		var apiErr *apierr.APIError
		if !asAPIError(err, &apiErr) || response.Code != apiErr.Status {
			t.Errorf("%s: status = %d, want %v", name, response.Code, err)
		}
	}
}

func asAPIError(err error, target **apierr.APIError) bool {
	value, ok := err.(*apierr.APIError)
	if ok {
		*target = value
	}
	return ok
}

func TestRevokeAnswersTheStatusAfterTheRevoke(t *testing.T) {
	repo := &repoStub{status: Status{SkillID: 4, Name: "Triage", State: "untrusted"}, revoked: 1}
	response := do(router(repo), http.MethodDelete, "/skill_trust/prompt_lib/5/4", "")
	if response.Code != http.StatusOK || len(repo.revokeFor) != 1 || repo.revokeFor[0] != 4 {
		t.Fatalf("status=%d revokeFor=%v", response.Code, repo.revokeFor)
	}
}

func TestTheSkillIdIsAPositive32BitInteger(t *testing.T) {
	repo := &repoStub{}
	for _, id := range []string{"0", "-1", "x", "2147483648"} {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
			if response := do(router(repo), method, "/skill_trust/prompt_lib/5/"+id, `{"revision": "`+revision+`"}`); response.Code != http.StatusBadRequest {
				t.Errorf("%s with skill id %q: status = %d, want 400", method, id, response.Code)
			}
		}
	}
	if len(repo.getFor)+len(repo.trusted)+len(repo.revokeFor) != 0 {
		t.Fatal("a bad skill id must reach no storage")
	}
}
