package repos

// The tenant chat tables keep `created_at`/`updated_at` as `timestamp`
// WITHOUT time zone, and their meaning is the UTC wall clock: pylon wrote them
// with server_default now() in UTC containers, and the web client appends `Z`
// to a naive timestamp before it parses one. Postgres converts a timestamptz
// (now(), clock_timestamp(), a DEFAULT now()) into such a column through the
// SESSION time zone, so a writer is right only if it says AT TIME ZONE 'UTC'.
//
// Every test connection in this module runs in a non-UTC session
// (internal/testpg). The tests here pin that, and pin that
// the writers and the conversation activity trigger (tenant/0150) store UTC
// whatever the session says.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/folders"
	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	runtimedomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/runtime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
)

// naiveUTCSkew bounds how far a stored stamp may sit from the UTC clock read
// around the write. A wrong zone is off by whole hours.
const naiveUTCSkew = 5 * time.Minute

// The guard is only a guard while the session is not UTC: a testpg.DSN
// change that silently stopped applying would make every test in this
// package pass on a UTC CI server again.
func TestPostgresIntegrationSessionTimeZoneIsNotUTC(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var zone string
	var offsetSeconds float64
	if err := pool.QueryRow(ctx, `
SELECT current_setting('TimeZone'),
       extract(epoch FROM (now() AT TIME ZONE current_setting('TimeZone')) - (now() AT TIME ZONE 'UTC'))`).
		Scan(&zone, &offsetSeconds); err != nil {
		t.Fatal(err)
	}
	if zone != testpg.TimeZone() {
		t.Fatalf("session TimeZone = %q, want %q", zone, testpg.TimeZone())
	}
	if offsetSeconds == 0 && os.Getenv(testpg.TimeZoneEnv) == "" {
		t.Fatalf("session time zone %q is UTC-equivalent; the naive-timestamp guard needs an offset", zone)
	}
}

