package repos

// The platform-model GRANT, read out of a real PostgreSQL and applied by the
// catalogue builder.
//
// ## What is under test, and why it needs a database
//
// The rule is stated in application/configurations/model_grant.go: a catalogue
// row is offered to every project (`all`, and the absent value), to none, or to
// the ids in `shared_with`. The DECISION is pure and is unit-tested beside it.
// What is not pure is the path between the stored `data` column and that
// decision: the adapter reads the grant off the row it scanned, and it reads it
// for the cross-project query alone.
//
// A grant that never left the database would make every model look granted, and
// a unit test over hand-built items cannot see it — the items would carry a
// grant nothing had to read. So each case below seeds a real jsonb column and
// asserts WHICH model names the built catalogue holds.
//
// The same seed answers for two projects, which is the part a single-project
// assertion cannot state: the feature is one row answering differently for two
// callers, and a filter that dropped every shared row would satisfy half of it.

import (
	"context"
	"testing"
	"time"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// grantedCatalogueProject is the project the grants below name. It is a third
// project, created by this file, so the grant under test cannot be confused
// with the project the other model tests already read.
const grantedCatalogueProject = 3

// seedGrantedCatalogueModels writes one catalogue row per scope into p_1, and
// creates the granted project's own (empty) schema.
//
// Every row is `shared = true`: the grant NARROWS `shared`, it does not replace
// it, so a row that stopped being shared would be excluded by the predicate
// rather than by the grant and would prove nothing about this rule.
func seedGrantedCatalogueModels(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
INSERT INTO centry.project (id) VALUES (3);
CREATE SCHEMA p_3;
CREATE TABLE p_3.configuration (LIKE p_2.configuration INCLUDING ALL);

INSERT INTO p_1.configuration (
    id, uuid, project_id, label, elitea_title, type, section, data, meta,
    shared, status_ok, source
) VALUES
    (11, '00000000-0000-0000-0000-000000000111', 1, 'Granted to all',
     'grant_all', 'llm_model', 'llm',
     '{"name":"grant-all","share_scope":"all"}'::jsonb,
     '{}'::jsonb, true, true, 'test'),
    (12, '00000000-0000-0000-0000-000000000112', 1, 'Granted to nobody',
     'grant_none', 'llm_model', 'llm',
     '{"name":"grant-none","share_scope":"none","shared_with":[]}'::jsonb,
     '{}'::jsonb, true, true, 'test'),
    (13, '00000000-0000-0000-0000-000000000113', 1, 'Granted to project 3',
     'grant_three', 'llm_model', 'llm',
     '{"name":"grant-three","share_scope":"projects","shared_with":[3]}'::jsonb,
     '{}'::jsonb, true, true, 'test'),
    (14, '00000000-0000-0000-0000-000000000114', 1, 'Written before the grant existed',
     'grant_absent', 'llm_model', 'llm',
     '{"name":"grant-absent"}'::jsonb,
     '{}'::jsonb, true, true, 'test');`); err != nil {
		t.Fatalf("seed granted catalogue models: %v", err)
	}
}

// catalogueNamesFor answers the model names one project reads, through the
// production path: the two repository queries the route makes, then the
// builder that merges them.
func catalogueNamesFor(
	t *testing.T, repository *CurrentModelsRepository, projectID int32,
) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	own, err := repository.List(ctx, projectID, configurationapp.CurrentModelSectionLLM, false)
	if err != nil {
		t.Fatalf("list project %d candidates: %v", projectID, err)
	}
	shared, err := repository.List(ctx, 1, configurationapp.CurrentModelSectionLLM, true)
	if err != nil {
		t.Fatalf("list catalogue candidates: %v", err)
	}
	response := configurationapp.BuildCurrentModelCatalog(configurationapp.CurrentModelCatalogRequest{
		Section:           configurationapp.CurrentModelSectionLLM,
		ProjectID:         projectID,
		PublicProjectID:   1,
		IncludeShared:     true,
		ProjectItems:      own,
		PublicSharedItems: shared,
	})
	names := make([]string, 0, len(response.Items))
	for _, item := range response.Items {
		names = append(names, item.Name)
	}
	return names
}

func containsName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

// TestCatalogueGrantDecidesWhichProjectSeesAModel is the whole rule, over one
// seed and two callers.
func TestCatalogueGrantDecidesWhichProjectSeesAModel(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	prepareCurrentConfigurationsProjectTwo(t, pool)
	seedGrantedCatalogueModels(t, pool)

	repository, err := NewCurrentModelsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}

	granted := catalogueNamesFor(t, repository, grantedCatalogueProject)
	other := catalogueNamesFor(t, repository, 2)

	// `all` and an ABSENT grant reach both. The absent case is the one every
	// existing deployment is in, so a rule that withdrew it would empty the
	// model picker of every project on upgrade.
	for _, name := range []string{"grant-all", "grant-absent"} {
		if !containsName(granted, name) {
			t.Errorf("project %d cannot see %q: %v", grantedCatalogueProject, name, granted)
		}
		if !containsName(other, name) {
			t.Errorf("project 2 cannot see %q: %v", name, other)
		}
	}

	// `none` reaches neither.
	if containsName(granted, "grant-none") || containsName(other, "grant-none") {
		t.Errorf("a model granted to no project reached a project: %v / %v", granted, other)
	}

	// `projects` reaches the named project and NOT the other one. Both halves
	// are asserted: a filter that dropped every scoped row would satisfy the
	// second on its own.
	if !containsName(granted, "grant-three") {
		t.Errorf("project %d cannot see the model it was granted: %v", grantedCatalogueProject, granted)
	}
	if containsName(other, "grant-three") {
		t.Errorf("project 2 sees a model granted to project %d only: %v", grantedCatalogueProject, other)
	}
}

// TestTheCatalogueProjectKeepsItsWithdrawnModels is what makes a `none` grant
// recoverable.
//
// The public project reads its own schema as its OWN scope, which the grant
// never filters, and the merge is skipped entirely when the caller IS the
// public project. Without that, setting a model to "no project" would remove it
// from the screen that has to set it back.
func TestTheCatalogueProjectKeepsItsWithdrawnModels(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	prepareCurrentConfigurationsProjectTwo(t, pool)
	seedGrantedCatalogueModels(t, pool)

	repository, err := NewCurrentModelsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	names := catalogueNamesFor(t, repository, 1)
	for _, name := range []string{"grant-none", "grant-three", "grant-all", "grant-absent"} {
		if !containsName(names, name) {
			t.Errorf("the catalogue project cannot see its own model %q: %v", name, names)
		}
	}
}

// TestAnOwnRowIsNeverFilteredByAGrant: the grant is a rule about the CATALOGUE
// scope. A project that happens to carry the field on one of its own rows keeps
// that model, because the row is its own and no grant was ever needed for it.
func TestAnOwnRowIsNeverFilteredByAGrant(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	prepareCurrentConfigurationsProjectTwo(t, pool)
	seedGrantedCatalogueModels(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
INSERT INTO p_2.configuration (
    id, uuid, project_id, label, elitea_title, type, section, data, meta,
    shared, status_ok, source
) VALUES (
    21, '00000000-0000-0000-0000-000000000221', 2, 'Own row carrying a grant',
    'project_two_own_scoped', 'llm_model', 'llm',
    '{"name":"own-scoped","share_scope":"none"}'::jsonb,
    '{}'::jsonb, false, true, 'test'
)`); err != nil {
		t.Fatalf("seed the project's own scoped row: %v", err)
	}

	repository, err := NewCurrentModelsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	if names := catalogueNamesFor(t, repository, 2); !containsName(names, "own-scoped") {
		t.Errorf("a project's own row was filtered by a catalogue grant: %v", names)
	}
}
