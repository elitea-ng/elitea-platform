package repos

// Unattended chains against a real PostgreSQL (ADR-0045, #1435, #1436).
// Requires ELITEA_TEST_DATABASE_URL; every test skips without it.

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
	outputapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	runtimedomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

type fixedUnattendedSettings struct {
	settings platformconfig.UnattendedSettings
}

func (f fixedUnattendedSettings) Get(context.Context) (platformconfig.UnattendedSettings, error) {
	return f.settings, nil
}

// seedLocalTurnConversation makes a conversation with the user and a model
// participant in project 1.
func seedLocalTurnConversation(t *testing.T, pool *pgxpool.Pool, user int64) string {
	t.Helper()
	ctx := t.Context()
	var conversationID int
	var conversationUUID string
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source, created_at)
VALUES (gen_random_uuid(), 'unattended', $1, 'elitea', now() AT TIME ZONE 'UTC') RETURNING id, uuid::text`, user).
		Scan(&conversationID, &conversationUUID); err != nil {
		t.Fatal(err)
	}
	userMeta := `{"id": ` + strconv.FormatInt(user, 10) + `}`
	for _, seed := range [][2]string{{"user", userMeta}, {"dummy", `{}`}} {
		if _, err := pool.Exec(ctx, `
WITH p AS (
    INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta, meta)
    VALUES (gen_random_uuid(), $2, $3::jsonb, '{}'::json) RETURNING id
)
INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, created_at)
SELECT $1, id, now() AT TIME ZONE 'UTC' FROM p`, conversationID, seed[0], seed[1]); err != nil {
			t.Fatal(err)
		}
	}
	return conversationUUID
}

func newUnattendedLocalTurnService(t *testing.T, pool *pgxpool.Pool, prefix string) *localturn.Service {
	t.Helper()
	ids := 0
	service, err := localturn.NewService(NewLocalTurnsRepo(pool), &localTurnPolicy{allowed: true},
		NewMemoriesRepo(pool), &localTurnAudit{},
		func() (string, error) {
			ids++
			return prefix + strings.Repeat("0", 32-len(prefix)-2) + strconv.Itoa(10+ids), nil
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return service.WithUnattended(fixedUnattendedSettings{platformconfig.DefaultUnattendedSettings()})
}

func chainState(t *testing.T, pool *pgxpool.Pool, root string) (continuations int, endState, endLimit string) {
	t.Helper()
	if err := pool.QueryRow(t.Context(), `
