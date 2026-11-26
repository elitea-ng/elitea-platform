package repos

// #3077: Enter pressed three times in a desktop new thread created three
// conversations. A create carrying the message's client_request_id makes one
// conversation per id per caller, also when the creates race.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

func TestCreateWithOneClientRequestIDMakesOneConversation(t *testing.T) {
	for _, key := range []string{"ELITEA_AI_PROJECT_ID", "AI_PROJECT_ID", "PUBLIC_PROJECT_ID", "SHARED_PROJECT_ID"} {
		t.Setenv(key, "2")
	}
	pool := newFreshInstallPool(t)
	ctx, cancel := context.WithTimeout(auth.ContextWithUser(context.Background(), auth.User{ID: "1"}), 30*time.Second)
	defer cancel()
	repo := NewConversationsRepo(pool)
	const id = "0b6f6d1e-4f2a-4c3b-9d1e-3077aaaaaaaa"

	// Three creates in the same instant, as three Enter presses sent them.
	var wg sync.WaitGroup
	results := make([]conversations.Conversation, 3)
	errs := make([]error, 3)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = repo.Create(ctx, "1", conversations.Conversation{Name: "fix the build", ClientRequestID: id, Source: "local_work"})
		}()
	}
	wg.Wait()
	replayed := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		if results[i].ID != results[0].ID || results[i].UUID != results[0].UUID {
			t.Fatalf("create %d answered conversation %s, create 0 answered %s", i, results[i].ID, results[0].ID)
		}
		if results[i].Replayed {
			replayed++
		}
	}
	if replayed != 2 {
		t.Fatalf("%d creates were replays, want 2", replayed)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM p_1.chat_conversations WHERE meta->>'client_request_id' = $1`, id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("%d conversations carry the id, want 1", count)
	}
	// The replay answers the participants the first create added (the user).
	if len(results[1].Participants) == 0 && len(results[2].Participants) == 0 {
		t.Fatal("a replay answered no participants")
	}

	// Another id, or none, creates another conversation.
	other, err := repo.Create(ctx, "1", conversations.Conversation{Name: "fix the build", ClientRequestID: "0b6f6d1e-4f2a-4c3b-9d1e-3077bbbbbbbb"})
	if err != nil || other.ID == results[0].ID || other.Replayed {
		t.Fatalf("another id: %v, %+v", err, other)
	}
	plain, err := repo.Create(ctx, "1", conversations.Conversation{Name: "fix the build"})
	if err != nil || plain.ID == results[0].ID || plain.Replayed {
		t.Fatalf("no id: %v, %+v", err, plain)
	}
}
