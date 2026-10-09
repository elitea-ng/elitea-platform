package projectaccess

import (
	"strings"
	"testing"
)

// The predicate admits a role in the project or the administration-mode
// super_admin, binds only the two placeholders it is given, and names no
// tenant schema.
func TestMembershipBindsOnlyItsPlaceholdersAndNamesNoTenantSchema(t *testing.T) {
	sql := Membership(3, 5)
	for _, want := range []string{
		"project_id = $3", "user_id = $5", "ur.user_id = $5",
		"role.name = 'super_admin'", "role.mode = 'administration'",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("predicate lacks %q:\n%s", want, sql)
		}
	}
	for _, forbidden := range []string{"$1", "$2", "$4", "p_"} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("predicate contains %q:\n%s", forbidden, sql)
		}
	}
}

func TestProjectExistsReadsTheProjectTable(t *testing.T) {
	if got, want := ProjectExists(2), "EXISTS (SELECT 1 FROM centry.project WHERE id = $2)"; got != want {
		t.Fatalf("ProjectExists = %q, want %q", got, want)
	}
}
