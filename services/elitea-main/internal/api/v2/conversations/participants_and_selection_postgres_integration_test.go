package conversations_test

// Real-PostgreSQL coverage for two legacy-parity defects:
//
//   - F5: POST participants answered with the WHOLE conversation's
//     participant list. Legacy answers with the rows of the request only
//     (added or already present), and the web callers treat the answer as
//     "the added rows".
//   - #6674: select_conversation ran its DELETE and INSERT as two autocommit
//     statements. A conversation deleted while the select was in flight
//     failed the INSERT on the foreign key and answered 500.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

func TestAddParticipantsAnswersOnlyTheRequestedRows(t *testing.T) {
	pool := newChatAuthorityPool(t)
	router := chatAuthorityRouter(pool)
	repo := repos.NewConversationsRepo(pool)
	created, err := repo.Create(chatActor("7"), "1", conversations.Conversation{Name: "Participants parity"})
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/1/conversations/%s/participants", created.ID)

	existing, err := repo.ListParticipants(context.Background(), "1", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(existing) == 0 {
		t.Fatal("precondition: the new conversation has no participants, so a full-list answer would look added-only")
	}

	decode := func(body []byte) []conversations.Participant {
		t.Helper()
		var participants []conversations.Participant
		if err := json.Unmarshal(body, &participants); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		return participants
	}
	userID := func(participant conversations.Participant) string {
		return fmt.Sprint(participant.EntityMeta["id"])
	}

	first := decode(callChatAuthority(t, router, "7", http.MethodPost, path,
		`[{"entity_name":"user","entity_meta":{"id":8}}]`, http.StatusOK).Body.Bytes())
	if len(first) != 1 || first[0].EntityName != "user" || userID(first[0]) != "8" {
		t.Fatalf("first add answered %+v, want only user 8", first)
	}

	// An already-present entity answers with its existing row; a new one is
	// created; the order follows the request; the author is not repeated.
	second := decode(callChatAuthority(t, router, "7", http.MethodPost, path,
		`[{"entity_name":"user","entity_meta":{"id":9}},{"entity_name":"user","entity_meta":{"id":8}}]`,
		http.StatusOK).Body.Bytes())
	if len(second) != 2 || userID(second[0]) != "9" || userID(second[1]) != "8" {
		t.Fatalf("second add answered %+v, want users 9 then 8", second)
	}
	if second[1].ID != first[0].ID {
		t.Fatalf("re-added user 8 got participant id %d, want the existing %d", second[1].ID, first[0].ID)
	}

	all, err := repo.ListParticipants(context.Background(), "1", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(existing)+2 {
		t.Fatalf("stored participants = %d, want %d", len(all), len(existing)+2)
	}
}

func TestSelectConversationIsAtomicAndAnswers404ForADeletedConversation(t *testing.T) {
	pool := newChatAuthorityPool(t)
	ctx := context.Background()
	// The tenant migration (0126) declares this foreign key; the hand-made
	// fixture table does not, and without it the race cannot fail.
	if _, err := pool.Exec(ctx, `ALTER TABLE p_1.chat_selected_conversations
		ADD FOREIGN KEY (conversation_id) REFERENCES p_1.chat_conversations(id) ON DELETE CASCADE`); err != nil {
		t.Fatal(err)
	}
	repo := repos.NewConversationsRepo(pool)
	keep, err := repo.Create(chatActor("7"), "1", conversations.Conversation{Name: "Keep"})
	if err != nil {
		t.Fatal(err)
	}
	doomed, err := repo.Create(chatActor("7"), "1", conversations.Conversation{Name: "Doomed"})
	if err != nil {
		t.Fatal(err)
	}

	countSelections := func() int {
		t.Helper()
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM p_1.chat_selected_conversations WHERE user_id=7`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}

	if err := repo.SelectConversation(ctx, "1", keep.ID, "7"); err != nil {
		t.Fatalf("select an existing conversation: %v", err)
	}
	if got := countSelections(); got != 1 {
		t.Fatalf("selection rows = %d, want 1", got)
	}

	// Hold an uncommitted delete of the doomed conversation. The select
	// resolves the id from the old snapshot, then its FK check blocks on the
	// row lock. The delete commits, and the FK check fails with 23503.
	deleter, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleter.Rollback(ctx) }()
	if _, err := deleter.Exec(ctx, `DELETE FROM p_1.chat_participant_mapping WHERE conversation_id=$1`, doomed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := deleter.Exec(ctx, `DELETE FROM p_1.chat_conversations WHERE id=$1`, doomed.ID); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- repo.SelectConversation(ctx, "1", doomed.ID, "7") }()
	time.Sleep(500 * time.Millisecond)
	if err := deleter.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var selectErr error
	select {
	case selectErr = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("select did not finish after the delete committed")
	}
	var apiErr *apierr.APIError
	if !errors.As(selectErr, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("select of a conversation deleted in flight = %v, want a typed 404", selectErr)
	}
	// The failed select rolled back its DELETE too: the earlier selection stays.
	if got := countSelections(); got != 1 {
		t.Fatalf("selection rows after the refused select = %d, want the earlier 1", got)
	}

	// A conversation that is already gone answers 404 before any write.
	if err := repo.SelectConversation(ctx, "1", doomed.ID, "7"); !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("select of a deleted conversation = %v, want a typed 404", err)
	}
}

// #6674, second half: one transaction alone let two concurrent selects both
// insert, because under READ COMMITTED the second DELETE cannot see the row
// the first INSERT committed. The per-user advisory lock keeps one row.
func TestConcurrentSelectsLeaveOneSelectionRow(t *testing.T) {
	pool := newChatAuthorityPool(t)
	ctx := context.Background()
	repo := repos.NewConversationsRepo(pool)
	first, err := repo.Create(chatActor("7"), "1", conversations.Conversation{Name: "First"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := repo.Create(chatActor("7"), "1", conversations.Conversation{Name: "Second"})
	if err != nil {
		t.Fatal(err)
	}
	countSelections := func() int {
		t.Helper()
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM p_1.chat_selected_conversations WHERE user_id=7`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	waitBoth := func(errs <-chan error, what string) {
		t.Helper()
		for range 2 {
			select {
			case err := <-errs:
				if err != nil {
					t.Fatalf("%s: %v", what, err)
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("%s did not finish", what)
			}
		}
	}
	selectBoth := func() <-chan error {
		errs := make(chan error, 2)
		go func() { errs <- repo.SelectConversation(ctx, "1", first.ID, "7") }()
		go func() { errs <- repo.SelectConversation(ctx, "1", second.ID, "7") }()
		return errs
	}

	// Deterministic case. An outside transaction holds the row lock on the
	// existing selection, so both selects reach their DELETE before either
	// can insert. Without the advisory lock both DELETEs then skip the gone
	// row and both INSERTs land.
	if err := repo.SelectConversation(ctx, "1", first.ID, "7"); err != nil {
		t.Fatal(err)
	}
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `DELETE FROM p_1.chat_selected_conversations WHERE user_id=7`); err != nil {
		t.Fatal(err)
	}
	blocked := selectBoth()
	time.Sleep(500 * time.Millisecond)
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	waitBoth(blocked, "select behind a held row lock")
	if got := countSelections(); got != 1 {
		t.Fatalf("selection rows after two blocked selects = %d, want 1", got)
	}

	// No prior row: neither DELETE locks anything. Repeat to give the race
	// room, with a deselect between rounds.
	for round := range 20 {
		if err := repo.DeselectConversation(ctx, "1", "7"); err != nil {
			t.Fatal(err)
		}
		waitBoth(selectBoth(), "concurrent select")
		if got := countSelections(); got != 1 {
			t.Fatalf("round %d: selection rows = %d, want 1", round, got)
		}
	}
}

// Rows that an older race duplicated must resolve to the newest selection,
// not to whichever row a heap scan meets first.
func TestSidebarSelectionPrefersTheNewestDuplicateRow(t *testing.T) {
	pool := newChatAuthorityPool(t)
	ctx := context.Background()
	router := chatAuthorityRouter(pool)
	repo := repos.NewConversationsRepo(pool)
	older, err := repo.Create(chatActor("7"), "1", conversations.Conversation{Name: "Older"})
	if err != nil {
		t.Fatal(err)
	}
	newer, err := repo.Create(chatActor("7"), "1", conversations.Conversation{Name: "Newer"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{older.ID, newer.ID} {
		if _, err := pool.Exec(ctx, `INSERT INTO p_1.chat_selected_conversations(conversation_id,user_id) VALUES ($1,7)`, id); err != nil {
			t.Fatal(err)
		}
	}
	var sidebar map[string]any
	if err := json.Unmarshal(callChatAuthority(t, router, "7", http.MethodGet, "/1/folders?grouped=true", "", http.StatusOK).Body.Bytes(), &sidebar); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(sidebar["selected_conversation_id"]); got != newer.ID {
		t.Fatalf("selected_conversation_id = %s, want the newest row's %s", got, newer.ID)
	}
}
