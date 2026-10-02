package scimdirectory

// Group provisioning against a real PostgreSQL.
//
// Every table here is created from the statement the deployed migration
// carries: `auth_core__*` and `centry.project` in the shape
// internal/infra/db/migrations/001_initial.sql creates them, and the two SCIM
// tables from shared migration 0098's own file. A test that built its own
// convenient schema would prove a copy nobody deploys.
//
// What is asserted is the ROW STATE after each call — who holds which role on
// which project — and not the SQL that produced it. The takeover defect this
// repository already carries a note about survived every unit test because the
// test asserted a guard's SQL string rather than what the guard matched.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

/* ── the grant ─────────────────────────────────────────────────────────── */

// A push gives the members the AUTHORED role, and records that it was the push
// that gave it.
func TestAPushGrantsTheAuthoredRoleAndRecordsThatItGaveIt(t *testing.T) {
	pool := newGroupPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	project := seedProject(t, pool, "Platform", 0)
	alice := seedUser(t, pool, "alice@corp.com")
	binding, err := store.CreateBinding(ctx, "Platform Team", project, "editor")
	require.NoError(t, err)

	group, err := store.ReplaceGroupMembers(ctx, binding.ID, []int{alice})
	require.NoError(t, err)
	require.True(t, holdsRole(t, pool, project, alice, "editor"))
	require.Len(t, group.Members, 1)
	require.True(t, group.Members[0].Granted)
	require.Equal(t, "alice@corp.com", group.Members[0].UserName)
}

// A group with no binding is refused. This is the assertion behind `POST
// /Groups` answering 400 rather than provisioning a project: a push chooses the
// members and never the project.
func TestAGroupWithNoBindingIsRefused(t *testing.T) {
	pool := newGroupPool(t)
	store := NewStore(pool)

	_, err := store.LookupGroup(context.Background(), "grp-1", "Finance")
	require.ErrorIs(t, err, ErrNoBinding)
}

// The first push matches on the NAME, and remembers the external id; the second
// matches on the external id, so a rename at the identity provider lands on the
// binding it belongs to.
func TestABindingIsFoundByNameFirstAndByExternalIdAfterwards(t *testing.T) {
	pool := newGroupPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	project := seedProject(t, pool, "Platform", 0)
	binding, err := store.CreateBinding(ctx, "Platform Team", project, "viewer")
	require.NoError(t, err)

	found, err := store.LookupGroup(ctx, "grp-1", "Platform Team")
	require.NoError(t, err)
	require.Equal(t, binding.ID, found.ID)
	require.NoError(t, store.AdoptGroup(ctx, found.ID, "grp-1", "Platform Team"))

	renamed, err := store.LookupGroup(ctx, "grp-1", "Core Platform")
	require.NoError(t, err)
	require.Equal(t, binding.ID, renamed.ID)
}

/* ── the revoke, and what it must not touch ────────────────────────────── */

// The member the group gave the role to loses it.
func TestAMemberDroppedFromTheGroupLosesTheRoleTheGroupGaveThem(t *testing.T) {
	pool := newGroupPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	project := seedProject(t, pool, "Platform", 0)
	alice := seedUser(t, pool, "alice@corp.com")
	binding, err := store.CreateBinding(ctx, "Platform Team", project, "editor")
	require.NoError(t, err)
	_, err = store.ReplaceGroupMembers(ctx, binding.ID, []int{alice})
	require.NoError(t, err)

	_, err = store.ReplaceGroupMembers(ctx, binding.ID, nil)
	require.NoError(t, err)
	require.False(t, holdsRole(t, pool, project, alice, "editor"))
}

// THE ONE THAT MATTERS. A person an administrator added to the project keeps
// their access when the identity provider stops naming them — including when
// the group's push had listed them, which is how a real deployment looks after
// the first sync.
func TestAMemberAddedByHandSurvivesEveryGroupSync(t *testing.T) {
	pool := newGroupPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	project := seedProject(t, pool, "Platform", 0)
	alice := seedUser(t, pool, "alice@corp.com")
	bob := seedUser(t, pool, "bob@corp.com")
	grantRoleByHand(t, pool, project, alice, "editor")

	binding, err := store.CreateBinding(ctx, "Platform Team", project, "editor")
	require.NoError(t, err)
	// The first push names both of them, so the group CLAIMS Alice without
	// having granted her.
	group, err := store.ReplaceGroupMembers(ctx, binding.ID, []int{alice, bob})
	require.NoError(t, err)
	require.False(t, memberOf(group, alice).Granted)
	require.True(t, memberOf(group, bob).Granted)

	// Both leave the group.
	_, err = store.ReplaceGroupMembers(ctx, binding.ID, nil)
	require.NoError(t, err)
	require.True(t, holdsRole(t, pool, project, alice, "editor"),
		"a membership the group did not create must survive the sync")
	require.False(t, holdsRole(t, pool, project, bob, "editor"))
}

