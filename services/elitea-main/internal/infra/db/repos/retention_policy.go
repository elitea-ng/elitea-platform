package repos

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/retention"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// RetentionPolicyRepo stores elitea_runtime.retention_policy and
// elitea_runtime.legal_holds (shared 0195, issue #1495).
type RetentionPolicyRepo struct {
	pool *pgxpool.Pool
}

// NewRetentionPolicyRepo builds the store.
func NewRetentionPolicyRepo(pool *pgxpool.Pool) *RetentionPolicyRepo {
	return &RetentionPolicyRepo{pool: pool}
}

const retentionPolicyColumns = `data_class, project_id, min_age_days, max_age_days,
	allow_project_override, updated_by, updated_at`

func scanRetentionPolicy(row pgx.Row) (retention.Policy, error) {
	var p retention.Policy
	var class string
	err := row.Scan(&class, &p.ProjectID, &p.MinAgeDays, &p.MaxAgeDays, &p.AllowProjectOverride, &p.UpdatedBy, &p.UpdatedAt)
	p.Class = retention.DataClass(class)
	return p, err
}

// ListPolicies returns every platform row and project override, platform rows
// first, in data class then project order.
func (r *RetentionPolicyRepo) ListPolicies(ctx context.Context) ([]retention.Policy, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+retentionPolicyColumns+`
		FROM elitea_runtime.retention_policy
		ORDER BY (scope = 'project'), data_class, project_id`)
	if err != nil {
		return nil, fmt.Errorf("retention: list policies: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (retention.Policy, error) {
		return scanRetentionPolicy(row)
	})
}

// PlatformPolicy returns the platform row of class.
func (r *RetentionPolicyRepo) PlatformPolicy(ctx context.Context, class retention.DataClass) (retention.Policy, error) {
	p, err := scanRetentionPolicy(r.pool.QueryRow(ctx, `SELECT `+retentionPolicyColumns+`
		FROM elitea_runtime.retention_policy WHERE scope = 'platform' AND data_class = $1`, string(class)))
	if errors.Is(err, pgx.ErrNoRows) {
		return retention.Policy{}, apierr.NotFound("no platform retention policy for " + string(class))
	}
	if err != nil {
		return retention.Policy{}, fmt.Errorf("retention: platform policy: %w", err)
	}
	return p, nil
}

// ProjectOverrides returns the project overrides of class, keyed by project.
func (r *RetentionPolicyRepo) ProjectOverrides(ctx context.Context, class retention.DataClass) (map[int64]retention.Policy, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+retentionPolicyColumns+`
		FROM elitea_runtime.retention_policy WHERE scope = 'project' AND data_class = $1`, string(class))
	if err != nil {
		return nil, fmt.Errorf("retention: project overrides: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (retention.Policy, error) {
		return scanRetentionPolicy(row)
	})
	if err != nil {
		return nil, fmt.Errorf("retention: project overrides: %w", err)
	}
	out := make(map[int64]retention.Policy, len(list))
	for _, p := range list {
		if p.ProjectID != nil {
			out[*p.ProjectID] = p
		}
	}
	return out, nil
}

// SetPlatformPolicy validates and stores a platform row. Existing overrides
// are left as they are: retention.Effective clamps one that the new bounds
// exclude, so a tightened maximum applies at once.
func (r *RetentionPolicyRepo) SetPlatformPolicy(ctx context.Context, p retention.Policy, actor int64) (retention.Policy, error) {
	if err := retention.ValidatePlatform(p); err != nil {
		return retention.Policy{}, apierr.BadRequest(err.Error())
	}
	stored, err := scanRetentionPolicy(r.pool.QueryRow(ctx, `
		INSERT INTO elitea_runtime.retention_policy
		    (data_class, scope, min_age_days, max_age_days, allow_project_override, updated_by, updated_at)
		VALUES ($1, 'platform', $2, $3, $4, $5, now())
		ON CONFLICT (data_class) WHERE scope = 'platform' DO UPDATE
		   SET min_age_days = EXCLUDED.min_age_days,
		       max_age_days = EXCLUDED.max_age_days,
		       allow_project_override = EXCLUDED.allow_project_override,
		       updated_by = EXCLUDED.updated_by,
		       updated_at = now()
		RETURNING `+retentionPolicyColumns,
		string(p.Class), p.MinAgeDays, p.MaxAgeDays, p.AllowProjectOverride, actor))
	if err != nil {
		return retention.Policy{}, fmt.Errorf("retention: store platform policy: %w", err)
	}
	return stored, nil
}

