package repos

// FU-284: a run-to-completion chain waits while a fan-out (Parallel or Map) is
// parked on its cards, as it waits on a root HITL pause. The unattended hook
// runs against the real chain repository and PostgreSQL; only the starter and
// the goal check are recorded.
// Requires ELITEA_TEST_DATABASE_URL; every test skips without it.

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runbrief"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runfinished"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattendedchain"
)

// parkSettleViews adapts the repository to unattendedchain.Chains, as
// runtimecomposition does.
type parkSettleViews struct{ *RunChainsRepository }

func (c parkSettleViews) SettleView(ctx context.Context, executionID string) (unattendedchain.SettleView, bool, error) {
	view, found, err := c.ChainSettleView(ctx, executionID)
	if err != nil || !found {
		return unattendedchain.SettleView{}, found, err
	}
	return unattendedchain.SettleView{
		Chain: view.Chain, Latest: view.Latest, OutputLimitReached: view.OutputLimitReached,
		OutputLimitSequence: view.OutputLimitSequence, StepLimitReached: view.StepLimitReached,
		Paused: view.Paused, IsError: view.IsError,
	}, true, nil
}

// recordedStarts counts the continuations the hook starts.
type recordedStarts struct{ starts int }

func (s *recordedStarts) StartCurrentApplication(
	context.Context, agentexecution.CurrentApplicationStartRequest,
) (agentexecution.CurrentApplicationStartOutcome, error) {
	s.starts++
	return agentexecution.CurrentApplicationStartOutcome{}, nil
}

func (s *recordedStarts) StartCurrentAdhoc(
	context.Context, agentexecution.CurrentAdhocStartRequest,
) (agentexecution.CurrentApplicationStartOutcome, error) {
	s.starts++
	return agentexecution.CurrentApplicationStartOutcome{}, nil
}

func (s *recordedStarts) ContinueCurrentAgent(
	context.Context, agentexecution.CurrentContinuationRequest,
) (agentexecution.CurrentApplicationStartOutcome, error) {
	s.starts++
	return agentexecution.CurrentApplicationStartOutcome{}, nil
}

// recordedGoals counts the goal checks; a check answers "not checked".
type recordedGoals struct{ checks int }

func (g *recordedGoals) ContinueText(context.Context, string) string { return "Continue." }

func (g *recordedGoals) CheckCloud(context.Context, string, string) (runbrief.Result, bool, error) {
	g.checks++
	return runbrief.Result{}, false, nil
}

// parkChain starts a run-to-completion cloud chain whose root run answered
// into response, and writes that response with meta, owned by taskID.
func parkChain(t *testing.T, pool *pgxpool.Pool, root, response, taskID, meta string, streaming bool) {
	t.Helper()
	ctx := t.Context()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return startRunChain(ctx, tx, unattended.ChainStart{
			RootExecutionID: root, ProjectID: 1, OwnerUserID: 4711,
			ConversationUUID: "8a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d", Origin: unattended.OriginCloud,
			TriggerOrigin: "manual", ResponseMessageID: response,
			Grant: unattended.Grant{
				GrantID: uuid.NewString(), ContinueStepLimit: true, GoalCheck: true,
				Start: &unattended.CloudStart{Contract: agentexecution.CloudStartContractApplication, ParticipantID: 7},
			},
			Limits: unattended.Limits{MaxContinuations: 5, Deadline: time.Hour},
		})
	}); err != nil {
		t.Fatal(err)
	}
	writeResponse(t, pool, response, taskID, meta, streaming)
}

// writeResponse writes a chat response row (its conversation and author are
// not checked: replica role).
func writeResponse(t *testing.T, pool *pgxpool.Pool, response, taskID, meta string, streaming bool) {
	t.Helper()
	ctx := t.Context()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, meta, is_streaming, task_id, created_at)
VALUES ($1::uuid, 1, 1, $2::jsonb, $3, $4, now() AT TIME ZONE 'UTC')`, response, meta, streaming, taskID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// writeWake records a fan-out wake of response that continues parked as
// wake (foreign keys not checked: replica role).
func writeWake(t *testing.T, pool *pgxpool.Pool, response, parked, wake string) {
	t.Helper()
	ctx := t.Context()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.execution_interrupt_wakes (
    root_response_id, decision_revision, project_id, parked_execution_id, parked_generation,
    idempotency_key, state, wake_execution_id)
VALUES ($1::uuid, 1, 1, $2, 1, 'continue-interrupt/' || $1 || '/1', 'ADMITTED', $3)`, response, parked, wake)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// chainLinks counts a chain's links.
func chainLinks(t *testing.T, pool *pgxpool.Pool, root string) int {
	t.Helper()
	var links int
	if err := pool.QueryRow(t.Context(), `
SELECT count(*) FROM elitea_runtime.run_chain_links WHERE root_execution_id = $1`, root).Scan(&links); err != nil {
		t.Fatal(err)
	}
	return links
}

