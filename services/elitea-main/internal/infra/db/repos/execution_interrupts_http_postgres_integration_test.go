package repos

// The public decision API through the real route, the real RBAC resolver and
// the real ledger (Track M2 task 2). Requires ELITEA_TEST_DATABASE_URL.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	agentexecutionapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/agentexecution"
	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

type interruptPrincipalValidator struct{}

func (interruptPrincipalValidator) ValidatePrincipal(_ context.Context, user auth.User) (auth.User, error) {
	return user, nil
}

type interruptPeerVerifier struct{}

func (interruptPeerVerifier) VerifyForwardedIdentityPeer(*http.Request) error { return nil }

func TestExecutionInterruptHTTPAgainstPostgres(t *testing.T) {
	h := newInterruptHarness(t)
	response := h.newResponse()
	key := h.raise(response, "fanout-interrupt-card-v1.json")
	route, err := agentexecutionapi.NewCurrentExecutionInterruptRoute(h.repo,
		apimw.AuthConfig{PrincipalValidator: interruptPrincipalValidator{}, ForwardedIdentityVerifier: interruptPeerVerifier{}},
		legacyrbac.NewPostgresResolver(h.pool))
	if err != nil {
		t.Fatal(err)
	}
	call := func(actor int64, method, path string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("X-Auth-Type", "user")
		req.Header.Set("X-Auth-ID", strconv.FormatInt(actor, 10))
		w := httptest.NewRecorder()
		route.ServeHTTP(w, req)
		return w
	}
	base := "/api/v2/elitea_core/task/prompt_lib/1/" + response + "/interrupts"
	decision := base + "/" + key + "/decision"
	tabA := []byte(`{"action":"approve","expected_revision":1,"request_id":"` + strings.Repeat("a1", 32) + `","value":""}`)
	tabB := []byte(`{"action":"block_with_comment","expected_revision":1,"request_id":"` + strings.Repeat("b2", 32) + `","value":"not now"}`)

	if w := call(interruptOwner, http.MethodGet, base, nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"PENDING"`) {
		t.Fatalf("GET before decision: %d %s", w.Code, w.Body.String())
	}
	// Refused callers change nothing: a member outside the conversation, a
	// member of another project, and the owner through another project.
	for name, attempt := range map[string]func() *httptest.ResponseRecorder{
		"outsider":        func() *httptest.ResponseRecorder { return call(interruptOutsider, http.MethodPost, decision, tabA) },
		"foreign actor":   func() *httptest.ResponseRecorder { return call(interruptForeigner, http.MethodPost, decision, tabA) },
		"foreign project": func() *httptest.ResponseRecorder { return call(interruptOwner, http.MethodPost, strings.Replace(decision, "/prompt_lib/1/", "/prompt_lib/2/", 1), tabA) },
		"outsider list":   func() *httptest.ResponseRecorder { return call(interruptOutsider, http.MethodGet, base, nil) },
	} {
		if w := attempt(); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}

	// Tab A submits twice at once; tab B (the question's author) races it.
	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, 3)
	wg.Go(func() { results[0] = call(interruptOwner, http.MethodPost, decision, tabA) })
	wg.Go(func() { results[1] = call(interruptOwner, http.MethodPost, decision, tabA) })
	wg.Go(func() { results[2] = call(interruptAsker, http.MethodPost, decision, tabB) })
	wg.Wait()
	applied, replays, conflicts := 0, 0, 0
	for _, w := range results {
		switch {
		case w.Code == 200 && strings.Contains(w.Body.String(), `"replay":false`):
			applied++
		case w.Code == 200 && strings.Contains(w.Body.String(), `"replay":true`):
			replays++
		case w.Code == 409 && strings.Contains(w.Body.String(), `"agent_interrupt_already_resolved"`):
			conflicts++
		default:
			t.Fatalf("unexpected response %d %s", w.Code, w.Body.String())
		}
	}
	// Either tab may win; exactly one decision is applied.
	if applied != 1 || replays+conflicts != 2 {
		t.Fatalf("applied=%d replays=%d conflicts=%d", applied, replays, conflicts)
	}
	var listed struct {
		DecisionRevision int64 `json:"decision_revision"`
		Interrupts       []struct {
			State    string `json:"state"`
			Revision int64  `json:"revision"`
		} `json:"interrupts"`
	}
	w := call(interruptAsker, http.MethodGet, base, nil)
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil || w.Code != 200 {
		t.Fatalf("GET after decision: %d %s", w.Code, w.Body.String())
	}
	if listed.DecisionRevision != 1 || len(listed.Interrupts) != 1 || listed.Interrupts[0].State != "DECIDED" || listed.Interrupts[0].Revision != 2 {
		t.Fatalf("list after decision = %+v", listed)
	}
	if w := call(interruptOwner, http.MethodPost, base+"/"+strings.Repeat("cd", 32)+"/decision", tabA); w.Code != 404 {
		t.Fatalf("unknown key: %d %s", w.Code, w.Body.String())
	}
}
