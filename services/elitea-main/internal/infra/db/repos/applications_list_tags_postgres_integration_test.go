package repos

// The tags every agents/pipelines list card and tag filter reads.
//
// `List` never mentioned tags. The row carried no `tags` key at all, and the
// `tags` request parameter the handler decodes into `ListRequest.Tags` reached
// a query that ignored it. The detail route DID fill tags
// (`applications/handler.go` versionTagsOrEmpty), so list and detail disagreed
// about the same agent, and the web app's tag rail wrote a search param that
// narrowed nothing (issue 841; the disclosure is in
// `apps/elitea-web/src/pages/agents/Applications.tsx`).

import (
	"reflect"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
)

// tagTestApplication creates an application with one base version and puts the
// named tags on that version, as the version editor's tag control does.
func tagTestApplication(t *testing.T, repo *ApplicationsRepo, pool *pgxpool.Pool, name string, tags ...string) applications.Application {
	t.Helper()
	app := createTestApplication(t, repo, name, 1, &applications.Version{
		Name: "base", AuthorID: 1, AgentType: "openai",
	})
	for _, tag := range tags {
		var tagID int
		if err := pool.QueryRow(testContext(t), `
INSERT INTO p_1.tags (name) VALUES ($1)
ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
RETURNING id`, tag).Scan(&tagID); err != nil {
			t.Fatalf("seed tag %q: %v", tag, err)
		}
		if _, err := pool.Exec(testContext(t), `
INSERT INTO p_1.application_version_tag_association (version_id, tag_id)
VALUES ($1, $2)`, app.Versions[0].ID, tagID); err != nil {
			t.Fatalf("associate tag %q: %v", tag, err)
		}
	}
	return app
}

func listedTags(t *testing.T, repo *ApplicationsRepo, req applications.ListRequest) map[string][]string {
	t.Helper()
	req.ProjectID = testProjectID
	if req.PageSize == 0 {
		req.PageSize = 50
	}
	page, err := repo.List(testContext(t), req)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	out := map[string][]string{}
	for _, row := range page.Rows {
		out[row.Name] = row.Tags
	}
	return out
}

func TestApplicationsRepoPostgres_ListCarriesTheVersionTags(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	seedUser(t, pool, 1, "author@example.com")

	tagTestApplication(t, repo, pool, "tagged-agent", "zeta", "alpha")
	tagTestApplication(t, repo, pool, "untagged-agent")

	got := listedTags(t, repo, applications.ListRequest{})

	// Sorted by name, not by association order: the card shows the first two
	// tags, so the order must not depend on which row the plan reads first.
	want := []string{"alpha", "zeta"}
	if !reflect.DeepEqual(got["tagged-agent"], want) {
		t.Errorf("tagged-agent tags = %#v, want %#v", got["tagged-agent"], want)
	}

	// An empty array and not nil. `tags` has no omitempty, so a nil slice
	// would put `"tags": null` on the wire and every `tags.length` read in
	// the web app would throw.
	empty, ok := got["untagged-agent"]
	if !ok {
		t.Fatalf("untagged-agent is not in the list")
	}
	if empty == nil {
		t.Errorf("untagged-agent tags = nil, want an empty array")
	}
	if len(empty) != 0 {
		t.Errorf("untagged-agent tags = %#v, want an empty array", empty)
	}
}

