package runtimecomposition

// The Local work fence through the composition root (ADR-0034 decision 7):
// New builds its agent start resolver with newCurrentAgentStartRepository,
// which opens the fence for a handed-off CLOUD_IDLE thread ONLY when the
// cloud executor seam is complete. A unit test of the resolver alone cannot
// see that wiring (unit-suite-blind-to-composition-root); this one drives
// the real resolver over a real migrated database with each seam shape.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/dbtest"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/testpg"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

// workspaceFenceSeedSQL is the minimum legacy schema the ledgered corpus
// alters (the same arrangement as the repos package's integration seed).
const workspaceFenceSeedSQL = `
CREATE SCHEMA centry;
CREATE TABLE centry.project (
    id INTEGER PRIMARY KEY,
    create_success BOOLEAN NOT NULL DEFAULT TRUE,
    suspended BOOLEAN NOT NULL DEFAULT FALSE
);
INSERT INTO centry.project (id, create_success, suspended) VALUES (1, TRUE, FALSE);
CREATE TABLE centry.notifications (
    id SERIAL PRIMARY KEY, uuid UUID NOT NULL UNIQUE, is_seen BOOLEAN NOT NULL,
    project_id INTEGER NOT NULL, user_id INTEGER NOT NULL, meta JSONB NOT NULL,
    event_type VARCHAR NOT NULL, created_at TIMESTAMP NOT NULL DEFAULT now(), updated_at TIMESTAMP
);
CREATE SCHEMA p_1;
CREATE TABLE p_1.application_versions (
    id SERIAL PRIMARY KEY,
    llm_settings JSONB NOT NULL DEFAULT '{}'::jsonb
);
CREATE TABLE p_1.skills (
    id SERIAL PRIMARY KEY, name VARCHAR(128) NOT NULL, description VARCHAR(2304) NOT NULL,
    owner_id INTEGER NOT NULL DEFAULT 1, author_id INTEGER NOT NULL DEFAULT 1, meta JSONB DEFAULT '{}'::jsonb
);
CREATE TABLE p_1.skill_versions (
    id SERIAL PRIMARY KEY, skill_id INTEGER NOT NULL REFERENCES p_1.skills(id) ON DELETE CASCADE,
    name VARCHAR(128) NOT NULL DEFAULT 'base', instructions TEXT NOT NULL,
    author_id INTEGER NOT NULL DEFAULT 1, meta JSONB DEFAULT '{}'::jsonb
);
CREATE TABLE p_1.configuration (
    id SERIAL PRIMARY KEY, uuid UUID NOT NULL UNIQUE, project_id INTEGER NOT NULL, label VARCHAR,
    elitea_title VARCHAR NOT NULL UNIQUE, type VARCHAR NOT NULL, section VARCHAR NOT NULL,
    data JSONB NOT NULL, meta JSONB NOT NULL, shared BOOLEAN NOT NULL, status_ok BOOLEAN NOT NULL,
    status_logs TEXT, source VARCHAR NOT NULL, author_id INTEGER,
    created_at TIMESTAMP NOT NULL DEFAULT now(), updated_at TIMESTAMP
);`

var (
	workspaceFenceTemplateOnce sync.Once
	workspaceFenceTemplate     string
	workspaceFenceTemplateErr  error
)

func newWorkspaceFencePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := testpg.DSN(os.Getenv("ELITEA_TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL to run the Local work fence composition test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	adminConfig.MaxConns = 2
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adminPool.Close)
	workspaceFenceTemplateOnce.Do(func() {
		buildCtx, buildCancel := dbtest.BuildContext(context.Background())
		defer buildCancel()
		workspaceFenceTemplate, workspaceFenceTemplateErr = dbtest.EnsureTemplate(buildCtx, adminPool, dbtest.Spec{
			Files: platformmigrations.Files, Seed: workspaceFenceSeedSQL, Tenants: []int64{1},
		})
	})
	if workspaceFenceTemplateErr != nil {
		t.Fatalf("build the template: %v", workspaceFenceTemplateErr)
	}
	name := fmt.Sprintf("elitea_ws_fence_%d_%d", os.Getpid(), time.Now().UnixNano())
	if err := dbtest.CreateFromTemplate(ctx, adminPool, workspaceFenceTemplate, name); err != nil {
		t.Fatal(err)
	}
	config := adminConfig.Copy()
	config.ConnConfig.Database = name
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer dropCancel()
		_, _ = adminPool.Exec(dropCtx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})
	return pool
}

// fenceSeam is a complete seam; the fence reads only its presence.
type fenceSeam struct{}