// A re-push of a member the group already granted must not downgrade the ledger
// to "was already a member". If it did, the membership would become permanent:
// the revoke path leaves such rows alone.
func TestARepushDoesNotTurnAGrantIntoAMembershipTheGroupMerelyFound(t *testing.T) {
	pool := newGroupPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	project := seedProject(t, pool, "Platform", 0)
	alice := seedUser(t, pool, "alice@corp.com")
	binding, err := store.CreateBinding(ctx, "Platform Team", project, "editor")
	require.NoError(t, err)

	for range 3 {
		_, err = store.ReplaceGroupMembers(ctx, binding.ID, []int{alice})
		require.NoError(t, err)
	}
	_, err = store.RemoveGroupMembers(ctx, binding.ID, []int{alice})
	require.NoError(t, err)
	require.False(t, holdsRole(t, pool, project, alice, "editor"))
}

// The project owner keeps their role. A project whose owner holds no role on it
// is one nobody can administer, and no identity provider push should be able to
// produce that.
func TestTheProjectOwnerKeepsTheirRoleWhenTheGroupDropsThem(t *testing.T) {
	pool := newGroupPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	owner := seedUser(t, pool, "owner@corp.com")
	project := seedProject(t, pool, "Platform", owner)
	binding, err := store.CreateBinding(ctx, "Platform Team", project, "admin")
	require.NoError(t, err)
	_, err = store.ReplaceGroupMembers(ctx, binding.ID, []int{owner})
	require.NoError(t, err)
	require.True(t, holdsRole(t, pool, project, owner, "admin"))

	group, err := store.ReplaceGroupMembers(ctx, binding.ID, nil)
	require.NoError(t, err)
	// The group no longer claims them…
	require.Empty(t, group.Members)
	// …and they still hold the role.
	require.True(t, holdsRole(t, pool, project, owner, "admin"))
}

// Two groups bound to the same project role hold their claims independently.
// One of them letting go must not take the other's member away.
func TestASecondGroupsClaimKeepsAMembershipAlive(t *testing.T) {
	pool := newGroupPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	project := seedProject(t, pool, "Platform", 0)
	alice := seedUser(t, pool, "alice@corp.com")
	first, err := store.CreateBinding(ctx, "Platform Team", project, "editor")
	require.NoError(t, err)
	second, err := store.CreateBinding(ctx, "Release Team", project, "editor")
	require.NoError(t, err)

	_, err = store.ReplaceGroupMembers(ctx, first.ID, []int{alice})
	require.NoError(t, err)
	_, err = store.ReplaceGroupMembers(ctx, second.ID, []int{alice})
	require.NoError(t, err)

	_, err = store.ReplaceGroupMembers(ctx, first.ID, nil)
	require.NoError(t, err)
	require.True(t, holdsRole(t, pool, project, alice, "editor"),
		"the other binding still claims this membership")

	_, err = store.ReplaceGroupMembers(ctx, second.ID, nil)
	require.NoError(t, err)
	require.False(t, holdsRole(t, pool, project, alice, "editor"))
}