SELECT continuations, COALESCE(end_state, ''), COALESCE(end_limit, '')
FROM elitea_runtime.run_chains WHERE root_execution_id = $1`, root).Scan(&continuations, &endState, &endLimit); err != nil {
		t.Fatalf("read chain %s: %v", root, err)
	}
	return continuations, endState, endLimit
}

func int64Pointer(value int64) *int64 { return &value }

// TestPostgresUnattendedLocalChainLimitAndLedger: the first turn starts the
// chain, a continuation passes the chain check, the next one is refused
// with the continuations limit and ends the chain. The commit writes one
// ledger row per exchange except bypass.
func TestPostgresUnattendedLocalChainLimitAndLedger(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	const user int64 = 4601
	conversation := seedLocalTurnConversation(t, pool, user)
	service := newUnattendedLocalTurnService(t, pool, "a1")
	const grantID = "0b7d1f6a-3c2e-4f8a-9d10-2a3b4c5d6e7f"
	start := func(question string, root string) (localturn.StartOutcome, error) {
		return service.Start(ctx, localturn.StartRequest{
			ProjectID: 1, ActorUserID: user, TokenID: "91", ConversationUUID: conversation,
			QuestionID: question, UserInput: "work",
			Unattended: &unattended.Request{
				GrantID: grantID, ChainRootExecutionID: root, ContinueStepLimit: true, HookAsks: true,
				RemoteConfirmations: true,
				Chain:               unattended.ChainRequest{MaxContinuations: int64Pointer(1)},
			},
		})
	}
	first, err := start("21111111-2222-4333-8444-555555555551", "")
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	if first.Unattended == nil || first.Unattended.ContinuationIndex != 0 ||
		first.Unattended.ChainRootExecutionID != first.ExecutionID || first.Unattended.Limits.MaxContinuations != 1 {
		t.Fatalf("first start chain = %+v", first.Unattended)
	}
	// A replayed first start is the same chain.
	if replay, err := start("21111111-2222-4333-8444-555555555551", ""); err != nil || replay.Created {
		t.Fatalf("replayed first start = %+v, %v", replay, err)
	}
	second, err := start("21111111-2222-4333-8444-555555555552", first.ExecutionID)
	if err != nil {
		t.Fatalf("continuation: %v", err)
	}
	if second.Unattended == nil || second.Unattended.ContinuationIndex != 1 {
		t.Fatalf("continuation chain = %+v", second.Unattended)
	}
	// A remote confirmation that the grant answered: Main writes its
	// decision row when it consumes the approval.
	repo := NewLocalTurnsRepo(pool)
	confirmed := localturn.ConfirmationCall{ExecutionID: second.ExecutionID, ToolkitID: 61, ApplicationID: 11,
		VersionID: 12, ToolName: "delete_file", ArgumentsSHA256: strings.Repeat("a", 64)}.Digest()
	confirmedInterrupt := localturn.ConfirmationInterruptID(confirmed, 0)
	if outcome, err := repo.ConsumeConfirmation(ctx, localturn.ConfirmationClaim{
		ExecutionID: second.ExecutionID, CallDigest: confirmed, InterruptID: confirmedInterrupt,
		IdempotencyKey: "k1", Fresh: true, Source: localturn.SourceGrant,
		Call: localturn.ConfirmationLedgerCall{ToolName: "delete_file", ToolkitName: "files", ToolkitType: "filesystem"},
	}); err != nil || !outcome.Accepted {
		t.Fatalf("grant confirmation = %+v, %v", outcome, err)
	}

	_, err = start("21111111-2222-4333-8444-555555555553", first.ExecutionID)
	var limit *unattended.ChainLimitError
	if !errors.As(err, &limit) || limit.Limit != unattended.LimitContinuations {
		t.Fatalf("third start = %v, want chain_limit_reached continuations", err)
	}
	if count, state, endLimit := chainState(t, pool, first.ExecutionID); count != 1 ||
		state != unattended.EndLimitReached || endLimit != unattended.LimitContinuations {
		t.Fatalf("chain = %d %q %q, want 1 limit_reached continuations", count, state, endLimit)
	}
	// An ended chain refuses any later continuation with the stored limit.
	if _, err := start("21111111-2222-4333-8444-555555555554", first.ExecutionID); !errors.As(err, &limit) {
		t.Fatalf("start on an ended chain = %v", err)
	}

	committed, err := service.Commit(ctx, localturn.CommitRequest{
		ProjectID: 1, ActorUserID: user, Credential: localturn.Credential{TokenID: "91"},
		ExecutionID: second.ExecutionID, UserMessage: "work", AssistantMessage: "done",
		HITLExchanges: []localturn.HITLExchange{
			// The confirmed call: the commit writes no second decision row.
			{InterruptID: "i-confirmed", Kind: "tool_confirmation", Prompt: "delete", Decision: "approve",
				Source: localturn.SourceGrant},
			{InterruptID: "i-grant", Kind: "command_approval", Prompt: "rm", Decision: "approve", Source: localturn.SourceGrant},
			// A grant answer of a kind the grant does not cover: no row, the
			// commit succeeds.
			{InterruptID: "i-uncovered", Kind: "question", Prompt: "which", Decision: "approve",
				Source: localturn.SourceGrant},
			{InterruptID: "i-timeout", Kind: "question", Prompt: "which", Decision: "answer", Source: localturn.SourceTimeout},
			{InterruptID: "i-bypass", Kind: "command_approval", Prompt: "ls", Decision: "approve", Source: localturn.SourceBypass},
			{InterruptID: "i-person", Kind: "command_approval", Prompt: "git", Decision: "reject"},
		},
		ChainOutcome: unattended.EndCompleted,
	})
	if err != nil || !committed.Created {
		t.Fatalf("commit = %+v, %v", committed, err)
	}
	all := readApprovalLedger(t, pool, second.ExecutionID)
	if got := ledgerOutcomes(all); strings.Join(got, ",") !=
		"decision:approved,decision:approved,decision:timed_out,decision:rejected" {
		t.Fatalf("ledger = %v", got)
	}
	if all[0].InterruptID != confirmedInterrupt || all[0].Reason != unattended.ReasonUnattendedGrant ||
		all[0].ToolName != "delete_file" {
		t.Fatalf("confirmation row = %+v", all[0])
	}
	for _, row := range all {
		if row.InterruptID == "i-confirmed" || row.InterruptID == "i-uncovered" {
			t.Fatalf("the commit wrote a row for %s: %+v", row.InterruptID, row)
		}
	}
	rows := all[1:]
	grant := rows[0]
	if grant.ActorKind != "user" || grant.ActorUserID == nil || *grant.ActorUserID != user ||
		grant.Reason != unattended.ReasonUnattendedGrant || grant.Path != approvalPathDesktop {
		t.Fatalf("grant row = %+v", grant)
	}
	var storedGrant string
	if err := pool.QueryRow(ctx, `
