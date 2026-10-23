package legacypgvector

import (
	"errors"
	"strings"
	"testing"
)

// Only exactly project_<id> and project_<id>_user, canonical, int4: anything
// else on a shared server is never read as a project's.
func TestNamesRoundTripAndRefuseLookalikes(t *testing.T) {
	if DatabaseName(7) != "project_7" || RoleName(7) != "project_7_user" {
		t.Fatalf("names = %s, %s", DatabaseName(7), RoleName(7))
	}
	for name, want := range map[string]int64{"project_7": 7, "project_2147483647": 2147483647} {
		if id, ok := ParseDatabaseName(name); !ok || id != want {
			t.Errorf("ParseDatabaseName(%q) = %d, %v", name, id, ok)
		}
	}
	if id, ok := ParseRoleName("project_7_user"); !ok || id != 7 {
		t.Errorf("ParseRoleName = %d, %v", id, ok)
	}
	for _, name := range []string{"project_", "project_0", "project_007", "project_+7", "project_-7",
		"project_2147483648", "project_7_user", "project_7x", "Project_7", "xproject_7"} {
		if _, ok := ParseDatabaseName(name); ok {
			t.Errorf("ParseDatabaseName(%q) accepted a lookalike", name)
		}
	}
	for _, name := range []string{"project_7", "project_007_user", "project_7_user_old", "project__user"} {
		if _, ok := ParseRoleName(name); ok {
			t.Errorf("ParseRoleName(%q) accepted a lookalike", name)
		}
	}
}

func TestNewDropperAcceptsTheStoredFormsAndRefusesTheRest(t *testing.T) {
	for raw, wantDatabase := range map[string]string{
		"postgres://admin:pw@db:5432/postgres":          "postgres",
		"postgresql+psycopg://admin:pw@db:5432/vectors": "vectors",
		" postgresql://admin:pw@db:5432 ":               "postgres",
	} {
		dropper, err := NewDropper(raw)
		if err != nil {
			t.Errorf("NewDropper(%q): %v", raw, err)
			continue
		}
		if dropper.AdminDatabase() != wantDatabase || dropper.AdminHost() != "db:5432" {
			t.Errorf("NewDropper(%q) = %s on %s", raw, dropper.AdminDatabase(), dropper.AdminHost())
		}
	}
	for _, raw := range []string{"", "mysql://db/x", "host=db user=admin", "postgres://admin:pw@db/project_7", "postgres://db/x\ny"} {
		if _, err := NewDropper(raw); !errors.Is(err, ErrInvalidAdminURL) {
			t.Errorf("NewDropper(%q) = %v, want ErrInvalidAdminURL", raw, err)
		}
	}
	// The error never carries the URL: it holds a password.
	if _, err := NewDropper("postgres://admin:s3cret@db/project_7"); err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error %v carries the password", err)
	}
}

func TestSummaryTellsADropFromANoOp(t *testing.T) {
	for dropped, want := range map[Dropped]string{
		{Database: "project_7", Role: "project_7_user", DatabaseDropped: true, RoleDropped: true}: "dropped legacy PgVector database project_7 and role project_7_user",
		{Database: "project_7", Role: "project_7_user", DatabaseDropped: true}:                    "dropped legacy PgVector database project_7 (no role project_7_user)",
		{Database: "project_7", Role: "project_7_user", RoleDropped: true}:                        "dropped legacy PgVector role project_7_user (no database project_7)",
		{Database: "project_7", Role: "project_7_user"}:                                           "no legacy PgVector database or role to drop",
	} {
		if got := dropped.Summary(); got != want {
			t.Errorf("Summary() = %q, want %q", got, want)
		}
	}
}