// A ledger row that only CLAIMS a membership must not keep it alive.
//
// The revoke asks whether another binding still GRANTS the membership, which is
// the same question the grant side asks. Asking merely whether another row
// mentions it strands the membership: nothing grants it, and no push can take
// it away.
func TestAClaimThatGrantedNothingDoesNotStrandAMembership(t *testing.T) {
	pool := newGroupPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	project := seedProject(t, pool, "Platform", 0)
	alice := seedUser(t, pool, "alice@corp.com")
	grantRoleByHand(t, pool, project, alice, "editor")

	first, err := store.CreateBinding(ctx, "Platform Team", project, "editor")
	require.NoError(t, err)
	// The first group finds the membership rather than granting it.
	group, err := store.ReplaceGroupMembers(ctx, first.ID, []int{alice})
	require.NoError(t, err)
	require.False(t, memberOf(group, alice).Granted)

	// An administrator then removes the membership by hand.
	_, err = pool.Exec(ctx,
		`DELETE FROM auth_core__project_user_role WHERE project_id = $1 AND user_id = $2`,
		project, alice)
	require.NoError(t, err)

	// A second group pushes her, and this time the push CREATES it.
	second, err := store.CreateBinding(ctx, "Release Team", project, "editor")
	require.NoError(t, err)
	group, err = store.ReplaceGroupMembers(ctx, second.ID, []int{alice})
	require.NoError(t, err)
	require.True(t, memberOf(group, alice).Granted)

	// The group that granted it lets go. The first group's row claims her but
	// granted nothing, so it must not hold the membership open.
	_, err = store.ReplaceGroupMembers(ctx, second.ID, nil)
	require.NoError(t, err)
	require.False(t, holdsRole(t, pool, project, alice, "editor"),
		"a claim that granted nothing must not keep the membership alive")
}

/* ── one PATCH, one transaction ────────────────────────────────────────── */

// A PATCH is sent once. An operation that lands while a later one is refused is
// a change the client believes it did not make, and nothing will send it again.
func TestAPatchWhoseLaterOperationFailsLeavesNothingBehind(t *testing.T) {
	pool := newGroupPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	project := seedProject(t, pool, "Platform", 0)
	alice := seedUser(t, pool, "alice@corp.com")
	bob := seedUser(t, pool, "bob@corp.com")
	binding, err := store.CreateBinding(ctx, "Platform Team", project, "editor")
	require.NoError(t, err)
	_, err = store.ReplaceGroupMembers(ctx, binding.ID, []int{alice})
	require.NoError(t, err)

	// The role goes away between the grant and the patch, so the SECOND
	// operation below cannot be applied.
	_, err = pool.Exec(ctx,
		`DELETE FROM auth_core__project_role WHERE project_id = $1 AND name = 'editor'`, project)
	require.NoError(t, err)

	_, err = store.ApplyGroupOperations(ctx, binding.ID, []GroupOperation{
		{Kind: GroupRemoveMembers, Members: []int{alice}},
		{Kind: GroupAddMembers, Members: []int{bob}},
	})
	var missing RoleMissingError
	require.ErrorAs(t, err, &missing)

	// The removal in the first operation was rolled back with the failure.
	group, err := store.GetGroup(ctx, binding.ID)
	require.NoError(t, err)
	require.Len(t, group.Members, 1)
	require.Equal(t, alice, group.Members[0].UserID)
}

// The operations of one PATCH are applied IN ORDER, which is what makes the
// add/remove delta an identity provider sends mean what it says.
func TestThePatchOperationsAreAppliedInOrder(t *testing.T) {
	pool := newGroupPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	project := seedProject(t, pool, "Platform", 0)
	alice := seedUser(t, pool, "alice@corp.com")
	bob := seedUser(t, pool, "bob@corp.com")
	binding, err := store.CreateBinding(ctx, "Platform Team", project, "editor")
	require.NoError(t, err)
	_, err = store.ReplaceGroupMembers(ctx, binding.ID, []int{alice})
	require.NoError(t, err)

	group, err := store.ApplyGroupOperations(ctx, binding.ID, []GroupOperation{
		{Kind: GroupAddMembers, Members: []int{bob}},
		{Kind: GroupRemoveMembers, Members: []int{alice}},
		{Kind: GroupRename, DisplayName: "Core Platform"},
	})
	require.NoError(t, err)
	require.Equal(t, "Core Platform", group.DisplayName)
	require.Len(t, group.Members, 1)
	require.Equal(t, bob, group.Members[0].UserID)
	require.True(t, holdsRole(t, pool, project, bob, "editor"))
	require.False(t, holdsRole(t, pool, project, alice, "editor"))
}

/* ── the roles a project really has ────────────────────────────────────── */

// The binding editor's role control reads THIS, not the general role listing,
// which answers a hardcoded admin/editor/viewer for a project with no roles. A
// picker fed by that offers a role the save then refuses.
func TestProjectRoleNamesAnswersWhatTheProjectHas(t *testing.T) {
	pool := newGroupPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	project := seedProject(t, pool, "Platform", 0)
	roles, err := store.ProjectRoleNames(ctx, project)
	require.NoError(t, err)
	require.Equal(t, []string{"admin", "editor", "viewer", "system"}, roles)

	// A project with no roles answers an EMPTY list rather than a default set.
	_, err = pool.Exec(ctx, `DELETE FROM auth_core__project_role WHERE project_id = $1`, project)
	require.NoError(t, err)
	roles, err = store.ProjectRoleNames(ctx, project)
	require.NoError(t, err)
	require.Empty(t, roles)

	_, err = store.ProjectRoleNames(ctx, 4242)
	require.ErrorAs(t, err, &UnknownProjectError{})
}

