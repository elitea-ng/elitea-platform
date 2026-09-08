package repos

// GetToolAnalytics against the real ledgered schema (issue 618).
//
// It runs on the migrated template, so shared migration 0119 is exercised as
// DDL rather than as a string in a Go file, and the ledger row the availability
// flag is read from is the one the runner actually wrote.
//
// It SKIPS without ELITEA_TEST_DATABASE_URL, and a skipped Go test prints the
// same `ok` as a passing one — so each case below asserts something a skip
// could not have produced.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/analytics"
)

func toolAnalyticsWindow() analytics.QueryParams {
	now := time.Now().UTC()
	return analytics.QueryParams{
		ProjectID: "1",
		From:      now.Add(-24 * time.Hour),
		To:        now.Add(24 * time.Hour),
		Period:    "custom",
	}
}

func recordToolCallForTest(t *testing.T, pool *pgxpool.Pool, record ToolCallRecord) {
	t.Helper()
	store, err := newPostgresSharedStore(pool)
	require.NoError(t, err)
	require.NoError(t, RecordToolCall(context.Background(), store, record))
}

// TestGetToolAnalytics_AttributesBothProducers is the assertion that
// discriminates a real fix from a chat_message_trace_step join: a tool call made
// OUTSIDE a chat turn appears in the breakdown.
//
// Before this work the endpoint answered ErrNoSource for every window, so the
// `Available` flag and a non-empty explicit_run row are both states the old code
// could not reach.
func TestGetToolAnalytics_AttributesBothProducers(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)
	started := time.Now().UTC().Add(-time.Hour)

	// OUTSIDE a chat turn — the toolkit test button / MCP tools/call.
	recordToolCallForTest(t, pool, ToolCallRecord{
		ProjectID: 1, Source: ToolCallSourceExplicitRun, SourceRef: "exec-outside-1",
		ToolkitID: 7, ToolkitType: "github", ToolName: "list_issues",
		StartedAt: started, FinishedAt: started.Add(400 * time.Millisecond),
		ActorUserID: 1, ExecutionID: "exec-outside-1",
	})
	recordToolCallForTest(t, pool, ToolCallRecord{
		ProjectID: 1, Source: ToolCallSourceExplicitRun, SourceRef: "exec-outside-2",
		ToolkitID: 7, ToolkitType: "github", ToolName: "list_issues",
		StartedAt: started, FinishedAt: started.Add(600 * time.Millisecond),
		IsError: true, ActorUserID: 1, ExecutionID: "exec-outside-2",
	})
	// INSIDE a chat turn — the agent's own tool call. It knows a toolkit name
	// and no id, which is why the row groups on both.
	recordToolCallForTest(t, pool, ToolCallRecord{
		ProjectID: 1, Source: ToolCallSourceAgentTurn, SourceRef: "41:run-a",
		ToolkitName: "Jira", ToolName: "create_issue",
		StartedAt:   started, FinishedAt: started.Add(time.Second),
	})

	breakdown, err := repo.GetToolAnalytics(context.Background(), toolAnalyticsWindow())
	require.NoError(t, err)
	require.True(t, breakdown.Available, "the producer is running, so the window is answerable")
	require.Len(t, breakdown.Tools, 2)
	require.False(t, breakdown.Truncated)

	byName := map[string]analytics.ToolAnalytics{}
	for _, tool := range breakdown.Tools {
		byName[tool.ToolName] = tool
	}

	outside, ok := byName["list_issues"]
	require.True(t, ok, "a tool call made outside a chat turn must appear: %+v", breakdown.Tools)
	require.Equal(t, int64(2), outside.RunCount)
	require.Equal(t, int64(1), outside.ErrorCount)
	require.Equal(t, 50.0, outside.ErrorRate)
	require.Equal(t, "7", outside.ToolkitID)
	require.InDelta(t, 500.0, outside.AvgDuration, 1.0)

	inside, ok := byName["create_issue"]
	require.True(t, ok, "a tool call made inside a chat turn must appear too")
	require.Equal(t, int64(1), inside.RunCount)
	require.Equal(t, int64(0), inside.ErrorCount)
	// The agent turn knows a toolkit NAME and no id. The row reports what the
	// producer knew rather than resolving the name to an id by guessing.
	require.Equal(t, "", inside.ToolkitID)
	require.Equal(t, "Jira", inside.ToolkitName)
	require.InDelta(t, 1000.0, inside.AvgDuration, 1.0)
}

