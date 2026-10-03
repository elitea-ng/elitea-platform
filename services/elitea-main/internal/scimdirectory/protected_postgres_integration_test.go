package scimdirectory

// The rows a SCIM credential must never manage, and the name parts, against a
// real PostgreSQL. The pool is newDirectoryPool's: the account and role tables
// in 001_initial.sql's shape, and the SCIM side table from migrations 0096 and
// 0134's own files.

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func seedAccount(t *testing.T, pool *pgxpool.Pool, email, name string) int {
	t.Helper()
	var id int
	require.NoError(t, pool.QueryRow(context.Background(),
		`INSERT INTO auth_core__user (email, name) VALUES ($1, $2) RETURNING id`, email, name,
	).Scan(&id))
	return id
}

func grantRole(t *testing.T, pool *pgxpool.Pool, userID int, role, mode string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO auth_core__user_role (user_id, role_id)
		 SELECT $1, id FROM auth_core__role WHERE name = $2 AND mode = $3`, userID, role, mode)
	require.NoError(t, err)
}

func storedAccount(t *testing.T, pool *pgxpool.Pool, id int) (email, name string, suspended bool) {
	t.Helper()
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT email, COALESCE(name, ''), suspended FROM auth_core__user WHERE id = $1`, id,
	).Scan(&email, &name, &suspended))
	return email, name, suspended
}

// H1: the takeover. An administrator who has not yet signed in through single
// sign-on is an unlinked row; re-addressing it to an attacker's address and
// signing in through any provider asserting that address adopted the row with
// its administration role. Every verb a SCIM client has is refused on it.
func TestEveryWriteToAnAdministratorIsRefused(t *testing.T) {
	pool := newDirectoryPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	admin := seedAccount(t, pool, "root@corp.com", "Root")
	grantRole(t, pool, admin, "admin", "administration")
	viewer := seedAccount(t, pool, "audit@corp.com", "Audit")
	grantRole(t, pool, viewer, "viewer", "administration")

	for _, id := range []int{admin, viewer} {
		attacker, inactive := "mallory@evil.com", false
		_, err := store.ApplyUserChanges(ctx, id, UserChanges{UserName: &attacker})
		require.ErrorIs(t, err, ErrProtected)
		var protected *ProtectedError
		require.ErrorAs(t, err, &protected)
		require.Contains(t, protected.Reason, "administration role")

		_, err = store.ApplyUserChanges(ctx, id, UserChanges{Active: &inactive})
		require.ErrorIs(t, err, ErrProtected)
		_, err = store.Replace(ctx, id, User{UserName: attacker, Active: true})
		require.ErrorIs(t, err, ErrProtected)
		_, err = store.SetActive(ctx, id, false)
		require.ErrorIs(t, err, ErrProtected)
		require.ErrorIs(t, store.Deactivate(ctx, id), ErrProtected)
	}

	// Create's ADOPTION branch is a write to the existing row too.
	_, err := store.Create(ctx, User{UserName: "ROOT@corp.com", DisplayName: "Owned", Active: false, ActiveStated: true})
	require.ErrorIs(t, err, ErrProtected)

	email, name, suspended := storedAccount(t, pool, admin)
	require.Equal(t, "root@corp.com", email)
	require.Equal(t, "Root", name)
	require.False(t, suspended)

	// No SCIM record was written for a refused row, so nothing marks it as
	// directory-managed either.
	var records int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM elitea_auth.scim_users WHERE user_id IN ($1, $2)`, admin, viewer).Scan(&records))
	require.Zero(t, records)
}

// A PROJECT-mode role is not an administration role and does not protect.
func TestADefaultModeRoleDoesNotProtectTheAccount(t *testing.T) {
	pool := newDirectoryPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	member := seedAccount(t, pool, "bob@corp.com", "Bob")
	grantRole(t, pool, member, "member", "default")

	updated, err := store.SetActive(ctx, member, false)
	require.NoError(t, err)
	require.False(t, updated.Active)
}

// The platform's own principals are refused, and so is moving a row INTO their
// reserved shapes: the project resolver maps system_user_<n>@centry.user to
// project <n> with no membership check, and the admin Users list hides the
// domain.
func TestPlatformPrincipalsAndReservedIdentitiesAreRefused(t *testing.T) {
	pool := newDirectoryPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	for _, row := range []struct{ email, name string }{
		{"system@centry.user", "system"},
		{"system_user_5@centry.user", "Project 5"},
		{"svc@corp.com", ":system:project:5:"},
	} {
		id := seedAccount(t, pool, row.email, row.name)
		_, err := store.SetActive(ctx, id, false)
		require.ErrorIs(t, err, ErrProtected, row.email)
		renamed := "x@corp.com"
		_, err = store.ApplyUserChanges(ctx, id, UserChanges{UserName: &renamed})
		require.ErrorIs(t, err, ErrProtected, row.email)
	}

	carol := seedAccount(t, pool, "carol@corp.com", "Carol")
	for _, address := range []string{"system_user_7@centry.user", "Carol@CENTRY.USER"} {
		_, err := store.ApplyUserChanges(ctx, carol, UserChanges{UserName: &address})
		require.ErrorIs(t, err, ErrProtected, address)
		_, err = store.Replace(ctx, carol, User{UserName: address, Active: true})
		require.ErrorIs(t, err, ErrProtected, address)
		_, err = store.Create(ctx, User{UserName: address, Active: true})
		require.ErrorIs(t, err, ErrProtected, address)
	}
	systemName := ":system:project:7:"
	_, err := store.ApplyUserChanges(ctx, carol, UserChanges{DisplayName: &systemName})
	require.ErrorIs(t, err, ErrProtected)

	email, name, _ := storedAccount(t, pool, carol)
	require.Equal(t, "carol@corp.com", email)
	require.Equal(t, "Carol", name)
}

// M4: PUT made the same case-insensitive uniqueness check PATCH makes.
func TestAReplaceOntoAnAddressTakenInAnotherCaseIsAConflict(t *testing.T) {
	pool := newDirectoryPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	alice := seedAccount(t, pool, "alice@corp.com", "Alice")
	seedAccount(t, pool, "Bob@Corp.com", "Bob")

	_, err := store.Replace(ctx, alice, User{UserName: "bob@corp.com", Active: true})
	require.ErrorIs(t, err, ErrConflict)

	var count int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM auth_core__user WHERE lower(email) = 'bob@corp.com'`).Scan(&count))
	require.Equal(t, 1, count)
}

