package repos

// The tag WRITE API, which used to write nothing.
//
// `POST /elitea_core/tags/prompt_lib/{project}` decoded the body and echoed it
// back with 201 and an `id` of 0; `DELETE .../{tag}` wrote 204 and returned.
// Neither verb had a repository call at all, so a tag row only ever appeared
// as a side effect of a version save, and a tag could never be removed —
// measured on a running stack, the leftover row had to be deleted with SQL
// because the API could not.
//
// The reads are here too, because the coverage filters are the half a unit
// test over a fake cannot state: `List` answers an EMPTY list on a query
// failure (the tag rail must still draw beside a project whose schema is not
// there), so a filter with broken SQL looks exactly like a project with no
// tags. Only a real database tells the two apart.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/tags"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
)

func newTagsTestRepo(t *testing.T) (*TagsRepo, *ApplicationsRepo, *pgxpool.Pool) {
	t.Helper()
	appsRepo, pool := newApplicationsTestRepo(t)
	return NewTagsRepo(pool), appsRepo, pool
}

func tagNames(rows []tags.Tag) []string {
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Name)
	}
	return names
}

func contains(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

// The create must produce a ROW, with the id the database chose.
func TestTagsRepoPostgres_CreateStoresARowWithARealID(t *testing.T) {
	repo, _, pool := newTagsTestRepo(t)
	ctx := testContext(t)

	stored, err := repo.Create(ctx, testProjectID,
		tags.Tag{Name: "autotest_tag_created", Data: map[string]any{"color": "blue"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if stored.ID <= 0 {
		t.Fatalf("create answered id %d — an id of 0 addresses no row", stored.ID)
	}

	var name, data string
	if err := pool.QueryRow(ctx,
		`SELECT name, COALESCE(data::text, 'null') FROM p_1.tags WHERE id = $1`, stored.ID,
	).Scan(&name, &data); err != nil {
		t.Fatalf("the id the create answered addresses no row: %v", err)
	}
	if name != "autotest_tag_created" {
		t.Errorf("stored name = %q", name)
	}
	if data != `{"color": "blue"}` {
		t.Errorf("stored data = %s, want the object the caller sent", data)
	}

	// …and the list the tag rail reads finds it, unattached to anything.
	listed, err := repo.List(ctx, testProjectID, tags.CoverageAll)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !contains(tagNames(listed), "autotest_tag_created") {
		t.Errorf("the created tag is not in the project's list: %v", tagNames(listed))
	}
}

// One name is one row: `tags.name` is unique per tenant schema and the version
// save keys on the name, so a second create must answer the first row rather
// than a conflict the client can do nothing with.
func TestTagsRepoPostgres_CreateIsIdempotentOnTheName(t *testing.T) {
	repo, _, pool := newTagsTestRepo(t)
	ctx := testContext(t)

	first, err := repo.Create(ctx, testProjectID, tags.Tag{Name: "autotest_tag_twice"})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	second, err := repo.Create(ctx, testProjectID, tags.Tag{Name: "autotest_tag_twice"})
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("the second create answered id %d, want the existing %d", second.ID, first.ID)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM p_1.tags WHERE name = $1`, "autotest_tag_twice").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("the project holds %d rows for one tag name", count)
	}
}

// The delete removes the row AND the associations that point at it, and says
// so when there is nothing to remove.
func TestTagsRepoPostgres_DeleteRemovesTheRowAndItsVersionLinks(t *testing.T) {
	repo, appsRepo, pool := newTagsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "one@elitea.ai")

	app := createTestApplication(t, appsRepo, "autotest_tagged_agent", 1,
		&applications.Version{Name: "base", AuthorID: 1, AgentType: "openai"})
	stored, err := repo.Create(ctx, testProjectID, tags.Tag{Name: "autotest_tag_deleted"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO p_1.application_version_tag_association (version_id, tag_id) VALUES ($1, $2)`,
		app.Versions[0].ID, stored.ID); err != nil {
		t.Fatalf("associate: %v", err)
	}

	if err := repo.Delete(ctx, testProjectID, strconv.Itoa(stored.ID)); err != nil {
		t.Fatalf("delete: %v", err)
	}

	var rows, links int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM p_1.tags WHERE id = $1`, stored.ID).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 0 {
		t.Error("the delete answered success and the tag row is still there")
	}
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM p_1.application_version_tag_association WHERE tag_id = $1`,
		stored.ID).Scan(&links); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if links != 0 {
		t.Errorf("%d association rows survived the delete", links)
	}

	// A tag the project does not hold answers 404, not the same 204 a real
	// delete does: a client cannot otherwise tell a delete from a no-op.
	err = repo.Delete(ctx, testProjectID, strconv.Itoa(stored.ID))
	if got := apiStatus(err); got != 404 {
		t.Errorf("deleting an absent tag answered status %d (%v), want 404", got, err)
	}
}

// The three narrowing filters, over a project holding one agent, one pipeline
// and one tag that belongs to neither.
func TestTagsRepoPostgres_EntityCoverageNarrowsTheList(t *testing.T) {
	repo, appsRepo, pool := newTagsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "one@elitea.ai")

	agent := createTestApplication(t, appsRepo, "autotest_coverage_agent", 1,
		&applications.Version{Name: "base", AuthorID: 1, AgentType: "openai"})
	pipeline := createTestApplication(t, appsRepo, "autotest_coverage_pipeline", 1,
		&applications.Version{Name: "base", AuthorID: 1, AgentType: "pipeline"})

	attach := func(versionID, name string) {
		t.Helper()
		stored, err := repo.Create(ctx, testProjectID, tags.Tag{Name: name})
		if err != nil {
			t.Fatalf("create tag %q: %v", name, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO p_1.application_version_tag_association (version_id, tag_id) VALUES ($1, $2)`,
			versionID, stored.ID); err != nil {
			t.Fatalf("associate %q: %v", name, err)
		}
	}
	attach(agent.Versions[0].ID, "autotest_tag_on_agent")
	attach(pipeline.Versions[0].ID, "autotest_tag_on_pipeline")
	if _, err := repo.Create(ctx, testProjectID, tags.Tag{Name: "autotest_tag_on_nothing"}); err != nil {
		t.Fatalf("create the unattached tag: %v", err)
	}

	for _, testCase := range []struct {
		coverage tags.EntityCoverage
		want     map[string]bool
	}{
		{tags.CoverageApplication, map[string]bool{
			"autotest_tag_on_agent": true, "autotest_tag_on_pipeline": false, "autotest_tag_on_nothing": false,
		}},
		{tags.CoveragePipeline, map[string]bool{
			"autotest_tag_on_agent": false, "autotest_tag_on_pipeline": true, "autotest_tag_on_nothing": false,
		}},
		{tags.CoverageSkill, map[string]bool{
			"autotest_tag_on_agent": false, "autotest_tag_on_pipeline": false, "autotest_tag_on_nothing": false,
		}},
		{tags.CoverageAll, map[string]bool{
			"autotest_tag_on_agent": true, "autotest_tag_on_pipeline": true, "autotest_tag_on_nothing": true,
		}},
	} {
		listed, err := repo.List(ctx, testProjectID, testCase.coverage)
		if err != nil {
			t.Fatalf("list %s: %v", testCase.coverage, err)
		}
		names := tagNames(listed)
		for name, want := range testCase.want {
			if got := contains(names, name); got != want {
				t.Errorf("coverage %s: %q present = %v, want %v (list was %v)",
					testCase.coverage, name, got, want, names)
			}
		}
	}
}
