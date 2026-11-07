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
