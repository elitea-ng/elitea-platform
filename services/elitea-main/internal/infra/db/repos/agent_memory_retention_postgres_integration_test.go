package repos

// Issue 1658: the retention sweep purges the memory of a deleted agent after
// the retention period (ADR-0042). It SKIPS without ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

type memoryAuditCapture struct{ events []securityaudit.SecurityEvent }

func (c *memoryAuditCapture) Append(_ context.Context, _ pgx.Tx, e securityaudit.SecurityEvent) error {
	c.events = append(c.events, e)
	return nil
}

func TestPurgeDeletedAgents_KeepsWithinThePeriodAndPurgesAfter(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const project, gone, live, held int64 = 1, 990181, 990182, 990183
	cleanup := func() {
		for _, q := range []string{
			`DELETE FROM p_1.agent_memory_entries WHERE application_id IN (990181, 990182, 990183)`,
			`DELETE FROM p_1.agent_memory_subject_optouts WHERE application_id IN (990181, 990182, 990183)`,
			`DELETE FROM p_1.agent_memory_orphans WHERE application_id IN (990181, 990182, 990183)`,
			`DELETE FROM p_1.applications WHERE id IN (990182)`,
			`DELETE FROM elitea_runtime.legal_holds WHERE project_id = 1 AND agent_id = 990183`,
		} {
			_, _ = pool.Exec(context.Background(), q)
		}
	}
	// The application tables are not in the ledgered schema (the agent
	// editor owns them): the sweep reads only applications.id.
	_, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS p_1.applications (
    id SERIAL PRIMARY KEY, name VARCHAR(128) NOT NULL, description VARCHAR(2304), owner_id INTEGER NOT NULL)`)
	require.NoError(t, err)
	cleanup()
	t.Cleanup(cleanup)
	_, err = pool.Exec(ctx, `INSERT INTO p_1.applications (id, name, description, owner_id) VALUES (990182, 'Live', 'fixture', 11)`)
	require.NoError(t, err)
	for _, id := range []int64{gone, live, held} {
		_, err = pool.Exec(ctx, `
INSERT INTO p_1.agent_memory_entries (application_id, kind, content, status, written_by_kind)
VALUES ($1, 'fact', 'a note', 'active', 'agent')`, id)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO p_1.agent_memory_subject_optouts (application_id, user_id) VALUES ($1, 5)`, id)
		require.NoError(t, err)
	}
	_, err = pool.Exec(ctx, `
INSERT INTO elitea_runtime.legal_holds (scope, project_id, agent_id, reason, created_by)
VALUES ('agent', 1, 990183, 'litigation', 1)`)
	require.NoError(t, err)

	audit := &memoryAuditCapture{}
	store, err := NewAgentMemory(pool, audit)
	require.NoError(t, err)
	now := time.Now()
	remaining := func(id int64) (entries, optouts int) {
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM p_1.agent_memory_entries WHERE application_id = $1`, id).Scan(&entries))
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM p_1.agent_memory_subject_optouts WHERE application_id = $1`, id).Scan(&optouts))
		return entries, optouts
	}

	// The purge is driven by the orphan rows: before the backfill (the agent
	// was deleted before tenant/0171) it sees nothing.
	cleared, err := store.PurgeDeletedAgents(ctx, project, now.Add(24*time.Hour), 10)
	require.NoError(t, err)
	require.Zero(t, cleared)
	// The bounded backfill records the one agent without an application
	// (the live and the held-but-live agents have one... the held agent has
	// none, so it is recorded too) and is idempotent.
	recorded, err := store.BackfillDeletedAgents(ctx, project, 1)
	require.NoError(t, err)
	require.Equal(t, 1, recorded, "bounded by the limit")
	recorded, err = store.BackfillDeletedAgents(ctx, project, 10)
	require.NoError(t, err)
	require.Equal(t, 1, recorded, "the rest, never the recorded one twice")
	recorded, err = store.BackfillDeletedAgents(ctx, project, 10)
	require.NoError(t, err)
	require.Zero(t, recorded)

	// First sight of the orphans: within the period, nothing is purged.
	cleared, err = store.PurgeDeletedAgents(ctx, project, now.Add(-30*24*time.Hour), 10)
	require.NoError(t, err)
	require.Zero(t, cleared)
	entries, optouts := remaining(gone)
	require.Equal(t, 1, entries)
	require.Equal(t, 1, optouts)

	// The deletion was 40 days ago, the period is 30: the agent's memory
	// goes, the live agent's stays, the held agent's stays.
	_, err = pool.Exec(ctx, `UPDATE p_1.agent_memory_orphans SET first_seen_at = $1`, now.Add(-40*24*time.Hour))
	require.NoError(t, err)
	cleared, err = store.PurgeDeletedAgents(ctx, project, now.Add(-30*24*time.Hour), 10)
	require.NoError(t, err)
	require.Equal(t, 1, cleared)
	entries, optouts = remaining(gone)
	require.Zero(t, entries)
	require.Zero(t, optouts)
	for _, id := range []int64{live, held} {
		entries, optouts = remaining(id)
		require.Equal(t, 1, entries, "agent %d keeps its entries", id)
		require.Equal(t, 1, optouts, "agent %d keeps its opt-outs", id)
	}

	// One agent.memory.cleared event, for the purged agent, with its reason.
	require.Len(t, audit.events, 1)
	require.Equal(t, "agent.memory.cleared", audit.events[0].Action)
	require.Equal(t, "990181", audit.events[0].ResourceID)
	require.Equal(t, []string{"retention_after_deletion"}, audit.events[0].Reasons)

	// A second pass has nothing more to clear.
	cleared, err = store.PurgeDeletedAgents(ctx, project, now.Add(-30*24*time.Hour), 10)
	require.NoError(t, err)
	require.Zero(t, cleared)
}

// The agent delete records when its memory lost its agent.
func TestApplicationDeleteRecordsTheMemoryOrphan(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	seedCurrentAgentContinuationSchema(t, pool)
	_, err := pool.Exec(context.Background(), `CREATE TABLE IF NOT EXISTS p_1.application_version_tag_association (
    version_id INTEGER NOT NULL)`)
	require.NoError(t, err)
	ctx := context.Background()
	const id int64 = 990184
	cleanup := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM p_1.agent_memory_entries WHERE application_id = 990184`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM p_1.agent_memory_orphans WHERE application_id = 990184`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM p_1.applications WHERE id = 990184`)
	}
	cleanup()
	t.Cleanup(cleanup)
	_, err = pool.Exec(ctx, `INSERT INTO p_1.applications (id, name, description, owner_id) VALUES (990184, 'Doomed', 'fixture', 11)`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO p_1.agent_memory_entries (application_id, kind, content, status, written_by_kind)
VALUES (990184, 'fact', 'a note', 'active', 'agent')`)
	require.NoError(t, err)

	require.NoError(t, NewApplicationsRepo(pool).Delete(ctx, "1", "990184"))

	var seen time.Time
	require.NoError(t, pool.QueryRow(ctx, `SELECT first_seen_at FROM p_1.agent_memory_orphans WHERE application_id = $1`, id).Scan(&seen))
	require.WithinDuration(t, time.Now(), seen, time.Minute)
}
