package migrate

import (
	"context"
	"fmt"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/testpg"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

func TestAgentStateHistoryPreservesLegacyLangGraphTables(t *testing.T) {
	databaseURL := testpg.DSN(os.Getenv("ELITEA_AGENTSTATE_TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("set ELITEA_AGENTSTATE_TEST_DATABASE_URL to run the agentstate migration story")
	}
	ctx := context.Background()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	require.NoError(t, err)
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	require.NoError(t, err)
	databaseName := fmt.Sprintf(
		"elitea_agentstate_migrate_%d_%d",
		os.Getpid(),
		time.Now().UnixNano(),
	)
	_, err = adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{databaseName}.Sanitize())
	require.NoError(t, err)

	stateConfig := adminConfig.Copy()
	stateConfig.ConnConfig.Database = databaseName
	statePool, err := pgxpool.NewWithConfig(ctx, stateConfig)
	require.NoError(t, err)
	defer func() {
		statePool.Close()
		_, dropErr := adminPool.Exec(
			context.Background(),
			"DROP DATABASE "+pgx.Identifier{databaseName}.Sanitize()+" WITH (FORCE)",
		)
		adminPool.Close()
		if dropErr != nil {
			t.Errorf("drop isolated agentstate database: %v", dropErr)
		}
	}()

	_, err = statePool.Exec(ctx, `
CREATE TABLE public.checkpoints (thread_id TEXT);
CREATE TABLE public.checkpoint_blobs (thread_id TEXT);
CREATE TABLE public.checkpoint_writes (thread_id TEXT);
CREATE TABLE public.checkpoint_migrations (version INTEGER);`)
	require.NoError(t, err)

	runner := New(statePool, platformmigrations.Files)
	require.NoError(t, runner.ApplyAgentState(ctx))
	require.NoError(t, runner.CheckHead(ctx, ScopeAgentState, "agentstate"))

	assertTableCount(t, ctx, statePool, "elitea_runtime", "agent_%", 7)
	assertTableCount(t, ctx, statePool, "public", "checkpoint%", 4)
	var centrySchemas int
	require.NoError(t, statePool.QueryRow(ctx, `
SELECT count(*) FROM information_schema.schemata WHERE schema_name = 'centry'`).Scan(&centrySchemas))
	require.Zero(t, centrySchemas)
}

func assertTableCount(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	schema string,
	pattern string,
	want int,
) {
	t.Helper()
	var got int
	require.NoError(t, pool.QueryRow(ctx, `
SELECT count(*)
FROM information_schema.tables
WHERE table_schema = $1 AND table_name LIKE $2`, schema, pattern).Scan(&got))
	require.Equal(t, want, got)
}
