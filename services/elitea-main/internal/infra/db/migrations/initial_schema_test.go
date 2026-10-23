package migrations

import (
	"io/fs"
	"strings"
	"testing"

	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

// The dependency that makes an empty database unmigratable without this file
// (#556). The shared history writes into `centry`, and no file in that history
// creates it, so `elitea-migrate` applies THIS schema first. A build that
// embeds an empty or a trimmed 001_initial.sql keeps compiling, applies
// nothing, and then fails on the first shared migration that needs the table:
//
//	migrate: apply shared/0030_execution_kernel.sql: ERROR: schema "centry"
//	does not exist (SQLSTATE 3F000)
//
// Both halves are asserted, because either one alone is satisfied by a file
// that cannot do the job: the schema without the table, or a table declared in
// some other schema.
func TestInitialSchemaCreatesTheObjectsTheSharedHistoryNeeds(t *testing.T) {
	t.Parallel()

	if strings.TrimSpace(Initial) == "" {
		t.Fatal("the embedded legacy-era schema is empty; elitea-migrate cannot build a database from nothing")
	}
	for _, statement := range []string{
		"CREATE SCHEMA IF NOT EXISTS centry",
		"CREATE TABLE IF NOT EXISTS centry.project",
	} {
		if !strings.Contains(Initial, statement) {
			t.Fatalf("the embedded legacy-era schema does not %q. "+
				"migrate.Bootstrap probes centry.project and the shared history "+
				"declares a foreign key to it, so a first install fails (#556)", statement)
		}
	}
}

// The other end of the same dependency, read from the corpus rather than
// restated. A shared migration that stopped referencing centry.project would
// make the assertion above pin a requirement nobody has any more.
func TestSharedHistoryStillDependsOnTheBootstrappedSchema(t *testing.T) {
	t.Parallel()

	entries, err := fs.ReadDir(platformmigrations.Files, "shared")
	if err != nil {
		t.Fatalf("read the shared history: %v", err)
	}
	for _, entry := range entries {
		body, err := fs.ReadFile(platformmigrations.Files, "shared/"+entry.Name())
		if err != nil {
			t.Fatalf("read shared/%s: %v", entry.Name(), err)
		}
		if strings.Contains(string(body), "centry.project") {
			return
		}
	}
	t.Fatal("no shared migration references centry.project any more. " +
		"If the shared history has become self-sufficient, migrate.Bootstrap and " +
		"the assertion above are the things to revisit (#556)")
}
