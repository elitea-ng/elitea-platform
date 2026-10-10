package social_test

// WHICH PROJECT `GET /social/author` IS ALLOWED TO CALL PERSONAL.
//
// THE DEFECT (issue 843). The resolver's third branch answered "the lowest-id
// project the caller holds a role in". For a member of a shared project that
// is the SHARED PROJECT: the switcher labelled a team project "Private", and —
// the part that made it permanent — the answer was not "". Provisioning re-arms
// only on "", so an account misdirected here was never given a personal project
// at all. An account that is a member of nothing was answered "" and recovered
// on its next request, which is why the defect looked like it did not exist.
//
// These tests run WITHOUT an ensurer on purpose. What is under test is what the
// resolver ANSWERS, and an ensurer would provision the missing project inside
// the request and change the answer under the assertion.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/social"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/personalproject"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// A MEMBERSHIP-ONLY SHARED PROJECT IS NOT A PERSONAL PROJECT.
//
// This is the branch that misdirected a whole team. The answer must be "", so
// that the SPA reads "no personal project yet" and the next authenticated
// request provisions the real one.
func TestASharedProjectMemberIsNotToldItIsTheirPersonalProject(t *testing.T) {
	pool := newPersonalProjectSocialPool(t)
	userID := seedAuthorUser(t, pool, "shared-only@autotest.local", "Teammate")
	lead := seedAuthorUser(t, pool, "team-lead@autotest.local", "Lead")
	seedProjectMembership(t, pool, seedProject(t, pool, "team-alpha", lead), userID)

	if got := readPersonalProjectID(t, pool, userID, "shared-only@autotest.local"); got != "" {
		t.Fatalf("personal_project_id = %q for a member of a shared project only; "+
			"the switcher labels that project \"Private\" and provisioning never "+
			"re-arms, so this account has no private space for good", got)
	}
}

// THE OWNER-NAMED PROJECT IS STILL ANSWERED, and it is answered even when a
// shared project with a LOWER id exists — the branch that used to win.
func TestTheCallersOwnPersonalProjectIsAnsweredAheadOfAnyShared(t *testing.T) {
	pool := newPersonalProjectSocialPool(t)
	userID := seedAuthorUser(t, pool, "has-both@autotest.local", "Owner")

	lead := seedAuthorUser(t, pool, "beta-lead@autotest.local", "Lead")
	// Seeded first, so it holds the lower id.
	seedProjectMembership(t, pool, seedProject(t, pool, "team-beta", lead), userID)
	personalID := seedProject(t, pool, personalproject.Name(userID), userID)
	seedProjectMembership(t, pool, personalID, userID)

	got := readPersonalProjectID(t, pool, userID, "has-both@autotest.local")
	if want := strconv.FormatInt(personalID, 10); got != want {
		t.Fatalf("personal_project_id = %q, want %q — the caller's own "+
			"project_user_<uid> project", got, want)
	}
}

// A PROJECT WITH THE RIGHT NAME THAT THE CALLER IS NOT A MEMBER OF IS NOT
// THEIRS EITHER. `centry.project.name` is free text on `POST /projects` and
// carries no unique index, so somebody else's row can hold this name; the
// resolver is membership-checked precisely because this value is used as an
// authorization scope (issue #166).
func TestAPersonalNameTheCallerIsNotAMemberOfIsNotAnswered(t *testing.T) {
	pool := newPersonalProjectSocialPool(t)
	userID := seedAuthorUser(t, pool, "name-squatted@autotest.local", "Unlucky")
	squatter := seedAuthorUser(t, pool, "squatter@autotest.local", "Squatter")
	seedProjectMembership(t, pool, seedProject(t, pool, personalproject.Name(userID), squatter), squatter)

	if got := readPersonalProjectID(t, pool, userID, "name-squatted@autotest.local"); got != "" {
		t.Fatalf("personal_project_id = %q for a project the caller holds no role in; "+
			"every read scoped to it answers 403", got)
	}
}

// The system-user fallback is untouched by the change: those identities already
// have a project and must not be given a second one.
func TestASystemUserStillResolvesThroughItsEmail(t *testing.T) {
	pool := newPersonalProjectSocialPool(t)
	userID := seedAuthorUser(t, pool, "system_user_77@centry.user", ":system:project:77")

	if got := readPersonalProjectID(t, pool, userID, "system_user_77@centry.user"); got != "77" {
		t.Fatalf("personal_project_id = %q for system_user_77@centry.user, want \"77\"", got)
	}
}

/* ── fixture ───────────────────────────────────────────────────────────── */

// readPersonalProjectID drives the endpoint rather than the unexported
// resolver, because the field the SPA routes on is what this is about.
func readPersonalProjectID(t *testing.T, pool *pgxpool.Pool, userID int64, email string) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/author/", nil)
	request = request.WithContext(auth.ContextWithUser(request.Context(),
		auth.User{ID: strconv.FormatInt(userID, 10), Email: email}))
	recorder := httptest.NewRecorder()
	handler.NewHandler(pool).Routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /author/ status = %d (body %s)", recorder.Code, recorder.Body.String())
	}
	var decoded struct {
		PersonalProjectID string `json:"personal_project_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode author response %s: %v", recorder.Body.String(), err)
	}
	return decoded.PersonalProjectID
}

// seedProject inserts one project row owned by the named account.
func seedProject(t *testing.T, pool *pgxpool.Pool, name string, ownerID int64) int64 {
	t.Helper()
	var projectID int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO centry.project (name, owner_id, keycloak_groups, create_success, suspended)
		VALUES ($1, $2, '{}', true, false)
		RETURNING id`, name, ownerID).Scan(&projectID); err != nil {
		t.Fatalf("seed project %s: %v", name, err)
	}
	return projectID
}

// seedProjectMembership gives the user a role in the project. Both halves are
// needed: the resolver reads the assignment, not the project row.
func seedProjectMembership(t *testing.T, pool *pgxpool.Pool, projectID, userID int64) {
	t.Helper()
	ctx := context.Background()
	var roleID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO auth_core__project_role (project_id, name) VALUES ($1, 'editor')
		ON CONFLICT (project_id, name) DO UPDATE SET name = EXCLUDED.name
		RETURNING id`, projectID).Scan(&roleID); err != nil {
		t.Fatalf("seed project role for %d: %v", projectID, err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO auth_core__project_user_role (project_id, user_id, role_id)
		VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, projectID, userID, roleID); err != nil {
		t.Fatalf("seed membership of %d in %d: %v", userID, projectID, err)
	}
}
