package repos

// Pin writes decide project membership inside their own statement, with the
// predicate the HTTP gate uses, and a new pin must name an entity that exists
// in the project's tenant schema.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// seedPinAccessTables creates the pylon-owned auth tables the membership
// predicate reads. The migrated template does not carry them (pylon owns them
// in a deployment), so a pin test that needs a member creates the minimum here.
// They are created once per database; a second call is a no-op.
func seedPinAccessTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
CREATE TABLE IF NOT EXISTS public.auth_core__role (
    id integer PRIMARY KEY,
    name varchar(64) NOT NULL,
    mode varchar(64) NOT NULL,
    UNIQUE (name, mode)
);
CREATE TABLE IF NOT EXISTS public.auth_core__user_role (
    id serial PRIMARY KEY,
    user_id integer NOT NULL,
    role_id integer NOT NULL,
    UNIQUE (user_id, role_id)
);
CREATE TABLE IF NOT EXISTS public.auth_core__project_role (
    id serial PRIMARY KEY,
    project_id integer NOT NULL,
    name varchar(64) NOT NULL,
    UNIQUE (project_id, name)
);
CREATE TABLE IF NOT EXISTS public.auth_core__project_user_role (
    id serial PRIMARY KEY,
    project_id integer NOT NULL,
    user_id integer NOT NULL,
    role_id integer NOT NULL,
    UNIQUE (project_id, user_id, role_id)
);
INSERT INTO public.auth_core__role (id, name, mode) VALUES
    (1, 'super_admin', 'administration'),
    (2, 'super_admin', 'default'),
    (3, 'super_admin', 'developer')
ON CONFLICT DO NOTHING;`); err != nil {
		t.Fatalf("create the auth tables the pin membership predicate reads: %v", err)
	}
}

// grantPinMembership makes each user a member of the project.
func grantPinMembership(t *testing.T, pool *pgxpool.Pool, projectID int, users ...int) {
	t.Helper()
	seedPinAccessTables(t, pool)
	var role int
	if err := pool.QueryRow(context.Background(), `
INSERT INTO public.auth_core__project_role (project_id, name) VALUES ($1, 'member')
ON CONFLICT (project_id, name) DO UPDATE SET name = EXCLUDED.name RETURNING id`, projectID).Scan(&role); err != nil {
		t.Fatalf("seed project %d role: %v", projectID, err)
	}
	for _, user := range users {
		if _, err := pool.Exec(context.Background(), `
INSERT INTO public.auth_core__project_user_role (project_id, user_id, role_id)
VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, projectID, user, role); err != nil {
			t.Fatalf("grant user %d membership of project %d: %v", user, projectID, err)
		}
	}
}

// seedPinFixtures creates the second project row and the tenant tables the
// migrated template lacks (pylon owns `applications` and `elitea_tools`), then
// one row per backed entity type. It returns the id of the row each type pins.
func seedPinFixtures(t *testing.T, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`INSERT INTO centry.project (id, create_success, suspended) VALUES (2, TRUE, FALSE) ON CONFLICT DO NOTHING`,
		`CREATE TABLE IF NOT EXISTS p_1.applications (id serial PRIMARY KEY, name varchar NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS p_1.elitea_tools (id serial PRIMARY KEY, name varchar NOT NULL)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	ids := map[string]int{}
	for entity, statement := range map[string]string{
		"application": `INSERT INTO p_1.applications (name) VALUES ('pin-app') RETURNING id`,
		"toolkit":     `INSERT INTO p_1.elitea_tools (name) VALUES ('pin-toolkit') RETURNING id`,
		"skill":       `INSERT INTO p_1.skills (name, description) VALUES ('pin-skill', 'd') RETURNING id`,
		"configuration": `INSERT INTO p_1.configuration (uuid, project_id, elitea_title, type, section, data, meta, shared, status_ok, source)
			VALUES (gen_random_uuid(), 1, 'pin-config', 'openapi', 'credentials', '{}', '{}', false, false, 'user') RETURNING id`,
	} {
		var id int
		if err := pool.QueryRow(ctx, statement).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", entity, err)
		}
		ids[entity] = id
	}
	// Agents and pipelines are rows of `applications`; MCP servers of `elitea_tools`.
	ids["agent"], ids["pipeline"], ids["mcp"] = ids["application"], ids["application"], ids["toolkit"]
	return ids
}

func pinCount(t *testing.T, pool *pgxpool.Pool, project int) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM centry.social_pins WHERE project_id = $1`, project).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func requirePinStatus(t *testing.T, err error, want int, what string) {
	t.Helper()
	var apiErr *apierr.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != want {
		t.Fatalf("%s = %v, want HTTP %d", what, err, want)
	}
}

var pinBackedEntities = []string{"application", "agent", "pipeline", "toolkit", "mcp", "skill", "configuration"}

