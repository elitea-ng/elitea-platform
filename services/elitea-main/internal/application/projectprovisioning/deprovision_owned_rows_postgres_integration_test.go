package projectprovisioning_test

// C1, found during the 2026-10 regression cleanup: deleting a project left
// its artifact bucket rows (three of them not even soft-deleted), the S3
// objects under p/<id>/, and rows in public.pipeline_runs, centry.social_pins,
// gateway.llm_budget_accumulators and elitea_runtime.tool_call_records. None
// of those tables carries a foreign key to centry.project, so nothing blocked
// the delete and nothing cleared them.
//
// The test seeds one row per table for the doomed project AND for a
// bystander id, deletes the project, and counts. The bystander rows prove
// the statements are scoped to the project rather than truncating.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/projectprovisioning"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

// ownedRowTables names every table C1 lists, plus the storage tables that
// belong to a bucket or a project. Each query counts the rows of $1.
var ownedRowTables = map[string]string{
	"elitea_storage.buckets":                `SELECT count(*) FROM elitea_storage.buckets WHERE project_id = $1`,
	"elitea_storage.objects":                `SELECT count(*) FROM elitea_storage.objects o JOIN elitea_storage.buckets b ON b.id = o.bucket_id WHERE b.project_id = $1`,
	"elitea_storage.bucket_permissions":     `SELECT count(*) FROM elitea_storage.bucket_permissions WHERE project_id = $1`,
	"elitea_storage.project_storage_policy": `SELECT count(*) FROM elitea_storage.project_storage_policy WHERE project_id = $1`,
	"elitea_storage.attachment_chunks":      `SELECT count(*) FROM elitea_storage.attachment_chunks WHERE project_id = $1`,
	"public.pipeline_runs":                  `SELECT count(*) FROM public.pipeline_runs WHERE project_id = ($1::bigint)::text`,
	"centry.social_pins":                    `SELECT count(*) FROM centry.social_pins WHERE project_id = $1`,
	"gateway.llm_budget_accumulators":       `SELECT count(*) FROM gateway.llm_budget_accumulators WHERE project_id = $1`,
	"elitea_runtime.tool_call_records":      `SELECT count(*) FROM elitea_runtime.tool_call_records WHERE project_id = $1`,
}

func seedProjectOwnedRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64) {
	t.Helper()
	statements := []string{
		// A live user bucket, a soft-deleted one, and an object row.
		`INSERT INTO elitea_storage.buckets (project_id, name, bucket_type) VALUES ($1, 'user-bucket', 'local')`,
		`INSERT INTO elitea_storage.buckets (project_id, name, bucket_type, deleted_at) VALUES ($1, 'gone-bucket', 'local', now())`,
		`INSERT INTO elitea_storage.objects (bucket_id, key, byte_length)
		 SELECT id, 'a.txt', 1 FROM elitea_storage.buckets WHERE project_id = $1 AND name = 'user-bucket'`,
		`INSERT INTO elitea_storage.bucket_permissions (project_id, user_id, bucket, permissions) VALUES ($1, 1, 'user-bucket', ARRAY['read'])`,
		`INSERT INTO elitea_storage.project_storage_policy (project_id) VALUES ($1)`,
		`INSERT INTO elitea_storage.attachment_chunks (project_id, conversation_id, file_id, chunk_index, total_chunks, file_name, bytes)
		 VALUES ($1, 'c', 'f', 0, 1, 'f.txt', '\x00')`,
		`INSERT INTO public.pipeline_runs (execution_id, project_id, application_id, version_id, conversation_uuid, origin)
		 VALUES ('exec-' || ($1::bigint)::text, ($1::bigint)::text, 1, 1, gen_random_uuid()::text, 'manual')`,
		`INSERT INTO centry.social_pins (entity, user_id, project_id, entity_id) VALUES ('application', 1, $1, 7)`,
		`INSERT INTO gateway.llm_budget_accumulators (project_id, scope, scope_id, period_start, period_end)
		 VALUES ($1::bigint, 'project', ($1::bigint)::text, date_trunc('month', now()), date_trunc('month', now()) + interval '1 month')`,
		`INSERT INTO elitea_runtime.tool_call_records (project_id, source, source_ref, tool_name, started_at)
		 VALUES ($1::bigint, 'agent_turn', 'ref-' || ($1::bigint)::text, 'search', now())`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement, projectID); err != nil {
			t.Fatalf("seed project %d: %v\n%s", projectID, err, statement)
		}
	}
}

func countOwnedRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, projectID int64) map[string]int {
	t.Helper()
	counts := make(map[string]int, len(ownedRowTables))
	for table, query := range ownedRowTables {
		var count int
		if err := pool.QueryRow(ctx, query, projectID).Scan(&count); err != nil {
			t.Fatalf("count %s for project %d: %v", table, projectID, err)
		}
		counts[table] = count
	}
	return counts
}

func TestDeprovisionRemovesTheProjectOwnedRowsThatHaveNoForeignKey(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	provisioner := newTestProvisioner(t, pool, migrate.New(pool, platformmigrations.Files))
	created, err := provisioner.Provision(ctx, projectprovisioning.Request{
		Name: "Owned Rows", OwnerID: 1, Limits: projectprovisioning.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	projectID := created.ProjectID
	bystander := projectID + 100000

	seedProjectOwnedRows(ctx, t, pool, projectID)
	seedProjectOwnedRows(ctx, t, pool, bystander)
	for table, rows := range countOwnedRows(ctx, t, pool, projectID) {
		if rows == 0 {
			t.Fatalf("the seed wrote no row in %s, so the delete proves nothing there", table)
		}
	}

	if result, err := provisioner.Deprovision(ctx, projectID); err != nil {
		t.Fatalf("deprovision: %v (steps=%+v)", err, result.RollbackSteps)
	}

	for table, rows := range countOwnedRows(ctx, t, pool, projectID) {
		if rows != 0 {
			t.Errorf("%s kept %d row(s) of the deleted project %d", table, rows, projectID)
		}
	}
	for table, rows := range countOwnedRows(ctx, t, pool, bystander) {
		if rows == 0 {
			t.Errorf("%s lost the bystander project's rows: the delete is not scoped", table)
		}
	}
}
