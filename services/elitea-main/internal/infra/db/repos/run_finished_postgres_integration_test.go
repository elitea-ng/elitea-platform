package repos

// run_finished rows against a real PostgreSQL (ADR-0045 decision 5, #1437).
// Requires ELITEA_TEST_DATABASE_URL; every test skips without it.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	executionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/localturn"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runfinished"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

// runFinishedRows reads the run_finished rows of a chain root.
func runFinishedRows(t *testing.T, pool *pgxpool.Pool, root string) []runfinished.Meta {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT meta::text FROM centry.notifications
WHERE event_type = $1 AND meta ->> 'chain_root_execution_id' = $2
ORDER BY id`, runfinished.EventType, root)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []runfinished.Meta
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var meta runfinished.Meta
		if err := json.Unmarshal([]byte(raw), &meta); err != nil {
			t.Fatal(err)
		}
		out = append(out, meta)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// startCloudChain starts a cloud chain with a manual trigger.
func startCloudChain(t *testing.T, pool *pgxpool.Pool, root string, owner int64) {
	t.Helper()
	ctx := t.Context()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return startRunChain(ctx, tx, unattended.ChainStart{
			RootExecutionID: root, ProjectID: 1, OwnerUserID: owner,
			ConversationUUID: "7c1e2d3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f",
			Origin:           unattended.OriginCloud, TriggerOrigin: "manual",
			Grant:  unattended.Grant{GrantID: uuid.NewString(), ContinueStepLimit: true},
			Limits: unattended.Limits{MaxContinuations: 5, Deadline: time.Hour},
		})
	}); err != nil {
		t.Fatal(err)
	}
}

// TestPostgresRunFinishedChainEnd: the noise rule decides the row, the row
// commits with the end state, and a second end or a replayed write adds
// nothing.
func TestPostgresRunFinishedChainEnd(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	chains := NewRunChainsRepository(pool)
	const owner int64 = 4701

	cases := []struct {
		name, state, limit string
		age                time.Duration
		want               bool
	}{
		{name: "short completed", state: unattended.EndCompleted},
		{name: "cancelled", state: unattended.EndCancelled, age: time.Hour},
		{name: "long completed", state: unattended.EndCompleted, age: 3 * time.Minute, want: true},
		{name: "failed", state: unattended.EndFailed, want: true},
		{name: "waiting", state: unattended.EndWaiting, want: true},
		{name: "limit", state: unattended.EndLimitReached, limit: unattended.LimitSpend, want: true},
	}
	for index, test := range cases {
		root := strings.Repeat("e", 31) + string(rune('a'+index))
		startCloudChain(t, pool, root, owner)
		if test.age > 0 {
			if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.run_chains SET created_at = created_at - $2::interval WHERE root_execution_id = $1`,
				root, test.age.String()); err != nil {
				t.Fatal(err)
			}
		}
		ended, err := chains.EndChain(ctx, root, test.state, test.limit)
		if err != nil || !ended {
			t.Fatalf("%s: end = %v, %v", test.name, ended, err)
		}
		if again, err := chains.EndChain(ctx, root, unattended.EndFailed, ""); err != nil || again {
			t.Fatalf("%s: second end = %v, %v", test.name, again, err)
		}
		rows := runFinishedRows(t, pool, root)
		if !test.want {
			if len(rows) != 0 {
				t.Fatalf("%s: rows = %+v, want none", test.name, rows)
			}
			continue
		}
		if len(rows) != 1 {
			t.Fatalf("%s: rows = %+v, want one", test.name, rows)
		}
		got := rows[0]
		if got.Outcome != test.state || got.Limit != test.limit || got.ProjectID != 1 ||
			got.ExecutionID != root || got.Origin != runfinished.OriginCloud ||
			got.ConversationUUID != "7c1e2d3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f" {
			t.Fatalf("%s: meta = %+v", test.name, got)
		}
		// A replayed write of the same chain is a no-op.
		if err := insertRunFinished(ctx, pool, owner, got); err != nil {
			t.Fatal(err)
		}
		if again := runFinishedRows(t, pool, root); len(again) != 1 {
			t.Fatalf("%s: replay rows = %d", test.name, len(again))
		}
	}
	var user int64
	if err := pool.QueryRow(ctx, `
SELECT user_id FROM centry.notifications WHERE event_type = $1 LIMIT 1`, runfinished.EventType).Scan(&user); err != nil ||
		user != owner {
		t.Fatalf("owner = %d, %v", user, err)
	}
}

