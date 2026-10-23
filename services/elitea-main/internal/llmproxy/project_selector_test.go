package llmproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/llmproxy/llmproxytest"
)

// The caller's personal project and the team project are deliberately
// different numbers. The edge resolves the caller's personal project by itself,
// so a test that names the personal project passes whether or not the selector
// is honoured (the personal-project selection trap).
const (
	edgePersonalProject = 7
	edgeTeamProject     = 4321
)

// stubPersonalResolver returns a fixed personal project for any user.
type stubPersonalResolver struct{ id int }

func (s stubPersonalResolver) PersonalProjectID(context.Context, string) (int, error) {
	return s.id, nil
}

// stubMemberQuerier answers the membership query without a database.
type stubMemberQuerier struct {
	allow map[int32]bool
	err   error
}

func (s stubMemberQuerier) IsCurrentUserProjectMember(
	_ context.Context,
	params sqlcgen.IsCurrentUserProjectMemberParams,
) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	return s.allow[params.ProjectID], nil
}

// edgeStack builds the real /llm chain: the project middleware in front of the
// streaming proxy, pointed at a backend that records what the gateway received.
func edgeStack(t *testing.T, allow map[int32]bool, gotHeaders *http.Header) http.Handler {
	t.Helper()
	return edgeStackWith(t, stubMemberQuerier{allow: allow}, gotHeaders)
}

// edgeStackWith is edgeStack over an explicit querier, so a test can make the
// membership query fail.
func edgeStackWith(t *testing.T, queries middleware.ProjectMemberQuerier, gotHeaders *http.Header) http.Handler {
	t.Helper()

	backend := llmproxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(backend.Close)

	mw := middleware.Project(middleware.ProjectConfig{
		Resolver:        stubPersonalResolver{id: edgePersonalProject},
		PublicProjectID: 1,
		Membership:      middleware.NewProjectMembershipWith(queries),
	})
	return mw(proxyTo(t, backend, "sekret"))
}

// edgeRequest builds an authenticated /llm request carrying a selector.
func edgeRequest(header, value string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o-mini"}`))
	if header != "" {
		req.Header.Set(header, value)
	}
	user := auth.User{ID: "42", UserID: "42", TokenID: "900", Name: "Regular User", AuthType: "token"}
	return withEdgeProvenance(req, user)
}

// assertBilledProject reads the signed identity the gateway received. The
// gateway bills strictly on X-Elitea-Project-Id, so this is the assertion that
// tells a correctly billed request from a wrongly billed one.
func assertBilledProject(t *testing.T, gotHeaders http.Header, want string) {
	t.Helper()
	if gotHeaders == nil {
		t.Fatal("the gateway was never called")
	}
	if got := gotHeaders.Get(HeaderProjectID); got != want {
		t.Errorf("gateway billed project %q, want %q", got, want)
	}
	// The identity must be signed over the billed project, or the gateway
	// rejects it and the assertion above would prove nothing.
	if !verifyIdentitySignature(gotHeaders, []byte("sekret")) {
		t.Errorf("forwarded identity signature did not verify")
	}
}