func newParkHook(pool *pgxpool.Pool) (*unattendedchain.Hook, *recordedStarts, *recordedGoals) {
	starts, goals := &recordedStarts{}, &recordedGoals{}
	hook := unattendedchain.New(parkSettleViews{NewRunChainsRepository(pool)}, starts, slog.Default()).WithGoals(goals)
	return hook, starts, goals
}

const parkedMeta = `{"thread_id": "t1", "fanout_parked_v1": {"schema_version": "elitea.fanout-parked.v1"}, "is_error": false}`

// TestPostgresChainWaitsOnAParkedFanout: a run that parked a fan-out on its
// cards ends the chain waiting: no goal check, no continuation, one link. The
// answered card's wake is not a link, so its settle leaves the chain as it is.
// On main the park read as finished, so the goal check ran beside the open
// cards (FU-284).
func TestPostgresChainWaitsOnAParkedFanout(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	root, wake := strings.Repeat("a1", 16), strings.Repeat("b1", 16)
	response := "9b0c1d2e-3f4a-4b5c-8d6e-7f8a9b0c1d21"
	parkChain(t, pool, root, response, root, parkedMeta, false)
	hook, starts, goals := newParkHook(pool)
	if err := hook.Settle(ctx, root, executionapp.SettlementSucceeded); err != nil {
		t.Fatal(err)
	}
	if _, state, _ := chainState(t, pool, root); state != unattended.EndWaiting {
		t.Fatalf("a parked fan-out ended the chain %q, want waiting", state)
	}
	if goals.checks != 0 || starts.starts != 0 || chainLinks(t, pool, root) != 1 {
		t.Fatalf("checks %d, starts %d, links %d while parked", goals.checks, starts.starts, chainLinks(t, pool, root))
	}
	// The answered card's wake runs and settles; the chain stays as it is.
	if err := hook.Settle(ctx, wake, executionapp.SettlementSucceeded); err != nil {
		t.Fatal(err)
	}
	if continuations, state, _ := chainState(t, pool, root); state != unattended.EndWaiting || continuations != 0 ||
		goals.checks != 0 || starts.starts != 0 || chainLinks(t, pool, root) != 1 {
		t.Fatalf("after the wake: state %q, continuations %d, checks %d, starts %d", state, continuations,
			goals.checks, starts.starts)
	}
}

// TestPostgresChainWaitsWhenTheParkWakeAlreadyTookTheResponse: a decision was
// waiting when the run parked, so its wake was queued in the settlement and
// already took the response (no park key, streaming, task_id = the wake)
// when the settle reads it. The chain still waits; no goal check runs beside
// the wake. On main this read as finished.
func TestPostgresChainWaitsWhenTheParkWakeAlreadyTookTheResponse(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	root, wake := strings.Repeat("a2", 16), strings.Repeat("b2", 16)
	response := "9b0c1d2e-3f4a-4b5c-8d6e-7f8a9b0c1d22"
	parkChain(t, pool, root, response, wake, `{"thread_id": "t1", "is_error": false}`, true)
	writeWake(t, pool, response, root, wake)
	hook, starts, goals := newParkHook(pool)
	if err := hook.Settle(ctx, root, executionapp.SettlementSucceeded); err != nil {
		t.Fatal(err)
	}
	if _, state, _ := chainState(t, pool, root); state != unattended.EndWaiting || goals.checks != 0 || starts.starts != 0 {
		t.Fatalf("state %q, checks %d, starts %d", state, goals.checks, starts.starts)
	}
}

// TestPostgresChainWaitsWhenTheParkWakeAlreadyFailed: the wake that took the
// response already finished with an error and a step limit before the parked
// run's settle read it (a late or replayed settle). Those are the wake's facts;
// the parked run still only parked, so the chain waits (it does not end failed
// or start a step-limit continuation).
func TestPostgresChainWaitsWhenTheParkWakeAlreadyFailed(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	root, wake := strings.Repeat("a6", 16), strings.Repeat("b6", 16)
	response := "9b0c1d2e-3f4a-4b5c-8d6e-7f8a9b0c1d26"
	parkChain(t, pool, root, response, wake,
		`{"thread_id": "t1", "is_error": true, "step_limit_reached": true}`, false)
	writeWake(t, pool, response, root, wake)
	hook, starts, goals := newParkHook(pool)
	if err := hook.Settle(ctx, root, executionapp.SettlementSucceeded); err != nil {
		t.Fatal(err)
	}
	if _, state, _ := chainState(t, pool, root); state != unattended.EndWaiting || goals.checks != 0 || starts.starts != 0 {
		t.Fatalf("state %q, checks %d, starts %d; want waiting with no check or continuation", state, goals.checks,
			starts.starts)
	}
}