/* ── deleting a group is not deleting a project ────────────────────────── */

func TestDeletingAGroupWithdrawsItsAccessAndLeavesTheProjectStanding(t *testing.T) {
	pool := newGroupPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	project := seedProject(t, pool, "Platform", 0)
	alice := seedUser(t, pool, "alice@corp.com")
	bob := seedUser(t, pool, "bob@corp.com")
	grantRoleByHand(t, pool, project, bob, "editor")

	binding, err := store.CreateBinding(ctx, "Platform Team", project, "editor")
	require.NoError(t, err)
	_, err = store.ReplaceGroupMembers(ctx, binding.ID, []int{alice, bob})
	require.NoError(t, err)

	require.NoError(t, store.DeleteGroup(ctx, binding.ID))

	require.True(t, projectExists(t, pool, project), "a group deletion must not delete the project")
	require.False(t, holdsRole(t, pool, project, alice, "editor"))
	require.True(t, holdsRole(t, pool, project, bob, "editor"),
		"the member added by hand keeps the access nobody took from them")
	_, err = store.GetGroup(ctx, binding.ID)
	require.ErrorIs(t, err, ErrNotFound)
	// The ledger went with the binding rather than outliving it.
	require.Equal(t, 0, ledgerRows(t, pool))
}

/* ── re-authoring a binding moves the access it granted ────────────────── */

func TestRebindingAGroupMovesTheAccessItGranted(t *testing.T) {
	pool := newGroupPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	project := seedProject(t, pool, "Platform", 0)
	other := seedProject(t, pool, "Research", 0)
	alice := seedUser(t, pool, "alice@corp.com")
	binding, err := store.CreateBinding(ctx, "Platform Team", project, "editor")
	require.NoError(t, err)
	_, err = store.ReplaceGroupMembers(ctx, binding.ID, []int{alice})
	require.NoError(t, err)

	// Same group, another project and another role.
	moved, err := store.UpdateBinding(ctx, binding.ID, "Platform Team", other, "viewer")
	require.NoError(t, err)
	require.Equal(t, other, moved.ProjectID)
	require.False(t, holdsRole(t, pool, project, alice, "editor"),
		"the access the old binding granted is withdrawn")
	require.True(t, holdsRole(t, pool, other, alice, "viewer"),
		"and re-applied under the role the binding now names")
}

/* ── what a binding refuses to be ──────────────────────────────────────── */

func TestABindingIsRefusedWhenTheProjectOrTheRoleDoesNotExist(t *testing.T) {
	pool := newGroupPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	project := seedProject(t, pool, "Platform", 0)

	_, err := store.CreateBinding(ctx, "Ghost", 4242, "editor")
	require.ErrorAs(t, err, &UnknownProjectError{})

	_, err = store.CreateBinding(ctx, "Platform Team", project, "auditor")
	var missing RoleMissingError
	require.ErrorAs(t, err, &missing)
	require.Equal(t, "auditor", missing.RoleName)
	require.Equal(t, project, missing.ProjectID)

	_, err = store.CreateBinding(ctx, "Platform Team", project, "editor")
	require.NoError(t, err)
	// One binding per group name, case-insensitively: two would make the first
	// push resolve to a project chosen at random from the two.
	_, err = store.CreateBinding(ctx, "platform team", project, "viewer")
	require.ErrorIs(t, err, ErrConflict)
}

// A binding that names a role the project HAD when it was authored, and no
// longer has, refuses the push rather than granting something else.
func TestAPushIsRefusedWhenTheBoundRoleHasBeenRemovedFromTheProject(t *testing.T) {
	pool := newGroupPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	project := seedProject(t, pool, "Platform", 0)
	alice := seedUser(t, pool, "alice@corp.com")
	binding, err := store.CreateBinding(ctx, "Platform Team", project, "editor")
	require.NoError(t, err)

	_, err = pool.Exec(ctx,
		`DELETE FROM auth_core__project_role WHERE project_id = $1 AND name = 'editor'`, project)
	require.NoError(t, err)

	_, err = store.ReplaceGroupMembers(ctx, binding.ID, []int{alice})
	var missing RoleMissingError
	require.ErrorAs(t, err, &missing)
	require.Equal(t, "editor", missing.RoleName)
}

