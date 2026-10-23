package eliteacore_test

// DEFECT: the project member listing UNIONs in every holder of a role NAMED
// `super_admin`, with no predicate on the role MODE.
//
// `public.auth_core__role` is keyed UNIQUE (name, mode)
// (internal/infra/db/migrations/001_initial.sql:649-650), so one deployment can
// hold three different `super_admin` roles: `administration`, `default` and
// `developer`. Only the `administration` role grants central platform access.
// A database restored from a legacy dump carries all three
// (testdata/postgres/legacy-rbac-matrix.json).
//
// Result before the fix: GET /api/v2/elitea_core/users/default/{projectID}
// reported every default-mode and developer-mode `super_admin` holder as a
// member of EVERY project, with the role `super_admin`, and counted them in
// `total`. The rows have no auth_core__project_user_role entry, so UsersDelete
// cannot remove them, and their address reaches every project viewer —
// migration 0068 grants `configuration.users.users.view` to admin, editor AND
// viewer. migrations/shared/0060_admin_central_rbac.sql:108-112 names this
// behaviour as the reason it seeds `admin` instead of `super_admin`; the
// workaround was applied, the query was never fixed.
//
// The test seeds one holder in each of the three modes. Against the pre-fix
// query the listing carries five rows and the two phantom addresses.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
)

func TestUsersListsOnlyTheAdministrationModeSuperAdmin(t *testing.T) {
	pool := newUsersWritePostgresPool(t)
	prepareUsersWriteFixture(t, pool)
	seedSuperAdminsInEveryMode(t, pool)
	router := usersWriteRouter(eliteacore.NewHandler(pool))

	listing := readMembers(t, router)

	present := map[string]bool{}
	for _, row := range listing.Rows {
		present[row.Email] = true
	}

	for _, wanted := range []string{
		"e2e-admin@autotest.local",      // project member
		"e2e-member@autotest.local",     // project member
		"platform-admin@autotest.local", // administration-mode super_admin
	} {
		if !present[wanted] {
			t.Errorf("%s is absent from the member list %+v", wanted, listing.Rows)
		}
	}

	for _, phantom := range []string{
		"default-super-admin@autotest.local",
		"developer-super-admin@autotest.local",
	} {
		if present[phantom] {
			t.Errorf("%s holds a super_admin role in a mode that grants no central access, "+
				"yet the project member list reports it as a member: %+v", phantom, listing.Rows)
		}
	}

	const wantTotal = 3
	if listing.Total != wantTotal {
		t.Errorf("total = %d, want %d; the count query must use the same mode predicate as the page query",
			listing.Total, wantTotal)
	}
}

// seedSuperAdminsInEveryMode creates the three same-named roles a legacy dump
// carries, and gives each one a distinct holder.
func seedSuperAdminsInEveryMode(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	holders := map[string]string{
		"administration": "platform-admin@autotest.local",
		"default":        "default-super-admin@autotest.local",
		"developer":      "developer-super-admin@autotest.local",
	}
	for mode, email := range holders {
		if _, err := pool.Exec(ctx,
			`INSERT INTO public.auth_core__role (name, mode) VALUES ('super_admin', $1)`, mode); err != nil {
			t.Fatalf("seed super_admin role in mode %s: %v", mode, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO public.auth_core__user (email, name) VALUES ($1, $2)`,
			email, fmt.Sprintf("%s holder", mode)); err != nil {
			t.Fatalf("seed holder %s: %v", email, err)
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT u.id, r.id
FROM public.auth_core__user u
JOIN public.auth_core__role r ON r.name = 'super_admin' AND r.mode = $2
WHERE u.email = $1`, email, mode); err != nil {
			t.Fatalf("assign super_admin/%s to %s: %v", mode, email, err)
		}
	}
}