// Tags come from EVERY version of the application, deduplicated by name, as
// legacy's ApplicationListModel.parse_versions_data builds them. `DISTINCT ON
// (a.id)` keeps one version row, so a joined aggregate would describe only
// that version.
func TestApplicationsRepoPostgres_ListUnionsTheTagsOfEveryVersion(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	seedUser(t, pool, 1, "author@example.com")

	app := tagTestApplication(t, repo, pool, "many-versions", "shared")

	var secondVersionID int
	if err := pool.QueryRow(testContext(t), `
INSERT INTO p_1.application_versions (application_id, name, status, author_id, agent_type)
VALUES ($1, 'v1', 'draft', 1, 'openai') RETURNING id`, app.ID).Scan(&secondVersionID); err != nil {
		t.Fatalf("seed the second version: %v", err)
	}
	for _, tag := range []string{"shared", "only-on-v1"} {
		var tagID int
		if err := pool.QueryRow(testContext(t), `
INSERT INTO p_1.tags (name) VALUES ($1)
ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name RETURNING id`, tag).Scan(&tagID); err != nil {
			t.Fatalf("seed tag %q: %v", tag, err)
		}
		if _, err := pool.Exec(testContext(t), `
INSERT INTO p_1.application_version_tag_association (version_id, tag_id) VALUES ($1, $2)`,
			secondVersionID, tagID); err != nil {
			t.Fatalf("associate tag %q: %v", tag, err)
		}
	}

	got := listedTags(t, repo, applications.ListRequest{})
	want := []string{"only-on-v1", "shared"}
	if !reflect.DeepEqual(got["many-versions"], want) {
		t.Errorf("many-versions tags = %#v, want %#v", got["many-versions"], want)
	}
}

func TestApplicationsRepoPostgres_ListFiltersByTag(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	seedUser(t, pool, 1, "author@example.com")

	tagTestApplication(t, repo, pool, "filter-both", "finance", "legal")
	tagTestApplication(t, repo, pool, "filter-one", "finance")
	tagTestApplication(t, repo, pool, "filter-none")

	byName := listedTags(t, repo, applications.ListRequest{Tags: "finance"})
	if _, ok := byName["filter-none"]; ok {
		t.Errorf("the tag filter kept an application with no tags")
	}
	if _, ok := byName["filter-one"]; !ok {
		t.Errorf("the tag filter dropped an application that carries the tag")
	}

	// Two entries are AND and not OR, as legacy's get_application_by_tags is.
	both := listedTags(t, repo, applications.ListRequest{Tags: "finance,legal"})
	if _, ok := both["filter-one"]; ok {
		t.Errorf("the two-tag filter kept an application that carries only one of them")
	}
	if _, ok := both["filter-both"]; !ok {
		t.Errorf("the two-tag filter dropped an application that carries both")
	}

	// The count is filtered too. A total taken over the unfiltered set makes
	// the pager offer pages the filtered list cannot fill.
	page, err := repo.List(testContext(t), applications.ListRequest{
		ProjectID: testProjectID, PageSize: 50, Tags: "finance,legal"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.Total != 1 {
		t.Errorf("total = %d, want 1", page.Total)
	}

	// A tag ID is accepted as well, which is the only form legacy took.
	var financeID int
	if err := pool.QueryRow(testContext(t), `SELECT id FROM p_1.tags WHERE name = 'finance'`).Scan(&financeID); err != nil {
		t.Fatalf("read the tag id: %v", err)
	}
	byID := listedTags(t, repo, applications.ListRequest{Tags: strconv.Itoa(financeID)})
	if len(byID) != 2 {
		t.Errorf("filter by tag id matched %d applications, want 2", len(byID))
	}

	// A blank filter is "no filter" and not "match nothing".
	blank := listedTags(t, repo, applications.ListRequest{Tags: " , "})
	if len(blank) != 3 {
		t.Errorf("a blank tag filter matched %d applications, want 3", len(blank))
	}
}

func TestSplitTagFilter(t *testing.T) {
	cases := map[string][]string{
		"":            {},
		"  ":          {},
		",,":          {},
		"a":           {"a"},
		" a , b ":     {"a", "b"},
		"a,,b,":       {"a", "b"},
		"12,finance":  {"12", "finance"},
		"with space ": {"with space"},
	}
	for raw, want := range cases {
		if got := splitTagFilter(raw); !reflect.DeepEqual(got, want) {
			t.Errorf("splitTagFilter(%q) = %#v, want %#v", raw, got, want)
		}
	}
}