// SetProjectOverride validates an override against the platform row, read
// under a share lock in the same transaction so a concurrent platform edit
// cannot slip between the check and the write, and stores it.
func (r *RetentionPolicyRepo) SetProjectOverride(ctx context.Context, p retention.Policy, actor int64) (retention.Policy, error) {
	if !retention.Known(p.Class) {
		return retention.Policy{}, apierr.BadRequest("unknown data class " + string(p.Class))
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return retention.Policy{}, fmt.Errorf("retention: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	platform, err := scanRetentionPolicy(tx.QueryRow(ctx, `SELECT `+retentionPolicyColumns+`
		FROM elitea_runtime.retention_policy WHERE scope = 'platform' AND data_class = $1
		FOR SHARE`, string(p.Class)))
	if errors.Is(err, pgx.ErrNoRows) {
		return retention.Policy{}, apierr.NotFound("no platform retention policy for " + string(p.Class))
	}
	if err != nil {
		return retention.Policy{}, fmt.Errorf("retention: platform policy: %w", err)
	}
	if err := retention.ValidateOverride(platform, p); err != nil {
		return retention.Policy{}, apierr.BadRequest(err.Error())
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM centry.project WHERE id = $1)`, *p.ProjectID).Scan(&exists); err != nil {
		return retention.Policy{}, fmt.Errorf("retention: project lookup: %w", err)
	}
	if !exists {
		return retention.Policy{}, apierr.NotFound("project not found")
	}
	stored, err := scanRetentionPolicy(tx.QueryRow(ctx, `
		INSERT INTO elitea_runtime.retention_policy
		    (data_class, scope, project_id, max_age_days, updated_by, updated_at)
		VALUES ($1, 'project', $2, $3, $4, now())
		ON CONFLICT (data_class, project_id) WHERE scope = 'project' DO UPDATE
		   SET max_age_days = EXCLUDED.max_age_days,
		       updated_by = EXCLUDED.updated_by,
		       updated_at = now()
		RETURNING `+retentionPolicyColumns,
		string(p.Class), *p.ProjectID, p.MaxAgeDays, actor))
	if err != nil {
		return retention.Policy{}, fmt.Errorf("retention: store override: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return retention.Policy{}, fmt.Errorf("retention: commit override: %w", err)
	}
	return stored, nil
}

// DeleteProjectOverride removes an override; the platform value applies
// again.
func (r *RetentionPolicyRepo) DeleteProjectOverride(ctx context.Context, class retention.DataClass, projectID int64) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM elitea_runtime.retention_policy
		WHERE scope = 'project' AND data_class = $1 AND project_id = $2`, string(class), projectID)
	if err != nil {
		return fmt.Errorf("retention: delete override: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return apierr.NotFound("no override for this project and data class")
	}
	return nil
}

const legalHoldColumns = `id, scope, project_id, agent_id, user_id, reason, created_by, created_at,
	released_by, released_at, release_reason`

func scanLegalHold(row pgx.Row) (retention.Hold, error) {
	var h retention.Hold
	var scope string
	err := row.Scan(&h.ID, &scope, &h.ProjectID, &h.AgentID, &h.UserID, &h.Reason, &h.CreatedBy, &h.CreatedAt,
		&h.ReleasedBy, &h.ReleasedAt, &h.ReleaseReason)
	h.Scope = retention.HoldScope(scope)
	return h, err
}

// ListHolds returns holds newest first; activeOnly drops released ones.
func (r *RetentionPolicyRepo) ListHolds(ctx context.Context, activeOnly bool) ([]retention.Hold, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+legalHoldColumns+`
		FROM elitea_runtime.legal_holds
		WHERE NOT $1 OR released_at IS NULL
		ORDER BY created_at DESC, id DESC
		LIMIT 1000`, activeOnly)
	if err != nil {
		return nil, fmt.Errorf("retention: list holds: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (retention.Hold, error) {
		return scanLegalHold(row)
	})
}

// CreateHold validates and stores a hold.
func (r *RetentionPolicyRepo) CreateHold(ctx context.Context, h retention.Hold) (retention.Hold, error) {
	if err := retention.ValidateHold(h); err != nil {
		return retention.Hold{}, apierr.BadRequest(err.Error())
	}
	if h.CreatedBy <= 0 {
		return retention.Hold{}, apierr.BadRequest("a hold needs a creating user")
	}
	stored, err := scanLegalHold(r.pool.QueryRow(ctx, `
		INSERT INTO elitea_runtime.legal_holds (scope, project_id, agent_id, user_id, reason, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+legalHoldColumns,
		string(h.Scope), h.ProjectID, h.AgentID, h.UserID, h.Reason, h.CreatedBy))
	if err != nil {
		return retention.Hold{}, fmt.Errorf("retention: create hold: %w", err)
	}
	return stored, nil
}

// ReleaseHold stamps a release. A released hold cannot be released again: the
// first release is the record.
func (r *RetentionPolicyRepo) ReleaseHold(ctx context.Context, id, actor int64, reason string) (retention.Hold, error) {
	if actor <= 0 {
		return retention.Hold{}, apierr.BadRequest("a release needs a releasing user")
	}
	if len(reason) > 2000 {
		return retention.Hold{}, apierr.BadRequest("release reason is longer than 2000 characters")
	}
	var releaseReason *string
	if reason != "" {
		releaseReason = &reason
	}
	stored, err := scanLegalHold(r.pool.QueryRow(ctx, `
		UPDATE elitea_runtime.legal_holds
		   SET released_by = $2, released_at = now(), release_reason = $3
		 WHERE id = $1 AND released_at IS NULL
		RETURNING `+legalHoldColumns, id, actor, releaseReason))
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if lookupErr := r.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM elitea_runtime.legal_holds WHERE id = $1)`, id).Scan(&exists); lookupErr != nil {
			return retention.Hold{}, fmt.Errorf("retention: release hold: %w", lookupErr)
		}
		if exists {
			return retention.Hold{}, apierr.Conflict("legal hold is already released")
		}
		return retention.Hold{}, apierr.NotFound("legal hold not found")
	}
	if err != nil {
		return retention.Hold{}, fmt.Errorf("retention: release hold: %w", err)
	}
	return stored, nil
}