/* ── resolving a member ────────────────────────────────────────────────── */

func TestAMemberIsResolvedByIdExternalIdOrAddressAndRefusedOtherwise(t *testing.T) {
	pool := newGroupPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	alice := seedUser(t, pool, "alice@corp.com")
	_, err := pool.Exec(ctx,
		`INSERT INTO elitea_auth.scim_users (user_id, external_id) VALUES ($1, '00u1abc')`, alice)
	require.NoError(t, err)

	for _, value := range []string{fmt.Sprint(alice), "00u1abc", "ALICE@CORP.COM"} {
		resolved, err := store.ResolveMember(ctx, value)
		require.NoError(t, err, "value %q", value)
		require.Equal(t, alice, resolved)
	}

	_, err = store.ResolveMember(ctx, "nobody@corp.com")
	require.ErrorAs(t, err, &UnknownMemberError{})
}

// A numeric member value can name one account by id and another by external id.
// Choosing either would put somebody in a project they are not in.
func TestAMemberValueThatNamesTwoAccountsIsRefused(t *testing.T) {
	pool := newGroupPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	alice := seedUser(t, pool, "alice@corp.com")
	bob := seedUser(t, pool, "bob@corp.com")
	_, err := pool.Exec(ctx,
		`INSERT INTO elitea_auth.scim_users (user_id, external_id) VALUES ($1, $2)`,
		bob, fmt.Sprint(alice))
	require.NoError(t, err)

	_, err = store.ResolveMember(ctx, fmt.Sprint(alice))
	require.ErrorAs(t, err, &AmbiguousMemberError{})
}

/* ── the listing and its filter ────────────────────────────────────────── */

func TestAGroupListingAnswersTheFilterItWasGiven(t *testing.T) {
	pool := newGroupPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	project := seedProject(t, pool, "Platform", 0)
	_, err := store.CreateBinding(ctx, "Platform Team", project, "editor")
	require.NoError(t, err)
	_, err = store.CreateBinding(ctx, "Release Team", project, "viewer")
	require.NoError(t, err)

	filter, err := ParseGroupFilter(`displayName eq "platform team"`)
	require.NoError(t, err)
	groups, total, err := store.ListGroups(ctx, filter, 1, 10)
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Equal(t, "Platform Team", groups[0].DisplayName)
	require.Equal(t, "Platform", groups[0].ProjectName)

	all, total, err := store.ListGroups(ctx, Filter{}, 1, 10)
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Len(t, all, 2)
}

/* ── helpers ───────────────────────────────────────────────────────────── */

func memberOf(group Group, userID int) GroupMember {
	for _, member := range group.Members {
		if member.UserID == userID {
			return member
		}
	}
	return GroupMember{}
}

func seedUser(t *testing.T, pool *pgxpool.Pool, email string) int {
	t.Helper()
	var id int
	require.NoError(t, pool.QueryRow(context.Background(),
		`INSERT INTO auth_core__user (email, name) VALUES ($1, $1) RETURNING id`, email).Scan(&id))
	return id
}

// seedProject creates a project with the four roles projectprovisioning gives
// every project, so a binding here names the same roles a real deployment has.
func seedProject(t *testing.T, pool *pgxpool.Pool, name string, ownerID int) int {
	t.Helper()
	ctx := context.Background()
	var id int
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO centry.project (name, owner_id, keycloak_groups, create_success)
		 VALUES ($1, $2, '{}'::json, true) RETURNING id`, name, ownerID).Scan(&id))
	for _, role := range []string{"admin", "editor", "viewer", "system"} {
		_, err := pool.Exec(ctx,
			`INSERT INTO auth_core__project_role (project_id, name) VALUES ($1, $2)`, id, role)
		require.NoError(t, err)
	}
	return id
}

func grantRoleByHand(t *testing.T, pool *pgxpool.Pool, projectID, userID int, role string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO auth_core__project_user_role (project_id, user_id, role_id)
		 SELECT $1, $2, id FROM auth_core__project_role WHERE project_id = $1 AND name = $3`,
		projectID, userID, role)
	require.NoError(t, err)
}

