package adminaccess_test

// The auditor's reach over the Access reads (ADR-0059 R3.9a, issue 2540), on
// the composed production router of internal/api/adminsurface: a holder of
// only security_auditor reads the administrator listing of R0.10b and the
// role definitions, and is refused the full user listing, which stays on
// admin.auth.users.

import (
	"net/http"
	"testing"

	registry "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/adminsurface"
)

func TestSecurityAuditorReadsTheAdministratorsAndNotTheUserDirectory(t *testing.T) {
	t.Parallel()
	composed, err := adminsurface.ComposedRouter()
	if err != nil {
		t.Fatal(err)
	}
	opts, err := composed.Options()
	if err != nil {
		t.Fatal(err)
	}
	auditor, ok := registry.LookupBuiltInRole(registry.RoleSecurityAuditor)
	if !ok {
		t.Fatal("no security_auditor role")
	}
	send := func(pattern string, params map[string]string) int {
		t.Helper()
		route := adminsurface.Route{Method: http.MethodGet, Pattern: pattern}
		variant := adminsurface.Variant{Params: params, OtherModes: []string{}}
		return adminsurface.Send(composed.Handler, route, variant, opts, auditor.Set()).Status
	}
	// The test router has no database, so an admitted read answers 5xx: what
	// matters is that the gate let it through.
	for _, pattern := range []string{
		"/api/v2/admin/access/roles",
		"/api/v2/admin/access/role-definitions",
	} {
		if status := send(pattern, nil); status == http.StatusForbidden || status == http.StatusUnauthorized {
			t.Errorf("GET %s as security_auditor: %d, want the gate to admit it", pattern, status)
		}
	}
	if status := send("/api/v2/admin/auth_users/{mode}", map[string]string{"mode": "administration"}); status != http.StatusForbidden {
		t.Errorf("GET /api/v2/admin/auth_users/administration as security_auditor: %d, want 403", status)
	}
	// The control: the user directory admits its own name.
	if status := adminsurface.Send(composed.Handler,
		adminsurface.Route{Method: http.MethodGet, Pattern: "/api/v2/admin/auth_users/{mode}"},
		adminsurface.Variant{Params: map[string]string{"mode": "administration"}, OtherModes: []string{}},
		opts, []string{registry.AdminAuthUsers}).Status; status == http.StatusForbidden {
		t.Errorf("GET /api/v2/admin/auth_users/administration with admin.auth.users: 403")
	}
}