func TestSocialPinMemberPinsAndUnpinsEveryBackedEntity(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ids := seedPinFixtures(t, pool)
	grantPinMembership(t, pool, 1, 7)
	pins := NewCurrentSocialPinsRepository(pool)

	for _, entity := range pinBackedEntities {
		t.Run(entity, func(t *testing.T) {
			id := fmt.Sprint(ids[entity])
			before := pinCount(t, pool, 1)
			if err := pins.Pin(asUser("7"), "1", entity, id); err != nil {
				t.Fatalf("member Pin: %v", err)
			}
			if got := pinCount(t, pool, 1); got != before+1 {
				t.Fatalf("pins = %d after a member pinned %s, want %d", got, entity, before+1)
			}
			// Repeating a pin rewrites the last pinner and adds no row.
			if err := pins.Pin(asUser("7"), "1", entity, id); err != nil {
				t.Fatalf("repeated Pin: %v", err)
			}
			if got := pinCount(t, pool, 1); got != before+1 {
				t.Fatalf("a repeated pin added a row: %d", got)
			}
			if err := pins.Unpin(asUser("7"), "1", entity, id); err != nil {
				t.Fatalf("member Unpin: %v", err)
			}
			if got := pinCount(t, pool, 1); got != before {
				t.Fatalf("pins = %d after Unpin, want %d", got, before)
			}
			// Removing an absent pin is a success.
			if err := pins.Unpin(asUser("7"), "1", entity, id); err != nil {
				t.Fatalf("Unpin of an absent pin by a member: %v", err)
			}
		})
	}
}

func TestSocialPinRefusesActorsWhoAreNotMembers(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ids := seedPinFixtures(t, pool)
	// 7 is the member; 5 belongs to project 2 only; 9 belongs to nothing.
	grantPinMembership(t, pool, 1, 7)
	grantPinMembership(t, pool, 2, 5)
	pins := NewCurrentSocialPinsRepository(pool)
	appID := fmt.Sprint(ids["application"])

	// A pin a member made earlier, which the refused actors must not remove.
	if err := pins.Pin(asUser("7"), "1", "application", appID); err != nil {
		t.Fatal(err)
	}
	baseline := pinCount(t, pool, 1)

	for _, actor := range []string{"5", "9"} {
		t.Run("actor "+actor, func(t *testing.T) {
			for _, entity := range pinBackedEntities {
				id := fmt.Sprint(ids[entity])
				requirePinStatus(t, pins.Pin(asUser(actor), "1", entity, id), http.StatusForbidden, "Pin "+entity)
			}
			requirePinStatus(t, pins.Unpin(asUser(actor), "1", "application", appID), http.StatusForbidden, "Unpin")
			if got := pinCount(t, pool, 1); got != baseline {
				t.Fatalf("pins = %d after refused writes, want the unchanged %d", got, baseline)
			}
		})
	}
	// The refusal precedes the existence check, so a non-member learns nothing
	// about which entity ids exist.
	requirePinStatus(t, pins.Pin(asUser("9"), "1", "application", "2147483647"), http.StatusForbidden, "Pin of an absent id by a non-member")
}

func TestSocialPinRefusesAProjectWithNoTenantSchemaWithoutNamingIt(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedPinFixtures(t, pool)
	grantPinMembership(t, pool, 1, 7)
	pins := NewCurrentSocialPinsRepository(pool)

	// Project 2 has a row but no p_2 schema; project 3 has neither. A
	// non-member must get the same 403 for both, not an undefined-schema 500.
	for _, project := range []string{"2", "3"} {
		requirePinStatus(t, pins.Pin(asUser("7"), project, "application", "1"), http.StatusForbidden, "Pin in project "+project)
		requirePinStatus(t, pins.Unpin(asUser("7"), project, "application", "1"), http.StatusForbidden, "Unpin in project "+project)
	}
}

func TestSocialPinNewPinMustNameAnExistingEntity(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedPinFixtures(t, pool)
	grantPinMembership(t, pool, 1, 7)
	pins := NewCurrentSocialPinsRepository(pool)

	for _, entity := range pinBackedEntities {
		requirePinStatus(t, pins.Pin(asUser("7"), "1", entity, "2147483647"), http.StatusNotFound, "Pin of an absent "+entity)
	}
	if got := pinCount(t, pool, 1); got != 0 {
		t.Fatalf("%d pin rows exist for entities that do not", got)
	}
}

func TestSocialPinTypesWithNoTenantTableCannotBePinnedButCanBeCleared(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedPinFixtures(t, pool)
	grantPinMembership(t, pool, 1, 7)
	pins := NewCurrentSocialPinsRepository(pool)

	for _, entity := range []string{"prompt", "collection", "datasource"} {
		requirePinStatus(t, pins.Pin(asUser("7"), "1", entity, "1"), http.StatusBadRequest, "Pin "+entity)
		// A row left by an earlier release is removable by a member.
		if _, err := pool.Exec(context.Background(), `INSERT INTO centry.social_pins (entity, project_id, entity_id, user_id)
			VALUES ($1, 1, 1, 7)`, entity); err != nil {
			t.Fatal(err)
		}
		if err := pins.Unpin(asUser("7"), "1", entity, "1"); err != nil {
			t.Fatalf("Unpin of a stale %s pin: %v", entity, err)
		}
	}
	if got := pinCount(t, pool, 1); got != 0 {
		t.Fatalf("%d stale pins left", got)
	}
}