func TestPostgresNaiveChatTimestampsAreUTCWallClock(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(auth.ContextWithUser(context.Background(), auth.User{ID: "1"}), 20*time.Second)
	defer cancel()

	before := time.Now().UTC()
	conversation, err := NewConversationsRepo(pool).Create(ctx, chatFoldersCorpusProject,
		conversations.Conversation{Name: "naive-utc"})
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	folder, err := NewFoldersRepo(pool).Create(ctx, chatFoldersCorpusProject, folders.Folder{Name: "naive-utc"})
	if err != nil {
		t.Fatalf("create folder: %v", err)
	}
	// A message group bumps its conversation's updated_at (tenant/0145's
	// trigger, rewritten by tenant/0150).
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, meta, is_streaming, created_at)
SELECT gen_random_uuid(), m.participant_id, m.conversation_id, '{}'::jsonb, FALSE, now() AT TIME ZONE 'UTC'
FROM p_1.chat_participant_mapping m WHERE m.conversation_id = $1::integer LIMIT 1`, conversation.ID); err != nil {
		t.Fatalf("insert message group: %v", err)
	}
	after := time.Now().UTC()

	stamps := []struct {
		name  string
		query string
		id    string
	}{
		{"chat_conversations.created_at", `SELECT created_at FROM p_1.chat_conversations WHERE id = $1::integer`, conversation.ID},
		{"chat_conversations.updated_at", `SELECT updated_at FROM p_1.chat_conversations WHERE id = $1::integer`, conversation.ID},
		{"chat_participant_mapping.created_at", `SELECT min(created_at) FROM p_1.chat_participant_mapping WHERE conversation_id = $1::integer`, conversation.ID},
		{"chat_conversation_folders.created_at", `SELECT created_at FROM p_1.chat_conversation_folders WHERE id = $1::integer`, folder.ID},
	}
	for _, stamp := range stamps {
		name := stamp.name
		var stored *time.Time
		if err := pool.QueryRow(ctx, stamp.query, stamp.id).Scan(&stored); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if stored == nil {
			t.Fatalf("%s is NULL", name)
		}
		// pgx returns a naive timestamp as that wall clock in time.UTC.
		if stored.Before(before.Add(-naiveUTCSkew)) || stored.After(after.Add(naiveUTCSkew)) {
			t.Errorf("%s = %v, want the UTC wall clock between %v and %v (session %s)",
				name, *stored, before, after, testpg.TimeZone())
		}
	}
	if !conversation.CreatedAt.IsZero() &&
		(conversation.CreatedAt.Before(before.Add(-naiveUTCSkew)) || conversation.CreatedAt.After(after.Add(naiveUTCSkew))) {
		t.Errorf("Create returned created_at %v, want the UTC wall clock", conversation.CreatedAt)
	}
}

// naiveWriterCase drives one writer of a naive column and names where the
// value it wrote is read back.
type naiveWriterCase struct {
	name  string
	write func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, groupID int64)
	read  string // one naive timestamp; $1 is the seeded message group id
}

// The writers the review named, each driven through its own code on the
// Toronto session, and the stored value compared with the UTC clock. The
// probe (dbtest.NaiveClockProbeSQL) is switched off for these writes so the
// assertion is about the value itself, not about the probe.
//
// The other fixed writers are driven the same way elsewhere:
// TestPostgresRuntimeOutputWritersStoreTheUTCWallClock below (node recovery,
// runtime terminal: they need an admitted, claimed execution), and, because
// those packages import this one, auth's
// TestPostgresLoginStoresLastLoginAsTheUTCWallClock and moderation's
// TestDecisionNotificationIsStampedWithTheUTCWallClock.
//
// This list is not exhaustive: the probe is the behavioural guard over every
// writer the suite drives. A writer that no test drives is the remaining gap.
func TestPostgresNamedNaiveWritersStoreTheUTCWallClock(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(auth.ContextWithUser(context.Background(), auth.User{ID: "1"}), 30*time.Second)
	defer cancel()

	conversation, err := NewConversationsRepo(pool).Create(ctx, chatFoldersCorpusProject,
		conversations.Conversation{Name: "naive-writers"})
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	var groupID int64
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_message_group (uuid, author_participant_id, conversation_id, meta, is_streaming, created_at)
SELECT gen_random_uuid(), m.participant_id, m.conversation_id, '{}'::jsonb, TRUE, now() AT TIME ZONE 'UTC'
FROM p_1.chat_participant_mapping m WHERE m.conversation_id = $1::integer LIMIT 1
RETURNING id`, conversation.ID).Scan(&groupID); err != nil {
		t.Fatalf("seed message group: %v", err)
	}
	const mentionRecipient int64 = 9301

	cases := []naiveWriterCase{
		{
			name: "agent_skills.go persistCurrentAgentTraceInvokedSkills -> chat_message_group.updated_at",
			write: func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, groupID int64) {
				withoutNaiveClockProbe(t, ctx, pool, func(tx pgx.Tx) error {
					return persistCurrentAgentTraceInvokedSkills(ctx, pgxExecutor{queryer: tx}, "p_1", groupID,
						json.RawMessage(`[{"skill_id":1,"name":"naive-utc-skill"}]`))
				})
			},
			read: `SELECT updated_at FROM p_1.chat_message_group WHERE id = $1`,
		},
		{
			name: "agent_stream_text.go appendCurrentAgentProvisionalText -> chat_message_items.created_at",
			write: func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, groupID int64) {
				withoutNaiveClockProbe(t, ctx, pool, func(tx pgx.Tx) error {
					return appendCurrentAgentProvisionalText(ctx, pgxExecutor{queryer: tx}, "p_1", groupID,
						"execution-naive-utc", 1, "streamed")
				})
			},
			read: `SELECT max(created_at) FROM p_1.chat_message_items WHERE message_group_id = $1`,
		},
		{
			name: "chat_mention_notification.go WriteChatMentionNotifications -> notifications.created_at and updated_at",
			write: func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, _ int64) {
				repo := NewChatMentionNotificationRepo(pool)
				if err := repo.WriteChatMentionNotifications(ctx, []agentexecutionapp.MentionNotification{{
					ProjectID: 1, UserID: mentionRecipient, ConversationUUID: conversation.UUID,
					MessageID: "naive-utc", SenderUserID: 1,
				}}); err != nil {
					t.Fatalf("write mention: %v", err)
				}
			},
			read: `SELECT LEAST(max(created_at), max(updated_at)) FROM centry.notifications WHERE user_id = 9301 AND $1::bigint > 0`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			before := time.Now().UTC()
			testCase.write(t, ctx, pool, groupID)
			after := time.Now().UTC()
			var stored *time.Time
			if err := pool.QueryRow(ctx, testCase.read, groupID).Scan(&stored); err != nil {
				t.Fatalf("read back: %v", err)
			}
			if stored == nil {
				t.Fatal("the writer stored NULL")
			}
			if stored.Before(before.Add(-naiveUTCSkew)) || stored.After(after.Add(naiveUTCSkew)) {
				t.Errorf("stored %v, want the UTC wall clock between %v and %v (session %s)",
					*stored, before, after, testpg.TimeZone())
			}
		})
	}
}

