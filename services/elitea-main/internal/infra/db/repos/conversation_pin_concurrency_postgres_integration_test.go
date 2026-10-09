package repos

// Concurrent conversation pins, unpins and deletes on one conversation.
//
// Pin, Unpin and Delete each touch two rows in one transaction: the
// conversation (Pin and Unpin stamp its sync_at, Delete removes it) and its
// shared centry.social_pins row. They used to take those row locks in
// different orders, so two of them on the same conversation could deadlock
// and PostgreSQL aborted one with SQLSTATE 40P01, which the routes answered
// with HTTP 500. These tests hold every writer to the order the code now
// enforces — the conversation row first, then the pin row — and check that
// the outcome stays exact: one shared row, one stamp per state change, 404
// for a conversation that is gone.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

const (
	// pinRaceWorkers stays at the test pool's MaxConns (12), so every
	// worker holds a connection at once and the transactions overlap.
	pinRaceWorkers = 12
	pinRaceRounds  = 8
)

// countPinStamps installs a test-only trigger that counts every change of
// the conversation's sync_at, i.e. every pin or unpin stamp.
func countPinStamps(t *testing.T, pool *pgxpool.Pool) func() int {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TABLE public.test_pin_stamps (n bigserial PRIMARY KEY)`,
		`CREATE FUNCTION public.test_count_pin_stamp() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		   IF NEW.sync_at IS DISTINCT FROM OLD.sync_at THEN
		     INSERT INTO public.test_pin_stamps DEFAULT VALUES;
		   END IF;
		   RETURN NEW;
		 END $$`,
		`CREATE TRIGGER test_count_pin_stamp AFTER UPDATE ON p_1.chat_conversations
		 FOR EACH ROW EXECUTE FUNCTION public.test_count_pin_stamp()`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("install stamp counter: %v", err)
		}
	}
	return func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM public.test_pin_stamps`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
}

