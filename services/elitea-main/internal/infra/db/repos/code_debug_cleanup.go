package repos

import (
	"context"
	"errors"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/jackc/pgx/v5"
	"strconv"
	"strings"
)

// Main's maintenance owner calls this bounded method. Reservations are never
// reallocated after abandonment. Committed artifacts use normal bucket retention.
// No execution, claim, or object row lock spans an object-store delete.
func (r *CodeDebugArtifactsRepository) CleanupCodeDebugStaging(ctx context.Context, limit int) (int, error) {
	if r == nil || r.pool == nil || r.artifacts == nil || r.artifacts.store == nil || limit < 1 || limit > 16 {
		return 0, storage.ErrContentRejected
	}
	cleaned := 0
	for range limit {
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			return cleaned, err
		}
		var project int64
		var key string
		err = tx.QueryRow(ctx, `WITH candidate AS (
   SELECT tenant_id,project_id,execution_id,original_generation,original_visit_id,original_visit_revision,original_visit_digest
   FROM elitea_runtime.code_debug_artifacts
   WHERE (state='staging' AND created_at<clock_timestamp()-interval '24 hours')
      OR (state='abandoned' AND (cleaned_at IS NULL OR cleaned_at<clock_timestamp()-interval '24 hours'))
   ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1
  ) UPDATE elitea_runtime.code_debug_artifacts d
   SET state='abandoned',abandoned_at=COALESCE(abandoned_at,clock_timestamp()),cleaned_at=NULL
   FROM candidate c
   WHERE d.tenant_id=c.tenant_id AND d.project_id=c.project_id AND d.execution_id=c.execution_id
     AND d.original_generation=c.original_generation AND d.original_visit_id=c.original_visit_id
     AND d.original_visit_revision=c.original_visit_revision AND d.original_visit_digest=c.original_visit_digest
   RETURNING d.project_id,d.object_key`).Scan(&project, &key)
		if errors.Is(err, pgx.ErrNoRows) {
			_ = tx.Rollback(ctx)
			return cleaned, nil
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return cleaned, err
		}
		ref, err := codeDebugCleanupRef(project, key)
		if err != nil {
			_ = tx.Rollback(ctx)
			return cleaned, err
		}
		if err = tx.Commit(ctx); err != nil {
			return cleaned, err
		}
		// The immutable reservation owns this exact key even if Put was uncertain.
		if err = r.artifacts.store.Delete(ctx, ref); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return cleaned, err
		}
		// Delete metadata only for this exact unpublished reservation, never a committed ref.
		_, err = r.pool.Exec(ctx, `DELETE FROM elitea_storage.objects o USING elitea_storage.buckets b,elitea_runtime.code_debug_artifacts d
   WHERE o.bucket_id=b.id AND b.project_id=$1 AND b.name='code-debug' AND o.key=$2
     AND d.project_id=$1 AND d.object_key=$2 AND d.state='abandoned'`, project, key)
		if err != nil {
			return cleaned, err
		}
		_, err = r.pool.Exec(ctx, `UPDATE elitea_runtime.code_debug_artifacts SET cleaned_at=clock_timestamp() WHERE project_id=$1 AND object_key=$2 AND state='abandoned'`, project, key)
		if err != nil {
			return cleaned, err
		}
		cleaned++
	}
	return cleaned, nil
}
func codeDebugCleanupRef(project int64, key string) (storage.ObjectRef, error) {
	if project < 1 || project > 2147483647 || len(key) != 69 || !strings.HasSuffix(key, ".json") || !codeDebugDigest(key[:64]) {
		return storage.ObjectRef{}, storage.ErrContentRejected
	}
	return storage.NewObjectRef(strconv.FormatInt(project, 10), "code-debug", key)
}
