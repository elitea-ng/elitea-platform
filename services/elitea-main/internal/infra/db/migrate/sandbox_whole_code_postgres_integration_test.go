package migrate

import (
	"context"
	"fmt"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

func TestSandboxWholeCodeOwnerMigrationBootstrapAndUpgrade(t *testing.T) {
	manifest, err := LoadManifest(platformmigrations.Files, ScopeAgentState)
	require.NoError(t, err)
	require.Equal(t, int64(13), Head(manifest))
	prefix := fstest.MapFS{}
	for _, entry := range manifest {
		if entry.Version <= 12 {
			prefix[entry.Path] = &fstest.MapFile{Data: entry.SQL}
		}
	}
	for _, upgrade := range []bool{false, true} {
		t.Run(fmt.Sprintf("upgrade_%t", upgrade), func(t *testing.T) {
			pool := sandboxWholeCodeIsolatedDatabase(t)
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			seed := func() {
				_, err := pool.Exec(ctx, `INSERT INTO elitea_runtime.sandbox_jobs
					(tenant_id,project_id,job_key,request_digest)
					VALUES ('code-migration-fixture',1,decode(repeat('01',32),'hex'),decode(repeat('02',32),'hex'))`)
				require.NoError(t, err)
			}
			var before string
			if upgrade {
				require.NoError(t, New(pool, prefix).ApplyAgentState(ctx))
				seed()
				require.NoError(t, pool.QueryRow(ctx, `SELECT to_jsonb(j)::text FROM elitea_runtime.sandbox_jobs j`).Scan(&before))
			}
			runner := New(pool, platformmigrations.Files)
			require.NoError(t, runner.ApplyAgentState(ctx))
			require.NoError(t, runner.CheckHead(ctx, ScopeAgentState, "agentstate"))
			if !upgrade {
				seed()
			} else {
				var after string
				require.NoError(t, pool.QueryRow(ctx, `SELECT (to_jsonb(j)-ARRAY[
					'code_recovery_binding_json','code_recovery_receipt_json','code_recovery_cleanup_at',
					'code_recovery_cleanup_failure','code_platform_binding_json'])::text
					FROM elitea_runtime.sandbox_jobs j`).Scan(&after))
				require.Equal(t, before, after)
			}
			var additionsNull bool
			require.NoError(t, pool.QueryRow(ctx, `SELECT code_recovery_binding_json IS NULL
				AND code_recovery_receipt_json IS NULL AND code_recovery_cleanup_at IS NULL
				AND code_recovery_cleanup_failure IS NULL AND code_platform_binding_json IS NULL
				FROM elitea_runtime.sandbox_jobs`).Scan(&additionsNull))
			require.True(t, additionsNull)
			require.NoError(t, runner.ApplyAgentState(ctx))
			for name, want := range map[string]string{
				"code_recovery_binding_json": "text", "code_recovery_receipt_json": "text",
				"code_recovery_cleanup_at":      "timestamp with time zone",
				"code_recovery_cleanup_failure": "text", "code_platform_binding_json": "text",
			} {
				var got, nullable string
				require.NoError(t, pool.QueryRow(ctx, `SELECT data_type,is_nullable FROM information_schema.columns
					WHERE table_schema='elitea_runtime' AND table_name='sandbox_jobs' AND column_name=$1`, name).Scan(&got, &nullable))
				require.Equal(t, want, got)
				require.Equal(t, "YES", nullable)
			}
			var indexPresent bool
			require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass('elitea_runtime.sandbox_jobs_code_no_effect_cleanup') IS NOT NULL`).Scan(&indexPresent))
			require.True(t, indexPresent)
			rows, err := pool.Query(ctx, `SELECT tenant_id,project_id,job_key,request_digest FROM elitea_runtime.sandbox_jobs WHERE owner_id=$1 AND phase='failed' AND failure_code=$2 AND dispatched_at IS NULL AND NOT cancellation_requested AND code_recovery_receipt_json IS NOT NULL AND runtime_id IS NOT NULL AND code_recovery_cleanup_at IS NULL AND (lease_until IS NULL OR lease_until <= clock_timestamp()) ORDER BY updated_at,tenant_id,project_id,job_key LIMIT $3`, "code-migration-owner", "recovery_verified_no_effect", int64(32))
			require.NoError(t, err)
			require.False(t, rows.Next())
			require.NoError(t, rows.Err())
			rows.Close()

			// These are storage constraint fixtures. They grant no runtime authority.
			for _, tc := range []struct {
				name, set string
				valid     bool
			}{
				{"binding_empty", "code_recovery_binding_json=''", false},
				{"binding_over", "code_recovery_binding_json=repeat('x',8193)", false},
				{"receipt_empty", "code_recovery_receipt_json=''", false},
				{"receipt_over", "code_recovery_receipt_json=repeat('x',1048577)", false},
				{"platform_empty", "code_platform_binding_json=''", false},
				{"platform_over", "code_platform_binding_json=repeat('x',8193)", false},
				{"receipt_unbound", "code_recovery_binding_json=NULL", false},
				{"receipt_live", "phase='reserved',failure_code=NULL", false},
				{"receipt_dispatched", "dispatched_at=clock_timestamp()", false},
				{"receipt_wrong_failure", "failure_code='other_failure'", false},
				{"cleanup_unproved", "code_recovery_receipt_json=NULL,code_recovery_cleanup_at=clock_timestamp()", false},
				{"cleanup_unknown_failure", "code_recovery_cleanup_failure='unknown'", false},
				{"binding_ceiling", "code_recovery_binding_json=repeat('x',8192)", true},
				{"receipt_ceiling", "code_recovery_receipt_json=repeat('x',1048576)", true},
				{"platform_ceiling", "code_platform_binding_json=repeat('x',8192)", true},
				{"termination_retry", "code_recovery_cleanup_failure='termination_unconfirmed'", true},
				{"cleanup_retry", "code_recovery_cleanup_failure='cleanup_unconfirmed'", true},
				{"cleanup_done", "code_recovery_cleanup_at=clock_timestamp()", true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					tx, err := pool.Begin(ctx)
					require.NoError(t, err)
					defer func() { _ = tx.Rollback(context.Background()) }()
					_, err = tx.Exec(ctx, `UPDATE elitea_runtime.sandbox_jobs SET phase='failed',
						failure_code='recovery_verified_no_effect',code_recovery_binding_json='{}',
						code_recovery_receipt_json='{}',runtime_id='storage-fixture-runtime',
						owner_id='code-migration-owner',lease_epoch=1,
						lease_until=clock_timestamp()-interval '1 second'`)
					require.NoError(t, err)
					_, err = tx.Exec(ctx, "UPDATE elitea_runtime.sandbox_jobs SET "+tc.set)
					if tc.valid {
						require.NoError(t, err)
					} else {
						var pgErr *pgconn.PgError
						require.ErrorAs(t, err, &pgErr)
						require.Equal(t, "23514", pgErr.Code)
					}
				})
			}
		})
	}
}

func sandboxWholeCodeIsolatedDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("ELITEA_AGENTSTATE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set ELITEA_AGENTSTATE_TEST_DATABASE_URL for the isolated Code owner migration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(url)
	require.NoError(t, err)
	admin, err := pgxpool.NewWithConfig(ctx, adminConfig)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	name := fmt.Sprintf("elitea_code_owner_migrate_%d_%d", os.Getpid(), time.Now().UnixNano())
	_, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	require.NoError(t, err)
	childConfig := adminConfig.Copy()
	childConfig.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, childConfig)
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		cleanupCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_, err := admin.Exec(cleanupCtx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		if err != nil {
			t.Errorf("drop isolated Code owner migration database: %v", err)
		}
	})
	return pool
}
