package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// The bound project, the caller's personal project (7) and the project a
// selector names are three different numbers. A test that reuses one number
// passes whether or not the binding decided the outcome.
const (
	boundProject = 512
	rivalProject = 613
)

// refusingMembership fails the test when the edge asks a membership question.
//
// A bound token MUST NOT run a membership query (spec-llm-project-scope §5).
// Membership was checked when the token was created, and the binding is deleted
// when the owner loses membership (§7.3). A per-request re-check would add a
// database round trip to every /llm call on the hot path, and it would make a
// database outage decide where money is spent.
type refusingMembership struct{ t *testing.T }

func (m refusingMembership) IsProjectMember(_ context.Context, userID int64, projectID int) (bool, error) {
	m.t.Helper()
	m.t.Errorf("the edge ran a membership query for a bound token (user %d, project %d); "+
		"a binding is checked at token creation and costs no query at request time", userID, projectID)
	return false, nil
}

// boundRequest builds an authenticated /llm request from a token bound to
// binding. A blank header name sends no selector.
//
// TokenProjectID is the field a credential validator populates from storage. No
// header and no name may reach it (spec §3.2).
func boundRequest(binding int64, header, value string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", nil)
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
	return withTokenProvenance(req, user)
}

// runBound drives the middleware over resolver and reports what the next
// handler saw.
func runBound(
	t *testing.T,
	resolver PersonalProjectResolver,
	req *http.Request,
) (*httptest.ResponseRecorder, ProjectContext, bool) {
	t.Helper()

	var seen ProjectContext
	var invoked bool
	mw := Project(ProjectConfig{
		Resolver:        resolver,
		PublicProjectID: 1,
		Membership:      refusingMembership{t: t},
	})
	rec := httptest.NewRecorder()
	mw(captureHandler(&seen, &invoked)).ServeHTTP(rec, req)
	return rec, seen, invoked
}

// assertBoundProjectBilled states the whole row 1-2 contract: the request
// succeeds, it reaches the proxy, and it bills the bound project.
func assertBoundProjectBilled(
	t *testing.T,
	rec *httptest.ResponseRecorder,
	seen ProjectContext,
	invoked bool,
) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !invoked {
		t.Fatal("next handler was not invoked")
	}
	if seen.ProjectID != boundProject {
		t.Errorf("billed project = %d, want the bound project %d", seen.ProjectID, boundProject)
	}
}

// TestProjectBinding_NoSelectorBillsTheBoundProject is spec §5 row 1.
//
// It also pins the two costs of the row: the personal-project resolver is not
// consulted, and no membership query runs.
func TestProjectBinding_NoSelectorBillsTheBoundProject(t *testing.T) {
	resolver := &fakeResolver{id: personalProject}

	rec, seen, invoked := runBound(t, resolver, boundRequest(boundProject, "", ""))

	assertBoundProjectBilled(t, rec, seen, invoked)
	if seen.ProjectID == personalProject {
		t.Errorf("billed the personal project %d; the token is bound to %d", personalProject, boundProject)
	}
	if resolver.called {
		t.Error("the personal-project lookup ran for a bound token; the binding answers the question already")
	}
}

// TestProjectBinding_MatchingSelectorBillsTheBoundProject is spec §5 row 2. A
// caller that names the project its key already bills is agreeing, not
// conflicting.
func TestProjectBinding_MatchingSelectorBillsTheBoundProject(t *testing.T) {
	for _, header := range ProjectSelectorHeaders() {
		t.Run(header, func(t *testing.T) {
			resolver := &fakeResolver{id: personalProject}

			rec, seen, invoked := runBound(t, resolver,
				boundRequest(boundProject, header, strconv.Itoa(boundProject)))

			assertBoundProjectBilled(t, rec, seen, invoked)
			if resolver.called {
				t.Error("the personal-project lookup ran for a bound token")
			}
		})
	}
}

