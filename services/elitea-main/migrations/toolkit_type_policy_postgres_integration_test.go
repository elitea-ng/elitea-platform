package migrations_test

// Shared migration 0114 — the toolkit type policy tables, the permission grant,
// and the store that reads and writes them, measured against a CLEAN database.
//
// # Why this runs against the real corpus and not a hand-made schema
//
// The properties under test are properties of the MIGRATION: that the CHECKs
// refuse an empty reason, that the grant cascade takes the exceptions with it,
// and that the new permission reaches the administration roles that 0060's
// early return would have skipped. A hand-built table would assert the test's
// own CREATE TABLE, which is the shape [[absence-reads-as-correctness]] warns
// about: a check that measures its own fixture reports success either way.
//
// newMigratedPool creates an empty database, applies the bootstrap schema, then
// applies every shared and tenant migration through the real runner. No seed
// script runs.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcatalogue"
)

// THE HEADLINE PROPERTY. A migrated database with no rows must serve the FULL
// catalogue. An allow-list seeded at install would make a fresh deployment
// offer no toolkit at all, and the failure would read as a broken catalogue
// rather than as a missing seed.
func TestToolkitTypePolicyIsEmptyOnAVirginDatabase(t *testing.T) {
	pool := newMigratedPool(t)
	ctx := context.Background()
	store := toolkitcatalogue.NewStore(pool)

	policies, err := store.ListPolicies(ctx)
	if err != nil {
		t.Fatalf("list policies: %v", err)
	}
	if len(policies) != 0 {
		t.Fatalf("a migrated database carries %d policy rows; 0114 must seed none", len(policies))
	}

	filter, err := store.ProjectFilter(ctx, 1)
	if err != nil {
		t.Fatalf("project filter: %v", err)
	}
	for _, toolkitType := range []string{"github", "jira", "kubernetes", "a type nobody knows"} {
		if !filter.Allows(toolkitType) {
			t.Errorf("an empty policy table withheld %q. Absence is not a decision to withhold.",
				toolkitType)
		}
	}
}