// TestPostgresRunFinishedHandedOffCloudChain: a short completed cloud chain
// on a handed-off thread writes its row.
func TestPostgresRunFinishedHandedOffCloudChain(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	root := strings.Repeat("f", 32)
	startCloudChain(t, pool, root, 4702)
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.workspace_handoffs (project_id, conversation_uuid, handoff_id, state, epoch, owner_user_id,
    has_upstream)
VALUES (1, '7c1e2d3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f', gen_random_uuid(), 'CLOUD_IDLE', 1, 4702, false)`); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRunChainsRepository(pool).EndChain(ctx, root, unattended.EndCompleted, ""); err != nil {
		t.Fatal(err)
	}
	if rows := runFinishedRows(t, pool, root); len(rows) != 1 || rows[0].Outcome != unattended.EndCompleted {
		t.Fatalf("rows = %+v", rows)
	}
}

// TestPostgresRunFinishedDesktopCommit: a desktop commit with chain_outcome
// writes exactly one row on the chain end; a replayed commit adds nothing.
func TestPostgresRunFinishedDesktopCommit(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	const user int64 = 4703
	conversation := seedLocalTurnConversation(t, pool, user)
	service := newUnattendedLocalTurnService(t, pool, "b7")
	started, err := service.Start(ctx, localturn.StartRequest{
		ProjectID: 1, ActorUserID: user, TokenID: "95", ConversationUUID: conversation,
		QuestionID: "41111111-2222-4333-8444-555555555551", UserInput: "work",
		Unattended: &unattended.Request{GrantID: "2a3b4c5d-6e7f-4a8b-9c0d-1e2f3a4b5c6d"},
	})
	if err != nil {
		t.Fatal(err)
	}
	commit := localturn.CommitRequest{
		ProjectID: 1, ActorUserID: user, Credential: localturn.Credential{TokenID: "95"},
		ExecutionID: started.ExecutionID, UserMessage: "work", AssistantMessage: "broke",
		ChainOutcome: unattended.EndFailed,
	}
	if _, err := service.Commit(ctx, commit); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Commit(ctx, commit); err != nil {
		t.Fatalf("replayed commit: %v", err)
	}
	rows := runFinishedRows(t, pool, started.ExecutionID)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want one", rows)
	}
	if rows[0].Outcome != unattended.EndFailed || rows[0].Origin != runfinished.OriginDesktop ||
		rows[0].ConversationUUID != conversation || rows[0].ExecutionID != started.ExecutionID {
		t.Fatalf("meta = %+v", rows[0])
	}
}

// TestPostgresRunFinishedSweep: a chain the sweep ends at its deadline
// writes its row.
func TestPostgresRunFinishedSweep(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	root := strings.Repeat("9", 32)
	startCloudChain(t, pool, root, 4704)
	if _, err := pool.Exec(ctx, `
UPDATE elitea_runtime.run_chains SET deadline = created_at WHERE root_execution_id = $1`, root); err != nil {
		t.Fatal(err)
	}
	swept, err := NewRunChainsRepository(pool).SweepChains(ctx, 10*time.Minute, 64)
	if err != nil || len(swept) != 1 {
		t.Fatalf("sweep = %v, %v", swept, err)
	}
	rows := runFinishedRows(t, pool, root)
	if len(rows) != 1 || rows[0].Outcome != unattended.EndLimitReached || rows[0].Limit != unattended.LimitDeadline {
		t.Fatalf("rows = %+v", rows)
	}
}

// TestPostgresRunFinishedScheduleRun: a settled schedule run without a
// chain writes one row for the policy's owner; a person's attended run on a
// thread that is not handed off writes none.
func TestPostgresRunFinishedScheduleRun(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := t.Context()
	repo, err := NewRunFinishedRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	conversation := "3d4e5f6a-7b8c-4d9e-8f0a-1b2c3d4e5f6a"
	seed := func(execution, origin string) {
		t.Helper()
		if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.execution_jobs (
    execution_id, generation, command_id, tenant_id, resource_project_id,
    projection_project_id, actor_id, principal_ref, capability_id,
    capability_version, input_bundle_id, request_digest, idempotency_scope,
    idempotency_key, state, desired_state, admitted_at, trigger_origin, settled_at
) VALUES (
    $1, 1, 'cmd-'||$1, '1', 1, 1, '4705', '4705', 'agent.execute.application.v1', 'v1', 'bundle-'||$1,
    decode(repeat('61', 32), 'hex'), 'scope-'||$1, 'key-'||$1, 'SUCCEEDED', 'RUNNING', now(), $2, now())`,
				execution, origin); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `
INSERT INTO elitea_runtime.agent_execution_jobs (
    execution_id, generation, capability_id, input_bundle_id, request_entry_id,
    client_stream_id, client_message_id, client_execution_generation, sio_event)
VALUES ($1, 1, 'agent.execute.application.v1', 'bundle-' || $1, 'request', $2, 'not-a-uuid', 'generation',
        'chat_predict')`, execution, conversation)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.unattended_run_policies
    (project_id, conversation_uuid, trigger_origin, version_id, owner_user_id, on_approval_required)
VALUES (1, $1::uuid, 'schedule', 41, 4706, 'fail')`, conversation); err != nil {
		t.Fatal(err)
	}
	scheduled := strings.Repeat("5", 32)
	attended := strings.Repeat("6", 32)
	seed(scheduled, "schedule")
	seed(attended, "manual")
	for range 2 {
		if err := repo.RecordSettledRun(ctx, scheduled, executionapp.SettlementSucceeded); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.RecordSettledRun(ctx, attended, executionapp.SettlementFailed); err != nil {
		t.Fatal(err)
	}
	rows := runFinishedRows(t, pool, scheduled)
	if len(rows) != 1 || rows[0].Outcome != runfinished.OutcomeCompleted || rows[0].ConversationUUID != conversation {
		t.Fatalf("schedule rows = %+v", rows)
	}
	var owner int64
	if err := pool.QueryRow(ctx, `
SELECT user_id FROM centry.notifications WHERE meta ->> 'chain_root_execution_id' = $1`, scheduled).Scan(&owner); err != nil ||
		owner != 4706 {
		t.Fatalf("owner = %d, %v", owner, err)
	}
	if rows := runFinishedRows(t, pool, attended); len(rows) != 0 {
		t.Fatalf("attended rows = %+v", rows)
	}
}
