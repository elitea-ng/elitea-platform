package repos

// Desktop local turns (ADR-0029 decision 5c) against a REAL database
// (ELITEA_TEST_DATABASE_URL): the start/commit use case over LocalTurnsRepo,
// the real MemoriesRepo recall, the /llm attribution verifier, and the
// conversation's changes_since delta, which must surface a committed turn to
// every other client.

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/memories"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/localturn"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
)

type localTurnPolicy struct{ allowed bool }

func (p *localTurnPolicy) Policy(context.Context) (platformconfig.NativeClientPolicy, error) {
	policy := platformconfig.DefaultNativeClientPolicy()
	policy.LocalWork.Allowed = p.allowed
	return policy, nil
}

type localTurnAudit struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *localTurnAudit) Record(_ context.Context, event audit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

type localTurnFixture struct {
	conversationUUID string
	conversationID   int
	dummyID          int
}

func TestPostgresLocalTurnStartCommitAndDelta(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const user int64 = 4501
	const stranger int64 = 4502

	var fixture localTurnFixture
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source)
VALUES (gen_random_uuid(), 'local turn', $1, 'elitea')
RETURNING id, uuid::text`, user).Scan(&fixture.conversationID, &fixture.conversationUUID); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	for _, participant := range []struct {
		entity string
		meta   string
	}{{"user", `{"id": ` + strconv.FormatInt(user, 10) + `}`}, {"dummy", `{}`}} {
		var id int
		if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta, meta)
VALUES (gen_random_uuid(), $1, $2::jsonb, '{}'::json) RETURNING id`, participant.entity, participant.meta).Scan(&id); err != nil {
			t.Fatalf("seed participant: %v", err)
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id) VALUES ($1, $2)`,
			fixture.conversationID, id); err != nil {
			t.Fatalf("map participant: %v", err)
		}
		if participant.entity == "dummy" {
			fixture.dummyID = id
		}
	}

	memoriesRepo := NewMemoriesRepo(pool)
	if _, err := memoriesRepo.Create(ctx, "1", strconv.FormatInt(user, 10), memories.MemoryEntry{
		Content: "Prefers tabs over spaces.", Enabled: true,
	}); err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	policy := &localTurnPolicy{}
	recorder := &localTurnAudit{}
	ids := 0
	service, err := localturn.NewService(NewLocalTurnsRepo(pool), policy, memoriesRepo, recorder,
		func() (string, error) {
			ids++
			return strings.Repeat("0", 31) + strconv.Itoa(ids%10), nil
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	const questionID = "6f1c2d3e-4a5b-4c6d-8e7f-901234567890"
	start := localturn.StartRequest{
		ProjectID: 1, ActorUserID: user, TokenID: "77", ConversationUUID: fixture.conversationUUID,
		QuestionID: questionID, UserInput: "Fix the failing test", AuditRoute: "/start",
	}

	// local_work.allowed false (the default) refuses the start.
	if _, err := service.Start(ctx, start); !errors.Is(err, localturn.ErrLocalWorkDisabled) {
		t.Fatalf("start with local work off = %v, want ErrLocalWorkDisabled", err)
	}
	policy.allowed = true

	started, err := service.Start(ctx, start)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !started.Created || started.ParticipantID != int64(fixture.dummyID) {
		t.Fatalf("start = %+v, want created, answered by the dummy participant %d", started, fixture.dummyID)
	}
	// The recall is the cloud resolver's: the memory saved before the user's
	// first turn in the project is reserved and recalled.
	if started.Recall.Count != 1 || !strings.Contains(started.Recall.Text, "Prefers tabs over spaces.") {
		t.Fatalf("start recall = %+v, want the saved memory", started.Recall)
	}

	// The /llm edge keeps the id for the caller only.
	verifier := NewExecutionAttributionVerifier(pool)
	assertVerified := func(project, userID string, want bool) {
		t.Helper()
		got, err := verifier.VerifyExecution(ctx, project, userID, started.ExecutionID)
		if err != nil || got != want {
			t.Fatalf("VerifyExecution(%s, %s) = %v, %v; want %v", project, userID, got, err, want)
		}
	}
	assertVerified("1", strconv.FormatInt(user, 10), true)
	assertVerified("1", strconv.FormatInt(stranger, 10), false)

	// A retried start replays the same execution, with a fresh recall.
	replayed, err := service.Start(ctx, start)
	if err != nil || replayed.Created || replayed.ExecutionID != started.ExecutionID ||
		replayed.ResponseMessageID != started.ResponseMessageID {
		t.Fatalf("replayed start = %+v, %v; want the same execution, not created", replayed, err)
	}

	// A web client's delta cursor, taken before the commit.
	conversations := NewConversationsRepo(pool)
	before, err := conversations.ListMessageChanges(ctx, "1", fixture.conversationUUID, "", 100)
	if err != nil {
		t.Fatalf("full sync: %v", err)
	}
	if len(before.Items) != 0 {
		t.Fatalf("a started turn wrote %d messages; the start writes none", len(before.Items))
	}

	exitCode := 1
	commit := localturn.CommitRequest{
		ProjectID: 1, ActorUserID: user, ExecutionID: started.ExecutionID,
		UserMessage:      "Fix the failing test",
		AssistantMessage: "Fixed: the assertion compared the wrong field.",
		ToolCalls: json.RawMessage(`{"run-1": {"run_id": "run-1", "tool_name": "shell",
			"tool_inputs": {"command": "cargo test"}, "tool_output": "1 failed",
			"timestamp_start": "2026-10-08T10:00:00Z", "timestamp_finish": "2026-10-08T10:00:05Z"}}`),
		ThinkingSteps: []json.RawMessage{json.RawMessage(`{"type": "thinking", "thinking": "look at the test",
			"timestamp_start": "2026-10-08T09:59:59Z"}`)},
		HITLExchanges: []localturn.HITLExchange{{
			InterruptID: "approve-1", Kind: "command_approval", ToolRunID: "run-1",
			Prompt: "Run cargo test?", Decision: "approve",
		}},
		Report: localturn.LocalWorkReport{
			SandboxMode: "workspace-write", Enforcement: "full",
			Commands: []localturn.CommandRecord{{Command: "cargo test", ExitCode: &exitCode}},
			Paths:    []string{"src/lib.rs"},
		},
		AuditRoute: "/commit",
	}
	// Only the owner may commit.
	stolen := commit
	stolen.ActorUserID = stranger
	if _, err := service.Commit(ctx, stolen); !errors.Is(err, localturn.ErrNotFound) {
		t.Fatalf("a stranger's commit = %v, want ErrNotFound", err)
	}

	committed, err := service.Commit(ctx, commit)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !committed.Created || committed.QuestionMessageID != questionID ||
		committed.ResponseMessageID != started.ResponseMessageID || committed.MemoriesUsed != 1 {
		t.Fatalf("commit = %+v", committed)
	}

	// Idempotent: the same body replays; a different body is refused.
	again, err := service.Commit(ctx, commit)
	if err != nil || again.Created || again.ResponseMessageID != committed.ResponseMessageID {
		t.Fatalf("retried commit = %+v, %v; want a replay", again, err)
	}
	changed := commit
	changed.AssistantMessage = "Something else."
	if _, err := service.Commit(ctx, changed); !errors.Is(err, localturn.ErrAlreadyCommitted) {
		t.Fatalf("a different second commit = %v, want ErrAlreadyCommitted", err)
	}
	if _, err := service.Start(ctx, start); !errors.Is(err, localturn.ErrAlreadyCommitted) {
		t.Fatalf("a start after the commit = %v, want ErrAlreadyCommitted", err)
	}

	// The delta surfaces both messages to another client.
	after, err := conversations.ListMessageChanges(ctx, "1", fixture.conversationUUID, before.NextCursor, 100)
	if err != nil {
		t.Fatalf("delta after commit: %v", err)
	}
	byUUID := map[string]map[string]any{}
	contents := map[string]string{}
	for _, item := range after.Items {
		byUUID[item.UUID] = item.Metadata
		contents[item.UUID] = item.Content
	}
	if len(byUUID) != 2 || contents[questionID] != "Fix the failing test" ||
		contents[started.ResponseMessageID] != "Fixed: the assertion compared the wrong field." {
		t.Fatalf("delta after commit = %+v, want the question and the answer", after.Items)
	}

	var questionMeta, responseMeta []byte
	if err := pool.QueryRow(ctx, `SELECT meta FROM p_1.chat_message_group WHERE uuid = $1::uuid`, questionID).Scan(&questionMeta); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT meta FROM p_1.chat_message_group WHERE uuid = $1::uuid`, started.ResponseMessageID).Scan(&responseMeta); err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	_ = json.Unmarshal(responseMeta, &response)
	if !strings.Contains(string(questionMeta), `"executed_by": "desktop"`) || response["executed_by"] != "desktop" {
		t.Fatalf("executed_by missing: question %s, answer %s", questionMeta, responseMeta)
	}
	if response["memories_used"] != float64(1) {
		t.Fatalf("answer meta memories_used = %v, want 1 (%s)", response["memories_used"], responseMeta)
	}
	if ids, _ := response["resolved_hitl_interrupt_ids"].([]any); len(ids) != 1 || ids[0] != "approve-1" {
		t.Fatalf("answer meta resolved_hitl_interrupt_ids = %v", response["resolved_hitl_interrupt_ids"])
	}
	var toolSteps, thinkingSteps int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE s.kind = 'tool_call' AND s.tool_name = 'shell' AND s.tool_output = '1 failed'),
       count(*) FILTER (WHERE s.kind = 'thinking_step')
