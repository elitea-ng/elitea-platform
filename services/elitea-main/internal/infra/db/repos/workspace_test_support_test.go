package repos

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspacestore/workspacestoretest"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// workspaceThreadFixture is one Local work thread of project 1.
type workspaceThreadFixture struct {
	thread          workspacestore.Thread
	conversationID  int64
	userParticipant int64
}

// seedLocalWorkThread creates a `local_work` conversation authored by owner,
// with the owner's user participant and a model (dummy) participant.
func seedLocalWorkThread(t *testing.T, pool *pgxpool.Pool, owner int64, source string) workspaceThreadFixture {
	t.Helper()
	ctx := context.Background()
	var fixture workspaceThreadFixture
	fixture.thread.ProjectID = 1
	ensureWorkspaceTestUser(t, pool, owner)
	if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_conversations (uuid, name, author_id, source, created_at)
VALUES (gen_random_uuid(), 'local work', $1, $2, now() AT TIME ZONE 'UTC') RETURNING id, uuid::text`, owner, source).
		Scan(&fixture.conversationID, &fixture.thread.ConversationUUID); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	for _, participant := range []struct{ entity, meta string }{
		{"user", `{"id": ` + strconv.FormatInt(owner, 10) + `}`}, {"dummy", `{}`},
	} {
		var id int64
		if err := pool.QueryRow(ctx, `
INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta, meta)
VALUES (gen_random_uuid(), $1, $2::jsonb, '{}'::json) RETURNING id`, participant.entity, participant.meta).Scan(&id); err != nil {
			t.Fatalf("seed participant: %v", err)
		}
		if _, err := pool.Exec(ctx, `
INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, created_at) VALUES ($1, $2, now() AT TIME ZONE 'UTC')`, fixture.conversationID, id); err != nil {
			t.Fatalf("map participant: %v", err)
		}
		if participant.entity == "user" {
			fixture.userParticipant = id
		}
	}
	return fixture
}

// ensureWorkspaceTestUser creates the thread owner when the database has a
// users table: the sweeper erases a thread whose owner is absent or suspended
// (user offboarding). The migrated test database has no users table unless a
// test creates one (withWorkspaceUsersTable).
func ensureWorkspaceTestUser(t *testing.T, pool *pgxpool.Pool, owner int64) {
	t.Helper()
	ctx := context.Background()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.auth_core__user') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		return
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO public.auth_core__user (id, email, name) VALUES ($1, $2, $3) ON CONFLICT (id) DO NOTHING`,
		owner, fmt.Sprintf("owner-%d@example.com", owner), fmt.Sprintf("Owner %d", owner)); err != nil {
		t.Fatalf("seed the owner: %v", err)
	}
}

// withWorkspaceUsersTable creates the auth migrations' users table with the
// columns the offboarding check reads.
func withWorkspaceUsersTable(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
CREATE TABLE IF NOT EXISTS public.auth_core__user (
    id integer PRIMARY KEY, email text NOT NULL, name text, suspended boolean NOT NULL DEFAULT false)`); err != nil {
		t.Fatalf("create the users table: %v", err)
	}
}

// seedHandoffRow puts a thread directly into a state, for the store tests.
func seedHandoffRow(t *testing.T, pool *pgxpool.Pool, thread workspacestore.Thread, owner int64,
	state workspacestore.State, epoch int64, hasUpstream bool, running string) {
	t.Helper()
	ensureWorkspaceTestUser(t, pool, owner)
	var runningValue *string
	if running != "" {
		runningValue = &running
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO elitea_runtime.workspace_handoffs
    (project_id, conversation_uuid, handoff_id, state, epoch, owner_user_id, has_upstream, running_execution_id)
VALUES ($1, $2::uuid, gen_random_uuid(), $3, $4, $5, $6, $7)
ON CONFLICT (project_id, conversation_uuid) DO UPDATE
SET state = EXCLUDED.state, epoch = EXCLUDED.epoch, running_execution_id = EXCLUDED.running_execution_id`,
		thread.ProjectID, thread.ConversationUUID, string(state), epoch, owner, hasUpstream, runningValue); err != nil {
		t.Fatalf("seed handoff row: %v", err)
	}
}

func digestOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func sha1Of(seed string) string {
	// Any 40-hex string is a valid ref value for the store.
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])[:40]
}

// uploadSegment stores bytes at the segment key, as the desktop's presigned
// PUT does.
func uploadSegment(t *testing.T, store *workspacestoretest.Store, thread workspacestore.Thread, content []byte, at time.Time) string {
	t.Helper()
	sha := digestOf(content)
	ref, err := storage.NewObjectRef(strconv.FormatInt(thread.ProjectID, 10), storage.WorkspaceBucketName,
		workspacestore.ObjectKey(thread.ConversationUUID, sha))
	if err != nil {
		t.Fatal(err)
	}
	store.Seed(ref, content, at)
	return sha
}

type fixedLimits struct{ limits workspacestore.Limits }

func (f fixedLimits) Limits(context.Context, int64) (workspacestore.Limits, error) {
	return f.limits, nil
}

func defaultWorkspaceLimits() workspacestore.Limits {
	return workspacestore.Limits{
		MaxSegmentBytes: 200 << 20, MaxFileBytes: 25 << 20, MaxFiles: 20000,
		MaxUnpackedBytes: 2 << 30, MaxProjectWorkspaceBytes: 50 << 30,
	}
}

type recordingAudit struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *recordingAudit) Record(_ context.Context, event audit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recordingAudit) actions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []string{}
	for _, event := range r.events {
		out = append(out, event.Action)
	}
	return out
}

func newWorkspaceStoreService(t *testing.T, pool *pgxpool.Pool, store *workspacestoretest.Store, limits workspacestore.Limits) (*workspacestore.Service, *recordingAudit) {
	t.Helper()
	repo, err := NewWorkspaceStoreRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &recordingAudit{}
	service, err := workspacestore.NewService(repo,
		workspacestore.Buckets{Default: workspacestore.PhysicalStore{Name: "elitea-test", Store: store}},
		fixedLimits{limits}, workspacestore.RecorderAudit{Recorder: recorder}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return service, recorder
}
