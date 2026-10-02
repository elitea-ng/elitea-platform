package repos

import (
	"fmt"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
)

func TestApplicationsRepoPostgres_DeletePreservesImportedNonCascadingSchema(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	for _, constraint := range []struct{ table, column, parent string }{
		{"application_versions", "application_id", "applications"},
		{"application_variables", "application_version_id", "application_versions"},
		{"application_version_tag_association", "version_id", "application_versions"},
	} {
		query := fmt.Sprintf("ALTER TABLE p_1.%s DROP CONSTRAINT %s_%s_fkey, ADD FOREIGN KEY (%s) REFERENCES p_1.%s(id)", constraint.table, constraint.table, constraint.column, constraint.column, constraint.parent)
		if _, err := pool.Exec(testContext(t), query); err != nil {
			t.Fatal(err)
		}
	}

	ctx := testContext(t)
	seedUser(t, pool, 1, "one@elitea.ai")

	app := createTestApplication(t, repo, "doomed", 1, &applications.Version{Name: "base"})
	versionID := app.Versions[0].ID
	if _, err := pool.Exec(ctx,
		`INSERT INTO p_1.application_variables (application_version_id, name, value) VALUES ($1, 'k', 'v')`,
		versionID); err != nil {
		t.Fatalf("seed variable: %v", err)
	}

	if _, err := pool.Exec(ctx, `CREATE TABLE p_1.delete_blocker (application_id bigint REFERENCES p_1.applications(id))`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO p_1.delete_blocker VALUES ($1)`, app.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(ctx, testProjectID, app.ID); err == nil {
		t.Fatal("blocked deletion unexpectedly succeeded")
	}
	var retained int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM p_1.application_variables WHERE application_version_id = $1`, versionID).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != 1 {
		t.Fatal("failed deletion removed owned variables")
	}
	if _, err := pool.Exec(ctx, `DROP TABLE p_1.delete_blocker`); err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(ctx, testProjectID, app.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	for label, query := range map[string]string{
		"applications":          `SELECT COUNT(*) FROM p_1.applications WHERE id = $1`,
		"application_versions":  `SELECT COUNT(*) FROM p_1.application_versions WHERE application_id = $1`,
		"application_variables": `SELECT COUNT(*) FROM p_1.application_variables WHERE application_version_id IN (SELECT id FROM p_1.application_versions WHERE application_id = $1)`,
	} {
		var count int
		if err := pool.QueryRow(ctx, query, app.ID).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", label, err)
		}
		if count != 0 {
			t.Errorf("%s still has %d rows after delete", label, count)
		}
	}
}
