package repos

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BucketPermissionRow is one project member's whole exception set, in the
// shape the legacy ACL listing serves: a map from bucket name to the
// permissions that member keeps on it.
//
// legacy/plugins/artifacts/api/v2/bucket_permissions.py:42-54 returns
// `{"total": n, "rows": [{"access_key_id", "user_id", "name",
// "bucket_permissions"}]}`. `access_key_id` is absent here because this
// platform has no per-user S3 access key to name — see
// migrations/shared/0118_artifact_bucket_permissions.sql. `email` is present
// because the reference table has an Email column
// (frontends/EliteaUI [fsd]/features/artifacts/ui/bucket-access/
// BucketAccessTable.jsx:34-39) that the legacy page had to fetch from a
// SECOND request against the admin user listing; one row that carries it is
// the same information with one fewer round trip.
type BucketPermissionRow struct {
	UserID int64
	Name   string
	Email  string
	// BucketPermissions maps bucket name to the granted verbs. An entry with
	// an EMPTY slice means "no access", which is not the same as an absent
	// entry ("no exception, so allowed"). Callers must not collapse the two.
	BucketPermissions map[string][]string
}

// ArtifactBucketPermissionsRepository reads and writes
// elitea_storage.bucket_permissions — the per-(project, user, bucket)
// exceptions that gate every artifact object verb.
type ArtifactBucketPermissionsRepository struct {
	pool *pgxpool.Pool
}

func NewArtifactBucketPermissionsRepository(pool *pgxpool.Pool) (*ArtifactBucketPermissionsRepository, error) {
	if pool == nil {
		return nil, errors.New("artifact bucket permissions database is required")
	}
	return &ArtifactBucketPermissionsRepository{pool: pool}, nil
}

// ListBucketPermissions returns every exception in the project, grouped by
// member and ordered by name so the listing is stable between reads.
//
// The join onto auth_core__user is a LEFT join. An exception whose user row
// was deleted still describes a restriction the table holds, and dropping it
// from the listing would make it invisible to the only page that can remove
// it.
func (r *ArtifactBucketPermissionsRepository) ListBucketPermissions(
	ctx context.Context, projectID int64,
) ([]BucketPermissionRow, error) {
	rows, err := r.pool.Query(ctx, `
SELECT bucket_permission.user_id,
       COALESCE(app_user.name, ''),
       COALESCE(app_user.email, ''),
       bucket_permission.bucket,
       bucket_permission.permissions
FROM elitea_storage.bucket_permissions AS bucket_permission
LEFT JOIN public.auth_core__user AS app_user
       ON app_user.id = bucket_permission.user_id
WHERE bucket_permission.project_id = $1
ORDER BY COALESCE(app_user.name, ''), bucket_permission.user_id, bucket_permission.bucket`,
		projectID)
	if err != nil {
		return nil, fmt.Errorf("list artifact bucket permissions: %w", err)
	}
	defer rows.Close()

	byUser := map[int64]*BucketPermissionRow{}
	order := []int64{}
	for rows.Next() {
		var userID int64
		var name, email, bucket string
		var permissions []string
		if err := rows.Scan(&userID, &name, &email, &bucket, &permissions); err != nil {
			return nil, fmt.Errorf("scan artifact bucket permission: %w", err)
		}
		entry, ok := byUser[userID]
		if !ok {
			entry = &BucketPermissionRow{
				UserID:            userID,
				Name:              name,
				Email:             email,
				BucketPermissions: map[string][]string{},
			}
			byUser[userID] = entry
			order = append(order, userID)
		}
		// A nil slice from an empty PostgreSQL array must reach JSON as `[]`,
		// not `null`: `[]` is the legacy encoding of "no access" and `null`
		// would decode back as "no exception at all".
		if permissions == nil {
			permissions = []string{}
		}
		entry.BucketPermissions[bucket] = permissions
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list artifact bucket permissions: %w", err)
	}

	result := make([]BucketPermissionRow, 0, len(order))
	for _, userID := range order {
		result = append(result, *byUser[userID])
	}
	return result, nil
}

// GetBucketPermission reads ONE member's exception for ONE bucket. found is
// false when the member has no exception for that bucket, which is the legacy
// default and means ALLOW — see utils/utils.py:99-132. A found exception with
// an empty permission list means "no access".
func (r *ArtifactBucketPermissionsRepository) GetBucketPermission(
	ctx context.Context, projectID, userID int64, bucket string,
) (permissions []string, found bool, err error) {
	err = r.pool.QueryRow(ctx, `
SELECT permissions
FROM elitea_storage.bucket_permissions
WHERE project_id = $1 AND user_id = $2 AND bucket = $3`,
		projectID, userID, bucket).Scan(&permissions)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get artifact bucket permission: %w", err)
	}
	if permissions == nil {
		permissions = []string{}
	}
	return permissions, true, nil
}