SELECT grant_id::text FROM elitea_runtime.approval_ledger WHERE execution_id = $1 AND interrupt_id = 'i-grant'`,
		second.ExecutionID).Scan(&storedGrant); err != nil || storedGrant != grantID {
		t.Fatalf("grant_id = %q, %v", storedGrant, err)
	}
	if rows[1].ActorKind != "system" || rows[1].Reason != desktopTimeoutReason {
		t.Fatalf("timeout row = %+v", rows[1])
	}
	assertApprovalAudit(t, pool, 1, all)
	// The chain already ended at its limit: the outcome does not overwrite it.
	if _, state, _ := chainState(t, pool, first.ExecutionID); state != unattended.EndLimitReached {
		t.Fatalf("end state = %q", state)
	}
}

// TestPostgresUnattendedChainCheckIsOneStatement: two continuations race
// for the last slot; exactly one wins.
func TestPostgresUnattendedChainCheckIsOneStatement(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	root := strings.Repeat("c", 32)
	conversation := "5b9e7d3c-1a2b-4c3d-8e4f-5a6b7c8d9e0f"
	start := unattended.ChainStart{
		RootExecutionID: root, ProjectID: 1, OwnerUserID: 4611, ConversationUUID: conversation,
		Origin: unattended.OriginCloud, TriggerOrigin: "manual",
		Grant:  unattended.Grant{GrantID: "6c0e8a4b-2d3f-4a5b-9c6d-7e8f9a0b1c2d", ContinueStepLimit: true},
		Limits: unattended.Limits{MaxContinuations: 1, Deadline: time.Hour},
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := startRunChain(ctx, tx, start); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var wins, refusals int
	var mu sync.Mutex
	var group sync.WaitGroup
	for index := range 8 {
		group.Go(func() {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			_, err = continueRunChain(ctx, tx, unattended.ChainContinue{
				RootExecutionID: root, ExecutionID: strings.Repeat("d", 31) + strconv.Itoa(index),
				Kind: unattended.LinkStepLimit, OwnerUserID: 4611, GrantID: start.Grant.GrantID,
				ProjectID: 1, ConversationUUID: conversation,
			})
			mu.Lock()
			defer mu.Unlock()
			var limit *unattended.ChainLimitError
			switch {
			case err == nil:
				if commitErr := tx.Commit(ctx); commitErr != nil {
					t.Error(commitErr)
				}
				wins++
			case errors.As(err, &limit) && limit.Limit == unattended.LimitContinuations:
				refusals++
			default:
				t.Errorf("continuation %d: %v", index, err)
			}
		})
	}
	group.Wait()
	if wins != 1 || refusals != 7 {
		t.Fatalf("wins = %d, refusals = %d; want 1 and 7", wins, refusals)
	}
	if count, _, _ := chainState(t, pool, root); count != 1 {
		t.Fatalf("continuations = %d", count)
	}
	// The deadline refuses too.
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.run_chains SET max_continuations = 5, deadline = created_at WHERE root_execution_id = $1`, root); err != nil {
		t.Fatal(err)
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = continueRunChain(ctx, tx, unattended.ChainContinue{
		RootExecutionID: root, ExecutionID: strings.Repeat("e", 32), Kind: unattended.LinkStepLimit,
		OwnerUserID: 4611, GrantID: start.Grant.GrantID, ProjectID: 1, ConversationUUID: conversation,
	})
	var limit *unattended.ChainLimitError
	if !errors.As(err, &limit) || limit.Limit != unattended.LimitDeadline {
		t.Fatalf("continuation after the deadline = %v", err)
	}
	// Another owner sees no chain.
	_, err = continueRunChain(ctx, tx, unattended.ChainContinue{
		RootExecutionID: root, ExecutionID: strings.Repeat("f", 32), Kind: unattended.LinkStepLimit,
		OwnerUserID: 4612, GrantID: start.Grant.GrantID, ProjectID: 1, ConversationUUID: conversation,
	})
	var refusal *unattended.RefusalError
	if !errors.As(err, &refusal) || refusal.Reason != unattended.RefusedChainNotFound {
		t.Fatalf("another owner's continuation = %v", err)
	}
}