// TestGetToolAnalytics_CountsAReplayedCallOnce covers the acceptance criterion
// "one inside a chat turn is attributed once, not twice". A streaming turn
// re-projects the same tool call on every partial message.
func TestGetToolAnalytics_CountsAReplayedCallOnce(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)
	started := time.Now().UTC().Add(-30 * time.Minute)

	recordToolCallForTest(t, pool, ToolCallRecord{
		ProjectID: 1, Source: ToolCallSourceAgentTurn, SourceRef: "77:run-b",
		ToolName: "search", StartedAt: started,
	})
	// The same call again, now finished. Same natural key.
	recordToolCallForTest(t, pool, ToolCallRecord{
		ProjectID: 1, Source: ToolCallSourceAgentTurn, SourceRef: "77:run-b",
		ToolName: "search", StartedAt: started, FinishedAt: started.Add(250 * time.Millisecond),
	})

	breakdown, err := repo.GetToolAnalytics(context.Background(), toolAnalyticsWindow())
	require.NoError(t, err)
	require.True(t, breakdown.Available)
	require.Len(t, breakdown.Tools, 1)
	require.Equal(t, int64(1), breakdown.Tools[0].RunCount, "a re-projected call is one call")
	require.InDelta(t, 250.0, breakdown.Tools[0].AvgDuration, 1.0,
		"the second write must settle the duration the first one did not have")
}

// TestGetToolAnalytics_HonoursTheWindow pins the filter the handler's period
// parameter feeds. Without it the tab would report a project's whole history
// under whatever range the user picked.
func TestGetToolAnalytics_HonoursTheWindow(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)
	now := time.Now().UTC()

	recordToolCallForTest(t, pool, ToolCallRecord{
		ProjectID: 1, Source: ToolCallSourceExplicitRun, SourceRef: "exec-recent",
		ToolName: "recent", StartedAt: now.Add(-time.Hour), FinishedAt: now.Add(-time.Hour),
		ExecutionID: "exec-recent",
	})
	recordToolCallForTest(t, pool, ToolCallRecord{
		ProjectID: 1, Source: ToolCallSourceExplicitRun, SourceRef: "exec-old",
		ToolName: "old", StartedAt: now.Add(-72 * time.Hour), FinishedAt: now.Add(-72 * time.Hour),
		ExecutionID: "exec-old",
	})

	breakdown, err := repo.GetToolAnalytics(context.Background(), toolAnalyticsWindow())
	require.NoError(t, err)
	require.Len(t, breakdown.Tools, 1)
	require.Equal(t, "recent", breakdown.Tools[0].ToolName)
}

// TestGetToolAnalytics_ScopesToTheProject pins the one project column the table
// exists to have. A row belonging to another project must not reach this
// breakdown.
func TestGetToolAnalytics_ScopesToTheProject(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)
	started := time.Now().UTC().Add(-time.Hour)

	recordToolCallForTest(t, pool, ToolCallRecord{
		ProjectID: 1, Source: ToolCallSourceExplicitRun, SourceRef: "exec-mine",
		ToolName: "mine", StartedAt: started, ExecutionID: "exec-mine",
	})
	recordToolCallForTest(t, pool, ToolCallRecord{
		ProjectID: 2, Source: ToolCallSourceExplicitRun, SourceRef: "exec-theirs",
		ToolName: "theirs", StartedAt: started, ExecutionID: "exec-theirs",
	})

	breakdown, err := repo.GetToolAnalytics(context.Background(), toolAnalyticsWindow())
	require.NoError(t, err)
	require.Len(t, breakdown.Tools, 1)
	require.Equal(t, "mine", breakdown.Tools[0].ToolName)
}

