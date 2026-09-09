package repos

import (
	"context"
	"errors"
	"testing"
	"time"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenant"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// THE START THAT ARRIVES ONE INSTANT BEFORE THE PREVIOUS RESPONSE SETTLES.
//
// The overlap gate — `NOT EXISTS (… pending_response.is_streaming …)` in
// ResolveCurrentApplicationTurn, ResolveCurrentAdhocTurn and
// InsertCurrentApplicationTurn — refuses while the conversation still holds a
// response marked as being written. That is correct, and it is why
// resolveAfterCurrentResponseSettles exists: the browser re-enables the
// composer on `pipeline_finish`, hundreds of milliseconds before the worker's
// terminal frame is projected and `is_streaming` is cleared, so a send the
// product just invited must WAIT for that window to close rather than be
// refused inside it.
//
// The wait had a hole, and this test is that hole. A resolve reads a snapshot
// taken when its statement starts; the settle probe that follows it is a
// SEPARATE transaction with a LATER snapshot. When the terminal projection
// commits between the two — which is exactly what happens after a HITL resume,
// because the resumed run's `pipeline_finish` and its terminal write are
// milliseconds apart rather than half a second — the probe correctly reports
// "settled" and the wait then returned the refusal the earlier snapshot had
// produced, WITHOUT ever re-reading. The settle it was waiting for was the one
// observation it threw away.
//
// Measured on the standalone stack before the fix (conversation 329,
// 2026-08-29): the resumed response's terminal write stamped 23:27:44.316, the
// 422 was logged at 23:27:44.328 — 12ms later, far too soon for the 3s budget
// to have expired, and the same resolve replayed by hand afterwards returned a
// row. Every ask_user / sensitive-tool conversation died on its NEXT send.
//
// WHAT MAKES THIS TEST DISCRIMINATING. It does not seed "settled" and assert
// the query resolves — that passes with or without the fix, because the SQL was
// never wrong (see TestPostgresCurrentApplicationTurnResolvesAfterAHITLResume
// below, which pins exactly that). It reproduces the ORDER: the first resolve
// runs against a streaming response and is refused, the terminal write lands
// immediately after it, and the probe — the real
// `CurrentConversationResponseSettling` query against a real conversation —
// then reports the conversation settled. Only a wait that re-resolves on that
// report can answer the turn.
func TestPostgresCurrentStartReResolvesWhenTheResponseSettlesUnderIt(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	conversationUUID := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031")
	responseID := commitPostgresCurrentStreamingTurn(
		t,
		pool,
		conversationUUID,
		"20000000-0000-4000-8000-000000000031",
		"30000000-0000-4000-8000-000000000031",
		"40000000-0000-4000-8000-000000000031",
		"first turn",
		"execution-settle-race",
	)

	repository, err := NewCurrentAgentStartRepository(pool, 1)
	if err != nil {
		t.Fatal(err)
	}

	resolveParams := sqlcgen.ResolveCurrentApplicationTurnParams{
		ActorUserID: 11, TargetParticipantID: 21,
		QuestionID:       mustCurrentPGUUID(t, "50000000-0000-4000-8000-000000000031"),
		ConversationUuid: conversationUUID, ProjectID: 1,
	}

	attempts := 0
	settled := false
	outcome := repository.resolveAfterCurrentResponseSettles(
		t.Context(),
		1,
		conversationUUID,
		func() error {
			attempts++
			resolveErr := resolvePostgresCurrentApplicationTurn(t, pool, resolveParams)
			if !settled {
				// The worker's terminal frame lands HERE: after this resolve's
				// snapshot, before the probe that follows it. Clearing
				// `is_streaming` is all FinalizeCurrentAgentFullMessage does to
				// the state this gate reads.
				settlePostgresCurrentResponse(t, pool, responseID)
				settled = true
			}
			return resolveErr
		},
	)
	if outcome != nil {
		t.Fatalf(
			"the start was refused although the response settled while it was being resolved: %v (attempts=%d)",
			outcome, attempts,
		)
	}
	if attempts < 2 {
		t.Fatalf(
			"the wait answered from the pre-settle snapshot: it must re-resolve once the probe reports the response settled (attempts=%d)",
			attempts,
		)
	}
}

// The counter-assertion, and the reason the fix above is a re-read rather than
// an admission: a response that is GENUINELY still being written must still be
// refused when the budget runs out. Without this, "re-resolve when the probe
// says settled" could be widened into "admit anything that waited", and the
// overlap gate — whose whole job is to stop two turns writing the same
// conversation at once — would stop gating.
func TestPostgresCurrentStartStillRefusesAResponseThatNeverSettles(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	conversationUUID := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031")
	commitPostgresCurrentStreamingTurn(
		t,
		pool,
		conversationUUID,
		"20000000-0000-4000-8000-000000000031",
		"30000000-0000-4000-8000-000000000031",
		"40000000-0000-4000-8000-000000000031",
		"first turn",
		"execution-never-settles",
	)

	repository, err := NewCurrentAgentStartRepository(pool, 1)
	if err != nil {
		t.Fatal(err)
	}
	resolveParams := sqlcgen.ResolveCurrentApplicationTurnParams{
		ActorUserID: 11, TargetParticipantID: 21,
		QuestionID:       mustCurrentPGUUID(t, "50000000-0000-4000-8000-000000000031"),
		ConversationUuid: conversationUUID, ProjectID: 1,
	}

	started := time.Now()
	outcome := repository.resolveAfterCurrentResponseSettles(
		t.Context(),
		1,
		conversationUUID,
		func() error { return resolvePostgresCurrentApplicationTurn(t, pool, resolveParams) },
	)
	if !errors.Is(outcome, agentexecutionapp.ErrUnsupportedCurrentAgentStart) {
		t.Fatalf("a response still being written must refuse the next start: %v", outcome)
	}
	if waited := time.Since(started); waited < currentAgentSettleBudget {
		t.Fatalf("the wait gave up after %s, before its %s budget", waited, currentAgentSettleBudget)
	}
}

// A conversation whose previous turn went through a HITL resume must resolve
// its next turn, and the RESOLVED interrupt must not be read as an outstanding
// one.
//
// `ResumeCurrentAgentHITL` moves the answered interrupt ids into
// `meta.resolved_hitl_interrupt_ids` and drops `hitl_interrupt` /
// `hitl_interrupts`; `FinalizeCurrentAgentFullMessage` then clears
// `is_streaming` and leaves the resolved ids in place. So the terminal state of
// a resumed turn differs from an ordinary one by exactly that key — which no
// clause in either resolver reads today.
//
// This test states that as a property rather than leaving it implied. It passes
// against the unfixed code (the SQL was never the defect; see the race above)
// and exists so that a future clause added for outstanding interrupts cannot
// silently start refusing conversations whose interrupts were ANSWERED, which
// is what the 422 above looked like from the outside.
func TestPostgresCurrentApplicationTurnResolvesAfterAHITLResume(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)

	conversationUUID := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000031")
	responseID := commitPostgresCurrentStreamingTurn(
		t,
		pool,
		conversationUUID,
		"20000000-0000-4000-8000-000000000031",
		"30000000-0000-4000-8000-000000000031",
		"40000000-0000-4000-8000-000000000031",
		"pick the environment",
		"execution-hitl-resumed",
	)
	// The terminal state a resumed turn leaves: answered, settled, and still
	// carrying the ids of the interrupts the user decided.
	if _, err := pool.Exec(t.Context(), `
WITH response_group AS (
    UPDATE p_1.chat_message_group
    SET is_streaming = FALSE,
        meta = (meta - 'hitl_interrupt' - 'hitl_interrupts')
            || jsonb_build_object(
                'thread_id', '10000000-0000-4000-8000-000000000031',
                'references', '[]'::jsonb,
                'is_error', FALSE,
                'error', '',
                'invoked_skills', '[]'::jsonb,
                'resolved_hitl_interrupt_ids', jsonb_build_array('hitl_e1:answered')
            )
    WHERE uuid = $1
    RETURNING id
), response_item AS (
    INSERT INTO p_1.chat_message_items (uuid, item_type, order_index, meta, message_group_id)
    SELECT gen_random_uuid(), 'text_message', 0, '{}'::jsonb, response_group.id
    FROM response_group
    RETURNING id
)
INSERT INTO p_1.chat_messages_text (id, content)
SELECT response_item.id, 'MOCK: resumed "User answered: Staging"'
FROM response_item`, responseID); err != nil {
		t.Fatal(err)
	}

	if err := resolvePostgresCurrentApplicationTurn(t, pool, sqlcgen.ResolveCurrentApplicationTurnParams{
		ActorUserID: 11, TargetParticipantID: 21,
		QuestionID:       mustCurrentPGUUID(t, "50000000-0000-4000-8000-000000000031"),
		ConversationUuid: conversationUUID, ProjectID: 1,
	}); err != nil {
		t.Fatalf("the turn after an answered clarification was refused: %v", err)
	}

	// The ad-hoc twin carries its own copy of the same gates, so it is asserted
	// rather than assumed: conversation 2 in the seed is the `dummy`-participant
	// one, and a resumed ad-hoc turn reaches exactly the same clauses.
	adhocConversation := mustCurrentPGUUID(t, "10000000-0000-4000-8000-000000000032")
	adhocResponse := commitPostgresCurrentStreamingAdhocTurn(
		t,
		pool,
		adhocConversation,
		"21000000-0000-4000-8000-000000000032",
		"31000000-0000-4000-8000-000000000032",
		"41000000-0000-4000-8000-000000000032",
		"pick the environment",
		"execution-hitl-resumed-adhoc",
	)
	if _, err := pool.Exec(t.Context(), `
WITH response_group AS (
    UPDATE p_1.chat_message_group
    SET is_streaming = FALSE,
        meta = (meta - 'hitl_interrupt' - 'hitl_interrupts')
            || jsonb_build_object(
                'is_error', FALSE,
                'resolved_hitl_interrupt_ids', jsonb_build_array('hitl_e1:answered')
            )
    WHERE uuid = $1
    RETURNING id
), response_item AS (
    INSERT INTO p_1.chat_message_items (uuid, item_type, order_index, meta, message_group_id)
    SELECT gen_random_uuid(), 'text_message', 0, '{}'::jsonb, response_group.id
    FROM response_group
    RETURNING id
)
INSERT INTO p_1.chat_messages_text (id, content)
SELECT response_item.id, 'MOCK: resumed "User answered: Staging"'
FROM response_item`, adhocResponse); err != nil {
		t.Fatal(err)
	}
	if err := withPostgresCurrentProjectTx(t, pool, func(tx pgx.Tx) error {
		_, resolveErr := sqlcgen.New(tx).ResolveCurrentAdhocTurn(
			t.Context(),
			sqlcgen.ResolveCurrentAdhocTurnParams{
				ActorUserID: 11, TargetParticipantID: 23,
				QuestionID:       mustCurrentPGUUID(t, "51000000-0000-4000-8000-000000000032"),
				ConversationUuid: adhocConversation, ProjectID: 1,
			},
		)
		return resolveErr
	}); err != nil {
		t.Fatalf("the ad-hoc turn after an answered clarification was refused: %v", err)
	}
}

