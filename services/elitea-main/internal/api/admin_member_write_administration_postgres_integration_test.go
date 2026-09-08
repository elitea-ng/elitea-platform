package api

// The admin Projects page's "Manage project member" dialog, end to end (F1).
//
// # THE DEFECT
//
// `POST /api/v2/admin/users/administration/{projectID}` is registered with
// `administration` as a STATIC path segment, deliberately: chi's trie must
// prefer it over the `{mode}` route beside it, whose gate resolves from the
// caller's MEMBERSHIP of the named project and therefore refuses every
// operator the dialog exists for.
//
// A static segment binds no URL parameter. The shared handler read
// `chi.URLParam(r, "mode")`, got "", fell through its switch and answered
// `404 {"error":"unknown mode"}` — to every submit of that dialog, in every
// deployment. The GET on the same static path worked throughout, because
// Handler.Users reads no mode, so the dialog listed members and could not add
// one.
//
// # WHY THIS TEST IS AN HTTP TEST AGAINST A REAL DATABASE
//
// The claim has three parts and no smaller surface carries all three:
//
//	the ROUTE resolves administration mode      → only the real router shows it
//	the GATE admits a NON-MEMBER global admin   → only the real resolver shows it
//	the WRITE lands                             → only the real table shows it
//
// A handler test with a hand-mounted `{mode}` route passes against the broken
// build, because it supplies the parameter the real route cannot. A status-only
// assertion passes against a handler that writes nothing — the #130 lesson this
// endpoint already learned once. So the caller here holds an administration
// role and no membership at all, and the assertion is a row in
// `auth_core__project_user_role`.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	v2skills "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

const (
	// The project the operator is NOT a member of. newCredentialJourneyPool
	// creates it as `other-tenant`, which is exactly the shape the dialog is
	// opened against: a project on the admin Projects list that the operator
	// has never joined.
	memberWriteProjectID = 2
	// The operator: an administration-mode `admin`, and nothing else. It holds
	// no project role anywhere, so a default-mode gate scores it zero.
	memberWriteAdminID = 9101
	// The control: an ordinary authenticated account with no central role.
	memberWritePlainID  = 9102
	memberWriteInvitee  = "f1-invited@autotest.local"
	memberWriteDeadline = 90 * time.Second
)

// seedMemberWriteFixture gives the project the four roles a provisioned
// project has, and gives the operator the administration role that holds
// `configuration.users.users.create` (shared/0082).
//
// The project roles are seeded here rather than assumed: the invite resolves
// the role NAMES the dialog sends against auth_core__project_role for that
// project, and a project with no roles rejects every name.
func seedMemberWriteFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), memberWriteDeadline)
	defer cancel()

	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__project_role (project_id, name)
SELECT $1, role_name
FROM (VALUES ('admin'), ('editor'), ('viewer'), ('system')) AS roles(role_name)
ON CONFLICT (project_id, name) DO NOTHING`, memberWriteProjectID); err != nil {
		t.Fatalf("seed the project roles: %v", err)
	}

	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user (id, email, name)
VALUES ($1, 'f1-operator@autotest.local', 'Operator'),
       ($2, 'f1-plain@autotest.local', 'Plain')
ON CONFLICT (id) DO NOTHING`, memberWriteAdminID, memberWritePlainID); err != nil {
		t.Fatalf("seed the callers: %v", err)
	}

	// The operator's ONLY grant. No auth_core__project_user_role row is
	// written for it anywhere: a membership would make the default-mode route
	// answer too, and this test could no longer tell the two apart.
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, role.id
FROM public.auth_core__role AS role
WHERE role.mode = 'administration' AND role.name = 'admin'
ON CONFLICT (user_id, role_id) DO NOTHING`, memberWriteAdminID); err != nil {
		t.Fatalf("grant the operator its administration role: %v", err)
	}

	var granted bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM public.auth_core__user_role AS assignment
    JOIN public.auth_core__role AS role ON role.id = assignment.role_id
    JOIN public.auth_core__role_permission AS grant_row ON grant_row.role_id = role.id
    WHERE assignment.user_id = $1
      AND role.mode = 'administration'
      AND grant_row.permission = 'configuration.users.users.create'
)`, memberWriteAdminID).Scan(&granted); err != nil {
		t.Fatalf("read the operator's grant: %v", err)
	}
	if !granted {
		t.Fatal("the operator does not hold configuration.users.users.create in " +
			"administration mode; the fixture, not the route, is what this run would measure")
	}
}

func newMemberWriteRouter(pool *pgxpool.Pool, userID int) http.Handler {
	id := fmt.Sprintf("%d", userID)
	return NewRouter(RouterConfig{
		Pool:       pool,
		SkillsRepo: struct{ v2skills.Repository }{},
		AuthValidator: testTokenValidator{user: auth.User{
			ID:     id,
			UserID: id,
			Email:  "f1-" + id + "@autotest.local",
			// AuthType is deliberately NOT "token": legacyrbac resolves a
			// token principal through auth_core__token, and this fixture mints
			// no token row. A session-shaped principal is resolved by its
			// UserID, which is the shape a browser session carries and the
			// shape the admin panel's caller has.
		}},
		PrincipalValidator: testPrincipalValidator{},
	})
}

