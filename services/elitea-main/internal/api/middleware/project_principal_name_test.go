package middleware

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// These tests cover issue #459: the ":system:project:<id>:" branch of
// resolveProjectID took the billing project from auth.User.Name with no
// membership check.
//
// The project ids are the ids the issue names. The caller's own project is a
// third number, so an assertion cannot pass by accident: a test that named the
// caller's own project would pass whether or not the name is honoured.
const (
	// namedForeignProject is the project the name asks for, and the caller is
	// not a member of it.
	namedForeignProject = 9999
	// namedOwnProject is the project the name asks for, and the caller IS a
	// member of it.
	namedOwnProject = 42
	// nameCallerProject is the caller's own project, from the resolver.
	nameCallerProject = 7
)

// nameRequest builds an authenticated /llm request whose principal name asks
// for a project. The user id is not any of the project ids above.
func nameRequest(name string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", nil)
	user := auth.User{ID: "11", UserID: "11", TokenID: "900", Name: name, AuthType: "token"}
	return withTokenProvenance(req, user)
}

// runNamed drives the middleware over a membership answer and reports what the
// next handler saw.
func runNamed(
	membership ProjectMembershipChecker,
	resolver *fakeResolver,
	req *http.Request,
) (*httptest.ResponseRecorder, ProjectContext, bool) {
	var seen ProjectContext
	var invoked bool
	mw := Project(ProjectConfig{
		Resolver:        resolver,
		PublicProjectID: 1,
		Membership:      membership,
	})
	rec := httptest.NewRecorder()
	mw(captureHandler(&seen, &invoked)).ServeHTTP(rec, req)
	return rec, seen, invoked
}

// TestPrincipalName_ForeignProjectIsRefused is direction 1 of issue #459. The
// name asks for project 9999. The caller is not a member of 9999. The resolved
// project must not be 9999.
func TestPrincipalName_ForeignProjectIsRefused(t *testing.T) {
	queries := &fakeMemberQuerier{allow: map[int32]bool{}}
	resolver := &fakeResolver{id: nameCallerProject}

	rec, seen, invoked := runNamed(
		NewProjectMembershipWith(queries),
		resolver,
		nameRequest(":system:project:9999:"),
	)

	if seen.ProjectID == namedForeignProject {
		t.Fatalf("billed project %d, which the principal name asked for and the caller may not use",
			namedForeignProject)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; the refusal is silent; body = %s", rec.Code, rec.Body.String())
	}
	if !invoked {
		t.Fatal("next handler was not invoked")
	}
	if seen.ProjectID != nameCallerProject {
		t.Errorf("billed project = %d, want the caller's own project %d", seen.ProjectID, nameCallerProject)
	}
	if len(queries.calls) != 1 {
		t.Fatalf("membership queries = %d, want 1", len(queries.calls))
	}
	if queries.calls[0].UserID != 11 || queries.calls[0].ProjectID != namedForeignProject {
		t.Errorf("membership asked for user %d project %d, want user 11 project %d",
			queries.calls[0].UserID, queries.calls[0].ProjectID, namedForeignProject)
	}
}

// TestPrincipalName_MemberProjectIsBilled is direction 2 of issue #459. The
// name asks for project 42. The caller is a member of 42. The legacy system
// project-user behaviour is unchanged.
func TestPrincipalName_MemberProjectIsBilled(t *testing.T) {
	queries := &fakeMemberQuerier{allow: map[int32]bool{namedOwnProject: true}}
	resolver := &fakeResolver{id: nameCallerProject}

	rec, seen, invoked := runNamed(
		NewProjectMembershipWith(queries),
		resolver,
		nameRequest(":system:project:42:"),
	)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !invoked {
		t.Fatal("next handler was not invoked")
	}
	if seen.ProjectID != namedOwnProject {
		t.Errorf("billed project = %d, want the named project %d", seen.ProjectID, namedOwnProject)
	}
	if resolver.called {
		t.Error("resolver must not be called for an admitted system project-user name")
	}
}

// TestPrincipalName_MembershipErrorIsRefused proves the fail-closed rule
// (spec-llm-project-scope §7 invariant 5). A database outage must not
// authorize spend on the project the name asks for.
func TestPrincipalName_MembershipErrorIsRefused(t *testing.T) {
	queries := &fakeMemberQuerier{err: errors.New("pool exhausted")}
	resolver := &fakeResolver{id: nameCallerProject}

	rec, seen, invoked := runNamed(
		NewProjectMembershipWith(queries),
		resolver,
		nameRequest(":system:project:9999:"),
	)

	if seen.ProjectID == namedForeignProject {
		t.Fatal("a failed membership query authorized the project the name asked for")
	}
	if rec.Code != http.StatusOK || !invoked {
		t.Fatalf("status = %d invoked = %v, want 200 and true", rec.Code, invoked)
	}
	if seen.ProjectID != nameCallerProject {
		t.Errorf("billed project = %d, want the caller's own project %d", seen.ProjectID, nameCallerProject)
	}
}

// TestPrincipalName_UnresolvedOwnerIsRefused proves that membership is a
// property of the owning user. A token principal whose owner is unknown cannot
// be checked, so its name cannot name a project.
func TestPrincipalName_UnresolvedOwnerIsRefused(t *testing.T) {
	queries := &fakeMemberQuerier{allow: map[int32]bool{namedForeignProject: true}}
	resolver := &fakeResolver{id: nameCallerProject}

	req := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", nil)
	// AuthType "token" with no UserID: OwningUserID refuses to answer.
	user := auth.User{ID: "900", TokenID: "900", Name: ":system:project:9999:", AuthType: "token"}
	req = withTokenProvenance(req, user)

	_, seen, _ := runNamed(NewProjectMembershipWith(queries), resolver, req)

	if seen.ProjectID == namedForeignProject {
		t.Fatal("a principal with no resolved owner named a project")
	}
	if len(queries.calls) != 0 {
		t.Errorf("membership queries = %d, want 0; the owner was never resolved", len(queries.calls))
	}
}
