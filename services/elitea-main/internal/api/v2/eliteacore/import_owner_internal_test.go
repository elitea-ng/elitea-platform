package eliteacore

// The integration test beside this file only runs where a PostgreSQL service is
// configured. These two run everywhere and catch the same #504 regression at
// the statement level.

import (
	"regexp"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

// owner_id must be named by the INSERT and bound to its own placeholder. A
// statement that omits it fails the corpus NOT NULL constraint with 23502; a
// statement that binds it to the same placeholder as author_id stores a user id
// in a project column and satisfies the constraint while doing so.
func TestImportToolkitInsertSQLBindsTheOwnerAndTheAuthorSeparately(t *testing.T) {
	t.Parallel()

	// importToolkitInsertSQL takes an ALREADY-QUOTED identifier, so the test
	// builds it the way the handlers do rather than restating the quoting.
	schema, err := tenantschema.Quote("1")
	if err != nil {
		t.Fatalf("Quote(1) failed: %v", err)
	}
	statement := importToolkitInsertSQL(schema)

	columns := regexp.MustCompile(`INSERT INTO "p_1"\.elitea_tools \(([^)]*)\)`).FindStringSubmatch(statement)
	if columns == nil {
		t.Fatalf("importToolkitInsertSQL produced an unrecognisable INSERT:\n%s", statement)
	}
	named := strings.Split(strings.ReplaceAll(columns[1], " ", ""), ",")

	values := regexp.MustCompile(`VALUES \(([^)]*)\)`).FindStringSubmatch(statement)
	if values == nil {
		t.Fatalf("importToolkitInsertSQL has no VALUES list:\n%s", statement)
	}
	bound := strings.Split(strings.ReplaceAll(values[1], " ", ""), ",")
	if len(bound) != len(named) {
		t.Fatalf("%d columns but %d values: %v vs %v", len(named), len(bound), named, bound)
	}

	placeholderOf := map[string]string{}
	for position, column := range named {
		placeholderOf[column] = bound[position]
	}
	for _, column := range []string{"owner_id", "author_id"} {
		if _, ok := placeholderOf[column]; !ok {
			t.Fatalf("%s is not among the inserted columns %v", column, named)
		}
		if !strings.HasPrefix(placeholderOf[column], "$") {
			t.Errorf("%s is bound to %q rather than to a placeholder, so the caller cannot supply it",
				column, placeholderOf[column])
		}
	}
	if placeholderOf["owner_id"] == placeholderOf["author_id"] {
		t.Errorf("owner_id and author_id share the placeholder %s; they are different things",
			placeholderOf["owner_id"])
	}
}

// A tenant schema is p_<project id>, so a path segment that is not a project id
// cannot address a schema either. It gets an error rather than a substitute
// value.
func TestTenantOwnerIDRejectsWhatIsNotAProjectID(t *testing.T) {
	t.Parallel()

	for _, projectID := range []string{"", "prompt_lib", "0", "-3", "1; DROP SCHEMA p_1"} {
		if _, err := tenantOwnerID(projectID); err == nil {
			t.Errorf("tenantOwnerID(%q) succeeded, want an error", projectID)
		}
	}
	ownerID, err := tenantOwnerID("5")
	if err != nil {
		t.Fatalf("tenantOwnerID(\"5\"): %v", err)
	}
	if ownerID != 5 {
		t.Errorf("tenantOwnerID(\"5\") = %d, want 5", ownerID)
	}
}
