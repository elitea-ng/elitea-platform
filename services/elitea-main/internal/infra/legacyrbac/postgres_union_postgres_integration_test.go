package legacyrbac_test

// Several administration roles per person (ADR-0059 R0.8b, issue 2574): the
// resolver unions the sets of every administration role a user holds, so a
// person who is both LLM manager and security auditor holds both sets, and
// nothing else.

import (
	"context"
	"slices"
	"strconv"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/legacyrbac"
)

func TestTwoAdministrationRolesResolveToTheUnionOfTheirSets(t *testing.T) {
	pool := newGrantPool(t)
	ctx := context.Background()
	roles := []string{adminaccess.RoleLLMManager, adminaccess.RoleSecurityAuditor}

	var want []string
	for _, name := range roles {
		role, ok := adminaccess.LookupBuiltInRole(name)
		if !ok || !role.DeclaresSet() {
			t.Fatalf("%s is not a built-in role with a declared set", name)
		}
		want = append(want, role.Set()...)
		if _, err := pool.Exec(ctx, `INSERT INTO public.auth_core__role (name, mode) VALUES ($1, 'administration')`, name); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__role_permission (role_id, permission)
SELECT role.id, granted.permission
FROM public.auth_core__role role, unnest($2::text[]) AS granted(permission)
WHERE role.name = $1 AND role.mode = 'administration'`, name, role.Set()); err != nil {
			t.Fatalf("grant the %s set: %v", name, err)
		}
	}
	slices.Sort(want)
	want = slices.Compact(want)

	var userID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO public.auth_core__user (email, name) VALUES ('r08b-union@autotest.local', 'Union')
RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("create the user: %v", err)
	}
	// A user who holds one of the two first: the union must grow when the
	// second arrives, and hold exactly both sets after it.
	resolver := legacyrbac.NewPostgresResolver(pool)
	id := strconv.FormatInt(userID, 10)
	principal := auth.User{ID: id, UserID: id, CredentialKind: auth.CredentialKindSession}
	var previous []string
	for count, name := range roles {
		if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user_role (user_id, role_id)
SELECT $1, id FROM public.auth_core__role WHERE name = $2 AND mode = 'administration'`, userID, name); err != nil {
			t.Fatalf("assign %s: %v", name, err)
		}
		resolution, err := resolver.ResolvePermissions(ctx, principal, auth.PermissionModeAdministration, "")
		if err != nil {
			t.Fatalf("resolve after %d roles: %v", count+1, err)
		}
		got := slices.Clone(resolution.Permissions)
		slices.Sort(got)
		if count == 0 {
			first, _ := adminaccess.LookupBuiltInRole(name)
			if !slices.Equal(got, first.Set()) {
				t.Fatalf("one role resolves to %v, want %v", got, first.Set())
			}
		} else if len(got) <= len(previous) {
			t.Fatalf("the second role did not widen the set: %v then %v", previous, got)
		}
		previous = got
	}
	if !slices.Equal(previous, want) {
		t.Fatalf("two roles resolve to %v, want the union %v", previous, want)
	}
}
