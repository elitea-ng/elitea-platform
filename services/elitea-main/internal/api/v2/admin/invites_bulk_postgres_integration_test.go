package admin_test

// Acceptance for the cross-project bulk membership invite (issue 247,
// internal/api/v2/admin/invites_bulk.go).
//
// A 200 proves nothing here. pylon's own version of this endpoint answers
// `{"ok": true, "logs": "..."}` for every request it does not crash on, and the
// class of defect this platform keeps finding — a route that answers success
// and writes nothing (#128, #130) — is exactly what a bulk write invites. So
// every case below reads `public.auth_core__project_user_role` back through SQL
// after the call, and the refusal cases assert that NOTHING was written.
//
// The fixture is the roles fixture (roles_write_postgres_integration_test.go),
// which already seeds the five projects these cases need:
//
//	1 promptlib_public   — roles admin/editor/viewer/system
//	2 a14-shared         — the same roles
//	3 project_user_9     — a PERSONAL project, refused
//	4 a14-support        — the same roles
//	5 projectAuserB-team — a SHARED project with NO roles at all

import (
	"context"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

/* ── harness ───────────────────────────────────────────────────────────── */

type bulkInviteResultBody struct {
	UserID      int64  `json:"user_id"`
	UserEmail   string `json:"user_email"`
	ProjectID   int64  `json:"project_id"`
	ProjectName string `json:"project_name"`
	Status      string `json:"status"`
	Outcome     string `json:"outcome"`
	Msg         string `json:"msg"`
}

type bulkInviteSummaryBody struct {
	OK        bool                   `json:"ok"`
	Role      string                 `json:"role"`
	Requested int                    `json:"requested"`
	Added     int                    `json:"added"`
	Skipped   int                    `json:"skipped"`
	Failed    int                    `json:"failed"`
	Results   []bulkInviteResultBody `json:"results"`
}

func (b bulkInviteSummaryBody) outcomeFor(t *testing.T, userID, projectID int64) bulkInviteResultBody {
	t.Helper()
	for _, result := range b.Results {
		if result.UserID == userID && result.ProjectID == projectID {
			return result
		}
	}
	t.Fatalf("no result for user %d in project %d; results = %+v", userID, projectID, b.Results)
	return bulkInviteResultBody{}
}

// bulkInviteRouter mounts the route exactly as internal/api/router.go does,
// minus the route-level permission middleware (covered by
// TestRequireCentralPermissions* in internal/api/middleware and by
// TestEveryGatedPermissionHasAGrant in internal/api).
func bulkInviteRouter(handler *admin.Handler) chi.Router {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal := auth.User{ID: "1", UserID: "1"}
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), principal)))
		})
	})
	router.Post("/admin/invites_bulk/administration", handler.BulkInviteMembers)
	return router
}