// The two runtime-output writers of chat_message_group.updated_at, each on a
// real admitted agent execution: the node-recovery pause projection
// (node_recovery_projection.go) and the runtime terminal
// (configuration_validation_results.go). Like the cases above, the probe is
// off for the write so the assertion is about the stored value.
func TestPostgresRuntimeOutputWritersStoreTheUTCWallClock(t *testing.T) {
	cases := []struct {
		name  string
		write func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, execution naiveRuntimeExecution)
	}{
		{
			name: "node_recovery_projection.go projectNodeRecovery -> chat_message_group.updated_at",
			write: func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, execution naiveRuntimeExecution) {
				claimID := execution.claimRunning(t, ctx, pool)
				_, receipt, _ := recoveryFixture(t)
				browser, err := json.Marshal(map[string]any{
					"type": "agent_node_recovery_required", "stream_id": execution.conversationID,
					"message_id": execution.responseID, "execution_generation": execution.clientGeneration,
					"sio_event":         "chat_predict",
					"response_metadata": map[string]any{"node_recovery_required_v1": json.RawMessage(receipt)},
				})
				if err != nil {
					t.Fatal(err)
				}
				frame := outputapp.NodeEventFrame{
					EventID:     "naive-utc-node-recovery-event",
					Fence:       runtimedomain.Fence{ExecutionID: execution.executionID, Generation: 1, CommandID: execution.commandID},
					BrowserData: browser,
				}
				withoutNaiveClockProbe(t, ctx, pool, func(tx pgx.Tx) error {
					return projectNodeRecovery(ctx, pgxExecutor{queryer: tx}, 1, frame, claimID)
				})
			},
		},
		{
			name: "configuration_validation_results.go persistCurrentAgentRuntimeTerminal -> chat_message_group.updated_at",
			write: func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, execution naiveRuntimeExecution) {
				withoutNaiveClockProbe(t, ctx, pool, func(tx pgx.Tx) error {
					return persistCurrentAgentRuntimeTerminal(ctx, pgxExecutor{queryer: tx}, 1,
						executiondomain.AgentApplicationCapability,
						outputRecord{ExecutionID: execution.executionID, Generation: 1},
						"RUNTIME_FAILED", "The run failed.")
				})
			},
		},
	}
	for index, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			pool := newMigratedPostgresIntegrationPool(t)
			seedCurrentActivitySchemas(t, pool)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			execution := naiveRuntimeExecution{
				conversationID:   fmt.Sprintf("10000000-0000-4000-8000-0000000009%02d", index),
				responseID:       fmt.Sprintf("20000000-0000-4000-8000-0000000009%02d", index),
				clientGeneration: fmt.Sprintf("30000000-0000-4000-8000-0000000009%02d", index),
			}
			admitted := admitPostgresAgentExecution(t, pool, execution.conversationID, execution.responseID, execution.clientGeneration)
			execution.executionID, execution.commandID = admitted.ExecutionID, admitted.CommandID
			seedCurrentAgentResponseGroup(t, pool, execution.conversationID, execution.responseID,
				execution.clientGeneration, execution.executionID)
			// Back-date the seeded stamp so a writer that left it alone fails too.
			if _, err := pool.Exec(ctx, `
UPDATE p_1.chat_message_group SET updated_at = (now() AT TIME ZONE 'UTC') - interval '2 days'
WHERE uuid::text = $1`, execution.responseID); err != nil {
				t.Fatal(err)
			}

			before := time.Now().UTC()
			testCase.write(t, ctx, pool, execution)
			after := time.Now().UTC()
			var stored *time.Time
			if err := pool.QueryRow(ctx, `SELECT updated_at FROM p_1.chat_message_group WHERE uuid::text = $1`,
				execution.responseID).Scan(&stored); err != nil {
				t.Fatalf("read back: %v", err)
			}
			if stored == nil {
				t.Fatal("the writer stored NULL")
			}
			if stored.Before(before.Add(-naiveUTCSkew)) || stored.After(after.Add(naiveUTCSkew)) {
				t.Errorf("stored %v, want the UTC wall clock between %v and %v (session %s)",
					*stored, before, after, testpg.TimeZone())
			}
		})
	}
}