// TestProjectBinding_ConflictingSelectorIsRefused is spec §5 row 3, the only
// refusal on this path.
//
// The caller made two statements that disagree: the key names one project and
// the header names another. Billing the bound project in silence would let the
// caller believe it redirected the spend, and the divergence would surface later
// as an accounting discrepancy nobody can attribute (ADR-0018).
func TestProjectBinding_ConflictingSelectorIsRefused(t *testing.T) {
	for _, header := range ProjectSelectorHeaders() {
		t.Run(header, func(t *testing.T) {
			resolver := &fakeResolver{id: personalProject}

			rec, seen, invoked := runBound(t, resolver,
				boundRequest(boundProject, header, strconv.Itoa(rivalProject)))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			if invoked {
				t.Error("next handler was invoked; a refused request must bill nothing")
			}
			if seen.ProjectID != 0 {
				t.Errorf("project %d reached the proxy on a refused request", seen.ProjectID)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}

			envelope := decodeProjectError(t, rec.Body.Bytes())
			if envelope.Error.Code != codeProjectScopeConflict {
				t.Errorf("error.code = %q, want %q", envelope.Error.Code, codeProjectScopeConflict)
			}
			if envelope.Error.Type != errTypeInvalidRequest {
				t.Errorf("error.type = %q, want %q", envelope.Error.Type, errTypeInvalidRequest)
			}

			// The message names both projects. That is the reason this rejects
			// instead of ignoring: the caller learns which two statements
			// disagreed (spec §8).
			for _, id := range []int{boundProject, rivalProject} {
				if !strings.Contains(envelope.Error.Message, strconv.Itoa(id)) {
					t.Errorf("error.message %q does not name project %d", envelope.Error.Message, id)
				}
			}
			// The message must not answer "does this caller belong to the
			// project it named". No membership query ran, and the answer would
			// be a membership oracle for any holder of any token.
			for _, leak := range []string{"member", "Member", "belong", "not in"} {
				if strings.Contains(envelope.Error.Message, leak) {
					t.Errorf("error.message %q discloses membership of the selector project", envelope.Error.Message)
				}
			}
		})
	}
}

// TestProjectBinding_ConflictHoldsForAMemberOfTheSelector proves the refusal
// does not depend on membership at all. The membership checker fails the test if
// it runs, so a 400 here can only come from the binding.
func TestProjectBinding_ConflictHoldsForAMemberOfTheSelector(t *testing.T) {
	resolver := &fakeResolver{id: rivalProject} // the caller's OWN project

	rec, _, invoked := runBound(t, resolver,
		boundRequest(boundProject, HeaderProjectSelector, strconv.Itoa(rivalProject)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; a selector that contradicts the binding is refused "+
			"even when it names the caller's own project; body = %s", rec.Code, rec.Body.String())
	}
	if invoked {
		t.Error("next handler was invoked on a refused request")
	}
}

// TestProjectBinding_UnusableSelectorIsNotAConflict is spec §6.2.
//
// A selector that parses to nothing is ABSENT, for a bound token exactly as for
// an unbound one. No project was named, so no statement contradicts the binding
// and the request proceeds on the binding. Refusing here would turn a typo, or a
// client that sends an empty header, into a hard failure.
func TestProjectBinding_UnusableSelectorIsNotAConflict(t *testing.T) {
	values := []string{"org-abc123", "0", "-3", "12.5", "9999999999999", "2147483648", "   ", ""}
	for _, value := range values {
		t.Run("value="+value, func(t *testing.T) {
			resolver := &fakeResolver{id: personalProject}

			rec, seen, invoked := runBound(t, resolver,
				boundRequest(boundProject, HeaderProjectSelector, value))

			assertBoundProjectBilled(t, rec, seen, invoked)
		})
	}

	t.Run("repeated header", func(t *testing.T) {
		resolver := &fakeResolver{id: personalProject}

		req := boundRequest(boundProject, "", "")
		req.Header.Add(HeaderProjectSelector, strconv.Itoa(rivalProject))
		req.Header.Add(HeaderProjectSelector, strconv.Itoa(rivalProject))

		rec, seen, invoked := runBound(t, resolver, req)

		assertBoundProjectBilled(t, rec, seen, invoked)
	})

	t.Run("OpenAI-Project is not a selector", func(t *testing.T) {
		resolver := &fakeResolver{id: personalProject}

		rec, seen, invoked := runBound(t, resolver,
			boundRequest(boundProject, HeaderProjectSelectorOpenAIProject, strconv.Itoa(rivalProject)))

		assertBoundProjectBilled(t, rec, seen, invoked)
	})
}