// A central administrator holds no role in the project and is admitted, exactly
// as the HTTP gate admits them; the same role in another mode is not.
func TestSocialPinAdmitsOnlyTheAdministrationModeSuperAdmin(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ids := seedPinFixtures(t, pool)
	seedPinAccessTables(t, pool)
	pins := NewCurrentSocialPinsRepository(pool)
	appID := fmt.Sprint(ids["application"])
	// users 11 (administration), 12 (default), 13 (developer) hold super_admin.
	for user, role := range map[int]int{11: 1, 12: 2, 13: 3} {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO public.auth_core__user_role (user_id, role_id) VALUES ($1, $2)`, user, role); err != nil {
			t.Fatal(err)
		}
	}
	if err := pins.Pin(asUser("11"), "1", "application", appID); err != nil {
		t.Fatalf("administration super_admin Pin: %v", err)
	}
	requirePinStatus(t, pins.Pin(asUser("12"), "1", "application", appID), http.StatusForbidden, "default-mode super_admin Pin")
	requirePinStatus(t, pins.Pin(asUser("13"), "1", "application", appID), http.StatusForbidden, "developer-mode super_admin Pin")
	requirePinStatus(t, pins.Unpin(asUser("12"), "1", "application", appID), http.StatusForbidden, "default-mode super_admin Unpin")
	if got := pinCount(t, pool, 1); got != 1 {
		t.Fatalf("pins = %d, want the one the administration super_admin wrote", got)
	}
	// An administrator still cannot pin into a project that does not exist.
	requirePinStatus(t, pins.Pin(asUser("11"), "3", "application", "1"), http.StatusNotFound, "Pin in an unknown project")
}

// A membership revoked after the actor last pinned ends their write access.
func TestSocialPinRefusesAfterMembershipIsRevoked(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ids := seedPinFixtures(t, pool)
	grantPinMembership(t, pool, 1, 7)
	pins := NewCurrentSocialPinsRepository(pool)
	appID := fmt.Sprint(ids["application"])
	toolkitID := fmt.Sprint(ids["toolkit"])

	if err := pins.Pin(asUser("7"), "1", "application", appID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM public.auth_core__project_user_role WHERE project_id = 1 AND user_id = 7`); err != nil {
		t.Fatal(err)
	}
	requirePinStatus(t, pins.Pin(asUser("7"), "1", "toolkit", toolkitID), http.StatusForbidden, "Pin after revocation")
	requirePinStatus(t, pins.Unpin(asUser("7"), "1", "application", appID), http.StatusForbidden, "Unpin after revocation")
	if got := pinCount(t, pool, 1); got != 1 {
		t.Fatalf("pins = %d, want the one written before the revocation", got)
	}
}

// The write STATEMENT carries the membership predicate on its own. The gate
// statement in front of it can pass and the membership still be gone by the
// time the write runs, so the write is exercised here without the gate: a
// non-member's write inside an open transaction must refuse and write nothing.
func TestSocialPinWriteStatementEnforcesMembershipOnItsOwn(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ids := seedPinFixtures(t, pool)
	grantPinMembership(t, pool, 1, 7)
	ctx := context.Background()
	appID := int32(ids["application"])

	for _, tc := range []struct {
		name  string
		actor int32
		want  int
	}{
		{"revoked or foreign actor", 9, http.StatusForbidden},
		{"member", 7, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				if err := pinEntity(ctx, tx, "p_1", "applications", "application", 1, appID, tc.actor); err != nil {
					return err
				}
				return unpinEntity(ctx, tx, "application", 1, appID, tc.actor)
			})
			if tc.want == 0 {
				if err != nil {
					t.Fatalf("member write: %v", err)
				}
				return
			}
			requirePinStatus(t, err, tc.want, "write by "+tc.name)
			if got := pinCount(t, pool, 1); got != 0 {
				t.Fatalf("a refused write left %d pins", got)
			}
		})
	}

	// And a pin written by a member survives a non-member's unpin statement.
	if err := NewCurrentSocialPinsRepository(pool).Pin(asUser("7"), "1", "application", fmt.Sprint(appID)); err != nil {
		t.Fatal(err)
	}
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return unpinEntity(ctx, tx, "application", 1, appID, 9)
	})
	requirePinStatus(t, err, http.StatusForbidden, "unpin statement by a non-member")
	if got := pinCount(t, pool, 1); got != 1 {
		t.Fatalf("a non-member's unpin statement removed the pin: %d left", got)
	}
}
