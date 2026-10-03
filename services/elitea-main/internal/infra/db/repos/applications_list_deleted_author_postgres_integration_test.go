package repos

import (
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
)

// An agent whose author's account was deleted keeps its author on the list row
// (#6702). The list joined the user table and took the author id from that
// join, so a deleted account dropped the author entirely: the list card showed
// no author, never "Deleted user". The id now comes from the version, and the
// row answers the same empty name and email the detail read answers.
func TestApplicationsRepoPostgres_ListKeepsAnAuthorWhoseAccountWasDeleted(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 31, "gone@elitea.ai")
	seedUser(t, pool, 32, "kept@elitea.ai")

	gone := createTestApplication(t, repo, "authored-by-a-deleted-user", 31, &applications.Version{Name: "base"})
	kept := createTestApplication(t, repo, "authored-by-a-live-user", 32, &applications.Version{Name: "base"})
	if _, err := pool.Exec(ctx, `DELETE FROM public.auth_core__user WHERE id = 31`); err != nil {
		t.Fatalf("delete user 31: %v", err)
	}

	list, err := repo.List(ctx, applications.ListRequest{ProjectID: testProjectID, Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	authors := map[string][]applications.Author{}
	for _, row := range list.Rows {
		authors[row.ID] = row.Authors
	}

	if got := authors[gone.ID]; len(got) != 1 || got[0].ID != "31" || got[0].Name != "" || got[0].Email != "" {
		t.Errorf("deleted author = %+v, want one author with id 31 and an empty name and email", got)
	}
	if got := authors[kept.ID]; len(got) != 1 || got[0].ID != "32" || got[0].Email != "kept@elitea.ai" {
		t.Errorf("live author = %+v, want id 32 kept@elitea.ai", got)
	}
}
