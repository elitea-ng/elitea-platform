package repos

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applications"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

// TestApplicationsRepoPostgres_ListCarriesTheProbationOfAnAutomaticHire covers
// the Workshop list row of an agent the workforce policy hired (issue 2091):
// the row says it is on probation, and until when, without a request per agent.
// A kept agent, a person's hire and an agent of another project carry nothing.
func TestApplicationsRepoPostgres_ListCarriesTheProbationOfAnAutomaticHire(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	if err := migrate.New(pool, platformmigrations.Files).ApplyShared(ctx); err != nil {
		t.Fatalf("apply the shared migrations: %v", err)
	}
	seedUser(t, pool, 1, "first@example.com")
	probation := createTestApplication(t, repo, "On probation", 1, &applications.Version{Name: "base", AgentType: "openai"})
	kept := createTestApplication(t, repo, "Kept", 1, &applications.Version{Name: "base", AgentType: "openai"})
	person := createTestApplication(t, repo, "By a person", 1, &applications.Version{Name: "base", AgentType: "openai"})

	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_lifecycle.agent_registry (tenant_project_id, application_id, auto_hired_at, probation_until)
VALUES (1, $1::bigint, '2026-11-01T00:00:00Z', '2026-11-15T00:00:00Z')
ON CONFLICT (tenant_project_id, application_id) DO UPDATE SET auto_hired_at = EXCLUDED.auto_hired_at, probation_until = EXCLUDED.probation_until`, probation.ID); err != nil {
		t.Fatalf("seed the probation: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_lifecycle.agent_registry (tenant_project_id, application_id, auto_hired_at, probation_until, probation_reviewed_at)
VALUES (1, $1::bigint, '2026-11-01T00:00:00Z', '2026-11-15T00:00:00Z', '2026-11-05T00:00:00Z')
ON CONFLICT (tenant_project_id, application_id) DO UPDATE SET auto_hired_at = EXCLUDED.auto_hired_at, probation_until = EXCLUDED.probation_until, probation_reviewed_at = EXCLUDED.probation_reviewed_at`, kept.ID); err != nil {
		t.Fatalf("seed the kept agent: %v", err)
	}
	// The same application id in ANOTHER project must not leak in.
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_lifecycle.agent_registry (tenant_project_id, application_id, auto_hired_at, probation_until)
VALUES (2, $1::bigint, '2026-11-01T00:00:00Z', '2026-11-15T00:00:00Z')
ON CONFLICT (tenant_project_id, application_id) DO UPDATE SET auto_hired_at = EXCLUDED.auto_hired_at, probation_until = EXCLUDED.probation_until`, person.ID); err != nil {
		t.Fatalf("seed the other project: %v", err)
	}

	result, err := repo.List(ctx, applications.ListRequest{ProjectID: "1", SortBy: "name", SortOrder: "asc"})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]map[string]any{}
	for _, row := range result.Rows {
		byName[row.Name] = row.Meta
	}
	if byName["On probation"]["on_probation"] != true || byName["On probation"]["probation_until"] != "2026-11-15T00:00:00Z" {
		t.Errorf("the automatic hire's row = %v, want on probation until 2026-11-15", byName["On probation"])
	}
	for _, name := range []string{"Kept", "By a person"} {
		if _, on := byName[name]["on_probation"]; on {
			t.Errorf("%q carries a probation: %v", name, byName[name])
		}
	}
}
