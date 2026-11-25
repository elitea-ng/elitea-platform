package admin_test

// The reserved break-glass account (ADR-0059 R2.6b, issue 2599) on the admin
// Users surface: the listing never shows it, on either tab, nor counts it,
// and no role assignment reaches it, even from a super_admin caller.
// Skipped without a PostgreSQL, like the other integration tests here.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/admin"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

func TestTheReservedAccountIsNotListedAndTakesNoRole(t *testing.T) {
	pool := newAdminUsersPool(t)
	prepareAdminUsersFixture(t, pool)
	router := adminUsersRouter(admin.NewHandler(pool, admin.WithPermissionResolver(superAdminResolver())),
		&auth.User{ID: "1", UserID: "1"})

	before := readUsers(t, router, "")
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO public.auth_core__user (email, name) VALUES ($1, 'Break-glass account')`,
		adminaccess.ReservedBreakGlassEmail); err != nil {
		t.Fatalf("seed the reserved account: %v", err)
	}
	id := userID(t, pool, adminaccess.ReservedBreakGlassEmail)

	for _, query := range []string{"", "tab=system", "tab=platform", "search=break-glass"} {
		listing := readUsers(t, router, query)
		if _, found := userByEmail(t, listing, adminaccess.ReservedBreakGlassEmail); found {
			t.Fatalf("listing %q shows the reserved account", query)
		}
		if query == "" && (listing.Total != before.Total ||
			listing.Counts.Platform != before.Counts.Platform || listing.Counts.System != before.Counts.System) {
			t.Fatalf("the reserved account is counted: total %d→%d, counts %+v→%+v",
				before.Total, listing.Total, before.Counts, listing.Counts)
		}
	}

	for _, role := range []string{"admin", "super_admin", "editor"} {
		recorder := adminDo(t, router, http.MethodPost, "/admin/auth_users/administration",
			map[string]any{"action": "set_admin_role", "user_id": id, "role_name": role})
		if recorder.Code != http.StatusForbidden ||
			!strings.Contains(recorder.Body.String(), adminaccess.CodeReservedAccount) {
			t.Fatalf("set_admin_role %s on the reserved account: %d %s; want 403 %s",
				role, recorder.Code, recorder.Body.String(), adminaccess.CodeReservedAccount)
		}
	}
	if names := adminRoleNamesSQL(t, pool, id); len(names) != 0 {
		t.Fatalf("the reserved account holds %v", names)
	}
}
