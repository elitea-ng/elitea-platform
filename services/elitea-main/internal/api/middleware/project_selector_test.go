package middleware

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
)

// The caller's personal project and the team project are deliberately
// different numbers. The /llm path resolves the caller's personal project by
// itself, so a test that names the personal project passes whether or not the
// selector is honoured. Every assertion below names teamProject.
const (
	personalProject = 7
	teamProject     = 4321
)

// fakeMemberQuerier answers the membership query without a database, and
// records what it was asked.
type fakeMemberQuerier struct {
	allow map[int32]bool
	err   error
	calls []sqlcgen.IsCurrentUserProjectMemberParams
}

func (f *fakeMemberQuerier) IsCurrentUserProjectMember(
	_ context.Context,
	params sqlcgen.IsCurrentUserProjectMemberParams,
) (bool, error) {
	f.calls = append(f.calls, params)
	if f.err != nil {
		return false, f.err
	}
	return f.allow[params.ProjectID], nil
}

// selectorRequest builds an authenticated /llm request. A blank header name
// sends no selector at all.
func selectorRequest(header, value string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", nil)
	if header != "" {
		req.Header.Set(header, value)
	}
	user := auth.User{ID: "42", UserID: "42", TokenID: "900", Name: "Regular User", AuthType: "token"}
	return withTokenProvenance(req, user)
}

// runSelector drives the middleware and reports what the next handler saw.
func runSelector(
	membership ProjectMembershipChecker,
	req *http.Request,
) (*httptest.ResponseRecorder, ProjectContext, bool) {
	var seen ProjectContext
	var invoked bool
	mw := Project(ProjectConfig{
		Resolver:        &fakeResolver{id: personalProject},
		PublicProjectID: 1,
		Membership:      membership,
	})
	rec := httptest.NewRecorder()
	mw(captureHandler(&seen, &invoked)).ServeHTTP(rec, req)
	return rec, seen, invoked
}

// assertFellBackToPersonal states the whole silent-fallback contract in one
// place: the request succeeds, it reaches the proxy, and it bills the caller's
// own project. The 200 covers "not 403 and not 500" as well. A status-only
// assertion cannot tell a correctly billed request from a wrongly billed one,
// so the billed project is asserted too.
func assertFellBackToPersonal(
	t *testing.T,
	rec *httptest.ResponseRecorder,
	seen ProjectContext,
	invoked bool,
) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; an inadmissible selector must not be an error; body = %s",
			rec.Code, rec.Body.String())
	}
	if !invoked {
		t.Fatal("next handler was not invoked")
	}
	if seen.ProjectID == teamProject {
		t.Errorf("billed project %d, which the selector named but the caller may not use", teamProject)
	}
	if seen.ProjectID != personalProject {
		t.Errorf("billed project = %d, want the personal project %d", seen.ProjectID, personalProject)
	}
}

// TestProjectSelector_MemberProjectIsBilled is the first discriminating test:
// the admitted project must be the one the header names, and not the caller's
// personal project.
func TestProjectSelector_MemberProjectIsBilled(t *testing.T) {
	queries := &fakeMemberQuerier{allow: map[int32]bool{teamProject: true}}

	rec, seen, invoked := runSelector(
		NewProjectMembershipWith(queries),
		selectorRequest(HeaderProjectSelector, "4321"),
	)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !invoked {
		t.Fatal("next handler was not invoked")
	}
	if seen.ProjectID != teamProject {
		t.Errorf("billed project = %d, want %d", seen.ProjectID, teamProject)
	}
	if seen.ProjectID == personalProject {
		t.Errorf("billed the caller's personal project %d; the header named %d", personalProject, teamProject)
	}

	// The admission must rest on a membership answer about that exact pair.
	if len(queries.calls) != 1 {
		t.Fatalf("membership queries = %d, want 1", len(queries.calls))
	}
	if got := queries.calls[0]; got.ProjectID != teamProject || got.UserID != 42 {
		t.Errorf("membership asked about %+v, want project %d and user 42", got, teamProject)
	}
}