// TestPostgresChainKeepsItsOutcomeWhenAnotherExecutionTookTheResponse: a
// response that a later execution took for another reason (no wake of this
// park) is not a wait: the chain finishes through its goal check, as before.
func TestPostgresChainKeepsItsOutcomeWhenAnotherExecutionTookTheResponse(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	root, other := strings.Repeat("a3", 16), strings.Repeat("b3", 16)
	response := "9b0c1d2e-3f4a-4b5c-8d6e-7f8a9b0c1d23"
	parkChain(t, pool, root, response, other, `{"thread_id": "t1", "is_error": false}`, true)
	// A wake of another park of the same response does not count either.
	writeWake(t, pool, response, strings.Repeat("c3", 16), other)
	hook, _, goals := newParkHook(pool)
	if err := hook.Settle(ctx, root, executionapp.SettlementSucceeded); err != nil {
		t.Fatal(err)
	}
	if _, state, _ := chainState(t, pool, root); state != unattended.EndCompleted || goals.checks != 1 {
		t.Fatalf("state %q, checks %d; want completed after one goal check", state, goals.checks)
	}
}

// TestPostgresChainStillContinuesARunThatDidNotPark: a run that reached its
// step limit, with no park, still starts its continuation.
func TestPostgresChainStillContinuesARunThatDidNotPark(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	root := strings.Repeat("a4", 16)
	response := "9b0c1d2e-3f4a-4b5c-8d6e-7f8a9b0c1d24"
	parkChain(t, pool, root, response, root, `{"thread_id": "t1", "step_limit_reached": true, "is_error": false}`, false)
	hook, starts, goals := newParkHook(pool)
	if err := hook.Settle(ctx, root, executionapp.SettlementSucceeded); err != nil {
		t.Fatal(err)
	}
	if _, state, _ := chainState(t, pool, root); state != "" || starts.starts != 1 || goals.checks != 0 {
		t.Fatalf("state %q, starts %d, checks %d; want an open chain and one continuation", state, starts.starts,
			goals.checks)
	}
}

// TestPostgresRunFinishedAnnouncesAParkedRunAsWaiting: a schedule run that
// parked a fan-out is announced as waiting, not completed (on main it was
// completed).
func TestPostgresRunFinishedAnnouncesAParkedRunAsWaiting(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	repo, err := NewRunFinishedRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	conversation := "6f7a8b9c-0d1e-4f2a-8b3c-4d5e6f7a8b9c"
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.unattended_run_policies
    (project_id, conversation_uuid, trigger_origin, version_id, owner_user_id, on_approval_required)
VALUES (1, $1::uuid, 'schedule', 41, 4712, 'fail')`, conversation); err != nil {
		t.Fatal(err)
	}
	execution := strings.Repeat("a5", 16)
	response := "9b0c1d2e-3f4a-4b5c-8d6e-7f8a9b0c1d25"
	seedSettledAgentRun(t, pool, execution, "schedule", conversation, response)
	writeResponse(t, pool, response, execution, parkedMeta, false)
	if err := repo.RecordSettledRun(ctx, execution, executionapp.SettlementSucceeded); err != nil {
		t.Fatal(err)
	}
	if rows := runFinishedRows(t, pool, execution); len(rows) != 1 || rows[0].Outcome != runfinished.OutcomeWaiting {
		t.Fatalf("a parked schedule run was announced as %+v, want waiting", rows)
	}
}

// TestClosedCardsEndTheResponsePark: when a new turn supersedes a parked
// response's cards (as stop, regenerate and a failed wake close them), the
// response no longer waits for a person: its park marker goes with the
// cards, and its other meta stays. A fork or an editor test run then does
// not read it as paused forever.
func TestClosedCardsEndTheResponsePark(t *testing.T) {
	w := newWakeHarness(t)
	parked := w.newResponse()
	w.queueWake(parked)
	if n := w.count(`SELECT count(*) FROM p_1.chat_message_group WHERE uuid = $1::uuid AND meta ? 'fanout_parked_v1'`, parked); n != 1 {
		t.Fatal("the fixture response is not parked")
	}
	newTurn := w.newResponse()
	err := w.repo.projects.WithinProjectTx(t.Context(), 1, interruptWriteTx, func(tx sqlExecutor) error {
		_, err := supersedeEarlierTurnInterrupts(t.Context(), tx, 1, w.conversation, newTurn, interruptAsker)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM p_1.chat_message_group WHERE uuid = $1::uuid
AND NOT meta ? 'fanout_parked_v1' AND meta ? 'thread_id'`, parked); n != 1 {
		t.Fatal("the superseded response still carries its park, or lost its other meta")
	}
	if n := w.count(`SELECT count(*) FROM elitea_runtime.execution_interrupts
WHERE root_response_id = $1::uuid AND state IN ('PENDING', 'DECIDED')`, parked); n != 0 {
		t.Fatalf("%d cards still open", n)
	}
}