// CR2: the parts are stored and read back as sent, a single part merges with
// the stored counterpart, and a name composed from parts fills an EMPTY display
// name only.
func TestNamePartsAreStoredAndNeverOverwriteADisplayName(t *testing.T) {
	pool := newDirectoryPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	created, err := store.Create(ctx, User{
		UserName: "john@corp.com", DisplayName: "Smith, John (Contractor)", Active: true,
		NameStated: true, GivenName: "John", FamilyName: "Smith", FormattedName: "John Smith",
	})
	require.NoError(t, err)
	require.Equal(t, "John", created.GivenName)
	require.Equal(t, "Smith", created.FamilyName)
	require.Equal(t, "John Smith", created.FormattedName)

	family := "Jones"
	updated, err := store.ApplyUserChanges(ctx, created.ID, UserChanges{FamilyName: &family})
	require.NoError(t, err)
	require.Equal(t, "John", updated.GivenName)
	require.Equal(t, "Jones", updated.FamilyName)
	require.Equal(t, "Smith, John (Contractor)", updated.DisplayName)

	// A re-sync create with only name parts (derived display name) leaves the
	// stored display name alone; an explicit displayName still replaces it.
	resynced, err := store.Create(ctx, User{
		UserName: "john@corp.com", DisplayName: "John Jones", DisplayNameDerived: true, Active: true,
	})
	require.NoError(t, err)
	require.Equal(t, "Smith, John (Contractor)", resynced.DisplayName)
	require.Equal(t, "Jones", resynced.FamilyName, "a create with no name object keeps the stored parts")

	// A PUT is the whole resource: a derived name does not overwrite, and an
	// omitted `name` clears the parts.
	replaced, err := store.Replace(ctx, created.ID, User{
		UserName: "john@corp.com", DisplayName: "", DisplayNameDerived: true, Active: true,
	})
	require.NoError(t, err)
	require.Equal(t, "Smith, John (Contractor)", replaced.DisplayName)
	require.Empty(t, replaced.GivenName)
	require.Empty(t, replaced.FamilyName)

	// An EMPTY display name is filled by a derived one.
	blank := seedAccount(t, pool, "blank@corp.com", "")
	filled, err := store.Create(ctx, User{
		UserName: "blank@corp.com", DisplayName: "Bea Blank", DisplayNameDerived: true, Active: true,
		NameStated: true, GivenName: "Bea", FamilyName: "Blank",
	})
	require.NoError(t, err)
	require.Equal(t, blank, filled.ID)
	require.Equal(t, "Bea Blank", filled.DisplayName)
}

// 0134 is guarded on to_regclass, so it applies cleanly to a database that
// does not hold the SCIM table at all.
func TestMigration0134IsANoOpWithoutTheSCIMTable(t *testing.T) {
	pool := newDirectoryPool(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx, `DROP TABLE elitea_auth.scim_users`)
	require.NoError(t, err)
	migration, err := os.ReadFile("../../migrations/shared/0134_scim_user_name_parts.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(migration))
	require.NoError(t, err)
}