func (fenceSeam) FetchPlan(context.Context, workspaceruntime.ClaimRef, uint64) (workspaceruntime.FetchPlan, error) {
	return workspaceruntime.FetchPlan{}, nil
}
func (fenceSeam) OpenSegment(context.Context, workspaceruntime.ClaimRef, string) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}
func (fenceSeam) PutSegment(context.Context, workspaceruntime.ClaimRef, string, uint64, io.Reader) (workspaceruntime.UploadResult, error) {
	return workspaceruntime.UploadResult{}, nil
}
func (fenceSeam) Publish(context.Context, workspaceruntime.ClaimRef, workspaceruntime.PublishRequest) (workspaceruntime.PublishResult, error) {
	return workspaceruntime.PublishResult{}, nil
}
func (fenceSeam) EnvSecret(context.Context, workspaceruntime.ClaimRef) (map[string]string, error) {
	return nil, nil
}
func (fenceSeam) ResolveCloudTurn(context.Context, workspaceruntime.TurnClaim) (workspaceruntime.Turn, bool, error) {
	return workspaceruntime.Turn{}, false, nil
}
func (fenceSeam) HandedOff(context.Context, workspaceruntime.TurnClaim) (bool, error) {
	return false, nil
}
func (fenceSeam) EffectiveCloudPolicy(context.Context, int64, string) (workspaceruntime.Policy, error) {
	return workspaceruntime.Policy{}, nil
}
func (fenceSeam) FinishExecution(context.Context, string) error { return nil }
func (fenceSeam) RecordEvent(context.Context, workspaceruntime.ClaimRef, workspaceruntime.ExecutorEvent) error {
	return nil
}
func (fenceSeam) ReconcileFinished(context.Context) (int, error)        { return 0, nil }
func (fenceSeam) WrapUpRequested(context.Context, string) (bool, error) { return false, nil }

func completeSeam() WorkspaceDependencies {
	return WorkspaceDependencies{Store: fenceSeam{}, Turns: fenceSeam{}, Policies: fenceSeam{}, Lifecycle: fenceSeam{}}
}

func TestComposedLocalWorkFenceOpensForCloudIdleOnlyWithTheExecutorSeam(t *testing.T) {
	pool := newWorkspaceFencePool(t)
	ctx := context.Background()
	var conversation string
	var conversationID, dummy int64
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source, created_at)
VALUES (gen_random_uuid(), 'local work', 11, 'local_work', now() AT TIME ZONE 'UTC') RETURNING id, uuid::text`).
		Scan(&conversationID, &conversation); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta, meta)
VALUES (gen_random_uuid(), 'dummy', '{}'::jsonb, '{}'::json) RETURNING id`).Scan(&dummy); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, created_at) VALUES ($1, $2, now() AT TIME ZONE 'UTC')`,
		conversationID, dummy); err != nil {
		t.Fatal(err)
	}
	setState := func(state, running string) {
		t.Helper()
		var runningValue *string
		if running != "" {
			runningValue = &running
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.workspace_handoffs (project_id, conversation_uuid, handoff_id, state, epoch, owner_user_id, running_execution_id)
VALUES (1, $1::uuid, gen_random_uuid(), $2, 2, 11, $3)
ON CONFLICT (project_id, conversation_uuid) DO UPDATE SET state = EXCLUDED.state, running_execution_id = EXCLUDED.running_execution_id`,
			conversation, state, runningValue); err != nil {
			t.Fatal(err)
		}
	}
	start := func(workspace WorkspaceDependencies) error {
		t.Helper()
		resolver, err := newCurrentAgentStartRepository(pool, 1, workspace)
		if err != nil {
			t.Fatal(err)
		}
		_, err = resolver.ResolveCurrentAdhoc(ctx, agentexecutionapp.CurrentAdhocStartRequest{
			ProjectID: 1, ActorUserID: 11, ConversationUUID: conversation, TargetParticipantID: dummy,
			QuestionID: uuid.NewString(), UserInput: "continue in the cloud",
			LLMSettings: []byte(`{}`), MCPTokens: []byte(`{}`),
		})
		return err
	}

	setState("CLOUD_IDLE", "")
	// The runtime configured with the executor: the fence opens. The turn is
	// then resolved as any other (here the fixture has no model, so it is
	// refused for that, never as a Local work thread).
	if err := start(completeSeam()); errors.Is(err, agentexecutionapp.ErrLocalWorkThread) || errors.Is(err, agentexecutionapp.ErrInvalidCurrentAgentStart) {
		t.Fatalf("CLOUD_IDLE with the executor = %v, want the fence open", err)
	}
	// No executor: a handed-off thread stays fenced.
	if err := start(WorkspaceDependencies{}); !errors.Is(err, agentexecutionapp.ErrLocalWorkThread) {
		t.Fatalf("CLOUD_IDLE without the executor = %v, want local_work_thread", err)
	}
	// A partial seam is refused at start and never opens the fence.
	partial := WorkspaceDependencies{Store: fenceSeam{}, Turns: fenceSeam{}, Policies: fenceSeam{}}
	if err := partial.validate(); !errors.Is(err, errWorkspaceHalfComposed) {
		t.Fatalf("a partial seam validated: %v", err)
	}
	if err := start(partial); !errors.Is(err, agentexecutionapp.ErrLocalWorkThread) {
		t.Fatalf("CLOUD_IDLE with a partial seam = %v, want local_work_thread", err)
	}
	// A running cloud turn refuses a second one, with the executor.
	setState("CLOUD_RUNNING", "exec-1")
	if err := start(completeSeam()); !errors.Is(err, agentexecutionapp.ErrCloudTurnRunning) {
		t.Fatalf("CLOUD_RUNNING = %v, want cloud_turn_running", err)
	}
	// The settlement hook that ends cloud turns exists only with the seam.
	if len(workspaceSettlementHooks(completeSeam(), nil)) != 1 || len(workspaceSettlementHooks(WorkspaceDependencies{}, nil)) != 0 {
		t.Fatal("the cloud turn settlement hook is not composed with the seam alone")
	}
}
