package middleware

// The `/llm` half of personal-project provisioning.
//
// WHY THIS FILE EXISTS. `/social/author` and this resolver are the two readers
// of "the caller's personal project", and only the first is on the SPA's path —
// a PAT-only caller (the SDK, a scheduled job, a scripted client) reaches only
// this one, and without the hook below it stayed on `project_not_resolved` for
// good. That hook was added with no test, which is the shape this repository
// keeps re-shipping: a path that answers while the behaviour behind it was
// never composed. Nothing failed if `ensure` stopped being called.
//
// So these tests assert the CALL, not the return value: PersonalProjectID's
// answer is unchanged by provisioning (it cannot be — the project does not
// exist yet), and asserting the answer is exactly how a missing hook passes.

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// recordingEnsurer records the ids it was asked to provision.
type recordingEnsurer struct {
	mu       sync.Mutex
	askedFor []int64
}

func (e *recordingEnsurer) EnsureAsync(userID int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.askedFor = append(e.askedFor, userID)
}

func (e *recordingEnsurer) calls() []int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]int64(nil), e.askedFor...)
}

// callerContext is the principal shape the Project middleware puts on the
// context before it calls this resolver.
func callerContext(userID string) context.Context {
	return auth.ContextWithUser(context.Background(), auth.User{ID: userID, UserID: userID})
}

// noPersonalProject answers every query the resolver makes for a caller that
// has none: no named project, and an address that is not a system user.
func noPersonalProject(t *testing.T) func(sql string, args ...any) pgx.Row {
	t.Helper()
	return func(sql string, _ ...any) pgx.Row {
		switch {
		case strings.Contains(sql, "FROM centry.project"):
			return fakeRow{err: pgx.ErrNoRows}
		case strings.Contains(sql, "FROM auth_core__user"):
			return fakeRow{vals: []any{"person@example.test"}}
		}
		t.Fatalf("unexpected query: %s", sql)
		return nil
	}
}

// THE REGRESSION TEST FOR THE WIRING. Drop `WithPersonalProjectEnsurer` from
// internal/api/router.go, or stop `resolveWithoutNamedProject` calling
// `ensure`, and this goes red.
func TestPersonalProjectIDAsksForAPersonalProjectTheCallerDoesNotHave(t *testing.T) {
	ensurer := &recordingEnsurer{}
	resolver := newResolver(noPersonalProject(t)).WithPersonalProjectEnsurer(ensurer)

	id, err := resolver.PersonalProjectID(callerContext("5"), "5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The answer is still "none": provisioning happens off this call.
	if id != 0 {
		t.Fatalf("got project %d, want 0 while provisioning is still in flight", id)
	}
	if calls := ensurer.calls(); len(calls) != 1 || calls[0] != 5 {
		t.Fatalf("ensurer asked for %v, want [5]", calls)
	}
}

// A caller who already HAS a personal project must not ask for another one —
// this endpoint is on the hot path for every /llm request.
func TestPersonalProjectIDAsksForNothingWhenTheProjectResolves(t *testing.T) {
	ensurer := &recordingEnsurer{}
	resolver := newResolver(func(sql string, _ ...any) pgx.Row {
		switch {
		case strings.Contains(sql, "FROM centry.project"):
			return fakeRow{vals: []any{77}}
		case strings.Contains(sql, "project_user_role"):
			return fakeRow{vals: []any{true}}
		}
		t.Fatalf("unexpected query: %s", sql)
		return nil
	}).WithPersonalProjectEnsurer(ensurer)

	id, err := resolver.PersonalProjectID(callerContext("5"), "5")
	if err != nil || id != 77 {
		t.Fatalf("got (%d,%v), want (77,nil)", id, err)
	}
	if calls := ensurer.calls(); len(calls) != 0 {
		t.Fatalf("ensurer was asked for %v, want none", calls)
	}
}

// A system project-user resolves through the email fallback and already has a
// project. Giving one its own would mean a project per project.
func TestPersonalProjectIDAsksForNothingForASystemUser(t *testing.T) {
	ensurer := &recordingEnsurer{}
	resolver := newResolver(func(sql string, _ ...any) pgx.Row {
		switch {
		case strings.Contains(sql, "FROM centry.project"):
			return fakeRow{err: pgx.ErrNoRows}
		case strings.Contains(sql, "FROM auth_core__user"):
			return fakeRow{vals: []any{"system_user_9@centry.user"}}
		}
		t.Fatalf("unexpected query: %s", sql)
		return nil
	}).WithPersonalProjectEnsurer(ensurer)

	id, err := resolver.PersonalProjectID(callerContext("5"), "5")
	if err != nil || id != 9 {
		t.Fatalf("got (%d,%v), want (9,nil)", id, err)
	}
	if calls := ensurer.calls(); len(calls) != 0 {
		t.Fatalf("ensurer was asked for %v, want none", calls)
	}
}

// THE PRINCIPAL GUARD. `auth.User.OwningUserID` refuses a token principal whose
// id is a TOKEN id, and `/social/author` applies it before asking for the same
// work. Both readers have to judge a principal the same way — otherwise
// `project_user_<token id>` names a project belonging to whichever account
// happens to share that number.
func TestPersonalProjectIDAsksForNothingWhenThePrincipalIsNotAnOwningUser(t *testing.T) {
	for name, principal := range map[string]auth.User{
		// A token that resolved no owner: ID carries the token row's id.
		"unresolved token principal": {ID: "5", TokenID: "5", AuthType: "token"},
		// A development stub or forwarded header with a non-numeric id.
		"non-numeric principal": {ID: "dev-user", UserID: "dev-user"},
		// The principal names a different account than the id under resolution.
		"principal for another account": {ID: "6", UserID: "6"},
	} {
		ensurer := &recordingEnsurer{}
		resolver := newResolver(noPersonalProject(t)).WithPersonalProjectEnsurer(ensurer)

		if _, err := resolver.PersonalProjectID(
			auth.ContextWithUser(context.Background(), principal), "5",
		); err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		if calls := ensurer.calls(); len(calls) != 0 {
			t.Fatalf("%s: ensurer was asked for %v, want none", name, calls)
		}
	}
}

// A resolver with no ensurer is the read-only one this type always was, and it
// must keep answering rather than panic.
func TestPersonalProjectIDWithoutAnEnsurerStillResolves(t *testing.T) {
	resolver := newResolver(noPersonalProject(t))

	id, err := resolver.PersonalProjectID(callerContext("5"), "5")
	if err != nil || id != 0 {
		t.Fatalf("got (%d,%v), want (0,nil)", id, err)
	}
}
