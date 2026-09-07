package repos

// The per-bucket access list against a REAL migrated database.
//
// The handler tests beside this one run on an in-memory double, so they prove
// the DECISIONS. They cannot prove that the table stores what the decisions
// read: an empty PostgreSQL array arriving in Go as a nil slice, a replace that
// leaves the removed rows behind, or a CHECK constraint the handler's own
// validation does not agree with are all invisible there and all fatal here.
//
// The distinction this file guards hardest is EMPTY versus ABSENT. `[]` means
// "no access" and no row means "no exception, so allowed". They are opposite
// decisions, and every layer between the table and the JSON has a way to
// collapse them.

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func newBucketPermissionsRepo(t *testing.T) (*ArtifactBucketPermissionsRepository, *pgxpool.Pool) {
	t.Helper()
	pool := newMigratedPostgresIntegrationPool(t)
	repo, err := NewArtifactBucketPermissionsRepository(pool)
	if err != nil {
		t.Fatalf("build the artifact bucket permissions repository: %v", err)
	}
	return repo, pool
}

// seedAuthCoreTables creates the three bootstrap-owned auth tables the listing
// and the admin bypass read.
//
// They are absent from the ledgered corpus on purpose — 001_initial.sql owns
// them, and shared/0118's own grants are guarded with to_regclass for the same
// reason. Every fixture in this package that needs them creates them in their
// 001_initial shape rather than claiming them in a migration, which is what
// makes a second migration for the same table impossible.
func seedAuthCoreTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
CREATE TABLE IF NOT EXISTS public.auth_core__user (
    id SERIAL PRIMARY KEY,
    email TEXT UNIQUE,
    name TEXT,
    last_login TIMESTAMP,
    suspended BOOLEAN NOT NULL DEFAULT false
);
CREATE TABLE IF NOT EXISTS public.auth_core__project_role (
    id SERIAL PRIMARY KEY,
    project_id INTEGER NOT NULL,
    name TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS public.auth_core__project_user_role (
    project_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    role_id INTEGER NOT NULL
)`); err != nil {
		t.Fatalf("create the bootstrap auth tables: %v", err)
	}
}

// seedProjectAdmin makes userID an `admin` of projectID.
func seedProjectAdmin(t *testing.T, pool *pgxpool.Pool, projectID, userID int64) {
	t.Helper()
	var roleID int64
	if err := pool.QueryRow(t.Context(), `
INSERT INTO public.auth_core__project_role (project_id, name)
VALUES ($1, 'admin') RETURNING id`, projectID).Scan(&roleID); err != nil {
		t.Fatalf("seed the project role: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
VALUES ($1, $2, $3)`, projectID, userID, roleID); err != nil {
		t.Fatalf("seed the role assignment: %v", err)
	}
}

func TestArtifactBucketPermissions_AbsentRowIsNotAnException(t *testing.T) {
	repo, _ := newBucketPermissionsRepo(t)
	permissions, found, err := repo.GetBucketPermission(t.Context(), 1, 7, "reports")
	if err != nil {
		t.Fatalf("GetBucketPermission: %v", err)
	}
	if found {
		t.Fatalf("an unseeded member reported an exception: %v", permissions)
	}
}

func TestArtifactBucketPermissions_EmptyArrayIsStoredAndReadBackAsNoAccess(t *testing.T) {
	repo, _ := newBucketPermissionsRepo(t)
	if err := repo.ReplaceUserBucketPermissions(t.Context(), 1, 7, map[string][]string{
		"reports": {},
	}); err != nil {
		t.Fatalf("ReplaceUserBucketPermissions: %v", err)
	}

	permissions, found, err := repo.GetBucketPermission(t.Context(), 1, 7, "reports")
	if err != nil {
		t.Fatalf("GetBucketPermission: %v", err)
	}
	if !found {
		t.Fatal("the no-access exception was not found; it reads as no exception at all")
	}
	if permissions == nil {
		t.Fatal("the no-access exception read back as nil, which marshals to null")
	}
	if len(permissions) != 0 {
		t.Fatalf("the no-access exception read back as %v", permissions)
	}
}

func TestArtifactBucketPermissions_ReplaceDeletesTheEntriesItOmits(t *testing.T) {
	repo, _ := newBucketPermissionsRepo(t)
	if err := repo.ReplaceUserBucketPermissions(t.Context(), 1, 7, map[string][]string{
		"reports":  {"read"},
		"datasets": {"read", "write"},
	}); err != nil {
		t.Fatalf("seed ReplaceUserBucketPermissions: %v", err)
	}
	if err := repo.ReplaceUserBucketPermissions(t.Context(), 1, 7, map[string][]string{
		"datasets": {"read"},
	}); err != nil {
		t.Fatalf("ReplaceUserBucketPermissions: %v", err)
	}

	if _, found, err := repo.GetBucketPermission(t.Context(), 1, 7, "reports"); err != nil {
		t.Fatalf("GetBucketPermission: %v", err)
	} else if found {
		t.Error("the omitted exception survived the replace")
	}
	permissions, found, err := repo.GetBucketPermission(t.Context(), 1, 7, "datasets")
	if err != nil {
		t.Fatalf("GetBucketPermission: %v", err)
	}
	if !found || len(permissions) != 1 || permissions[0] != "read" {
		t.Errorf("the surviving exception = %v (found=%v), want [read]", permissions, found)
	}
}