// TestPostgresUnattendedGrantApprovalNodeEvent: a grant approval writes the
// owner's ledger decision; a tool outside the grant is invalid output.
func TestPostgresUnattendedGrantApprovalNodeEvent(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	root := strings.Repeat("b", 32)
	grant := unattended.Grant{
		GrantID:        "7d1f9b5c-3e4a-4b6c-8d7e-8f9a0b1c2d3e",
		SensitiveTools: []unattended.Tool{{ToolkitName: "jira", ToolName: "create_issue"}},
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := startRunChain(ctx, tx, unattended.ChainStart{
		RootExecutionID: root, ProjectID: 1, OwnerUserID: 4621,
		ConversationUUID: "8e2a0c6d-4f5b-4c7d-9e8f-9a0b1c2d3e4f", Origin: unattended.OriginCloud,
		TriggerOrigin: "manual", Grant: grant, Limits: unattended.Limits{MaxContinuations: 1, Deadline: time.Hour},
		ResponseMessageID: "9f3b1d7e-5a6c-4d8e-8f9a-0b1c2d3e4f5a",
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	frame := func(tool string) outputapp.NodeEventFrame {
		data, _ := json.Marshal(map[string]any{
			"type": unattendedApprovalEventType, "message_id": "9f3b1d7e-5a6c-4d8e-8f9a-0b1c2d3e4f5a",
			"response_metadata": map[string]any{unattendedApprovalMetadataKey: map[string]any{
				"grant_id": grant.GrantID, "interrupt_id": "hitl_" + strings.Repeat("1", 32),
				"toolkit_name": "jira", "toolkit_type": "jira", "tool_name": tool,
				"argument_digest": "sha256:" + strings.Repeat("a", 64),
			}},
		})
		return outputapp.NodeEventFrame{Fence: runtimedomain.Fence{ExecutionID: root}, BrowserData: data}
	}
	project := func(tool string) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := projectUnattendedApproval(ctx, pgxExecutor{queryer: tx}, 1, frame(tool)); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if err := project("delete_issue"); !errors.Is(err, outputapp.ErrInvalidNodeEventOutput) {
		t.Fatalf("a tool outside the grant = %v", err)
	}
	for range 2 { // a replayed frame writes nothing new
		if err := project("create_issue"); err != nil {
			t.Fatalf("grant approval: %v", err)
		}
	}
	rows := readApprovalLedger(t, pool, root)
	if len(rows) != 1 || rows[0].Outcome != ApprovalOutcomeApproved || rows[0].ActorUserID == nil ||
		*rows[0].ActorUserID != 4621 || rows[0].Reason != unattended.ReasonUnattendedGrant ||
		rows[0].ToolName != "create_issue" || rows[0].Path != approvalPathRoot {
		t.Fatalf("ledger = %+v", rows)
	}
	assertApprovalAudit(t, pool, 1, rows)
}

// TestPostgresUnattendedChainSweep: the sweep ends a chain whose latest
// local turn expired uncommitted (cancelled), one whose latest turn
// committed with no continuation (failed), one past its deadline
// (limit_reached), and leaves a live chain alone. A second pass ends
// nothing.
func TestPostgresUnattendedChainSweep(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	const user int64 = 4631
	conversation := seedLocalTurnConversation(t, pool, user)
	service := newUnattendedLocalTurnService(t, pool, "a3")
	starts := make([]localturn.StartOutcome, 4)
	for index := range starts {
		started, err := service.Start(ctx, localturn.StartRequest{
			ProjectID: 1, ActorUserID: user, TokenID: "93", ConversationUUID: conversation,
			QuestionID: "31111111-2222-4333-8444-55555555555" + strconv.Itoa(index), UserInput: "work",
			Unattended: &unattended.Request{GrantID: "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5" + strconv.Itoa(index)},
		})
		if err != nil {
			t.Fatalf("start %d: %v", index, err)
		}
		starts[index] = started
	}
	// 0: expired uncommitted 11 minutes ago.
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.local_turn_executions
SET started_at = now() - interval '2 hours', expires_at = now() - interval '11 minutes'
WHERE execution_id = $1`, starts[0].ExecutionID); err != nil {
		t.Fatal(err)
	}
	// 1: committed 11 minutes ago without chain_outcome.
	if _, err := service.Commit(ctx, localturn.CommitRequest{
		ProjectID: 1, ActorUserID: user, Credential: localturn.Credential{TokenID: "93"},
		ExecutionID: starts[1].ExecutionID, UserMessage: "work", AssistantMessage: "done",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.local_turn_executions SET committed_at = now() - interval '11 minutes'
WHERE execution_id = $1`, starts[1].ExecutionID); err != nil {
		t.Fatal(err)
	}
	// 2: the deadline passed.
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.run_chains SET deadline = created_at WHERE root_execution_id = $1`, starts[2].ExecutionID); err != nil {
		t.Fatal(err)
	}
	// 3: live.
	chains := NewRunChainsRepository(pool)
	swept, err := chains.SweepChains(ctx, 10*time.Minute, 64)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, chain := range swept {
		got[chain.RootExecutionID] = chain.EndState
	}
	want := map[string]string{
		starts[0].ExecutionID: unattended.EndCancelled,
		starts[1].ExecutionID: unattended.EndFailed,
		starts[2].ExecutionID: unattended.EndLimitReached,
	}
	for root, state := range want {
		if got[root] != state {
			t.Fatalf("swept %v, want %v", got, want)
		}
	}
	if _, ok := got[starts[3].ExecutionID]; ok {
		t.Fatalf("the sweep ended a live chain: %v", got)
	}
	if _, _, limit := chainState(t, pool, starts[2].ExecutionID); limit != unattended.LimitDeadline {
		t.Fatalf("deadline end_limit = %q", limit)
	}
	again, err := chains.SweepChains(ctx, 10*time.Minute, 64)
	if err != nil || len(again) != 0 {
		t.Fatalf("second sweep = %v, %v", again, err)
	}
	// A chain the sweep ended refuses a continuation.
	_, err = service.Start(ctx, localturn.StartRequest{
		ProjectID: 1, ActorUserID: user, TokenID: "93", ConversationUUID: conversation,
		QuestionID: "31111111-2222-4333-8444-555555555559", UserInput: "more",
		Unattended: &unattended.Request{
			GrantID: "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c51", ChainRootExecutionID: starts[1].ExecutionID,
		},
	})
	if !errors.Is(err, unattended.ErrChainEnded) {
		t.Fatalf("continuation of a swept chain = %v, want ErrChainEnded", err)
	}
}

// TestPostgresUnattendedChainSettleViewAndEndOnce: the settle view reads the
// latest run, and the end state is written once.
func TestPostgresUnattendedChainSettleViewAndEndOnce(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	root := strings.Repeat("9", 32)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := startRunChain(ctx, tx, unattended.ChainStart{
		RootExecutionID: root, ProjectID: 1, OwnerUserID: 4641,
		ConversationUUID: "0a4c2e8f-6b7d-4e9f-8a0b-1c2d3e4f5a6b", Origin: unattended.OriginCloud,
		TriggerOrigin: "manual", Grant: unattended.Grant{GrantID: "2b5d3f9a-7c8e-4f0a-9b1c-2d3e4f5a6b7c"},
		Limits: unattended.Limits{MaxContinuations: 1, Deadline: time.Hour},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	chains := NewRunChainsRepository(pool)
	view, found, err := chains.ChainSettleView(ctx, root)
	if err != nil || !found || !view.Latest || view.Chain.RootExecutionID != root {
		t.Fatalf("view = %+v, %v, %v", view, found, err)
	}
	if ended, err := chains.EndChain(ctx, root, unattended.EndCompleted, ""); err != nil || !ended {
		t.Fatalf("first end = %v, %v", ended, err)
	}
	if ended, err := chains.EndChain(ctx, root, unattended.EndFailed, ""); err != nil || ended {
		t.Fatalf("second end = %v, %v", ended, err)
	}
	if _, state, _ := chainState(t, pool, root); state != unattended.EndCompleted {
		t.Fatalf("end state = %q", state)
	}
}