// TestGetToolAnalytics_RefusesAWindowThatPredatesTheProducer is the no-zero-fill
// contract at the repository.
//
// The producer began when shared migration 0119 was applied. A window that
// closed before that has no tool data, which is NOT the same claim as "no tool
// ran" — so Available is false and Tools stays nil for the handler to omit.
func TestGetToolAnalytics_RefusesAWindowThatPredatesTheProducer(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)

	recordToolCallForTest(t, pool, ToolCallRecord{
		ProjectID: 1, Source: ToolCallSourceExplicitRun, SourceRef: "exec-now",
		ToolName: "now", StartedAt: time.Now().UTC(), ExecutionID: "exec-now",
	})

	params := toolAnalyticsWindow()
	params.From = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	params.To = time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)

	breakdown, err := repo.GetToolAnalytics(context.Background(), params)
	require.NoError(t, err, "an unanswerable window is not a failure")
	require.False(t, breakdown.Available)
	require.Nil(t, breakdown.Tools, "no empty list: the handler must have nothing to publish")
}

// TestGetToolAnalytics_AvailableWithNoRowsIsAMeasurement is the third state the
// issue requires to stay distinguishable from the other two: the producer WAS
// running and nothing ran.
func TestGetToolAnalytics_AvailableWithNoRowsIsAMeasurement(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)

	breakdown, err := repo.GetToolAnalytics(context.Background(), toolAnalyticsWindow())
	require.NoError(t, err)
	require.True(t, breakdown.Available)
	require.NotNil(t, breakdown.Tools)
	require.Empty(t, breakdown.Tools)
}

// TestGetToolAnalytics_RefusesWhenTheTableIsAbsent keeps the NAMED absence. A
// database that has not run 0119 gets the honest refusal, not an empty tab.
func TestGetToolAnalytics_RefusesWhenTheTableIsAbsent(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repo := NewAnalyticsRepo(pool)

	_, err := pool.Exec(context.Background(), `DROP TABLE elitea_runtime.tool_call_records`)
	require.NoError(t, err)

	_, err = repo.GetToolAnalytics(context.Background(), toolAnalyticsWindow())
	require.ErrorIs(t, err, analytics.ErrNoSource)
	require.Contains(t, err.Error(), "0119")
}

// TestRecordToolCall_RefusesAnIncompleteRecord keeps the producer loud. A row
// with no tool name or no clock is not written and not silently skipped: a
// producer that stops recording is the failure this table exists to prevent.
func TestRecordToolCall_RefusesAnIncompleteRecord(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	store, err := newPostgresSharedStore(pool)
	require.NoError(t, err)

	for name, record := range map[string]ToolCallRecord{
		"no project":   {Source: ToolCallSourceExplicitRun, SourceRef: "a", ToolName: "t", StartedAt: time.Now()},
		"no source":    {ProjectID: 1, SourceRef: "a", ToolName: "t", StartedAt: time.Now()},
		"bad source":   {ProjectID: 1, Source: "guessed", SourceRef: "a", ToolName: "t", StartedAt: time.Now()},
		"no reference": {ProjectID: 1, Source: ToolCallSourceAgentTurn, ToolName: "t", StartedAt: time.Now()},
		"no tool":      {ProjectID: 1, Source: ToolCallSourceAgentTurn, SourceRef: "a", StartedAt: time.Now()},
		"no clock":     {ProjectID: 1, Source: ToolCallSourceAgentTurn, SourceRef: "a", ToolName: "t"},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, RecordToolCall(context.Background(), store, record), ErrInvalidToolCallRecord)
		})
	}
}
