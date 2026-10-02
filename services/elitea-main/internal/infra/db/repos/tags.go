package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/tags"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/entitydiscovery"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

type TagsRepo struct {
	pool *pgxpool.Pool
}

func NewTagsRepo(pool *pgxpool.Pool) *TagsRepo {
	return &TagsRepo{pool: pool}
}

// List uses the same actor-scoped read as REST and internal MCP discovery.
func (r *TagsRepo) List(ctx context.Context, projectID string, coverage tags.EntityCoverage) ([]tags.Tag, error) {
	page, err := r.ListFiltered(ctx, projectID, entitydiscovery.Filters{Coverage: string(coverage), Limit: 1000})
	return page.Rows, err
}
func (r *TagsRepo) ListFiltered(ctx context.Context, projectID string, filters entitydiscovery.Filters) (entitydiscovery.Page[tags.Tag], error) {
	return entitydiscovery.New(r.pool).Tags(ctx, projectID, filters)
}

// Create stores one tag and answers the stored row.
//
// It is idempotent on the NAME, because `tags.name` is unique per tenant
// schema and one name is one row shared by every version that carries it —
// the same rule the version write applies
// (internal/api/v2/applications/handler.go replaceVersionTags). A second
// create of a name that is already there answers that row, id included,
// rather than a 409 the client can do nothing with.
//
// `data` is written when the row is NEW and left alone when it is not, for
// the reason the version write states: the blob belongs to every entity using
// the tag, so one caller's create must not re-colour another's tag.
func (r *TagsRepo) Create(ctx context.Context, projectID string, tag tags.Tag) (tags.Tag, error) {
	s, err := tenantSchema(projectID)
	if err != nil {
		return tags.Tag{}, err
	}
	var dataJSON []byte
	if tag.Data != nil {
		encoded, err := json.Marshal(tag.Data)
		if err != nil {
			return tags.Tag{}, apierr.BadRequest("invalid tag data")
		}
		dataJSON = encoded
	}

	var stored tags.Tag
	var dataStr string
	if err := r.pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO %s.tags (name, data) VALUES ($1, $2::jsonb)
		ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
		RETURNING id, name, COALESCE(data::text, 'null')`, s), tag.Name, dataJSON,
	).Scan(&stored.ID, &stored.Name, &dataStr); err != nil {
		return tags.Tag{}, fmt.Errorf("tags: create: %w", err)
	}
	if dataStr != "" && dataStr != "null" {
		_ = json.Unmarshal([]byte(dataStr), &stored.Data) // DB-stored JSON
	}
	return stored, nil
}

// Delete removes the tag row and every association that points at it.
//
// The associations are deleted EXPLICITLY even though both association tables
// declare `ON DELETE CASCADE` on `tag_id` in this repository's own schema
// (internal/infra/db/migrations/001_initial.sql): a schema this service
// inherited from pylon rather than created is not guaranteed to carry the
// same constraint, and a delete that depends on one is a delete that fails
// with a foreign-key violation on exactly the deployments nobody tests on.
//
// One transaction, so a tag can never lose its associations and keep its row.
func (r *TagsRepo) Delete(ctx context.Context, projectID, tagID string) error {
	s, err := tenantSchema(projectID)
	if err != nil {
		return err
	}
	if !isNumericRowID(tagID) {
		return apierr.NotFound("tag not found")
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("tags: delete: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, table := range []string{
		"application_version_tag_association",
		"skill_version_tag_association",
	} {
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			`DELETE FROM %s.%s WHERE tag_id = $1`, s, table), tagID); err != nil {
			return fmt.Errorf("tags: delete associations: %w", err)
		}
	}

	var deleted int
	if err := tx.QueryRow(ctx, fmt.Sprintf(
		`DELETE FROM %s.tags WHERE id = $1 RETURNING id`, s), tagID).Scan(&deleted); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apierr.NotFound("tag not found")
		}
		return fmt.Errorf("tags: delete: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("tags: delete: commit: %w", err)
	}
	return nil
}