// TestProjectSelector_NonMemberFallsBackSilently is the second discriminating
// test, and it is the row that ADR-0018 changed. A selector the caller may not
// use is ignored. The request proceeds on the caller's own project.
//
// It must not be a 403. Issue #318 forbids a new failure mode for an existing
// caller, and the UI's Node and Python samples send a project id today.
func TestProjectSelector_NonMemberFallsBackSilently(t *testing.T) {
	queries := &fakeMemberQuerier{allow: map[int32]bool{}} // member of nothing

	rec, seen, invoked := runSelector(
		NewProjectMembershipWith(queries),
		selectorRequest(HeaderProjectSelector, "4321"),
	)

	assertFellBackToPersonal(t, rec, seen, invoked)

	// The fallback must follow a real membership answer about that exact pair.
	if len(queries.calls) != 1 {
		t.Fatalf("membership queries = %d, want 1", len(queries.calls))
	}
	if got := queries.calls[0]; got.ProjectID != teamProject || got.UserID != 42 {
		t.Errorf("membership asked about %+v, want project %d and user 42", got, teamProject)
	}
}

// TestProjectSelector_AbsentKeepsPersonalProject holds the compatibility line:
// a caller that sends no selector sees exactly the behaviour it saw before.
func TestProjectSelector_AbsentKeepsPersonalProject(t *testing.T) {
	queries := &fakeMemberQuerier{allow: map[int32]bool{teamProject: true}}

	rec, seen, invoked := runSelector(NewProjectMembershipWith(queries), selectorRequest("", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !invoked {
		t.Fatal("next handler was not invoked")
	}
	if seen.ProjectID != personalProject {
		t.Errorf("billed project = %d, want the personal project %d", seen.ProjectID, personalProject)
	}
	if len(queries.calls) != 0 {
		t.Errorf("membership was queried %d times without a selector", len(queries.calls))
	}
}

// TestProjectSelector_OwnProjectSkipsMembership shows the caller may name the
// project it already resolves to. That needs no membership query.
func TestProjectSelector_OwnProjectSkipsMembership(t *testing.T) {
	queries := &fakeMemberQuerier{allow: map[int32]bool{}}

	rec, seen, _ := runSelector(
		NewProjectMembershipWith(queries),
		selectorRequest(HeaderProjectSelector, "7"),
	)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if seen.ProjectID != personalProject {
		t.Errorf("billed project = %d, want %d", seen.ProjectID, personalProject)
	}
	if len(queries.calls) != 0 {
		t.Errorf("membership was queried %d times for the caller's own project", len(queries.calls))
	}
}

// TestProjectSelector_AcceptedHeaderNames covers each accepted name. The legacy
// runtime accepted X-Project-Id and OpenAI-Organization, and those two are the
// whole accepted set (ADR-0018).
func TestProjectSelector_AcceptedHeaderNames(t *testing.T) {
	want := []string{HeaderProjectSelector, HeaderProjectSelectorOpenAIOrg}

	if got := ProjectSelectorHeaders(); len(got) != len(want) {
		t.Fatalf("ProjectSelectorHeaders() = %v, want %v", got, want)
	}
	for i, name := range ProjectSelectorHeaders() {
		if name != want[i] {
			t.Fatalf("ProjectSelectorHeaders()[%d] = %s, want %s", i, name, want[i])
		}
	}

	for _, header := range want {
		t.Run(header, func(t *testing.T) {
			queries := &fakeMemberQuerier{allow: map[int32]bool{teamProject: true}}

			rec, seen, _ := runSelector(NewProjectMembershipWith(queries), selectorRequest(header, "4321"))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
			if seen.ProjectID != teamProject {
				t.Errorf("%s: billed project = %d, want %d", header, seen.ProjectID, teamProject)
			}
		})
	}
}

// TestProjectSelector_OpenAIProjectIsNotASelector pins the ADR-0018 refusal.
//
// The web UI fills OpenAI-Project from model.project_id — the project that owns
// the model, not the project that pays. The models query passes includeShared,
// so that value is frequently the shared project. Reading it as the selector
// would bill the shared project for every user who copies the UI's own sample.
//
// The caller here IS a member of the named project, so the only reason the
// project can be refused is the header name.
func TestProjectSelector_OpenAIProjectIsNotASelector(t *testing.T) {
	queries := &fakeMemberQuerier{allow: map[int32]bool{teamProject: true}}

	rec, seen, invoked := runSelector(
		NewProjectMembershipWith(queries),
		selectorRequest(HeaderProjectSelectorOpenAIProject, "4321"),
	)

	assertFellBackToPersonal(t, rec, seen, invoked)
	if len(queries.calls) != 0 {
		t.Errorf("membership was queried %d times for OpenAI-Project, which is not a selector", len(queries.calls))
	}
}

// TestProjectSelector_Precedence pins the documented order: X-Project-Id wins
// over OpenAI-Organization. OpenAI-Project loses to both, because it is not
// read at all.
func TestProjectSelector_Precedence(t *testing.T) {
	queries := &fakeMemberQuerier{allow: map[int32]bool{teamProject: true, 999: true}}

	req := selectorRequest(HeaderProjectSelector, "4321")
	req.Header.Set(HeaderProjectSelectorOpenAIOrg, "999")
	req.Header.Set(HeaderProjectSelectorOpenAIProject, "999")

	_, seen, _ := runSelector(NewProjectMembershipWith(queries), req)

	if seen.ProjectID != teamProject {
		t.Errorf("billed project = %d, want %d from %s", seen.ProjectID, teamProject, HeaderProjectSelector)
	}
}

// TestProjectSelector_OpenAIProjectLosesToOpenAIOrg proves OpenAI-Project does
// not shadow the accepted OpenAI-Organization selector beside it. The UI sends
// both header names on the same request.
func TestProjectSelector_OpenAIProjectLosesToOpenAIOrg(t *testing.T) {
	queries := &fakeMemberQuerier{allow: map[int32]bool{teamProject: true, 999: true}}

	req := selectorRequest(HeaderProjectSelectorOpenAIOrg, "4321")
	req.Header.Set(HeaderProjectSelectorOpenAIProject, "999")

	_, seen, _ := runSelector(NewProjectMembershipWith(queries), req)

	if seen.ProjectID != teamProject {
		t.Errorf("billed project = %d, want %d from %s", seen.ProjectID, teamProject, HeaderProjectSelectorOpenAIOrg)
	}
	if seen.ProjectID == 999 {
		t.Errorf("billed %d, which only OpenAI-Project named", 999)
	}
}

// TestProjectSelector_MalformedIsAbsent proves a selector that names no project
// is treated as absent (spec §6.2). It is not an error: a 400 here would be the
// new failure mode issue #318 forbids.
func TestProjectSelector_MalformedIsAbsent(t *testing.T) {
	for _, value := range []string{"org-abc123", "0", "-3", "12.5", "9999999999999", "2147483648"} {
		t.Run(value, func(t *testing.T) {
			queries := &fakeMemberQuerier{allow: map[int32]bool{teamProject: true}}

			rec, seen, invoked := runSelector(
				NewProjectMembershipWith(queries),
				selectorRequest(HeaderProjectSelector, value),
			)

			assertFellBackToPersonal(t, rec, seen, invoked)
			if len(queries.calls) != 0 {
				t.Errorf("membership was queried %d times for a selector that names no project", len(queries.calls))
			}
		})
	}
}

// TestProjectSelector_BlankValueIsAbsent treats an empty header as no header,
// so a lower-precedence header may still carry the selector.
func TestProjectSelector_BlankValueIsAbsent(t *testing.T) {
	queries := &fakeMemberQuerier{allow: map[int32]bool{}}

	rec, seen, _ := runSelector(NewProjectMembershipWith(queries), selectorRequest(HeaderProjectSelector, "   "))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if seen.ProjectID != personalProject {
		t.Errorf("billed project = %d, want %d", seen.ProjectID, personalProject)
	}

	t.Run("blank header defers to the next accepted name", func(t *testing.T) {
		queries := &fakeMemberQuerier{allow: map[int32]bool{teamProject: true}}
		req := selectorRequest(HeaderProjectSelector, "")
		req.Header.Set(HeaderProjectSelectorOpenAIOrg, "4321")

		_, seen, _ := runSelector(NewProjectMembershipWith(queries), req)

		if seen.ProjectID != teamProject {
			t.Errorf("billed project = %d, want %d", seen.ProjectID, teamProject)
		}
	})
}

// TestProjectSelector_RepeatedHeaderIsAbsent follows the
// posture this service applies to identity-bearing headers: when two copies of an
// identity-bearing header disagree, no rule can say which copy the caller
// meant, so the selector is absent.
func TestProjectSelector_RepeatedHeaderIsAbsent(t *testing.T) {
	for _, name := range []string{HeaderProjectSelector, HeaderProjectSelectorOpenAIOrg} {
		t.Run(name, func(t *testing.T) {
			queries := &fakeMemberQuerier{allow: map[int32]bool{teamProject: true, personalProject: true}}

			req := selectorRequest("", "")
			req.Header.Add(name, "4321")
			req.Header.Add(name, "4321")

			rec, seen, invoked := runSelector(NewProjectMembershipWith(queries), req)

			assertFellBackToPersonal(t, rec, seen, invoked)
			if len(queries.calls) != 0 {
				t.Errorf("membership was queried %d times for a repeated header", len(queries.calls))
			}
		})
	}

	// A repeated first-precedence header does not hand the decision to the
	// next name either. Otherwise a caller could pick which value wins by
	// duplicating the one it does not want.
	t.Run("repeated X-Project-Id does not defer to OpenAI-Organization", func(t *testing.T) {
		queries := &fakeMemberQuerier{allow: map[int32]bool{teamProject: true}}

		req := selectorRequest("", "")
		req.Header.Add(HeaderProjectSelector, "1234")
		req.Header.Add(HeaderProjectSelector, "5678")
		req.Header.Set(HeaderProjectSelectorOpenAIOrg, "4321")

		rec, seen, invoked := runSelector(NewProjectMembershipWith(queries), req)

		assertFellBackToPersonal(t, rec, seen, invoked)
	})
}

// TestProjectSelector_NilCheckerFallsBack proves an unconfigured checker admits
// no selector. It falls back to the caller's own project, and never to the
// selector: a checker that cannot run must not authorize spend elsewhere.
func TestProjectSelector_NilCheckerFallsBack(t *testing.T) {
	rec, seen, invoked := runSelector(nil, selectorRequest(HeaderProjectSelector, "4321"))

	assertFellBackToPersonal(t, rec, seen, invoked)
}

// TestProjectSelector_QueryErrorFallsBack is spec §5 row 6. A membership query
// that errors is treated as "not a member". The caller's own project pays.
// Failing open here would let a database outage authorize spend on an arbitrary
// project.
func TestProjectSelector_QueryErrorFallsBack(t *testing.T) {
	queries := &fakeMemberQuerier{err: errors.New("connection refused")}

	rec, seen, invoked := runSelector(
		NewProjectMembershipWith(queries),
		selectorRequest(HeaderProjectSelector, "4321"),
	)

	assertFellBackToPersonal(t, rec, seen, invoked)
	if len(queries.calls) != 1 {
		t.Errorf("membership queries = %d, want 1", len(queries.calls))
	}
}

// TestProjectSelector_TokenScopedCallerCannotMoveSpend proves a project-scoped
// system token cannot redirect spend to another project. Its user is not a
// member there, so the request keeps the token's own project.
func TestProjectSelector_TokenScopedCallerCannotMoveSpend(t *testing.T) {
	queries := &fakeMemberQuerier{allow: map[int32]bool{}}

	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", nil)
	req.Header.Set(HeaderProjectSelector, "4321")
	user := auth.User{ID: "42", UserID: "42", TokenID: "900", Name: ":system:project:7:", AuthType: "token"}
	req = withTokenProvenance(req, user)

	rec, seen, invoked := runSelector(NewProjectMembershipWith(queries), req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !invoked {
		t.Fatal("next handler was not invoked")
	}
	if seen.ProjectID == teamProject {
		t.Errorf("token scoped to project 7 billed project %d", teamProject)
	}
	if seen.ProjectID != 7 {
		t.Errorf("billed project = %d, want the token's own project 7", seen.ProjectID)
	}
}

// TestProjectSelector_UnresolvedOwnerIsIgnored proves the check runs against the
// owning user and never against a token id. A token principal whose owner was
// not resolved cannot be checked, so it cannot name a project.
func TestProjectSelector_UnresolvedOwnerIsIgnored(t *testing.T) {
	queries := &fakeMemberQuerier{allow: map[int32]bool{teamProject: true}}

	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", nil)
	req.Header.Set(HeaderProjectSelector, "4321")
	// TokenID set, UserID empty: the owner was never resolved.
	user := auth.User{ID: "900", TokenID: "900", Name: "Regular User", AuthType: "token"}
	req = withTokenProvenance(req, user)

	rec, seen, invoked := runSelector(NewProjectMembershipWith(queries), req)

	assertFellBackToPersonal(t, rec, seen, invoked)
	if len(queries.calls) != 0 {
		t.Errorf("membership was queried %d times; the token id must never be used as a user id", len(queries.calls))
	}
}

// TestProjectHeadersStrippedOutbound proves the strip list keeps OpenAI-Project,
// which the accepted-selector list drops. The edge must not read that header,
// and it must not forward it either.
func TestProjectHeadersStrippedOutbound(t *testing.T) {
	stripped := ProjectHeadersStrippedOutbound()

	for _, name := range []string{
		HeaderProjectSelector,
		HeaderProjectSelectorOpenAIProject,
		HeaderProjectSelectorOpenAIOrg,
	} {
		found := false
		for _, got := range stripped {
			if got == name {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is not stripped from the outbound request", name)
		}
	}

	for _, accepted := range ProjectSelectorHeaders() {
		found := false
		for _, got := range stripped {
			if got == accepted {
				found = true
			}
		}
		if !found {
			t.Errorf("accepted selector %s is not stripped from the outbound request", accepted)
		}
	}
}

// TestIsProjectMember_OutOfRangeUserRefuses proves an id that does not fit the
// int4 column is refused, and that the refusal is not reported as an outage.
func TestIsProjectMember_OutOfRangeUserRefuses(t *testing.T) {
	queries := &fakeMemberQuerier{allow: map[int32]bool{teamProject: true}}
	checker := NewProjectMembershipWith(queries)

	for _, userID := range []int64{0, -1, math.MaxInt32 + 1} {
		member, err := checker.IsProjectMember(context.Background(), userID, teamProject)
		if err != nil {
			t.Fatalf("user %d: err = %v, want nil", userID, err)
		}
		if member {
			t.Errorf("user %d was admitted as a member", userID)
		}
	}
	if len(queries.calls) != 0 {
		t.Errorf("the query ran %d times for an out-of-range user id", len(queries.calls))
	}
}

// TestNewProjectMembership_NilPoolIsNilInterface guards the typed-nil trap. A
// nil *pgxpool.Pool boxed into the interface would compare non-nil and panic on
// the first query.
func TestNewProjectMembership_NilPoolIsNilInterface(t *testing.T) {
	if checker := NewProjectMembership(nil); checker != nil {
		t.Errorf("NewProjectMembership(nil) = %#v, want a nil interface", checker)
	}
	if checker := NewProjectMembershipWith(nil); checker != nil {
		t.Errorf("NewProjectMembershipWith(nil) = %#v, want a nil interface", checker)
	}
}
