package repos

// The publish state every agents-list tab filters on.
//
// `Application.Status` was never selected by `List`, so every listed agent
// carried the empty string and `omitempty` dropped the key from the response.
// The web app's Drafts, Published, Moderation, Approval and Rejected tabs
// filter on exactly that field client-side
// (`apps/elitea-web/src/pages/agents/PrivateAgentsList.tsx`), so all five were
// permanently empty and no publish could ever fill one. A test that asserted
// only "List returns rows" passed throughout.

import (
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
)

func TestApplicationsRepoPostgres_ListReportsThePublishState(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	seedUser(t, pool, 1, "author@example.com")

	createTestApplication(t, repo, "list-status-draft", 1, &applications.Version{
		Name: "base", AuthorID: 1, AgentType: "openai",
	})
	published := createTestApplication(t, repo, "list-status-published", 1, &applications.Version{
		Name: "base", AuthorID: 1, AgentType: "openai",
	})

	// The publish handler writes a SECOND version at `published` and leaves the
	// first as a draft. The fixture reproduces that shape rather than flipping
	// the only version, so this also proves an agent whose base version is
	// still a draft is reported as published.
	if _, err := pool.Exec(testContext(t), `
INSERT INTO p_1.application_versions (application_id, name, status, author_id, agent_type)
VALUES ($1, 'v1', 'published', 1, 'openai')`, published.ID); err != nil {
		t.Fatalf("seed the published version: %v", err)
	}

	page, err := repo.List(testContext(t), applications.ListRequest{ProjectID: testProjectID, PageSize: 50})
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	statuses := map[string]string{}
	for _, row := range page.Rows {
		statuses[row.Name] = row.Status
	}
	if statuses["list-status-draft"] != "draft" {
		t.Errorf("draft agent status = %q, want %q", statuses["list-status-draft"], "draft")
	}
	if statuses["list-status-published"] != "published" {
		t.Errorf("published agent status = %q, want %q", statuses["list-status-published"], "published")
	}
}

// An `embedded` version is created by a PARENT agent's publish to carry its
// sub-agents. Reading one as published would put an agent in the Published tab
// because something else was published.
func TestApplicationsRepoPostgres_ListDoesNotCallAnEmbeddedClonePublished(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	seedUser(t, pool, 1, "author@example.com")

	embedded := createTestApplication(t, repo, "list-status-embedded", 1, &applications.Version{
		Name: "base", AuthorID: 1, AgentType: "openai",
	})
	if _, err := pool.Exec(testContext(t), `
UPDATE p_1.application_versions SET status = 'embedded' WHERE application_id = $1`, embedded.ID); err != nil {
		t.Fatalf("seed the embedded version: %v", err)
	}

	page, err := repo.List(testContext(t), applications.ListRequest{ProjectID: testProjectID, PageSize: 50})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, row := range page.Rows {
		if row.Name == "list-status-embedded" && row.Status == "published" {
			t.Fatalf("an embedded clone is reported as published")
		}
	}
}
