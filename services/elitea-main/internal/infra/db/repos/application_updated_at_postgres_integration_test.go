package repos

// `updated_at` on an agent must be the moment the agent last changed.
//
// It was the Go zero time on every agent in every deployment. The domain
// struct declares the field, `omitempty` is inert on a time.Time, and no
// column existed to scan — so `GET /elitea_core/applications/prompt_lib/1`
// answered `"updated_at": "0001-01-01T00:00:00Z"` for every row, and any
// client sorting or showing "last modified" was reading a constant. The spec
// documented that as a known gap, which made it permanent rather than fixed.
//
// tenant/0134 adds the column (and `create_tenant_schema` declares it for a
// fresh install); the writers stamp it inside the statement that makes the
// change, so a save cannot land with the timestamp unmoved. This file is the
// proof of the second half: each of the four writes a user can make moves it.
//
// The reads matter as much as the writes: `List` builds its own SELECT and
// `Get`/`Create`/`Update` share `applicationColumns`, so a column that is
// stamped and never selected is the same lie in a different place.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
)

// listedUpdatedAt is the agent's `updated_at` as the AGENTS LIST answers it —
// the projection every client reads, and a different query from Get's.
func listedUpdatedAt(t *testing.T, repo *ApplicationsRepo, applicationID string) time.Time {
	t.Helper()
	page, err := repo.List(testContext(t), applications.ListRequest{
		ProjectID: testProjectID, Page: 1, PageSize: 50,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, row := range page.Rows {
		if row.ID == applicationID {
			return row.UpdatedAt
		}
	}
	t.Fatalf("application %s is not in the list", applicationID)
	return time.Time{}
}

func TestApplicationsRepoPostgres_UpdatedAtMovesWithEveryWrite(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "one@elitea.ai")

	app := createTestApplication(t, repo, "autotest_updated_at", 1,
		&applications.Version{Name: "base", AuthorID: 1, AgentType: "openai"})

	// A brand-new agent reports WHEN IT WAS MADE, not the zero time. This is
	// the assertion the whole defect hid behind: the value was constant, so
	// nothing downstream of it could ever have been right.
	if app.UpdatedAt.IsZero() {
		t.Fatal("a created agent reports the zero time as updated_at")
	}
	if app.UpdatedAt.Before(app.CreatedAt) {
		t.Errorf("updated_at %s is before created_at %s", app.UpdatedAt, app.CreatedAt)
	}
	if listed := listedUpdatedAt(t, repo, app.ID); listed.IsZero() {
		t.Error("the agents LIST reports the zero time as updated_at — the column is stamped and unread")
	}

	previous := app.UpdatedAt
	moved := func(t *testing.T, what string, now time.Time) {
		t.Helper()
		if !now.After(previous) {
			t.Errorf("%s left updated_at at %s", what, now)
		}
		previous = now
	}

	// 1. The agent itself: a rename.
	renamed := "autotest_updated_at_renamed"
	updated, err := repo.Update(ctx, applications.UpdateRequest{
		ProjectID: testProjectID, ApplicationID: app.ID, Name: &renamed,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	moved(t, "a rename", updated.UpdatedAt)

	// 2. A version save — the ordinary editor save, and the one a user would
	//    describe as "saving the agent".
	instructions := "The revised brief."
	if _, err := repo.UpdateVersion(ctx, testProjectID, app.ID, app.Versions[0].ID,
		applications.Version{Instructions: instructions}); err != nil {
		t.Fatalf("update version: %v", err)
	}
	moved(t, "a version save", listedUpdatedAt(t, repo, app.ID))

	// 3. A new version.
	second, err := repo.CreateVersion(ctx, testProjectID, app.ID,
		applications.Version{Name: "autotest_v2", AuthorID: 1, AgentType: "openai"})
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	moved(t, "a new version", listedUpdatedAt(t, repo, app.ID))

	// 4. …and removing it again.
	if err := repo.DeleteVersion(ctx, testProjectID, app.ID, second.ID); err != nil {
		t.Fatalf("delete version: %v", err)
	}
	moved(t, "deleting a version", listedUpdatedAt(t, repo, app.ID))

	// The read the editor uses answers the same value as the list.
	read, err := repo.Get(ctx, testProjectID, app.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !read.UpdatedAt.Equal(previous) {
		t.Errorf("the agent read answers updated_at %s and the list answers %s",
			read.UpdatedAt, previous)
	}
}

// A request that changes nothing must not report a change: `Update` returns
// early when no column is set, and the stamp is appended after that return.
func TestApplicationsRepoPostgres_AnUpdateThatSetsNothingLeavesUpdatedAtAlone(t *testing.T) {
	repo, pool := newApplicationsTestRepo(t)
	ctx := testContext(t)
	seedUser(t, pool, 1, "one@elitea.ai")

	app := createTestApplication(t, repo, "autotest_untouched", 1,
		&applications.Version{Name: "base", AuthorID: 1, AgentType: "openai"})

	unchanged, err := repo.Update(ctx, applications.UpdateRequest{
		ProjectID: testProjectID, ApplicationID: app.ID,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !unchanged.UpdatedAt.Equal(app.UpdatedAt) {
		t.Errorf("an empty update moved updated_at from %s to %s",
			app.UpdatedAt, unchanged.UpdatedAt)
	}
}
