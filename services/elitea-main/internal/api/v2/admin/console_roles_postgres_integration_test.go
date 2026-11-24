package admin_test

// The console capability document reads its roles from PostgreSQL
// (ADR-0059 R3.1a, internal/api/v2/adminaccess/store.go): the roles a user
// holds and the roles that grant an item, each built-in by its seed record.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	console "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/adminaccess"
)

func TestTheConsoleRolesReadTheSeedRecord(t *testing.T) {
	env := newEscalationEnvironment(t)
	seedBuiltIn(t, env.pool, "admin")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	roles := console.NewPostgresRoles(env.pool)

	held, err := roles.HeldRoles(ctx, int64(env.ids[escalationAdmin]))
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || held[0].Name != "admin" || !held[0].BuiltIn || held[0].Marker != "declared" {
		t.Errorf("held roles of the admin: got %+v, want the built-in admin", held)
	}
	plain, err := roles.HeldRoles(ctx, int64(env.ids["esc-plain-target@autotest.local"]))
	if err != nil || len(plain) != 0 {
		t.Errorf("held roles of a user with none: got %+v, %v; want none", plain, err)
	}

	granting, err := roles.GrantingRoles(ctx, []string{adminaccess.ConfigurationRolesPermissionsView,
		adminaccess.ConfigurationRolesPermissionsEdit})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]console.GrantingRole{}
	for _, role := range granting {
		byName[role.Name] = role
	}
	editor, ok := byName["matrix_editor"]
	if !ok || editor.BuiltIn || !slices.Equal(editor.Permissions, []string{
		adminaccess.ConfigurationRolesPermissionsEdit, adminaccess.ConfigurationRolesPermissionsView,
	}) {
		t.Errorf("matrix_editor: got %+v, want a custom role with both names", editor)
	}
	if _, ok := byName["modes_operator"]; ok {
		t.Errorf("modes_operator holds neither name but is listed: %+v", granting)
	}
}
