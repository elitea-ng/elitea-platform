package legacyrbac_test

// The blast radius of the three default-mode grants added by
// migrations/shared/0063_trace_and_cost_read_permissions.sql (issue 253),
// pinned the way 0062's was.
//
// The routes those grants exist for — the LLM cost breakdown and the two chat
// trace reads — are gated on `models.monitoring.tracing.view`,
// `models.chat.messages.list` and `models.chat.messages.details`. On a
// Go-bootstrapped database nothing granted any of the three before this
// migration, so gating on them without it is 403-for-everyone: a broken page
// that looks like a bug in the handler.
//
// Unlike 0062's grant this one changes no permission-set SIZE transition —
// 0062 already made a member's set non-empty — so the whole radius is: the
// three permissions become resolvable for project MEMBERS, and for nobody else.
// Both halves are asserted below against a real PostgreSQL, and against the
// migration FILE rather than a copy of its SQL, so an edit to the file that
// changed which roles it grants cannot pass here.

import (
	"context"
	"slices"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

var issue253Permissions = []string{
	"models.chat.messages.details",
	"models.chat.messages.list",
	"models.monitoring.tracing.view",
}

func TestTraceAndCostGrantsResolveForAProjectMember(t *testing.T) {
	pool := newGrantPool(t)
	seedFreshDatabaseShape(t, pool)
	resolver := legacyrbac.NewPostgresResolver(pool)
	member := auth.User{ID: "1", UserID: "1"}

	before, err := resolver.ResolvePermissions(context.Background(), member, auth.PermissionModeDefault, "1")
	if err != nil {
		t.Fatal(err)
	}
	for _, permission := range issue253Permissions {
		if slices.Contains(before.Permissions, permission) {
			t.Fatalf("%s resolves before the migration; this test's premise is that it does not", permission)
		}
	}

	sql, err := migrations.Files.ReadFile("shared/0063_trace_and_cost_read_permissions.sql")
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	if _, err := pool.Exec(context.Background(), string(sql)); err != nil {
		t.Fatalf("apply 0063: %v", err)
	}

	after, err := resolver.ResolvePermissions(context.Background(), member, auth.PermissionModeDefault, "1")
	if err != nil {
		t.Fatal(err)
	}
	for _, permission := range issue253Permissions {
		if !slices.Contains(after.Permissions, permission) {
			t.Fatalf("%s does not resolve after the migration: the route it gates would be "+
				"403 for every user, which reads as a broken page rather than a missing grant",
				permission)
		}
	}
}

func TestTraceAndCostGrantsGiveANonMemberNothing(t *testing.T) {
	pool := newGrantPool(t)
	seedFreshDatabaseShape(t, pool)

	sql, err := migrations.Files.ReadFile("shared/0063_trace_and_cost_read_permissions.sql")
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	if _, err := pool.Exec(context.Background(), string(sql)); err != nil {
		t.Fatalf("apply 0063: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO public.auth_core__user (id, email, name) VALUES (3, 'out253@example.com', 'Out')`); err != nil {
		t.Fatal(err)
	}

	resolution, err := legacyrbac.NewPostgresResolver(pool).ResolvePermissions(
		context.Background(), auth.User{ID: "3", UserID: "3"}, auth.PermissionModeDefault, "1")
	if err != nil {
		t.Fatal(err)
	}
	if len(resolution.Permissions) != 0 {
		t.Fatalf("non-member permissions = %v, want empty: the central fallback is joined "+
			"through the caller's project roles and must not reach outside the project",
			resolution.Permissions)
	}
}