// withPostgresCurrentProjectTx runs one COMMITTED project-bound transaction on
// the pool. Committed rather than rolled back, because these tests hand the
// pool to a repository that opens its own connections: a rollback-scoped seed
// is invisible to them.
func withPostgresCurrentProjectTx(t *testing.T, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	t.Helper()
	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	committed = true
	return nil
}

// commitPostgresCurrentStreamingTurn writes one application turn and leaves its
// response exactly as InsertCurrentApplicationTurn leaves it: streaming, with
// no terminal projection yet.
func commitPostgresCurrentStreamingTurn(
	t *testing.T,
	pool *pgxpool.Pool,
	conversationUUID pgtype.UUID,
	questionIDRaw, questionItemIDRaw, responseIDRaw, input, executionID string,
) pgtype.UUID {
	t.Helper()
	var responseID pgtype.UUID
	if err := withPostgresCurrentProjectTx(t, pool, func(tx pgx.Tx) error {
		responseID = insertPostgresCurrentApplicationTurn(
			t, sqlcgen.New(tx), conversationUUID,
			questionIDRaw, questionItemIDRaw, responseIDRaw, input, executionID,
		)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return responseID
}

func commitPostgresCurrentStreamingAdhocTurn(
	t *testing.T,
	pool *pgxpool.Pool,
	conversationUUID pgtype.UUID,
	questionIDRaw, questionItemIDRaw, responseIDRaw, input, executionID string,
) pgtype.UUID {
	t.Helper()
	var responseID pgtype.UUID
	if err := withPostgresCurrentProjectTx(t, pool, func(tx pgx.Tx) error {
		responseID = insertPostgresCurrentAdhocTurn(
			t, sqlcgen.New(tx), conversationUUID,
			questionIDRaw, questionItemIDRaw, responseIDRaw, input, executionID,
		)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return responseID
}

// settlePostgresCurrentResponse is what FinalizeCurrentAgentFullMessage does to
// the one column the overlap gate reads, committed on its own connection so the
// change is visible to the probe's separate transaction.
func settlePostgresCurrentResponse(t *testing.T, pool *pgxpool.Pool, responseID pgtype.UUID) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
UPDATE p_1.chat_message_group
SET is_streaming = FALSE,
    updated_at = clock_timestamp()
WHERE uuid = $1`, responseID); err != nil {
		t.Fatal(err)
	}
}

// resolvePostgresCurrentApplicationTurn runs the REAL resolver and maps its
// no-rows answer the way ResolveCurrentApplication does, so the closure these
// tests hand the wait behaves like the production one.
func resolvePostgresCurrentApplicationTurn(
	t *testing.T,
	pool *pgxpool.Pool,
	params sqlcgen.ResolveCurrentApplicationTurnParams,
) error {
	t.Helper()
	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tenant.BindProject(t.Context(), tx, tenant.Project{ID: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlcgen.New(tx).ResolveCurrentApplicationTurn(t.Context(), params); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return agentexecutionapp.ErrUnsupportedCurrentAgentStart
		}
		t.Fatal(err)
	}
	return nil
}