// The replace must not reach another member or another project. A DELETE that
// forgot either predicate would clear exceptions nobody asked to change.
func TestArtifactBucketPermissions_ReplaceIsScopedToOneMemberInOneProject(t *testing.T) {
	repo, _ := newBucketPermissionsRepo(t)
	seed := map[string][]string{"reports": {"read"}}
	for _, member := range []struct{ projectID, userID int64 }{{1, 7}, {1, 8}, {2, 7}} {
		if err := repo.ReplaceUserBucketPermissions(t.Context(), member.projectID, member.userID, seed); err != nil {
			t.Fatalf("seed ReplaceUserBucketPermissions: %v", err)
		}
	}
	if err := repo.ReplaceUserBucketPermissions(t.Context(), 1, 7, map[string][]string{}); err != nil {
		t.Fatalf("ReplaceUserBucketPermissions: %v", err)
	}

	for _, member := range []struct{ projectID, userID int64 }{{1, 8}, {2, 7}} {
		_, found, err := repo.GetBucketPermission(t.Context(), member.projectID, member.userID, "reports")
		if err != nil {
			t.Fatalf("GetBucketPermission: %v", err)
		}
		if !found {
			t.Errorf("project %d user %d lost an exception it did not own",
				member.projectID, member.userID)
		}
	}
}

func TestArtifactBucketPermissions_DeleteReportsWhetherARowWasThere(t *testing.T) {
	repo, _ := newBucketPermissionsRepo(t)
	if err := repo.ReplaceUserBucketPermissions(t.Context(), 1, 7, map[string][]string{
		"reports": {"read"},
	}); err != nil {
		t.Fatalf("seed ReplaceUserBucketPermissions: %v", err)
	}

	removed, err := repo.DeleteUserBucketPermission(t.Context(), 1, 7, "reports")
	if err != nil {
		t.Fatalf("DeleteUserBucketPermission: %v", err)
	}
	if !removed {
		t.Error("the first delete reported no row")
	}
	removed, err = repo.DeleteUserBucketPermission(t.Context(), 1, 7, "reports")
	if err != nil {
		t.Fatalf("DeleteUserBucketPermission: %v", err)
	}
	if removed {
		t.Error("the second delete reported a row that was already gone")
	}
}

func TestArtifactBucketPermissions_ListGroupsByMember(t *testing.T) {
	repo, pool := newBucketPermissionsRepo(t)
	seedAuthCoreTables(t, pool)
	if err := repo.ReplaceUserBucketPermissions(t.Context(), 1, 7, map[string][]string{
		"reports":  {"read"},
		"datasets": {},
	}); err != nil {
		t.Fatalf("seed ReplaceUserBucketPermissions: %v", err)
	}
	if err := repo.ReplaceUserBucketPermissions(t.Context(), 2, 7, map[string][]string{
		"other": {"read"},
	}); err != nil {
		t.Fatalf("seed ReplaceUserBucketPermissions: %v", err)
	}

	rows, err := repo.ListBucketPermissions(t.Context(), 1)
	if err != nil {
		t.Fatalf("ListBucketPermissions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("listed %d rows, want 1", len(rows))
	}
	if rows[0].UserID != 7 {
		t.Fatalf("listed user %d, want 7", rows[0].UserID)
	}
	if len(rows[0].BucketPermissions) != 2 {
		t.Fatalf("listed %v, want both buckets", rows[0].BucketPermissions)
	}
	if got := rows[0].BucketPermissions["datasets"]; got == nil || len(got) != 0 {
		t.Errorf("the no-access entry listed as %v, want an empty array", got)
	}
}

// The table refuses what the route refuses. A CHECK the handler does not agree
// with is a 500 in production; a handler rule the table does not carry is a
// rule with a hole in it for every other writer.
func TestArtifactBucketPermissions_TableRefusesAnUnknownVerb(t *testing.T) {
	repo, _ := newBucketPermissionsRepo(t)
	err := repo.ReplaceUserBucketPermissions(t.Context(), 1, 7, map[string][]string{
		"reports": {"admin"},
	})
	if err == nil {
		t.Fatal("the table accepted a permission the route rejects")
	}
}

// IsProjectAdmin answers false rather than erroring on a database with no
// membership rows at all — the bypass must not turn an ordinary refusal into a
// 500.
func TestArtifactBucketPermissions_IsProjectAdminReadsTheProjectRole(t *testing.T) {
	repo, pool := newBucketPermissionsRepo(t)
	seedAuthCoreTables(t, pool)

	isAdmin, err := repo.IsProjectAdmin(t.Context(), 1, 7)
	if err != nil {
		t.Fatalf("IsProjectAdmin: %v", err)
	}
	if isAdmin {
		t.Fatal("a member with no role reported as a project admin")
	}

	seedProjectAdmin(t, pool, 1, 7)
	if isAdmin, err = repo.IsProjectAdmin(t.Context(), 1, 7); err != nil {
		t.Fatalf("IsProjectAdmin: %v", err)
	} else if !isAdmin {
		t.Error("an assigned project admin did not read back as one")
	}

	// The role is per project. The same member in another project is not an
	// admin there.
	if isAdmin, err = repo.IsProjectAdmin(t.Context(), 2, 7); err != nil {
		t.Fatalf("IsProjectAdmin: %v", err)
	} else if isAdmin {
		t.Error("a project admin in project 1 read as an admin of project 2")
	}
}