// newBulkInviteEnvironment seeds the roles fixture plus three accounts: two
// ordinary users and one service account, which pylon's own loop skips
// (invites_bulkusers.py:57).
func newBulkInviteEnvironment(t *testing.T) (*pgxpool.Pool, chi.Router, int64, int64, int64) {
	t.Helper()
	pool := newRolesPool(t)
	prepareRolesFixture(t, pool)

	ctx := context.Background()
	newUser := func(email, name string) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO public.auth_core__user (email, name) VALUES ($1, $2) RETURNING id`,
			email, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := newUser("bulk-first@autotest.local", "First")
	second := newUser("bulk-second@autotest.local", "Second")
	// The service account. Both shapes the resolver refuses are present: the
	// pylon name prefix AND this platform's own `@centry.user` address.
	service := newUser("project_4@centry.user", ":system:project:4")
	return pool, bulkInviteRouter(admin.NewHandler(pool)), first, second, service
}

// storedRoleNames reads the membership table directly. This is the assertion a
// route that answers 200 and writes nothing cannot pass.
func storedRoleNames(t *testing.T, pool *pgxpool.Pool, projectID, userID int64) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT role.name
FROM public.auth_core__project_user_role AS assignment
JOIN public.auth_core__project_role AS role
  ON role.id = assignment.role_id AND role.project_id = assignment.project_id
WHERE assignment.project_id = $1 AND assignment.user_id = $2
ORDER BY role.name`, projectID, userID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

func postBulkInvite(t *testing.T, router chi.Router, body any) (int, bulkInviteSummaryBody) {
	t.Helper()
	recorder := adminDo(t, router, http.MethodPost, "/admin/invites_bulk/administration", body)
	if recorder.Code != http.StatusOK {
		return recorder.Code, bulkInviteSummaryBody{}
	}
	var summary bulkInviteSummaryBody
	decodeJSONBody(t, recorder.Body.Bytes(), &summary)
	return recorder.Code, summary
}

/* ── the cross product ─────────────────────────────────────────────────── */

func TestBulkInviteWritesEveryPairOfTheCrossProduct(t *testing.T) {
	pool, router, first, second, _ := newBulkInviteEnvironment(t)

	status, summary := postBulkInvite(t, router, map[string]any{
		"users": []int64{first, second}, "projects": []int64{1, 2}, "role": "editor",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !summary.OK || summary.Requested != 4 || summary.Added != 4 || summary.Failed != 0 {
		t.Fatalf("summary = %+v, want ok with 4 requested and 4 added", summary)
	}

	for _, userID := range []int64{first, second} {
		for _, projectID := range []int64{1, 2} {
			if names := storedRoleNames(t, pool, projectID, userID); len(names) != 1 || names[0] != "editor" {
				t.Fatalf("user %d holds %v in project %d, want [editor]", userID, names, projectID)
			}
		}
	}
	// The e-mail is carried so a console can render the report without holding
	// the source list; an empty one would make every row read as "unknown".
	if result := summary.outcomeFor(t, first, 1); result.UserEmail != "bulk-first@autotest.local" {
		t.Fatalf("result carries user_email %q", result.UserEmail)
	}
}

func TestBulkInviteAddsTheRoleAndKeepsTheOnesAlreadyHeld(t *testing.T) {
	pool, router, first, _, _ := newBulkInviteEnvironment(t)

	// The user is already an admin of project 2. pylon's own write REPLACES
	// the role set (rpc/roles.py:137-148), which over "all users" demotes every
	// project admin to whatever the operator typed. This must not.
	if _, err := pool.Exec(context.Background(), `
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
SELECT 2, $1, role.id FROM public.auth_core__project_role role
WHERE role.project_id = 2 AND role.name = 'admin'`, first); err != nil {
		t.Fatal(err)
	}

	if _, summary := postBulkInvite(t, router, map[string]any{
		"users": []int64{first}, "projects": []int64{2}, "role": "viewer",
	}); summary.Added != 1 {
		t.Fatalf("summary = %+v, want one addition", summary)
	}

	names := storedRoleNames(t, pool, 2, first)
	if len(names) != 2 || names[0] != "admin" || names[1] != "viewer" {
		t.Fatalf("stored roles = %v, want [admin viewer] — the existing role must survive", names)
	}
}

/* ── mixed batch ───────────────────────────────────────────────────────── */

// TestBulkInviteReportsEveryOutcomeOfAMixedBatch is the case pylon cannot
// express: one submission whose pairs end five different ways, each named.
func TestBulkInviteReportsEveryOutcomeOfAMixedBatch(t *testing.T) {
	pool, router, first, _, service := newBulkInviteEnvironment(t)
	const missingUser = int64(999001)
	const missingProject = int64(999002)

	status, summary := postBulkInvite(t, router, map[string]any{
		// project 1 succeeds, project 3 is personal, project 5 has no roles,
		// and project 999002 does not exist.
		"users":    []int64{first, service, missingUser},
		"projects": []int64{1, 3, 5, missingProject},
		"role":     "editor",
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a partial batch is a report, not a rejected write", status)
	}
	if summary.OK {
		t.Fatal("summary.ok is true, but nine of the twelve pairs were refused")
	}
	if summary.Requested != 12 || summary.Added != 1 {
		t.Fatalf("summary = %+v, want 12 requested and exactly 1 added", summary)
	}

	for _, expected := range []struct {
		userID, projectID int64
		outcome           string
	}{
		{first, 1, "added"},
		{first, 3, "personal_project"},
		{first, 5, "unknown_role"},
		{first, missingProject, "unknown_project"},
		{service, 1, "system_user"},
		{missingUser, 1, "unknown_user"},
	} {
		result := summary.outcomeFor(t, expected.userID, expected.projectID)
		if result.Outcome != expected.outcome {
			t.Errorf("pair (user %d, project %d) reported %q, want %q (msg %q)",
				expected.userID, expected.projectID, result.Outcome, expected.outcome, result.Msg)
		}
	}

	// Only the ONE valid pair was written. A bulk write that also placed the
	// service account, or wrote into the personal project, would still have
	// answered the same summary shape.
	if names := storedRoleNames(t, pool, 1, first); len(names) != 1 || names[0] != "editor" {
		t.Fatalf("user %d holds %v in project 1, want [editor]", first, names)
	}
	for _, refused := range []struct{ projectID, userID int64 }{
		{3, first}, {5, first}, {1, service},
	} {
		if names := storedRoleNames(t, pool, refused.projectID, refused.userID); len(names) != 0 {
			t.Fatalf("refused pair (user %d, project %d) still wrote %v",
				refused.userID, refused.projectID, names)
		}
	}
}

/* ── unknown role ──────────────────────────────────────────────────────── */

// The role is resolved PER PROJECT: auth_core__project_role is keyed on
// (project_id, name). A project that does not define the name must be refused,
// never handed another project's role row — which would grant a privilege the
// operator did not ask for, in a project they did not look at.
func TestBulkInviteRefusesARoleTheProjectDoesNotDefine(t *testing.T) {
	pool, router, first, _, _ := newBulkInviteEnvironment(t)

	_, summary := postBulkInvite(t, router, map[string]any{
		"users": []int64{first}, "projects": []int64{1, 5}, "role": "editor",
	})
	if summary.OK || summary.Added != 1 || summary.Failed != 1 {
		t.Fatalf("summary = %+v, want one addition and one failure", summary)
	}
	if result := summary.outcomeFor(t, first, 5); result.Outcome != "unknown_role" {
		t.Fatalf("project 5 reported %q, want unknown_role", result.Outcome)
	}
	if names := storedRoleNames(t, pool, 5, first); len(names) != 0 {
		t.Fatalf("project 5 has no editor role, yet %v was written", names)
	}

	// A role name that exists NOWHERE is the same refusal, with nothing
	// written anywhere.
	_, summary = postBulkInvite(t, router, map[string]any{
		"users": []int64{first}, "projects": []int64{1, 2}, "role": "wizard",
	})
	if summary.OK || summary.Added != 0 || summary.Failed != 2 {
		t.Fatalf("summary = %+v, want two failures", summary)
	}
	if names := storedRoleNames(t, pool, 2, first); len(names) != 0 {
		t.Fatalf("an undefined role wrote %v into project 2", names)
	}
}

/* ── cross-tenant refusal ──────────────────────────────────────────────── */

// A personal project is one account's own space — the platform provisions
// exactly one per user, named `project_user_<id>` — so a bulk form may not
// place a stranger in it. pylon has no such guard: its "add every user to a
// project" page would write every account on the platform into project_user_9.
func TestBulkInviteRefusesAPersonalProject(t *testing.T) {
	pool, router, first, second, _ := newBulkInviteEnvironment(t)

	_, summary := postBulkInvite(t, router, map[string]any{
		"users": []int64{first, second}, "projects": []int64{3}, "role": "editor",
	})
	if summary.OK || summary.Added != 0 || summary.Failed != 2 {
		t.Fatalf("summary = %+v, want both pairs refused", summary)
	}
	for _, userID := range []int64{first, second} {
		if result := summary.outcomeFor(t, userID, 3); result.Outcome != "personal_project" {
			t.Fatalf("user %d reported %q, want personal_project", userID, result.Outcome)
		}
		if names := storedRoleNames(t, pool, 3, userID); len(names) != 0 {
			t.Fatalf("user %d was written into the personal project as %v", userID, names)
		}
	}
}

// The refusal is ANCHORED on the provisioning shape, not on a LIKE pattern
// whose `_` is a wildcard. Project 5 is a SHARED project whose name matches
// `project_user_%` only under that reading, and refusing it would leave an
// operator with a team project they cannot fill.
func TestBulkInviteDoesNotMistakeATeamProjectForAPersonalOne(t *testing.T) {
	pool, router, first, _, _ := newBulkInviteEnvironment(t)

	// Give project 5 the role it lacks, so the only thing left that could
	// refuse the pair is the personal-project guard.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO public.auth_core__project_role (project_id, name) VALUES (5, 'editor')`); err != nil {
		t.Fatal(err)
	}

	_, summary := postBulkInvite(t, router, map[string]any{
		"users": []int64{first}, "projects": []int64{5}, "role": "editor",
	})
	if result := summary.outcomeFor(t, first, 5); result.Outcome != "added" {
		t.Fatalf("project 5 (projectAuserB-team) reported %q, want added — it is a shared project",
			result.Outcome)
	}
	if names := storedRoleNames(t, pool, 5, first); len(names) != 1 || names[0] != "editor" {
		t.Fatalf("stored roles in project 5 = %v, want [editor]", names)
	}
}

/* ── idempotence ───────────────────────────────────────────────────────── */

// Re-running the same batch is expected: an operator who is unsure whether the
// first submission landed will submit it again. The second run must report
// `already_member`, not a second grant and not an error.
func TestBulkInviteIsIdempotentOnASecondRun(t *testing.T) {
	pool, router, first, second, _ := newBulkInviteEnvironment(t)
	batch := map[string]any{
		"users": []int64{first, second}, "projects": []int64{1, 4}, "role": "viewer",
	}

	if _, summary := postBulkInvite(t, router, batch); summary.Added != 4 {
		t.Fatalf("first run = %+v, want 4 additions", summary)
	}
	_, summary := postBulkInvite(t, router, batch)
	if !summary.OK || summary.Added != 0 || summary.Skipped != 4 || summary.Failed != 0 {
		t.Fatalf("second run = %+v, want 4 skipped and nothing added or failed", summary)
	}
	for _, result := range summary.Results {
		if result.Outcome != "already_member" || result.Status != "ok" {
			t.Fatalf("re-run pair %+v, want an ok/already_member row", result)
		}
	}
	// One role row per pair, not two. ON CONFLICT DO NOTHING is what makes the
	// re-run safe, and a missing unique constraint would show up here.
	for _, userID := range []int64{first, second} {
		for _, projectID := range []int64{1, 4} {
			if names := storedRoleNames(t, pool, projectID, userID); len(names) != 1 {
				t.Fatalf("user %d holds %v in project %d after two runs, want one row",
					userID, names, projectID)
			}
		}
	}
}

// Duplicate ids in one submission are the same problem inside a single request.
func TestBulkInviteDeduplicatesTheSubmittedIds(t *testing.T) {
	_, router, first, _, _ := newBulkInviteEnvironment(t)

	_, summary := postBulkInvite(t, router, map[string]any{
		"users": []int64{first, first, first}, "projects": []int64{1, 1}, "role": "editor",
	})
	if summary.Requested != 1 || summary.Added != 1 || len(summary.Results) != 1 {
		t.Fatalf("summary = %+v, want one pair after de-duplication", summary)
	}
}

/* ── request validation ────────────────────────────────────────────────── */

func TestBulkInviteRejectsAnIncompleteRequest(t *testing.T) {
	_, router, first, _, _ := newBulkInviteEnvironment(t)

	for name, body := range map[string]any{
		"no users":    map[string]any{"projects": []int64{1}, "role": "editor"},
		"no projects": map[string]any{"users": []int64{first}, "role": "editor"},
		"no role":     map[string]any{"users": []int64{first}, "projects": []int64{1}},
		"blank role":  map[string]any{"users": []int64{first}, "projects": []int64{1}, "role": "   "},
		"zero ids":    map[string]any{"users": []int64{0}, "projects": []int64{1}, "role": "editor"},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := adminDo(t, router, http.MethodPost, "/admin/invites_bulk/administration", body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
			}
		})
	}
}