// ListUserBucketPermissions reads one member's whole exception map. The
// bucket LISTING route needs it, because it must decide visibility for every
// bucket in one pass rather than per bucket.
func (r *ArtifactBucketPermissionsRepository) ListUserBucketPermissions(
	ctx context.Context, projectID, userID int64,
) (map[string][]string, error) {
	rows, err := r.pool.Query(ctx, `
SELECT bucket, permissions
FROM elitea_storage.bucket_permissions
WHERE project_id = $1 AND user_id = $2`, projectID, userID)
	if err != nil {
		return nil, fmt.Errorf("list user artifact bucket permissions: %w", err)
	}
	defer rows.Close()

	result := map[string][]string{}
	for rows.Next() {
		var bucket string
		var permissions []string
		if err := rows.Scan(&bucket, &permissions); err != nil {
			return nil, fmt.Errorf("scan user artifact bucket permission: %w", err)
		}
		if permissions == nil {
			permissions = []string{}
		}
		result[bucket] = permissions
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list user artifact bucket permissions: %w", err)
	}
	return result, nil
}

// ReplaceUserBucketPermissions makes the member's stored exception set equal
// to permissions, in ONE transaction.
//
// REPLACE, not merge, because that is what the legacy PUT does:
// rpc/s3_credentials.py:362 assigns `data['bucket_permissions'] =
// bucket_permissions` outright. The reference UI depends on it — the only way
// it removes an exception is to PUT the map with that key deleted
// (BucketAccessTable.jsx), so a merge would make removal impossible.
func (r *ArtifactBucketPermissionsRepository) ReplaceUserBucketPermissions(
	ctx context.Context, projectID, userID int64, permissions map[string][]string,
) error {
	transaction, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("replace artifact bucket permissions: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	buckets := make([]string, 0, len(permissions))
	for bucket := range permissions {
		buckets = append(buckets, bucket)
	}
	sort.Strings(buckets)

	if _, err := transaction.Exec(ctx, `
DELETE FROM elitea_storage.bucket_permissions
WHERE project_id = $1 AND user_id = $2 AND NOT (bucket = ANY($3::text[]))`,
		projectID, userID, buckets); err != nil {
		return fmt.Errorf("replace artifact bucket permissions: %w", err)
	}

	for _, bucket := range buckets {
		verbs := permissions[bucket]
		if verbs == nil {
			verbs = []string{}
		}
		if _, err := transaction.Exec(ctx, `
INSERT INTO elitea_storage.bucket_permissions (project_id, user_id, bucket, permissions)
VALUES ($1, $2, $3, $4::text[])
ON CONFLICT (project_id, user_id, bucket)
DO UPDATE SET permissions = EXCLUDED.permissions, updated_at = now()`,
			projectID, userID, bucket, verbs); err != nil {
			return fmt.Errorf("replace artifact bucket permissions: %w", err)
		}
	}

	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("replace artifact bucket permissions: %w", err)
	}
	return nil
}

// DeleteUserBucketPermission removes ONE exception. removed reports whether a
// row was there, so the route can answer 404 for a member/bucket pair that
// carries no exception rather than reporting success over nothing.
func (r *ArtifactBucketPermissionsRepository) DeleteUserBucketPermission(
	ctx context.Context, projectID, userID int64, bucket string,
) (removed bool, err error) {
	tag, err := r.pool.Exec(ctx, `
DELETE FROM elitea_storage.bucket_permissions
WHERE project_id = $1 AND user_id = $2 AND bucket = $3`, projectID, userID, bucket)
	if err != nil {
		return false, fmt.Errorf("delete artifact bucket permission: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// IsProjectAdmin reports whether the member holds the `admin` project role.
//
// It is the ONE bypass over an exception. legacy has this bypass in exactly
// one place — the bucket listing filter calls
// `rpc.call.admin_check_user_is_admin` (api/v2/buckets.py:64-75) — and NOT on
// the object verbs, so a legacy project admin can be locked out of a bucket by
// an exception written against their own account, with no route left to remove
// it (the ACL routes are themselves gated on a permission the exception does
// not touch, but the artifacts page they live on reads the bucket first).
// Applying the bypass everywhere closes that trap and matches what the
// reference UI tells the operator the exceptions are: a restriction on
// MEMBERS, authored by administrators.
func (r *ArtifactBucketPermissionsRepository) IsProjectAdmin(
	ctx context.Context, projectID, userID int64,
) (bool, error) {
	var isAdmin bool
	err := r.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM public.auth_core__project_user_role AS assignment
    JOIN public.auth_core__project_role AS project_role
      ON project_role.id = assignment.role_id
     AND project_role.project_id = assignment.project_id
    WHERE assignment.project_id = $1
      AND assignment.user_id = $2
      AND project_role.name IN ('admin', 'super_admin')
)`, projectID, userID).Scan(&isAdmin)
	if err != nil {
		return false, fmt.Errorf("check artifact project admin: %w", err)
	}
	return isAdmin, nil
}