// TestProjectBinding_SurvivesAnUnresolvablePersonalProject pins the ordering.
//
// The binding is read BEFORE the personal-project lookup. A bound token whose
// owner has no resolvable personal project must succeed on its binding, and must
// not fall into the project_not_resolved refusal.
func TestProjectBinding_SurvivesAnUnresolvablePersonalProject(t *testing.T) {
	resolvers := map[string]*fakeResolver{
		"resolver returns nothing": {id: 0},
		"resolver errors":          {err: errors.New("db down")},
	}
	for name, resolver := range resolvers {
		t.Run(name, func(t *testing.T) {
			rec, seen, invoked := runBound(t, resolver, boundRequest(boundProject, "", ""))

			assertBoundProjectBilled(t, rec, seen, invoked)
			if rec.Code == http.StatusBadRequest {
				t.Error("the request failed with project_not_resolved; the binding resolves the project by itself")
			}
		})
	}

	t.Run("nil resolver", func(t *testing.T) {
		rec, seen, invoked := runBound(t, nil, boundRequest(boundProject, "", ""))
		assertBoundProjectBilled(t, rec, seen, invoked)
	})
}

// TestProjectBinding_BeatsThePrincipalNameBranch pins the same ordering against
// the other source of a project id.
//
// resolveProjectID parses ":system:project:<id>:" out of the principal name. A
// bound token never reaches that code. The name is caller-supplied free text in
// every path that could ever populate it, so a binding read from storage must
// win (spec §7 invariant 2).
func TestProjectBinding_BeatsThePrincipalNameBranch(t *testing.T) {
	binding := int64(boundProject)
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", nil)
	user := auth.User{
		ID:             "42",
		UserID:         "42",
		TokenID:        "900",
		Name:           ":system:project:" + strconv.Itoa(rivalProject) + ":",
		AuthType:       "token",
		TokenProjectID: &binding,
	}
	req = withTokenProvenance(req, user)

	rec, seen, invoked := runBound(t, &fakeResolver{id: personalProject}, req)

	assertBoundProjectBilled(t, rec, seen, invoked)
	if seen.ProjectID == rivalProject {
		t.Errorf("the principal name billed project %d over the stored binding %d", rivalProject, boundProject)
	}
}

// TestBoundProjectID_UnusableBindingReadsAsUnbound covers the range guard. A
// stored binding outside the int4 project-id range names no project, so the
// caller falls back to its own project instead of claiming another one.
func TestBoundProjectID_UnusableBindingReadsAsUnbound(t *testing.T) {
	cases := map[string]*int64{
		"no binding":   nil,
		"zero":         int64Ptr(0),
		"negative":     int64Ptr(-1),
		"out of int32": int64Ptr(1 << 40),
	}
	for name, binding := range cases {
		t.Run(name, func(t *testing.T) {
			id, bound := boundProjectID(auth.User{TokenProjectID: binding})
			if bound {
				t.Errorf("boundProjectID = (%d, true), want unbound", id)
			}
		})
	}

	t.Run("usable binding", func(t *testing.T) {
		id, bound := boundProjectID(auth.User{TokenProjectID: int64Ptr(boundProject)})
		if !bound || id != boundProject {
			t.Errorf("boundProjectID = (%d, %v), want (%d, true)", id, bound, boundProject)
		}
	})
}

// TestProjectBinding_UnboundTokenIsUnchanged holds the compatibility line from
// the other side: a token with no binding keeps every row 4-8 behaviour. The
// membership checker refuses to answer here, which is the nil-checker case, so
// the selector cannot be admitted and the personal project pays.
func TestProjectBinding_UnboundTokenIsUnchanged(t *testing.T) {
	resolver := &fakeResolver{id: personalProject}

	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", nil)
	user := auth.User{ID: "42", UserID: "42", TokenID: "900", Name: "Regular User", AuthType: "token"}
	req = withTokenProvenance(req, user)

	var seen ProjectContext
	var invoked bool
	mw := Project(ProjectConfig{Resolver: resolver, PublicProjectID: 1})
	rec := httptest.NewRecorder()
	mw(captureHandler(&seen, &invoked)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !invoked {
		t.Fatal("next handler was not invoked")
	}
	if seen.ProjectID != personalProject {
		t.Errorf("billed project = %d, want the personal project %d", seen.ProjectID, personalProject)
	}
	if !resolver.called {
		t.Error("the personal-project lookup did not run for an unbound token")
	}
}

func int64Ptr(v int64) *int64 { return &v }

type projectErrorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func decodeProjectError(t *testing.T, body []byte) projectErrorEnvelope {
	t.Helper()
	var envelope projectErrorEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("body is not valid JSON: %s", body)
	}
	return envelope
}