// naiveRuntimeExecution is one admitted agent execution with its response
// message group.
type naiveRuntimeExecution struct {
	conversationID, responseID, clientGeneration string
	executionID, commandID                       string
}

// claimRunning gives the execution a live claim and the RUNNING state the
// node-recovery pause requires, and returns the claim id.
func (e naiveRuntimeExecution) claimRunning(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	claimID := "claim-" + e.executionID
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO elitea_runtime.workload_sessions (workload_session_id, workload_identity, producer_id, issued_at, expires_at)
VALUES ($1, 'spiffe://elitea.test/worker', 'rust-worker-1', clock_timestamp() - interval '1 minute', clock_timestamp() + interval '1 hour')`,
			[]any{"session-" + e.executionID}},
		{`INSERT INTO elitea_runtime.execution_claims (
    claim_id, execution_id, generation, workload_session_id, workload_identity, producer_id,
    claim_attempt, lease_epoch, fence_token, claimed_at, lease_expires_at
) VALUES ($1, $2, 1, $3, 'spiffe://elitea.test/worker', 'rust-worker-1', 1, 1, $4, clock_timestamp(), clock_timestamp() + interval '10 minutes')`,
			[]any{claimID, e.executionID, "session-" + e.executionID, make([]byte, 32)}},
		{`UPDATE elitea_runtime.execution_jobs
SET state = 'RUNNING', desired_state = 'RUNNING', invocation_state = 'MAY_HAVE_STARTED'
WHERE execution_id = $1 AND generation = 1`, []any{e.executionID}},
	} {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("claim the execution: %v", err)
		}
	}
	return claimID
}

// The probe refuses the bug it exists for, a bare now() into a naive column
// on the Toronto session, and lets the UTC stamp and a back-dated value pass.
func TestPostgresNaiveClockProbeRefusesASessionLocalStamp(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	insert := func(name, value string) error {
		_, err := pool.Exec(ctx, `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source, created_at)
VALUES (gen_random_uuid(), $1, 1, 'elitea', `+value+`)`, name)
		return err
	}
	if err := insert("bare now", "now()"); err == nil || !strings.Contains(err.Error(), "session-local clock") {
		t.Fatalf("a bare now() into chat_conversations.created_at was accepted: %v", err)
	}
	if err := insert("column default", "DEFAULT"); err == nil || !strings.Contains(err.Error(), "session-local clock") {
		t.Fatalf("the column DEFAULT now() was accepted: %v", err)
	}
	if err := insert("utc now", "now() AT TIME ZONE 'UTC'"); err != nil {
		t.Fatalf("the UTC stamp was refused: %v", err)
	}
	if err := insert("back-dated", "(now() AT TIME ZONE 'UTC') - interval '3 days'"); err != nil {
		t.Fatalf("a deliberately back-dated value was refused: %v", err)
	}
}

// withoutNaiveClockProbe runs write in a transaction with the probe off, so
// the caller can read back the value a writer stored.
func withoutNaiveClockProbe(t *testing.T, ctx context.Context, pool *pgxpool.Pool, write func(pgx.Tx) error) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('elitea.naive_clock_probe', 'off', true)`); err != nil {
		t.Fatal(err)
	}
	if err := write(tx); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