// The permission 0114 grants must reach the administration roles a clean
// database seeds. Without it every route on the new admin surface answers 403
// to the operator, which reads as a broken page rather than a missing grant.
func TestZeroOneOneFourGrantsTheToolkitCatalogueManagePermission(t *testing.T) {
	pool := newMigratedPool(t)

	holders := administrationGrantHolders(t, pool, "toolkit_catalogue.type.manage")
	want := []string{"admin", "super_admin"}
	if len(holders) != len(want) {
		t.Fatalf("administration holders of toolkit_catalogue.type.manage = %v, want %v", holders, want)
	}
	for index := range want {
		if holders[index] != want[index] {
			t.Fatalf("administration holders of toolkit_catalogue.type.manage = %v, want %v", holders, want)
		}
	}

	// The migration also names `system`. 001_initial.sql seeds no
	// administration-mode `system` role, so the WHERE clause matches nothing
	// here — which is why the expected holder set above has two members and not
	// three. Asserting the absence pins that reading: if a later change starts
	// seeding the role, this test says so rather than drifting.
	var systemRoles int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM auth_core__role WHERE mode = 'administration' AND name = 'system'`,
	).Scan(&systemRoles); err != nil {
		t.Fatalf("count administration system roles: %v", err)
	}
	if systemRoles != 0 {
		t.Fatalf("a clean database now seeds %d administration `system` roles; "+
			"the expected holder set above must gain `system`", systemRoles)
	}
}

// The CHECKs are the rule, not the Go validation. A writer with psql must not be
// able to record a decision nobody can justify.
func TestZeroOneOneFourRefusesADecisionWithNoReason(t *testing.T) {
	pool := newMigratedPool(t)
	ctx := context.Background()

	for name, statement := range map[string]string{
		"empty reason": `INSERT INTO centry.toolkit_type_policy
			(toolkit_type, availability, reason, decided_by)
			VALUES ('github', 'disabled', '', 'operator')`,
		"whitespace reason": `INSERT INTO centry.toolkit_type_policy
			(toolkit_type, availability, reason, decided_by)
			VALUES ('github', 'disabled', '   ', 'operator')`,
		"unknown availability": `INSERT INTO centry.toolkit_type_policy
			(toolkit_type, availability, reason, decided_by)
			VALUES ('github', 'maybe', 'a reason', 'operator')`,
		"no decider": `INSERT INTO centry.toolkit_type_policy
			(toolkit_type, availability, reason, decided_by)
			VALUES ('github', 'disabled', 'a reason', '')`,
	} {
		statement := statement
		t.Run(name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, statement); err == nil {
				t.Fatal("the database accepted a decision the CHECK must refuse")
			}
		})
	}
}

// A grant is an exception to a decision. Without the decision the foreign key
// refuses it, and the store turns that into a named error rather than a
// constraint name.
func TestAGrantNeedsItsPolicy(t *testing.T) {
	pool := newMigratedPool(t)
	ctx := context.Background()
	store := toolkitcatalogue.NewStore(pool)

	_, err := store.SaveGrant(ctx, toolkitcatalogue.Grant{
		ToolkitType:  "github",
		ProjectID:    42,
		Availability: toolkitcatalogue.AvailabilityEnabled,
		Reason:       "the client bought the connector",
		GrantedBy:    "operator@example.com",
	})
	if err == nil {
		t.Fatal("a grant with no policy row was accepted")
	}
	if err.Error() != toolkitcatalogue.ErrPolicyNotFound.Error() {
		t.Fatalf("grant with no policy: got %v, want %v", err, toolkitcatalogue.ErrPolicyNotFound)
	}
}

// The whole round trip the admin page drives, and the served catalogue reads,
// against real rows: restrict a type, grant one project, and measure BOTH
// projects.
func TestRestrictThenGrantReachesExactlyOneProject(t *testing.T) {
	pool := newMigratedPool(t)
	ctx := context.Background()
	store := toolkitcatalogue.NewStore(pool)

	if _, err := store.SavePolicy(ctx, toolkitcatalogue.Policy{
		ToolkitType:  "sql",
		Availability: toolkitcatalogue.AvailabilityRestricted,
		Reason:       "direct database access is offered per contract",
		DecidedBy:    "operator@example.com",
	}); err != nil {
		t.Fatalf("save policy: %v", err)
	}
	if _, err := store.SaveGrant(ctx, toolkitcatalogue.Grant{
		ToolkitType:  "sql",
		ProjectID:    7,
		Availability: toolkitcatalogue.AvailabilityEnabled,
		Reason:       "contract 4471",
		GrantedBy:    "operator@example.com",
	}); err != nil {
		t.Fatalf("save grant: %v", err)
	}

	granted, err := store.ProjectFilter(ctx, 7)
	if err != nil {
		t.Fatalf("project filter 7: %v", err)
	}
	if !granted.Allows("sql") {
		t.Error("the granted project did not get the restricted type")
	}
	other, err := store.ProjectFilter(ctx, 8)
	if err != nil {
		t.Fatalf("project filter 8: %v", err)
	}
	if other.Allows("sql") {
		t.Error("an ungranted project got a restricted type")
	}
	// Every OTHER type is untouched by one restriction.
	if !granted.Allows("github") || !other.Allows("github") {
		t.Error("restricting one type withheld another")
	}

	// Reverting to the default must take the exception with it. A grant left
	// behind would be a refusal nobody can see on the page, because the page
	// renders exceptions under their policy row.
	if err := store.DeletePolicy(ctx, "sql"); err != nil {
		t.Fatalf("delete policy: %v", err)
	}
	grants, err := store.ListGrants(ctx)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants) != 0 {
		t.Fatalf("deleting the policy left %d grants behind", len(grants))
	}
	reverted, err := store.ProjectFilter(ctx, 8)
	if err != nil {
		t.Fatalf("project filter after revert: %v", err)
	}
	if !reverted.Allows("sql") {
		t.Error("a reverted type stayed withheld")
	}
}

// A per-project DENY under an enabled deployment policy, and a repeat write
// that replaces rather than duplicates.
func TestPerProjectDenyAndReplacement(t *testing.T) {
	pool := newMigratedPool(t)
	ctx := context.Background()
	store := toolkitcatalogue.NewStore(pool)

	if _, err := store.SavePolicy(ctx, toolkitcatalogue.Policy{
		ToolkitType:  "slack",
		Availability: toolkitcatalogue.AvailabilityEnabled,
		Reason:       "offered by default",
		DecidedBy:    "first@example.com",
	}); err != nil {
		t.Fatalf("save policy: %v", err)
	}
	if _, err := store.SaveGrant(ctx, toolkitcatalogue.Grant{
		ToolkitType:  "slack",
		ProjectID:    9,
		Availability: toolkitcatalogue.AvailabilityDisabled,
		Reason:       "this client has no Slack tenancy",
		GrantedBy:    "operator@example.com",
	}); err != nil {
		t.Fatalf("save grant: %v", err)
	}

	denied, err := store.ProjectFilter(ctx, 9)
	if err != nil {
		t.Fatalf("project filter: %v", err)
	}
	if denied.Allows("slack") {
		t.Error("a per-project deny did not apply")
	}

	// A second write REPLACES, including the decider: a decision that kept a
	// previous operator's reason would attribute a sentence to someone who did
	// not write it.
	saved, err := store.SavePolicy(ctx, toolkitcatalogue.Policy{
		ToolkitType:  "slack",
		Availability: toolkitcatalogue.AvailabilityDisabled,
		Reason:       "withdrawn after the security review",
		DecidedBy:    "second@example.com",
	})
	if err != nil {
		t.Fatalf("replace policy: %v", err)
	}
	if saved.DecidedBy != "second@example.com" || saved.Reason != "withdrawn after the security review" {
		t.Fatalf("a replacement kept stale provenance: %+v", saved)
	}
	policies, err := store.ListPolicies(ctx)
	if err != nil {
		t.Fatalf("list policies: %v", err)
	}
	if len(policies) != 1 {
		t.Fatalf("a replacement produced %d rows", len(policies))
	}

	// Revoking an exception that is not there is reported, not silently
	// accepted: a revoke that changed nothing must not read as a change.
	if err := store.DeleteGrant(ctx, "slack", 4242); err == nil {
		t.Error("revoking a grant that does not exist reported success")
	}
	if err := store.DeleteGrant(ctx, "slack", 9); err != nil {
		t.Fatalf("delete grant: %v", err)
	}
}

// A store built with no pool must report that, never answer "no rows". The
// catalogue path decides what an unreadable policy means at ITS call site, and
// it cannot make that decision if the store hides the failure.
func TestAStoreWithNoPoolReportsItRatherThanAnsweringEmpty(t *testing.T) {
	t.Parallel()

	var pool *pgxpool.Pool
	store := toolkitcatalogue.NewStore(pool)
	if _, err := store.ListPolicies(context.Background()); err == nil {
		t.Fatal("a store with no pool answered a listing")
	}
	if _, err := store.ProjectFilter(context.Background(), 1); err == nil {
		t.Fatal("a store with no pool answered a filter")
	}
}
