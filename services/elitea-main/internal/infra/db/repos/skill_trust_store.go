package repos

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/skilltrust"
	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/skilltrust"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// SkillTrustRepo stores a project's trust in the text of skills written in other
// projects (tenant 0250, ADR-0051 amendment 1, A1.6).
type SkillTrustRepo struct {
	pool *pgxpool.Pool
}

func NewSkillTrustRepo(pool *pgxpool.Pool) *SkillTrustRepo { return &SkillTrustRepo{pool: pool} }

var _ skilltrust.Repository = (*SkillTrustRepo)(nil)

// read answers the skill the caller may read and what the project trusts of it.
// A skill the caller cannot read, or that does not exist, is one not-found.
func (r *SkillTrustRepo) read(ctx context.Context, projectID string, skillID int64) (skilltrust.Status, string, error) {
	project, err := strconv.ParseInt(projectID, 10, 64)
	if err != nil || project <= 0 {
		return skilltrust.Status{}, "", apierr.NotFound("project not found")
	}
	schema, err := tenantSchema(projectID)
	if err != nil {
		return skilltrust.Status{}, "", err
	}
	skills, err := NewProjectSkillsRepo(r.pool).ResolveCurrentProjectSkills(ctx, project, []int64{skillID})
	if err != nil {
		if errors.Is(err, agentexecutionapp.ErrProjectSkillUnknown) {
			return skilltrust.Status{}, "", apierr.NotFound("skill not found")
		}
		return skilltrust.Status{}, "", err
	}
	skill := skills[0]
	status := skilltrust.Status{
		SkillID: skillID, Name: skill.Name, Revision: domain.Revision(skill.Instructions),
		State: "not_required", History: []skilltrust.Record{},
	}
	var parent string
	if err := r.pool.QueryRow(ctx, fmt.Sprintf(`
SELECT COALESCE(meta ->> 'parent_project_id', '') FROM %s.skill_versions WHERE skill_id = $1 AND name = 'base'`, schema), skillID).Scan(&parent); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return skilltrust.Status{}, "", fmt.Errorf("skill trust: read lineage: %w", err)
	}
	publisher, foreign := domain.Foreign(parent, project)
	status.Foreign = foreign
	if publisher > 0 {
		status.PublisherProjectID = &publisher
	}
	rows, err := r.pool.Query(ctx, fmt.Sprintf(`
SELECT revision, publisher_project_id, trusted_by, trusted_at, revoked_by, revoked_at, source
FROM %s.skill_trust WHERE skill_id = $1 ORDER BY id`, schema), skillID)
	if err != nil {
		return skilltrust.Status{}, "", fmt.Errorf("skill trust: read history: %w", err)
	}
	defer rows.Close()
	var active []string
	for rows.Next() {
		var record skilltrust.Record
		var publisherID, trustedBy, revokedBy *int32
		var trustedAt time.Time
		var revokedAt *time.Time
		if err := rows.Scan(&record.Revision, &publisherID, &trustedBy, &trustedAt, &revokedBy, &revokedAt, &record.Source); err != nil {
			return skilltrust.Status{}, "", fmt.Errorf("skill trust: scan: %w", err)
		}
		record.TrustedAt = trustedAt.UTC()
		if publisherID != nil {
			id := int64(*publisherID)
			record.PublisherProjectID = &id
		}
		if trustedBy != nil {
			id := int64(*trustedBy)
			record.TrustedBy = &id
		}
		if revokedBy != nil {
			id := int64(*revokedBy)
			record.RevokedBy = &id
		}
		if revokedAt != nil {
			at := revokedAt.UTC()
			record.RevokedAt = &at
		} else {
			active = append(active, record.Revision)
		}
		status.History = append(status.History, record)
	}
	if err := rows.Err(); err != nil {
		return skilltrust.Status{}, "", fmt.Errorf("skill trust: read history: %w", err)
	}
	if foreign {
		status.State = "untrusted"
		if domain.Trusted(skill.Instructions, active) {
			status.State = "trusted"
		}
	}
	return status, schema, nil
}

func (r *SkillTrustRepo) Get(ctx context.Context, projectID string, skillID int64) (skilltrust.Status, error) {
	status, _, err := r.read(ctx, projectID, skillID)
	return status, err
}

func (r *SkillTrustRepo) Trust(ctx context.Context, projectID string, skillID int64, revision string) (skilltrust.Status, bool, error) {
	actor, ok := actorOf(ctx)
	if !ok {
		return skilltrust.Status{}, false, apierr.Forbidden("trust is recorded for a person")
	}
	status, schema, err := r.read(ctx, projectID, skillID)
	if err != nil {
		return skilltrust.Status{}, false, err
	}
	if !status.Foreign {
		return skilltrust.Status{}, false, apierr.BadRequest("this skill was written in this project and needs no trust")
	}
	// A review of old text must not trust new text.
	if revision != status.Revision {
		return skilltrust.Status{}, false, apierr.Conflict("the skill's text changed since it was read; read it again before trusting it")
	}
	if status.State == "trusted" {
		return status, false, nil
	}
	if _, err := r.pool.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.skill_trust (skill_id, revision, publisher_project_id, trusted_by, source)
VALUES ($1, $2, $3, $4, 'user') ON CONFLICT DO NOTHING`, schema), skillID, revision, status.PublisherProjectID, actor); err != nil {
		return skilltrust.Status{}, false, fmt.Errorf("skill trust: record: %w", err)
	}
	status, _, err = r.read(ctx, projectID, skillID)
	return status, true, err
}

func (r *SkillTrustRepo) Revoke(ctx context.Context, projectID string, skillID int64) (skilltrust.Status, int, error) {
	actor, ok := actorOf(ctx)
	if !ok {
		return skilltrust.Status{}, 0, apierr.Forbidden("trust is revoked by a person")
	}
	_, schema, err := r.read(ctx, projectID, skillID)
	if err != nil {
		return skilltrust.Status{}, 0, err
	}
	tag, err := r.pool.Exec(ctx, fmt.Sprintf(`
UPDATE %s.skill_trust SET revoked_at = now(), revoked_by = $2 WHERE skill_id = $1 AND revoked_at IS NULL`, schema), skillID, actor)
	if err != nil {
		return skilltrust.Status{}, 0, fmt.Errorf("skill trust: revoke: %w", err)
	}
	status, _, err := r.read(ctx, projectID, skillID)
	return status, int(tag.RowsAffected()), err
}

func actorOf(ctx context.Context) (int64, bool) {
	user, ok := auth.UserFromContext(ctx)
	if !ok {
		return 0, false
	}
	return user.OwningUserID()
}
