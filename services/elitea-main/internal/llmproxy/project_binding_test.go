package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy/llmproxytest"
)

// The bound project, the caller's personal project (7) and the project a
// selector names are three different numbers, so every assertion below states
// which of the three decided the outcome.
const (
	edgeBoundProject = 512
	edgeRivalProject = 613
)

// refusingMembershipChecker fails the test when the edge asks a membership
// question. A bound token was membership-checked when it was created, so it
// costs no query on the request path (spec-llm-project-scope §5).
type refusingMembershipChecker struct{ t *testing.T }

func (c refusingMembershipChecker) IsProjectMember(_ context.Context, userID int64, projectID int) (bool, error) {
	c.t.Helper()
	c.t.Errorf("the edge ran a membership query for a bound token (user %d, project %d)", userID, projectID)
	return false, nil
}

// failingPersonalResolver stands for an owner with no resolvable personal
// project.
type failingPersonalResolver struct{}

func (failingPersonalResolver) PersonalProjectID(context.Context, string) (int, error) {
	return 0, errors.New("no personal project")
}

// boundEdgeStack builds the real /llm chain — the project middleware in front
// of the streaming proxy — pointed at a backend that records the signed
// identity the gateway received.
func boundEdgeStack(
	t *testing.T,
	resolver middleware.PersonalProjectResolver,
	gotHeaders *http.Header,
) http.Handler {
	t.Helper()

	backend := llmproxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(backend.Close)

	mw := middleware.Project(middleware.ProjectConfig{
		Resolver:        resolver,
		PublicProjectID: 1,
		Membership:      refusingMembershipChecker{t: t},
	})
	return mw(proxyTo(t, backend, "sekret"))
}

// boundEdgeRequest builds an authenticated /llm request from a token bound to
// binding. A blank header name sends no selector.
func boundEdgeRequest(binding int64, header, value string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-mini"}`))
	if header != "" {
		req.Header.Set(header, value)
	}
	user := auth.User{
		ID:             "42",
		UserID:         "42",
		TokenID:        "900",
		Name:           "Regular User",
		AuthType:       "token",
		TokenProjectID: &binding,
	}
	return withEdgeProvenance(req, user)
}

