package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/tags"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

type TagsRepo struct {
	pool *pgxpool.Pool
}

func NewTagsRepo(pool *pgxpool.Pool) *TagsRepo {
	return &TagsRepo{pool: pool}
}

// coverageFilter is the WHERE clause that narrows the project's tags to the
// ones one kind of entity carries. It is a correlated EXISTS over the
// association tables rather than a join, so a tag two agents share is still
// one row.
//
// The application/pipeline split is drawn at the APPLICATION, matching the
// legacy rule (`Entity.versions.any(agent_type == 'pipeline')`): an
// application with one pipeline version is a pipeline, and its tags belong to
// the pipeline coverage even if the tagged version is a classic one.
func coverageFilter(s string, coverage tags.EntityCoverage) string {
	const applicationJoin = `
		SELECT 1
		FROM %[1]s.application_version_tag_association a
		JOIN %[1]s.application_versions v ON v.id = a.version_id
		WHERE a.tag_id = t.id`
	switch coverage {
	case tags.CoverageApplication:
		return fmt.Sprintf(` WHERE EXISTS (`+applicationJoin+`
		  AND NOT EXISTS (
			SELECT 1 FROM %[1]s.application_versions pv
			WHERE pv.application_id = v.application_id AND pv.agent_type = 'pipeline'))`, s)
	case tags.CoveragePipeline:
		return fmt.Sprintf(` WHERE EXISTS (`+applicationJoin+`
		  AND EXISTS (
			SELECT 1 FROM %[1]s.application_versions pv
			WHERE pv.application_id = v.application_id AND pv.agent_type = 'pipeline'))`, s)
	case tags.CoverageSkill:
		return fmt.Sprintf(` WHERE EXISTS (
			SELECT 1 FROM %[1]s.skill_version_tag_association sa
			WHERE sa.tag_id = t.id)`, s)
	default:
		// CoverageAll: every tag row the project holds, including one that
		// nothing carries yet. A tag created through the write API and not
		// yet attached to anything is only ever visible here.
		return ""
	}
}

// List answers the project's tags, optionally narrowed to one entity kind.
//
// A query failure answers an EMPTY list rather than an error, which is the
// behaviour this read has always had: the tag rail is drawn beside a list
// that must still render for a project whose tenant schema is not there yet.
// It is stated here because it also means a broken filter looks like an empty
// project — which is why the coverage filters are pinned by a PostgreSQL
// integration test rather than by a unit test over a fake.
func (r *TagsRepo) List(ctx context.Context, projectID string, coverage tags.EntityCoverage) ([]tags.Tag, error) {
	s := schema(projectID)
	q := fmt.Sprintf(`SELECT t.id, t.name, COALESCE(t.data::text, 'null') FROM %s.tags t`, s) +
		coverageFilter(s, coverage) + ` ORDER BY t.name`

	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return []tags.Tag{}, nil
	}
	defer rows.Close()

	var items []tags.Tag
	for rows.Next() {
		var t tags.Tag
		var dataStr string
		if err := rows.Scan(&t.ID, &t.Name, &dataStr); err != nil {
			continue
		}
		if dataStr != "" && dataStr != "null" {
			_ = json.Unmarshal([]byte(dataStr), &t.Data) // best-effort: DB column is trusted JSON
		}
		items = append(items, t)
	}
	if items == nil {
		items = []tags.Tag{}
	}
	return items, nil
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
