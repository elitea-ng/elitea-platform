package repos

// The agent memory list against the real ledgered schema (#1608).
//
// The list's WHERE text carries ILIKE wildcards ('%'). It was once spliced
// through fmt.Sprintf, which read them as verbs and sent SQL that did not
// parse, so every list answered 500 while the handler tests, on a fake
// store, passed. This runs the real query. It SKIPS without
// ELITEA_TEST_DATABASE_URL, and a skip prints the same `ok`, so it asserts
// rows a skip could not have produced.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmemory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

func TestListMemory_RunsTheRealQueryWithEveryFilter(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const project, application int64 = 1, 990161
	_, err := pool.Exec(ctx, `DELETE FROM p_1.agent_memory_entries WHERE application_id = $1`, application)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO p_1.agent_memory_entries (application_id, kind, content, subject_user_id, status, written_by_kind)
VALUES ($1, 'preference', 'Prefers 100% short answers', 7, 'active', 'agent'),
       ($1, 'fact', 'Uses Go', NULL, 'pending', 'person')`, application)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM p_1.agent_memory_entries WHERE application_id = $1`, application)
	})

	store := &AgentMemory{pool: pool}
	manager := AgentMemoryViewer{UserID: 1, Manager: true}

	rows, total, err := store.ListMemory(ctx, project, application, manager, AgentMemoryFilter{})
	require.NoError(t, err)
	require.Equal(t, 2, total)
	require.Len(t, rows, 2)

	// The search escapes its own wildcard: "100%" matches the text, not everything.
	rows, total, err = store.ListMemory(ctx, project, application, manager, AgentMemoryFilter{Query: "100%"})
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Equal(t, "Prefers 100% short answers", rows[0].Content)

	rows, _, err = store.ListMemory(ctx, project, application, manager, AgentMemoryFilter{
		Source: "person", Status: "pending", Kind: "fact",
		CreatedAfter: time.Now().Add(-time.Hour), CreatedBefore: time.Now().Add(time.Hour), Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "Uses Go", rows[0].Content)

	// A person who is not a manager does not see a note about someone else.
	_, total, err = store.ListMemory(ctx, project, application, AgentMemoryViewer{UserID: 3}, AgentMemoryFilter{})
	require.NoError(t, err)
	require.Equal(t, 1, total)
}

