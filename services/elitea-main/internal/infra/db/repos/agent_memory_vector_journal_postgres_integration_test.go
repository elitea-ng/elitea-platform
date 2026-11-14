package repos

// The agent memory vector journal against the real ledgered schema (issue
// 1657, shared/0248 and tenant/0248): the trigger writes one row for each
// change an index can see, in the change's own transaction, and the claim,
// close and release run as SQL. It SKIPS without ELITEA_TEST_DATABASE_URL,
// and a skip prints the same `ok`, so every check asserts a row a skip could
// not have produced.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentmemoryvector"
)

func TestAgentMemoryVectorJournal_TheTriggerJournalsEveryChangeTheIndexCanSee(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const project, application int64 = 1, 990657
	cleanup := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM p_1.agent_memory_entries WHERE application_id = $1`, application)
		_, _ = pool.Exec(context.Background(), `DELETE FROM centry.agent_memory_vector_journal WHERE application_id = $1`, application)
	}
	cleanup()
	t.Cleanup(cleanup)

	journal, err := NewAgentMemoryVectorJournal(pool)
	require.NoError(t, err)
	rowsFor := func() int {
		var count int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM centry.agent_memory_vector_journal WHERE project_id = $1 AND application_id = $2`,
			project, application).Scan(&count))
		return count
	}
	insert := func(kind, content, status string) int64 {
		var id int64
		require.NoError(t, pool.QueryRow(ctx, `
INSERT INTO p_1.agent_memory_entries (application_id, kind, content, status, written_by_kind)
VALUES ($1, $2, $3, $4, 'agent') RETURNING id`, application, kind, content, status).Scan(&id))
		return id
	}
	generation := func(id int64) int64 {
		var value int64
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT generation FROM centry.agent_memory_vector_journal WHERE project_id = $1 AND entry_id = $2`,
			project, id).Scan(&value))
		return value
	}

	// A pending entry and a cursor are never the index's: no row.
	pending := insert("fact", "waits for approval", "pending")
	insert("cursor", "last seen 12", "active")
	require.Equal(t, 0, rowsFor())

	// An active entry is journalled, with the agent it belongs to.
	active := insert("fact", "deploys run on friday", "active")
	require.Equal(t, 1, rowsFor())
	jobs, err := journal.Claim(ctx, 1000, time.Minute)
	require.NoError(t, err)
	var mine []int64
	for _, job := range jobs {
		if job.ApplicationID == application {
			require.Equal(t, project, job.ProjectID)
			require.Equal(t, int64(1), job.Generation)
			require.Equal(t, 1, job.Attempts)
			mine = append(mine, job.EntryID)
		}
	}
	require.Equal(t, []int64{active}, mine)

	// A claimed row is leased: a second claim does not take it again.
	again, err := journal.Claim(ctx, 1000, time.Minute)
	require.NoError(t, err)
	for _, job := range again {
		require.NotEqual(t, active, job.EntryID, "a leased row was claimed twice")
	}

	// The entry as the index must hold it.
	entries, err := journal.Entries(ctx, project, []int64{active, pending})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "deploys run on friday", entries[active].Content)
	require.Equal(t, application, entries[active].ApplicationID)

	// An edit while the row is worked reopens it at a higher generation, and
	// closing at the generation that was read leaves it open.
	_, err = pool.Exec(ctx, `UPDATE p_1.agent_memory_entries SET content = 'deploys run on thursday', version = version + 1 WHERE id = $1`, active)
	require.NoError(t, err)
	require.Equal(t, int64(2), generation(active))
	require.NoError(t, journal.Complete(ctx, jobs))
	require.Equal(t, 1, rowsFor(), "a row reopened by a later change must stay")

	// A pin changes nothing the index holds: the generation does not move.
	_, err = pool.Exec(ctx, `UPDATE p_1.agent_memory_entries SET pinned = true, updated_at = clock_timestamp() WHERE id = $1`, active)
	require.NoError(t, err)
	require.Equal(t, int64(2), generation(active))

	// The reopened row is due at once (the trigger cleared its lease). A
	// failure releases it after a pause, with the class of the failure.
	jobs, err = journal.Claim(ctx, 1000, time.Minute)
	require.NoError(t, err)
	mineJobs := filterAgent(jobs, application)
	require.Len(t, mineJobs, 1)
	require.Equal(t, int64(2), mineJobs[0].Generation)
	require.NoError(t, journal.Fail(ctx, mineJobs, "embedding unavailable; retry", time.Hour))
	var lastError string
	var due bool
	require.NoError(t, pool.QueryRow(ctx, `
SELECT last_error, next_attempt_at <= now() FROM centry.agent_memory_vector_journal WHERE project_id = $1 AND entry_id = $2`,
		project, active).Scan(&lastError, &due))
	require.Equal(t, "embedding unavailable; retry", lastError)
	require.False(t, due, "a failed row must wait out its backoff")

	// Archiving takes the entry out of the active set and journals it, so its
	// point is removed; deleting an archived entry journals nothing new.
	_, err = pool.Exec(ctx, `UPDATE p_1.agent_memory_entries SET status = 'archived' WHERE id = $1`, active)
	require.NoError(t, err)
	require.Equal(t, int64(3), generation(active))
	entries, err = journal.Entries(ctx, project, []int64{active})
	require.NoError(t, err)
	require.Empty(t, entries, "an archived entry must not be embedded")
	_, err = pool.Exec(ctx, `DELETE FROM p_1.agent_memory_entries WHERE id = $1`, active)
	require.NoError(t, err)
	require.Equal(t, int64(3), generation(active))

	// Deleting active entries (clear, opt-out, purge, forget) journals each.
	first := insert("episode", "one", "active")
	second := insert("preference", "two", "active")
	_, err = pool.Exec(ctx, `DELETE FROM p_1.agent_memory_entries WHERE application_id = $1`, application)
	require.NoError(t, err)
	// The insert wrote generation 1 and the delete reopened it.
	require.Equal(t, int64(2), generation(first))
	require.Equal(t, int64(2), generation(second))
	entries, err = journal.Entries(ctx, project, []int64{first, second})
	require.NoError(t, err)
	require.Empty(t, entries, "a deleted entry is no entry")
}

func filterAgent(jobs []agentmemoryvector.Job, application int64) []agentmemoryvector.Job {
	var out []agentmemoryvector.Job
	for _, job := range jobs {
		if job.ApplicationID == application {
			out = append(out, job)
		}
	}
	return out
}

func TestAgentMemoryVectorJournal_ANewModelQueuesTheProjectsEntriesAgain(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx := context.Background()
	const project, application int64 = 1, 990658
	cleanup := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM p_1.agent_memory_entries WHERE application_id = $1`, application)
		_, _ = pool.Exec(context.Background(), `DELETE FROM centry.agent_memory_vector_journal WHERE application_id = $1`, application)
		_, _ = pool.Exec(context.Background(), `DELETE FROM centry.agent_memory_vector_models WHERE project_id = $1`, project)
	}
	cleanup()
	t.Cleanup(cleanup)
	journal, err := NewAgentMemoryVectorJournal(pool)
	require.NoError(t, err)

	var id int64
	require.NoError(t, pool.QueryRow(ctx, `
INSERT INTO p_1.agent_memory_entries (application_id, kind, content, status, written_by_kind)
VALUES ($1, 'fact', 'one', 'active', 'agent') RETURNING id`, application).Scan(&id))
	_, err = pool.Exec(ctx, `DELETE FROM centry.agent_memory_vector_journal WHERE entry_id = $1`, id)
	require.NoError(t, err)

	changed, err := journal.NoteModel(ctx, project, "model-a")
	require.NoError(t, err)
	require.False(t, changed, "the first record is not a change")
	changed, err = journal.NoteModel(ctx, project, "model-a")
	require.NoError(t, err)
	require.False(t, changed)
	changed, err = journal.NoteModel(ctx, project, "model-b")
	require.NoError(t, err)
	require.True(t, changed)

	require.NoError(t, journal.Requeue(ctx, project))
	var generation int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT generation FROM centry.agent_memory_vector_journal WHERE project_id = $1 AND entry_id = $2`,
		project, id).Scan(&generation))
	require.Equal(t, int64(1), generation)
}