func holdsRole(t *testing.T, pool *pgxpool.Pool, projectID, userID int, role string) bool {
	t.Helper()
	var holds bool
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT EXISTS (
		     SELECT 1 FROM auth_core__project_user_role AS membership
		     JOIN auth_core__project_role AS role ON role.id = membership.role_id
		     WHERE membership.project_id = $1 AND membership.user_id = $2 AND role.name = $3
		 )`, projectID, userID, role).Scan(&holds))
	return holds
}

func projectExists(t *testing.T, pool *pgxpool.Pool, projectID int) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM centry.project WHERE id = $1)`, projectID).Scan(&exists))
	return exists
}

func ledgerRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM elitea_auth.scim_group_members`).Scan(&count))
	return count
}

/* ── database bootstrap ────────────────────────────────────────────────── */

func newGroupPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL integration test", environment)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	require.NoError(t, err)
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	require.NoError(t, err)
	require.NoError(t, adminPool.Ping(ctx))

	databaseName := fmt.Sprintf("elitea_scim_groups_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{databaseName}.Sanitize()
	_, err = adminPool.Exec(ctx, "CREATE DATABASE "+quoted)
	require.NoError(t, err)

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	require.NoError(t, err)

	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
		adminPool.Close()
	})

	// The tables this package reads and writes, in the shape
	// internal/infra/db/migrations/001_initial.sql creates them. They are
	// restated rather than read from that file because it is a thousand
	// statements of unrelated schema; a drift in these columns fails every test
	// here rather than passing quietly.
	_, err = pool.Exec(ctx, `
		CREATE SCHEMA centry;
		CREATE TABLE auth_core__user (
			id SERIAL PRIMARY KEY,
			email TEXT UNIQUE,
			name TEXT,
			last_login TIMESTAMP,
			suspended BOOLEAN NOT NULL DEFAULT false
		);
		CREATE TABLE auth_core__project_role (
			id SERIAL PRIMARY KEY,
			project_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			UNIQUE (project_id, name)
		);
		CREATE TABLE auth_core__project_user_role (
			id SERIAL PRIMARY KEY,
			project_id INTEGER NOT NULL,
			user_id INTEGER NOT NULL REFERENCES auth_core__user(id) ON DELETE CASCADE,
			role_id INTEGER NOT NULL REFERENCES auth_core__project_role(id) ON DELETE CASCADE,
			UNIQUE (project_id, user_id, role_id)
		);
		CREATE TABLE centry.project (
			id SERIAL PRIMARY KEY,
			name VARCHAR(256) NOT NULL,
			owner_id INTEGER NOT NULL,
			keycloak_groups JSON NOT NULL,
			create_success BOOLEAN NOT NULL DEFAULT false,
			suspended BOOLEAN NOT NULL DEFAULT false
		)`)
	require.NoError(t, err)

	for _, file := range []string{
		"../../migrations/shared/0096_scim_provisioning.sql",
		"../../migrations/shared/0098_scim_group_bindings.sql",
	} {
		migration, err := os.ReadFile(file)
		require.NoError(t, err, "the migration file must be readable: this test proves IT, not a copy of it")
		_, err = pool.Exec(ctx, string(migration))
		require.NoError(t, err)
		// Applying it twice must be a no-op, which is what a partially applied
		// deployment does on its next release.
		_, err = pool.Exec(ctx, string(migration))
		require.NoError(t, err, "%s must be idempotent", file)
	}

	return pool
}

func TestAPatchCanReplaceAGroupsExternalIDInTheSameTransactionAsMembers(t *testing.T) {
	pool := newGroupPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	project := seedProject(t, pool, "Platform", 0)
	alice := seedUser(t, pool, "alice@corp.com")
	binding, err := store.CreateBinding(ctx, "Platform Team", project, "editor")
	require.NoError(t, err)

	group, err := store.ApplyGroupOperations(ctx, binding.ID, []GroupOperation{
		{Kind: GroupSetExternalID, ExternalID: "grp-9"},
		{Kind: GroupAddMembers, Members: []int{alice}},
	})
	require.NoError(t, err)
	require.Equal(t, "grp-9", group.ExternalID)
	require.Len(t, group.Members, 1)

	group, err = store.ApplyGroupOperations(ctx, binding.ID, []GroupOperation{
		{Kind: GroupSetExternalID, ExternalID: ""},
	})
	require.NoError(t, err)
	require.Equal(t, "", group.ExternalID)
}