// Issue 1660: the auto limit holds at run time. A version that reached the
// store with write auto (import, fork, publish, a later tier raise) runs as
// auto_attended once the agent's tier is above the limit.
func TestCapAutoWriteDowngradesAboveTheLimitAtRunTime(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const project, application int64 = 1, 990185
	store := &AgentMemory{pool: pool}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM elitea_lifecycle.agent_registry WHERE tenant_project_id = 1 AND application_id = $1`, application)
		_, _ = pool.Exec(context.Background(), `DELETE FROM centry.platform_config WHERE section = 'agent_lifecycle' AND key = 'memory_auto_max_tier'`)
	})
	auto := agentmemory.Settings{Enabled: true, Write: agentmemory.WriteAuto}
	setTier := func(tier string) {
		_, err := pool.Exec(ctx, `
INSERT INTO elitea_lifecycle.agent_registry (tenant_project_id, application_id, risk_tier)
VALUES (1, $1, $2)
ON CONFLICT (tenant_project_id, application_id) DO UPDATE SET risk_tier = $2`, application, tier)
		require.NoError(t, err)
	}

	// No registry row is low: auto stays (the default limit is medium).
	require.Equal(t, agentmemory.WriteAuto, store.capAutoWrite(ctx, project, application, auto).Write)
	setTier("medium")
	require.Equal(t, agentmemory.WriteAuto, store.capAutoWrite(ctx, project, application, auto).Write)
	// A later tier raise above the limit: the run downgrades.
	setTier("high")
	got := store.capAutoWrite(ctx, project, application, auto)
	require.Equal(t, agentmemory.WriteAutoAttended, got.Write)
	require.True(t, got.Enabled, "only the write mode changes")
	// A mode that is not auto is never touched.
	review := agentmemory.Settings{Enabled: true, Write: agentmemory.WriteReview}
	require.Equal(t, agentmemory.WriteReview, store.capAutoWrite(ctx, project, application, review).Write)
	// Raising the limit lifts it again.
	_, err := pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS centry.platform_config (
    section VARCHAR(64) NOT NULL, key VARCHAR(128) NOT NULL, value JSONB NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT now(), updated_by VARCHAR(255), PRIMARY KEY (section, key))`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO centry.platform_config (section, key, value, updated_at)
VALUES ('agent_lifecycle', 'memory_auto_max_tier', '"critical"', now() AT TIME ZONE 'UTC')
ON CONFLICT (section, key) DO UPDATE SET value = '"critical"'`)
	require.NoError(t, err)
	require.Equal(t, agentmemory.WriteAuto, store.capAutoWrite(ctx, project, application, auto).Write)
}

func TestPostgresCapAutoWriteForAgentsUsesMaxRiskTier(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const project, rootApp, nestedApp int64 = 1, 990186, 990187
	store := &AgentMemory{pool: pool}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM elitea_lifecycle.agent_registry WHERE tenant_project_id = 1 AND application_id IN ($1, $2)`, rootApp, nestedApp)
		_, _ = pool.Exec(context.Background(), `DELETE FROM centry.platform_config WHERE section = 'agent_lifecycle' AND key = 'memory_auto_max_tier'`)
	})
	auto := agentmemory.Settings{Enabled: true, Write: agentmemory.WriteAuto}
	setTier := func(appID int64, tier string) {
		_, err := pool.Exec(ctx, `
INSERT INTO elitea_lifecycle.agent_registry (tenant_project_id, application_id, risk_tier)
VALUES (1, $1, $2)
ON CONFLICT (tenant_project_id, application_id) DO UPDATE SET risk_tier = $2`, appID, tier)
		require.NoError(t, err)
	}
	// Low root + low nested -> stays auto (default limit is medium).
	setTier(rootApp, "low")
	setTier(nestedApp, "low")
	require.Equal(t, agentmemory.WriteAuto, store.capAutoWriteForAgents(ctx, project, []int64{rootApp, nestedApp}, auto).Write)

	// High root + low nested -> downgraded to auto_attended (no laundering through low child).
	setTier(rootApp, "high")
	require.Equal(t, agentmemory.WriteAutoAttended, store.capAutoWriteForAgents(ctx, project, []int64{rootApp, nestedApp}, auto).Write)

	// Low root + high nested -> downgraded to auto_attended.
	setTier(rootApp, "low")
	setTier(nestedApp, "high")
	require.Equal(t, agentmemory.WriteAutoAttended, store.capAutoWriteForAgents(ctx, project, []int64{rootApp, nestedApp}, auto).Write)

	// FU353-02: low root -> high intermediate hop -> low nested -> downgraded to auto_attended.
	const midApp int64 = 990187
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM elitea_lifecycle.agent_registry WHERE tenant_project_id = 1 AND application_id = $1`, midApp)
	})
	setTier(rootApp, "low")
	setTier(midApp, "high")
	setTier(nestedApp, "low")
	require.Equal(t, agentmemory.WriteAutoAttended, store.capAutoWriteForAgents(ctx, project, []int64{rootApp, midApp, nestedApp}, auto).Write)
}

func TestPostgresNestedAgentMemoryIsolationAndOptOut(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const project, rootApp, nestedApp, siblingApp int64 = 1, 990188, 990189, 990190
	store, err := ComposeAgentMemory(pool)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM p_1.agent_memory_entries WHERE application_id IN ($1, $2, $3)`, rootApp, nestedApp, siblingApp)
		_, _ = pool.Exec(context.Background(), `DELETE FROM p_1.agent_memory_subject_optouts WHERE application_id IN ($1, $2, $3)`, rootApp, nestedApp, siblingApp)
	})
	run := storage.AgentMemoryRun{
		ProjectID:                  project,
		ApplicationID:              nestedApp,
		DelegatedFromApplicationID: rootApp,
		ActorID:                    7,
		ConversationUUID:           "11111111-1111-4111-8111-111111111111",
		ExecutionID:                "exec-fu353-iso",
		Settings:                   agentmemory.DefaultNew,
		Attended:                   false,
		Present:                    map[int64]string{7: "Dana"},
	}
	id, err := store.InsertAgentMemory(ctx, run, storage.AgentMemoryWrite{
		Kind: agentmemory.KindFact, Content: "nested only fact", Status: agentmemory.StatusActive,
	})
	require.NoError(t, err)
	require.Greater(t, id, int64(0))

	// Only nestedApp sees the entry; rootApp and siblingApp see none.
	nestedEntries, err := store.ActiveAgentMemory(ctx, project, nestedApp)
	require.NoError(t, err)
	require.Len(t, nestedEntries, 1)
	rootEntries, err := store.ActiveAgentMemory(ctx, project, rootApp)
	require.NoError(t, err)
	require.Empty(t, rootEntries)
	siblingEntries, err := store.ActiveAgentMemory(ctx, project, siblingApp)
	require.NoError(t, err)
	require.Empty(t, siblingEntries)

	// Root cannot forget nestedApp's entry.
	rootRun := run
	rootRun.ApplicationID = rootApp
	forgotten, err := store.ForgetAgentMemory(ctx, rootRun, id)
	require.NoError(t, err)
	require.False(t, forgotten)

	// Opt-out on nestedApp blocks about_user writes to nestedApp.
	_, err = pool.Exec(ctx, `INSERT INTO p_1.agent_memory_subject_optouts (application_id, user_id) VALUES ($1, $2)`, nestedApp, 7)
	require.NoError(t, err)
	_, err = store.InsertAgentMemory(ctx, run, storage.AgentMemoryWrite{
		Kind: agentmemory.KindPreference, Content: "prefers yaml", SubjectUserID: 7, Status: agentmemory.StatusPending,
	})
	require.ErrorIs(t, err, storage.ErrAgentMemorySubjectOptedOut)

	// Nested target itself can forget its own entry.
	forgotten, err = store.ForgetAgentMemory(ctx, run, id)
	require.NoError(t, err)
	require.True(t, forgotten)
}