FROM p_1.chat_message_trace_step s JOIN p_1.chat_message_group g ON g.id = s.message_group_id
WHERE g.uuid = $1::uuid`, started.ResponseMessageID).Scan(&toolSteps, &thinkingSteps); err != nil {
		t.Fatal(err)
	}
	if toolSteps != 1 || thinkingSteps != 1 {
		t.Fatalf("trace rows = %d tool, %d thinking; want 1 and 1", toolSteps, thinkingSteps)
	}

	// The commit's audit names what the client reported.
	if len(recorder.events) != 2 || !strings.Contains(recorder.events[1].Action, "cargo test") ||
		!strings.Contains(recorder.events[1].Action, "src/lib.rs") || recorder.events[1].EntityName != started.ExecutionID {
		t.Fatalf("audit events = %+v, want start + commit with the report", recorder.events)
	}
}

// TestPostgresLocalTurnExpiresLikeAnAbandonedRun: a turn past its deadline is
// no longer live for /llm and cannot be committed.
func TestPostgresLocalTurnExpiresLikeAnAbandonedRun(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const user int64 = 4511
	var conversationID int
	var conversationUUID string
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source)
VALUES (gen_random_uuid(), 'expiring', $1, 'elitea') RETURNING id, uuid::text`, user).
		Scan(&conversationID, &conversationUUID); err != nil {
		t.Fatal(err)
	}
	for _, seed := range []string{`('user', '{"id": 4511}')`, `('dummy', '{}')`} {
		if _, err := pool.Exec(ctx, `
WITH p AS (
    INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta, meta)
    SELECT gen_random_uuid(), v.name, v.meta::jsonb, '{}'::json FROM (VALUES `+seed+`) AS v(name, meta)
    RETURNING id
)
INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id) SELECT $1, id FROM p`, conversationID); err != nil {
			t.Fatal(err)
		}
	}
	service, err := localturn.NewService(NewLocalTurnsRepo(pool), &localTurnPolicy{allowed: true},
		NewMemoriesRepo(pool), &localTurnAudit{},
		func() (string, error) { return "abcdefabcdefabcdefabcdefabcdef01", nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	started, err := service.Start(ctx, localturn.StartRequest{
		ProjectID: 1, ActorUserID: user, TokenID: "78", ConversationUUID: conversationUUID,
		QuestionID: "11111111-2222-4333-8444-555555555555", UserInput: "hello",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if time.Until(started.ExpiresAt) < 23*time.Hour {
		t.Fatalf("expires_at = %v, want the 24 h cloud agent deadline", started.ExpiresAt)
	}
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.local_turn_executions
SET started_at = now() - interval '25 hours', expires_at = now() - interval '1 hour'
WHERE execution_id = $1`, started.ExecutionID); err != nil {
		t.Fatal(err)
	}
	live, err := NewExecutionAttributionVerifier(pool).VerifyExecution(ctx, "1", "4511", started.ExecutionID)
	if err != nil || live {
		t.Fatalf("an expired local turn verified as live (%v, %v)", live, err)
	}
	if _, err := service.Commit(ctx, localturn.CommitRequest{
		ProjectID: 1, ActorUserID: user, ExecutionID: started.ExecutionID,
		UserMessage: "hello", AssistantMessage: "hi",
	}); !errors.Is(err, localturn.ErrExpired) {
		t.Fatalf("commit after the deadline = %v, want ErrExpired", err)
	}
	var messages int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM p_1.chat_message_group WHERE conversation_id = $1`, conversationID).Scan(&messages); err != nil || messages != 0 {
		t.Fatalf("an expired commit wrote %d messages (%v)", messages, err)
	}
}