func serveMemberWrite(
	t *testing.T, router http.Handler, method, path string, body any,
) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal the request body: %v", err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, testAuthHeader(request))
	return recorder
}

// projectRolesOf reads the role names the named address holds in the project,
// straight from the table the write is supposed to touch.
func projectRolesOf(t *testing.T, pool *pgxpool.Pool, projectID int, email string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), memberWriteDeadline)
	defer cancel()

	rows, err := pool.Query(ctx, `
SELECT role.name
FROM public.auth_core__project_user_role AS assignment
JOIN public.auth_core__project_role AS role ON role.id = assignment.role_id
JOIN public.auth_core__user AS account ON account.id = assignment.user_id
WHERE assignment.project_id = $1 AND account.email = $2
ORDER BY role.name`, projectID, email)
	if err != nil {
		t.Fatalf("read the project membership: %v", err)
	}
	defer rows.Close()

	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan a membership row: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the membership rows: %v", err)
	}
	return names
}

// A global administrator who is a member of nothing adds a member, and the row
// exists afterwards.
//
// Against the pre-fix build this fails at the status line with
// `404 {"error":"unknown mode"}`, which is what the dialog reported.
func TestAdministrationModeMemberInviteWritesTheMembershipRow(t *testing.T) {
	pool := newCredentialJourneyPool(t)
	seedMemberWriteFixture(t, pool)
	router := newMemberWriteRouter(pool, memberWriteAdminID)

	if before := projectRolesOf(t, pool, memberWriteProjectID, memberWriteInvitee); len(before) != 0 {
		t.Fatalf("the fixture already makes %s a member: %v", memberWriteInvitee, before)
	}

	path := fmt.Sprintf("/api/v2/admin/users/administration/%d", memberWriteProjectID)
	recorder := serveMemberWrite(t, router, http.MethodPost, path, map[string]any{
		"emails": []string{memberWriteInvitee},
		"roles":  []string{"editor"},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST %s status = %d, want 200 (body %s).\n"+
			"  404 with \"unknown mode\" means the handler read {mode} on a route "+
			"that binds no such parameter, which is what every submit of the "+
			"admin member dialog answered.", path, recorder.Code, recorder.Body.String())
	}

	roles := projectRolesOf(t, pool, memberWriteProjectID, memberWriteInvitee)
	if len(roles) != 1 || roles[0] != "editor" {
		t.Fatalf("auth_core__project_user_role for %s = %v, want [editor]; "+
			"a 200 that wrote nothing is the #130 defect in a second shape",
			memberWriteInvitee, roles)
	}

	// The role EDIT the same dialog submits, on the same static route.
	var invitedID int
	ctx, cancel := context.WithTimeout(context.Background(), memberWriteDeadline)
	defer cancel()
	if err := pool.QueryRow(ctx,
		`SELECT id FROM public.auth_core__user WHERE email = $1`, memberWriteInvitee,
	).Scan(&invitedID); err != nil {
		t.Fatalf("read the invited user id: %v", err)
	}
	recorder = serveMemberWrite(t, router, http.MethodPut, path, map[string]any{
		"userId": fmt.Sprintf("%d", invitedID),
		"roles":  []string{"admin"},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT %s status = %d, want 200 (body %s)",
			path, recorder.Code, recorder.Body.String())
	}
	if roles := projectRolesOf(t, pool, memberWriteProjectID, memberWriteInvitee); len(roles) != 1 ||
		roles[0] != "admin" {
		t.Fatalf("after the role edit the membership is %v, want [admin]", roles)
	}
}

// The refusing direction. Without it, a route that admitted EVERY caller would
// satisfy the test above.
func TestAdministrationModeMemberInviteRefusesACallerWithNoCentralRole(t *testing.T) {
	pool := newCredentialJourneyPool(t)
	seedMemberWriteFixture(t, pool)
	router := newMemberWriteRouter(pool, memberWritePlainID)

	const refused = "f1-refused@autotest.local"
	path := fmt.Sprintf("/api/v2/admin/users/administration/%d", memberWriteProjectID)
	recorder := serveMemberWrite(t, router, http.MethodPost, path, map[string]any{
		"emails": []string{refused},
		"roles":  []string{"editor"},
	})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("POST %s as a caller with no administration role status = %d, "+
			"want 403 (body %s)", path, recorder.Code, recorder.Body.String())
	}
	if roles := projectRolesOf(t, pool, memberWriteProjectID, refused); len(roles) != 0 {
		t.Fatalf("the refused request still wrote %v", roles)
	}
}