func conversationPinRows(t *testing.T, pool *pgxpool.Pool, id int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM centry.social_pins
		WHERE entity = 'conversation' AND project_id = 1 AND entity_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// raceConversationPins starts every call together and returns their errors.
func raceConversationPins(pins *CurrentSocialPinsRepository, numericID string, unpin func(worker int) bool) []error {
	start := make(chan struct{})
	errs := make([]error, pinRaceWorkers)
	var wg sync.WaitGroup
	for worker := range pinRaceWorkers {
		wg.Go(func() {
			<-start
			if unpin(worker) {
				errs[worker] = pins.Unpin(asUser("7"), "1", "conversation", numericID)
			} else {
				errs[worker] = pins.Pin(asUser("7"), "1", "conversation", numericID)
			}
		})
	}
	close(start)
	wg.Wait()
	return errs
}

func TestConcurrentConversationPinsAndUnpinsNeverDeadlock(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	pins := NewCurrentSocialPinsRepository(pool)
	numericID, _, _ := seedConversationWithParticipant(t, repo, "contended")
	id, _ := strconv.ParseInt(numericID, 10, 64)
	stamps := countPinStamps(t, pool)

	for round := range pinRaceRounds {
		// Every caller pins an unpinned conversation: one creates the shared
		// row and stamps it, the rest only rewrite the last pinner.
		before := stamps()
		for worker, err := range raceConversationPins(pins, numericID, func(int) bool { return false }) {
			if err != nil {
				t.Fatalf("round %d: concurrent Pin %d: %v", round, worker, err)
			}
		}
		if rows := conversationPinRows(t, pool, id); rows != 1 {
			t.Fatalf("round %d: %d shared pin rows after concurrent pins, want 1", round, rows)
		}
		if got := stamps() - before; got != 1 {
			t.Fatalf("round %d: concurrent pins stamped the conversation %d times, want once", round, got)
		}

		// Every caller unpins it: one removes the row and stamps, the rest
		// find nothing to remove.
		before = stamps()
		for worker, err := range raceConversationPins(pins, numericID, func(int) bool { return true }) {
			if err != nil {
				t.Fatalf("round %d: concurrent Unpin %d: %v", round, worker, err)
			}
		}
		if rows := conversationPinRows(t, pool, id); rows != 0 {
			t.Fatalf("round %d: %d shared pin rows after concurrent unpins, want 0", round, rows)
		}
		if got := stamps() - before; got != 1 {
			t.Fatalf("round %d: concurrent unpins stamped the conversation %d times, want once", round, got)
		}

		// Pins and unpins interleave. Which one wins is a race; that every
		// call answers, and at most one shared row is left, is not.
		for worker, err := range raceConversationPins(pins, numericID, func(worker int) bool { return worker%2 == 1 }) {
			if err != nil {
				t.Fatalf("round %d: interleaved call %d: %v", round, worker, err)
			}
		}
		if rows := conversationPinRows(t, pool, id); rows > 1 {
			t.Fatalf("round %d: %d shared pin rows after interleaved calls", round, rows)
		}
		if err := pins.Unpin(asUser("7"), "1", "conversation", numericID); err != nil {
			t.Fatalf("round %d: reset Unpin: %v", round, err)
		}
	}
}

// TestAnUnpinRacingTheConversationDeleteIsNotADeadlock forces the
// interleaving that deadlocked when Unpin took the pin row before the
// conversation row. The test's transaction deletes the conversation as
// Delete does and holds it until Unpin is blocked, then removes the pin and
// commits. Unpin must wait for the conversation row and answer 404, and
// neither side may be aborted.
func TestAnUnpinRacingTheConversationDeleteIsNotADeadlock(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewConversationsRepo(pool)
	pins := NewCurrentSocialPinsRepository(pool)
	numericID, _, _ := seedConversationWithParticipant(t, repo, "unpin racing delete")
	id, _ := strconv.ParseInt(numericID, 10, 64)
	ctx := context.Background()
	if err := pins.Pin(asUser("7"), "1", "conversation", numericID); err != nil {
		t.Fatal(err)
	}

	deleting, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleting.Rollback(ctx) }()
	var deleterPID int
	if err := deleting.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&deleterPID); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`SELECT set_config('elitea.sync_cascade', 'conversation', true)`,
		`DELETE FROM p_1.chat_participant_mapping WHERE conversation_id = $1`,
		`DELETE FROM p_1.chat_message_items WHERE message_group_id IN (SELECT id FROM p_1.chat_message_group WHERE conversation_id = $1)`,
		`DELETE FROM p_1.chat_message_group WHERE conversation_id = $1`,
		`DELETE FROM p_1.chat_selected_conversations WHERE conversation_id = $1`,
		`DELETE FROM p_1.chat_conversations WHERE id = $1`,
	} {
		args := []any{id}
		if !strings.Contains(statement, "$1") {
			args = nil
		}
		if _, err := deleting.Exec(ctx, statement, args...); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	unpinned := make(chan error, 1)
	go func() { unpinned <- pins.Unpin(asUser("7"), "1", "conversation", numericID) }()

	deadline := time.Now().Add(4 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> $1`, deleterPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case err := <-unpinned:
			t.Fatalf("Unpin finished (%v) before the delete committed; the race was not forced", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("Unpin never blocked on the deleting transaction")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := deleteConversationPin(ctx, deleting, "1", id); err != nil {
		t.Fatalf("Delete's pin removal behind a waiting Unpin: %v", err)
	}
	if err := deleting.Commit(ctx); err != nil {
		t.Fatalf("Delete commit behind a waiting Unpin: %v", err)
	}
	unpinErr := <-unpinned
	var refusal *apierr.APIError
	if !errors.As(unpinErr, &refusal) || refusal.Status != http.StatusNotFound {
		t.Fatalf("Unpin on a conversation deleted under it = %v, want 404", unpinErr)
	}
	if rows := conversationPinRows(t, pool, id); rows != 0 {
		t.Fatalf("%d pin rows outlived the deleted conversation", rows)
	}
}