// TestEdge_NamedProjectReachesTheGatewayIdentity is the end-to-end form of the
// first discriminating test. The gateway bills strictly on the signed
// X-Elitea-Project-Id, so this asserts the money lands on the named project.
func TestEdge_NamedProjectReachesTheGatewayIdentity(t *testing.T) {
	var gotHeaders http.Header
	stack := edgeStack(t, map[int32]bool{edgeTeamProject: true}, &gotHeaders)

	rec := httptest.NewRecorder()
	stack.ServeHTTP(rec, edgeRequest(middleware.HeaderProjectSelector, "4321"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	assertBilledProject(t, gotHeaders, "4321")
}

// TestEdge_OpenAIOrganizationIsTheSelector covers the second accepted name end
// to end.
func TestEdge_OpenAIOrganizationIsTheSelector(t *testing.T) {
	var gotHeaders http.Header
	stack := edgeStack(t, map[int32]bool{edgeTeamProject: true}, &gotHeaders)

	rec := httptest.NewRecorder()
	stack.ServeHTTP(rec, edgeRequest(middleware.HeaderProjectSelectorOpenAIOrg, "4321"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	assertBilledProject(t, gotHeaders, "4321")
}

// TestEdge_NonMemberBillsThePersonalProject is the end-to-end form of the second
// discriminating test, and the row ADR-0018 changed. A selector the caller may
// not use is ignored in silence: the call proceeds, and it bills the caller's
// own project.
//
// The status assertions are explicit because issue #318 names them: not 403,
// and not 500. A 403 would break every existing caller of the UI's generated
// samples.
func TestEdge_NonMemberBillsThePersonalProject(t *testing.T) {
	var gotHeaders http.Header
	stack := edgeStack(t, map[int32]bool{}, &gotHeaders) // member of nothing

	rec := httptest.NewRecorder()
	stack.ServeHTTP(rec, edgeRequest(middleware.HeaderProjectSelector, "4321"))

	if rec.Code == http.StatusForbidden {
		t.Fatalf("status = 403; a non-member selector must be ignored, not refused")
	}
	if rec.Code >= http.StatusInternalServerError {
		t.Fatalf("status = %d; a non-member selector must not be a server error", rec.Code)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	assertBilledProject(t, gotHeaders, "7")
	if got := gotHeaders.Get(HeaderProjectID); got == "4321" {
		t.Errorf("gateway billed project 4321, which the caller may not use")
	}
}

// TestEdge_MembershipErrorBillsThePersonalProject is spec §5 row 6 end to end.
// A membership query that errors never bills the selector.
func TestEdge_MembershipErrorBillsThePersonalProject(t *testing.T) {
	var gotHeaders http.Header
	stack := edgeStackWith(t, stubMemberQuerier{err: errors.New("connection refused")}, &gotHeaders)

	rec := httptest.NewRecorder()
	stack.ServeHTTP(rec, edgeRequest(middleware.HeaderProjectSelector, "4321"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	assertBilledProject(t, gotHeaders, "7")
}

// TestEdge_OpenAIProjectDoesNotBill is the ADR-0018 refusal end to end. The UI
// fills OpenAI-Project from the project that owns the model, which is often the
// shared project. The caller here IS a member of the named project, so the only
// reason the money stays on the personal project is the header name.
func TestEdge_OpenAIProjectDoesNotBill(t *testing.T) {
	var gotHeaders http.Header
	stack := edgeStack(t, map[int32]bool{edgeTeamProject: true}, &gotHeaders)

	rec := httptest.NewRecorder()
	stack.ServeHTTP(rec, edgeRequest(middleware.HeaderProjectSelectorOpenAIProject, "4321"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	assertBilledProject(t, gotHeaders, "7")
	if got := gotHeaders.Get(HeaderProjectID); got == "4321" {
		t.Errorf("OpenAI-Project billed project 4321; that header names the model owner, not the payer")
	}
}

// TestEdge_AbsentSelectorBillsThePersonalProject holds the compatibility line
// end to end.
func TestEdge_AbsentSelectorBillsThePersonalProject(t *testing.T) {
	var gotHeaders http.Header
	stack := edgeStack(t, map[int32]bool{edgeTeamProject: true}, &gotHeaders)

	rec := httptest.NewRecorder()
	stack.ServeHTTP(rec, edgeRequest("", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	assertBilledProject(t, gotHeaders, "7")
}

// TestEdge_ProjectHeadersAreNotForwarded proves the edge consumes every project
// header. An Elitea project id must not travel onward under a name a real
// provider reads.
func TestEdge_ProjectHeadersAreNotForwarded(t *testing.T) {
	var gotHeaders http.Header
	stack := edgeStack(t, map[int32]bool{edgeTeamProject: true}, &gotHeaders)

	req := edgeRequest(middleware.HeaderProjectSelector, "4321")
	req.Header.Set(middleware.HeaderProjectSelectorOpenAIProject, "4321")
	req.Header.Set(middleware.HeaderProjectSelectorOpenAIOrg, "4321")

	rec := httptest.NewRecorder()
	stack.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	for _, name := range middleware.ProjectHeadersStrippedOutbound() {
		if got := gotHeaders.Get(name); got != "" {
			t.Errorf("project header %s reached the gateway with %q", name, got)
		}
	}
	// The three names are asserted literally as well, so a shrinking strip list
	// cannot quietly empty the loop above.
	for _, name := range []string{"X-Project-Id", "OpenAI-Project", "OpenAI-Organization"} {
		if got := gotHeaders.Get(name); got != "" {
			t.Errorf("project header %s reached the gateway with %q", name, got)
		}
	}
	assertBilledProject(t, gotHeaders, "4321")
}
