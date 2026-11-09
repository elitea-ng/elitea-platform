package repos

// The application list over a project schema the legacy runtime built and the
// production upgrade path then migrated.
//
// Start: p_1 has no social_likes table (internal/infra/db/dbtest/legacyshape).
// The database is upgraded as Main starts up: Bootstrap, shared history, tenant
// history for project 1 (tenant migration 0164 creates the table).
//
// Asserts: the "liked by me" filter and the like-count sort both run, and the
// filter returns the one application the actor liked.

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/applications"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/dbtest/legacyupgraded"
)

func TestAnUpgradedLegacyBuiltProjectListsApplicationsLikedByTheActor(t *testing.T) {
	pool := legacyupgraded.NewPool(t)
	repo := NewApplicationsRepo(pool)
	ctx := testContext(t)
	// The list joins the user table for author names; seed the author as the
	// other repository tests do.
	seedUser(t, pool, 1, "first@example.com")
	liked := createTestApplication(t, repo, "Liked", 1, &applications.Version{Name: "base", AgentType: "openai"})
	createTestApplication(t, repo, "Not liked", 1, &applications.Version{Name: "base", AgentType: "openai"})
	if _, err := pool.Exec(ctx, `
		INSERT INTO p_1.social_likes (entity_name, entity_id, user_id, created_at)
		VALUES ('application', $1, 1, now() AT TIME ZONE 'UTC')`, liked.ID); err != nil {
		t.Fatalf("insert a like into the upgraded schema: %v", err)
	}

	mine, err := repo.List(ctx, applications.ListRequest{ProjectID: "1", MyLiked: true, SortBy: "name", SortOrder: "asc"})
	if err != nil {
		t.Fatalf("list with the liked-by-me filter: %v", err)
	}
	if mine.Total != 1 || len(mine.Rows) != 1 || mine.Rows[0].ID != liked.ID {
		t.Fatalf("liked-by-me list = total %d, rows %+v; want only %q", mine.Total, mine.Rows, "Liked")
	}

	byLikes, err := repo.List(ctx, applications.ListRequest{ProjectID: "1", SortBy: "likes", SortOrder: "desc"})
	if err != nil {
		t.Fatalf("list sorted by like count: %v", err)
	}
	if byLikes.Total != 2 || byLikes.Rows[0].ID != liked.ID {
		t.Fatalf("like-count sort = total %d, first %q; want the liked application first", byLikes.Total, byLikes.Rows[0].ID)
	}
}