// TestEdge_BoundTokenBillsTheBoundProject is spec §5 row 1 end to end. The
// gateway bills strictly on the signed X-Elitea-Project-Id, so this asserts the
// money lands on the bound project and not on the caller's personal project.
func TestEdge_BoundTokenBillsTheBoundProject(t *testing.T) {
	var gotHeaders http.Header
	stack := boundEdgeStack(t, stubPersonalResolver{id: edgePersonalProject}, &gotHeaders)

	rec := httptest.NewRecorder()
	stack.ServeHTTP(rec, boundEdgeRequest(edgeBoundProject, "", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	assertBilledProject(t, gotHeaders, strconv.Itoa(edgeBoundProject))
	if got := gotHeaders.Get(HeaderProjectID); got == strconv.Itoa(edgePersonalProject) {
		t.Errorf("the gateway billed the personal project %d over the binding %d",
			edgePersonalProject, edgeBoundProject)
	}
}

// TestEdge_BoundTokenMatchingSelectorBillsTheBoundProject is spec §5 row 2.
func TestEdge_BoundTokenMatchingSelectorBillsTheBoundProject(t *testing.T) {
	for _, header := range middleware.ProjectSelectorHeaders() {
		t.Run(header, func(t *testing.T) {
			var gotHeaders http.Header
			stack := boundEdgeStack(t, stubPersonalResolver{id: edgePersonalProject}, &gotHeaders)

			rec := httptest.NewRecorder()
			stack.ServeHTTP(rec, boundEdgeRequest(edgeBoundProject, header, strconv.Itoa(edgeBoundProject)))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
			assertBilledProject(t, gotHeaders, strconv.Itoa(edgeBoundProject))
		})
	}
}

// TestEdge_BoundTokenConflictingSelectorIsRefused is spec §5 row 3 end to end.
// The gateway is never called, so nothing is billed, and the body names both
// projects.
func TestEdge_BoundTokenConflictingSelectorIsRefused(t *testing.T) {
	for _, header := range middleware.ProjectSelectorHeaders() {
		t.Run(header, func(t *testing.T) {
			var gotHeaders http.Header
			stack := boundEdgeStack(t, stubPersonalResolver{id: edgePersonalProject}, &gotHeaders)

			rec := httptest.NewRecorder()
			stack.ServeHTTP(rec, boundEdgeRequest(edgeBoundProject, header, strconv.Itoa(edgeRivalProject)))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			if gotHeaders != nil {
				t.Errorf("the gateway was called with project %q; a refused request must bill nothing",
					gotHeaders.Get(HeaderProjectID))
			}

			var envelope struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
					Code    string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("body is not valid JSON: %s", rec.Body.String())
			}
			if envelope.Error.Code != "project_scope_conflict" {
				t.Errorf("error.code = %q, want project_scope_conflict", envelope.Error.Code)
			}
			if envelope.Error.Type != "invalid_request_error" {
				t.Errorf("error.type = %q, want invalid_request_error", envelope.Error.Type)
			}
			for _, id := range []int{edgeBoundProject, edgeRivalProject} {
				if !strings.Contains(envelope.Error.Message, strconv.Itoa(id)) {
					t.Errorf("error.message %q does not name project %d", envelope.Error.Message, id)
				}
			}
		})
	}
}

// TestEdge_BoundTokenUnusableSelectorIsNotAConflict is spec §6.2 end to end. A
// selector that names no project is absent, so the request proceeds on the
// binding.
func TestEdge_BoundTokenUnusableSelectorIsNotAConflict(t *testing.T) {
	for _, value := range []string{"org-abc123", "0", "-3", "2147483648", "   "} {
		t.Run("value="+value, func(t *testing.T) {
			var gotHeaders http.Header
			stack := boundEdgeStack(t, stubPersonalResolver{id: edgePersonalProject}, &gotHeaders)

			rec := httptest.NewRecorder()
			stack.ServeHTTP(rec, boundEdgeRequest(edgeBoundProject, middleware.HeaderProjectSelector, value))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
			assertBilledProject(t, gotHeaders, strconv.Itoa(edgeBoundProject))
		})
	}

	t.Run("repeated header", func(t *testing.T) {
		var gotHeaders http.Header
		stack := boundEdgeStack(t, stubPersonalResolver{id: edgePersonalProject}, &gotHeaders)

		req := boundEdgeRequest(edgeBoundProject, "", "")
		req.Header.Add(middleware.HeaderProjectSelector, strconv.Itoa(edgeRivalProject))
		req.Header.Add(middleware.HeaderProjectSelector, strconv.Itoa(edgeRivalProject))

		rec := httptest.NewRecorder()
		stack.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		assertBilledProject(t, gotHeaders, strconv.Itoa(edgeBoundProject))
	})
}

// TestEdge_BoundTokenWithoutAPersonalProjectSucceeds pins the ordering end to
// end. The binding is read before the personal-project lookup, so an owner with
// no personal project still bills the bound project.
func TestEdge_BoundTokenWithoutAPersonalProjectSucceeds(t *testing.T) {
	var gotHeaders http.Header
	stack := boundEdgeStack(t, failingPersonalResolver{}, &gotHeaders)

	rec := httptest.NewRecorder()
	stack.ServeHTTP(rec, boundEdgeRequest(edgeBoundProject, "", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	assertBilledProject(t, gotHeaders, strconv.Itoa(edgeBoundProject))
}

// TestEdge_BoundTokenSelectorIsNotForwarded proves the edge consumes the
// selector on the bound path as well. An Elitea project id must not travel
// onward under a name a real provider reads.
func TestEdge_BoundTokenSelectorIsNotForwarded(t *testing.T) {
	var gotHeaders http.Header
	stack := boundEdgeStack(t, stubPersonalResolver{id: edgePersonalProject}, &gotHeaders)

	req := boundEdgeRequest(edgeBoundProject, middleware.HeaderProjectSelector, strconv.Itoa(edgeBoundProject))
	req.Header.Set(middleware.HeaderProjectSelectorOpenAIProject, strconv.Itoa(edgeRivalProject))
	req.Header.Set(middleware.HeaderProjectSelectorOpenAIOrg, strconv.Itoa(edgeBoundProject))

	rec := httptest.NewRecorder()
	stack.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	for _, name := range []string{"X-Project-Id", "OpenAI-Project", "OpenAI-Organization"} {
		if got := gotHeaders.Get(name); got != "" {
			t.Errorf("project header %s reached the gateway with %q", name, got)
		}
	}
	assertBilledProject(t, gotHeaders, strconv.Itoa(edgeBoundProject))
}