// boundRequestWithProjectState is boundRequest plus the lifecycle state the
// credential validator read for the bound project.
func boundRequestWithProjectState(binding int64, active bool) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", nil)
	user := auth.User{
		ID:                 "42",
		UserID:             "42",
		TokenID:            "900",
		Name:               "Regular User",
		AuthType:           "token",
		TokenProjectID:     &binding,
		TokenProjectActive: &active,
	}
	return withTokenProvenance(req, user)
}

// DEFECT: the bound arm of resolveEdgeProject consulted neither the resolver
// nor any project state, and ProjectSuspend flips centry.project.suspended
// without revoking a binding. A token bound to project P therefore kept
// resolving to P after an operator suspended P. The gateway kept decrypting
// P's provider credentials and charging P's budget.
//
// Evidence: an UNBOUND caller who names P with X-Project-Id is already
// refused, because that path runs IsCurrentUserProjectMember, whose query
// requires `project.suspended IS FALSE`. The two arms disagreed, and only the
// bound one kept spending.
func TestProjectBinding_InactiveBoundProjectIsRefused(t *testing.T) {
	resolver := &fakeResolver{id: personalProject}

	rec, seen, invoked := runBound(t, resolver, boundRequestWithProjectState(boundProject, false))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
	if invoked {
		t.Error("next handler was invoked; a refused request must bill nothing")
	}
	if seen.ProjectID != 0 {
		t.Errorf("project %d reached the proxy on a refused request", seen.ProjectID)
	}
	// The refusal must NOT fall through to the unbound path. A fallback would
	// bill the caller's personal project and silently move the spend that the
	// suspension was meant to stop.
	if resolver.called {
		t.Error("the personal-project lookup ran; a refused binding must not fall back to another project")
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	envelope := decodeProjectError(t, rec.Body.Bytes())
	if envelope.Error.Code != codeProjectInactive {
		t.Errorf("error.code = %q, want %q", envelope.Error.Code, codeProjectInactive)
	}
	if envelope.Error.Type != errTypePermission {
		t.Errorf("error.type = %q, want %q", envelope.Error.Type, errTypePermission)
	}
	// The message must not name the lifecycle column that failed, following
	// the token routes: the caller learns the binding is unusable, not the
	// tenant's state.
	for _, leak := range []string{"suspend", "Suspend", "create_success", "deleted"} {
		if strings.Contains(envelope.Error.Message, leak) {
			t.Errorf("error.message %q discloses the project lifecycle state", envelope.Error.Message)
		}
	}
}

// An ACTIVE bound project still bills the binding, and still costs no query.
func TestProjectBinding_ActiveBoundProjectStillBillsTheBinding(t *testing.T) {
	resolver := &fakeResolver{id: personalProject}

	rec, seen, invoked := runBound(t, resolver, boundRequestWithProjectState(boundProject, true))

	assertBoundProjectBilled(t, rec, seen, invoked)
	if resolver.called {
		t.Error("the personal-project lookup ran for a bound token")
	}
}

// A validator that reads no storage leaves the state undetermined. The binding
// must keep working, or the change would fail every such deployment closed.
func TestProjectBinding_UndeterminedProjectStateKeepsTheBinding(t *testing.T) {
	resolver := &fakeResolver{id: personalProject}

	rec, seen, invoked := runBound(t, resolver, boundRequest(boundProject, "", ""))

	assertBoundProjectBilled(t, rec, seen, invoked)
}

// A conflicting selector must not hide the refusal, and the refusal must not
// hide the conflict. An inactive binding is refused first: the caller cannot
// correct it by changing the header.
func TestProjectBinding_InactiveBoundProjectBeatsASelectorConflict(t *testing.T) {
	resolver := &fakeResolver{id: personalProject}
	req := boundRequestWithProjectState(boundProject, false)
	req.Header.Set(HeaderProjectSelector, strconv.Itoa(rivalProject))

	rec, _, invoked := runBound(t, resolver, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
	if invoked {
		t.Error("next handler was invoked on a refused request")
	}
	if envelope := decodeProjectError(t, rec.Body.Bytes()); envelope.Error.Code != codeProjectInactive {
		t.Errorf("error.code = %q, want %q", envelope.Error.Code, codeProjectInactive)
	}
}
