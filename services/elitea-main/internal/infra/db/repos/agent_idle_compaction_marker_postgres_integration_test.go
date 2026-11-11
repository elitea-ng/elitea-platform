package repos

// The idle compaction run's own marker (shared 0240, #1921) against a real
// PostgreSQL. Requires ELITEA_TEST_DATABASE_URL; every test skips without it.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/analytics"
)

// The admission marks the idle run's own job; the source's job is not.
func TestIdleCompactionAdmissionMarksItsOwnJob(t *testing.T) {
	h := newIdleHarness(t)
	source := h.settledAnswer(h.newResponse(), h.agent)
	require.True(t, h.arm(source))
	h.makeDue(source)
	created, err := h.admitter.Sweep(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, created, "state %q", h.checkState(source))
	idle := h.idleExecution(source)
	require.Equal(t, 1, h.count(`SELECT count(*) FROM elitea_runtime.execution_jobs WHERE execution_id = $1 AND idle_compaction`, idle))
	require.Equal(t, 0, h.count(`SELECT count(*) FROM elitea_runtime.execution_jobs WHERE execution_id = $1 AND idle_compaction`, source))
}

// The analytics keep an idle run's calls in the idle_compaction bucket, and
// out of the active-user figures, after the source's job is pruned and its
// agent_idle_compactions row is CASCADE-deleted with it.
func TestIdleCompactionCallsStayTaggedAfterTheSourceIsPruned(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	const (
		source = "cccccccccccccccccccccccccccccccc"
		idle   = "dddddddddddddddddddddddddddddddd"
	)
	seedOriginExecutionAs(t, pool, 1, source, "manual", "7", now.Add(-time.Minute), nil)
	seedOriginExecutionAs(t, pool, 1, idle, "manual", "8", now.Add(-time.Minute), nil)
	_, err := pool.Exec(ctx, `
INSERT INTO elitea_runtime.agent_idle_compactions
    (project_id, conversation_uuid, participant_id, source_execution_id, source_generation, request_started_at,
     cache_ttl_seconds, due_at, idempotency_key, state, idle_execution_id)
VALUES (1, gen_random_uuid(), 1, $1, 1, $3, 300, $3, 'idle-compaction/' || $1, 'DONE', $2)`,
		source, idle, now.Add(-time.Minute))
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE elitea_runtime.execution_jobs SET idle_compaction = true WHERE execution_id = $1`, idle)
	require.NoError(t, err)
	plantRunCall(t, pool, 1, 8, idle, now, "gpt-4o", 200, 100, 100)

	// The source's job is pruned; its idle row goes with it (shared 0230).
	_, err = pool.Exec(ctx, `DELETE FROM elitea_runtime.execution_jobs WHERE execution_id = $1`, source)
	require.NoError(t, err)
	var rows int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM elitea_runtime.agent_idle_compactions WHERE idle_execution_id = $1`, idle).Scan(&rows))
	require.Zero(t, rows, "the idle row CASCADE-deletes with the source's job")

	params := analytics.QueryParams{ProjectID: "1", From: now.Add(-time.Hour), To: now.Add(time.Hour)}
	summary, err := repo.GetUsageSummary(ctx, params)
	require.NoError(t, err)
	require.EqualValues(t, 0, summary.ActiveUsers, "an idle run is still no person's activity")
	require.Len(t, summary.Automated, 1)
	require.Equal(t, "idle_compaction", summary.Automated[0].TriggerOrigin)
	require.EqualValues(t, 1, summary.Automated[0].LLMCalls)

	execution, err := repo.GetExecutionAnalytics(ctx, "1", idle)
	require.NoError(t, err)
	require.Equal(t, "idle_compaction", execution.TriggerOrigin)
}
