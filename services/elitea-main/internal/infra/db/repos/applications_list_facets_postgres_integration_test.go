package repos

// The status and origin facets of the agents list: the counts the Workshop's
// filters offer, and the filters that narrow the rows.

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applications"
)

// The list reports how many agents each status and origin holds, and the
// status and origin filters narrow the rows without changing those counts.
func TestApplicationsRepoPostgres_ListCountsStatusAndOriginFacets(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	seedUser(t, pool, 1, "author@example.com")

	createTestApplication(t, repo, "facet-own-draft", 1, &applications.Version{Name: "base", AuthorID: 1, AgentType: "openai"})
	createTestApplication(t, repo, "facet-forked-draft", 1, &applications.Version{
		Name: "base", AuthorID: 1, AgentType: "openai",
		Meta: map[string]any{"parent_entity_id": "7", "parent_project_id": "2"},
	})
	published := createTestApplication(t, repo, "facet-own-published", 1, &applications.Version{Name: "base", AuthorID: 1, AgentType: "openai"})
	if _, err := pool.Exec(testContext(t), `
INSERT INTO p_1.application_versions (application_id, name, status, author_id, agent_type, created_at)
VALUES ($1, 'v1', 'published', 1, 'openai', now() AT TIME ZONE 'UTC')`, published.ID); err != nil {
		t.Fatalf("seed the published version: %v", err)
	}

	page, err := repo.List(testContext(t), applications.ListRequest{ProjectID: testProjectID, PageSize: 50, Status: "draft"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.Total != 2 {
		t.Fatalf("status=draft total = %d, want 2", page.Total)
	}
	if page.Facets == nil {
		t.Fatalf("facets are missing")
	}
	if got := page.Facets.Statuses; got["draft"] != 2 || got["published"] != 1 {
		t.Errorf("status facets = %v, want draft 2 and published 1 whatever status is selected", got)
	}
	if got := page.Facets.Origins; got["own"] != 2 || got["forked"] != 1 {
		t.Errorf("origin facets = %v, want own 2 and forked 1", got)
	}

	forked, err := repo.List(testContext(t), applications.ListRequest{ProjectID: testProjectID, PageSize: 50, Origin: "forked"})
	if err != nil {
		t.Fatalf("list forked: %v", err)
	}
	if forked.Total != 1 || forked.Rows[0].Name != "facet-forked-draft" {
		t.Errorf("origin=forked rows = %+v, want only facet-forked-draft", forked.Rows)
	}
}
