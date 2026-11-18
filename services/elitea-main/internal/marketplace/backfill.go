package marketplace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
)

const backfillName = "0250_published_versions"

// Backfill builds the listings of the versions published before shared 0250.
// The database does not know the public project, so the service does it, once,
// on the first read: for each published version in the public project's schema
// it takes the snapshot from the source clone when the source project still has
// it, and from the public copy when it has not. A version that cannot be read
// is logged and skipped; the others are written. The marker is set only when
// the walk finished, so a failed walk runs again.
type Backfill struct {
	pool          *pgxpool.Pool
	publicSchema  string
	publicProject int64
	done          atomic.Bool
}

// NewBackfill returns the backfill for a deployment. publicSchema is the quoted
// schema of the public project.
func NewBackfill(pool *pgxpool.Pool, publicSchema string, publicProject int64) *Backfill {
	return &Backfill{pool: pool, publicSchema: publicSchema, publicProject: publicProject}
}

// Ensure runs the backfill when it has not run. It is safe to call on every
// read: after the first success it answers from memory.
func (b *Backfill) Ensure(ctx context.Context) error {
	if b == nil || b.pool == nil || b.done.Load() {
		return nil
	}
	var marked bool
	if err := b.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM elitea_marketplace.backfills WHERE name = $1)`, backfillName).Scan(&marked); err != nil {
		return fmt.Errorf("marketplace: read the backfill marker: %w", err)
	}
	if marked {
		b.done.Store(true)
		return nil
	}
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("marketplace: begin the backfill: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// One walk at a time across replicas.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, backfillName); err != nil {
		return fmt.Errorf("marketplace: lock the backfill: %w", err)
	}
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM elitea_marketplace.backfills WHERE name = $1)`, backfillName).Scan(&marked); err != nil {
		return fmt.Errorf("marketplace: read the backfill marker: %w", err)
	}
	if marked {
		b.done.Store(true)
		return nil
	}
	complete, err := b.walk(ctx, tx)
	if err != nil {
		return err
	}
	if !complete {
		// The public schema is not there yet: nothing to mark.
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO elitea_marketplace.backfills (name) VALUES ($1) ON CONFLICT DO NOTHING`, backfillName); err != nil {
		return fmt.Errorf("marketplace: mark the backfill: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("marketplace: commit the backfill: %w", err)
	}
	b.done.Store(true)
	return nil
}

func isMissingSchema(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "42P01" || pgErr.Code == "3F000")
}

type publishedAgent struct {
	appID, versionID       int64
	versionName            string
	sharedOwner, sharedApp *int64
	sharedVersion          *int64
	author                 *int64
	publishedBy            *int64
	createdAt              time.Time
}

func (b *Backfill) walk(ctx context.Context, tx pgx.Tx) (bool, error) {
	rows, err := tx.Query(ctx, fmt.Sprintf(`
SELECT a.id, v.id, v.name, a.shared_owner_id, a.shared_id, v.shared_id, NULLIF(v.author_id, 0),
       NULLIF(v.meta->>'published_by', '')::bigint, v.created_at
FROM %[1]s.applications a JOIN %[1]s.application_versions v ON v.application_id = a.id
WHERE v.status = 'published' AND COALESCE(v.meta->>'status', '') <> 'embedded'
ORDER BY v.created_at, v.id`, b.publicSchema))
	if err != nil {
		if isMissingSchema(err) {
			return false, nil
		}
		return false, fmt.Errorf("marketplace: read the published agents: %w", err)
	}
	var agents []publishedAgent
	for rows.Next() {
		var row publishedAgent
		if err := rows.Scan(&row.appID, &row.versionID, &row.versionName, &row.sharedOwner, &row.sharedApp,
			&row.sharedVersion, &row.author, &row.publishedBy, &row.createdAt); err != nil {
			rows.Close()
			return false, err
		}
		agents = append(agents, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, row := range agents {
		if err := b.backfillAgent(ctx, tx, row); err != nil {
			slog.WarnContext(ctx, "marketplace backfill: agent version skipped",
				"application_id", row.appID, "version_id", row.versionID, "error", err)
		}
	}
	return true, b.walkSkills(ctx, tx)
}

func (b *Backfill) backfillAgent(ctx context.Context, tx pgx.Tx, row publishedAgent) error {
	sub, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = sub.Rollback(ctx)
		}
	}()
	sourceProject, sourceEntity := b.publicProject, row.appID
	src := AgentSource{Schema: b.publicSchema, ProjectID: b.publicProject, ApplicationID: row.appID, VersionID: row.versionID}
	if row.sharedOwner != nil && row.sharedApp != nil && row.sharedVersion != nil {
		sourceProject, sourceEntity = *row.sharedOwner, *row.sharedApp
		if schema, err := tenantschema.QuoteInt(sourceProject); err == nil {
			// The source clone, when the project still has it.
			var exists bool
			if err := sub.QueryRow(ctx, fmt.Sprintf(
				`SELECT EXISTS (SELECT 1 FROM %s.application_versions WHERE id = $1 AND application_id = $2)`, schema),
				*row.sharedVersion, sourceEntity).Scan(&exists); err == nil && exists {
				src = AgentSource{Schema: schema, ProjectID: sourceProject, ApplicationID: sourceEntity, VersionID: *row.sharedVersion}
			}
		}
	}
	snap, err := BuildAgentSnapshot(ctx, sub, src)
	if err != nil {
		return err
	}
	var publisher int64
	switch {
	case row.publishedBy != nil:
		publisher = *row.publishedBy
	case row.author != nil:
		publisher = *row.author
	}
	if _, err := RecordPublish(ctx, sub, PublishRecord{
		Snapshot: snap, SourceProjectID: sourceProject, SourceEntityID: sourceEntity, EntityID: row.appID,
		VersionID: row.versionID, VersionName: row.versionName, PublisherID: publisher, PublishedAt: row.createdAt,
	}); err != nil {
		return err
	}
	committed = true
	return sub.Commit(ctx)
}

func (b *Backfill) walkSkills(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, fmt.Sprintf(`
SELECT s.id, v.id, v.name, s.shared_owner_id, s.shared_id, NULLIF(v.author_id, 0),
       NULLIF(v.meta->>'published_by', '')::bigint, v.created_at
FROM %[1]s.skills s JOIN %[1]s.skill_versions v ON v.skill_id = s.id
WHERE v.status = 'published' ORDER BY v.created_at, v.id`, b.publicSchema))
	if err != nil {
		if isMissingSchema(err) {
			return nil
		}
		return fmt.Errorf("marketplace: read the published skills: %w", err)
	}
	type publishedSkill struct {
		skillID, versionID int64
		name               string
		sharedOwner        *int64
		sharedSkill        *int64
		author, by         *int64
		createdAt          time.Time
	}
	var skills []publishedSkill
	for rows.Next() {
		var row publishedSkill
		if err := rows.Scan(&row.skillID, &row.versionID, &row.name, &row.sharedOwner, &row.sharedSkill, &row.author, &row.by, &row.createdAt); err != nil {
			rows.Close()
			return err
		}
		skills = append(skills, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, row := range skills {
		if err := b.backfillSkill(ctx, tx, row.skillID, row.versionID, row.name, row.sharedOwner, row.sharedSkill, row.author, row.by, row.createdAt); err != nil {
			slog.WarnContext(ctx, "marketplace backfill: skill version skipped",
				"skill_id", row.skillID, "version_id", row.versionID, "error", err)
		}
	}
	return nil
}

func (b *Backfill) backfillSkill(ctx context.Context, tx pgx.Tx, skillID, versionID int64, name string,
	sharedOwner, sharedSkill, author, by *int64, createdAt time.Time) error {
	sub, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = sub.Rollback(ctx)
		}
	}()
	snap, err := BuildSkillSnapshot(ctx, sub, SkillSource{Schema: b.publicSchema, ProjectID: b.publicProject, SkillID: skillID, VersionID: versionID})
	if err != nil {
		return err
	}
	sourceProject, sourceEntity := b.publicProject, skillID
	if sharedOwner != nil && sharedSkill != nil {
		sourceProject, sourceEntity = *sharedOwner, *sharedSkill
	}
	var publisher int64
	switch {
	case by != nil:
		publisher = *by
	case author != nil:
		publisher = *author
	}
	if _, err := RecordPublish(ctx, sub, PublishRecord{
		Snapshot: snap, SourceProjectID: sourceProject, SourceEntityID: sourceEntity, EntityID: skillID,
		VersionID: versionID, VersionName: name, PublisherID: publisher, PublishedAt: createdAt,
		Category: skillCategory(snap.Tags),
	}); err != nil {
		return err
	}
	committed = true
	return sub.Commit(ctx)
}

// skillCategory is the category a skill's tags carry: the platform files a
// skill under its category as a tag. The first tag that names one of the
// built-in categories wins; with none, the listing has no category.
func skillCategory(tags []string) string {
	builtin := map[string]bool{
		"Business Analyst": true, "Quality Assurance": true, "Development": true, "DevOps": true,
		"Project Management": true, "Knowledge & Documentation": true, "Elitea": true, "Epam": true, "Other": true,
	}
	for _, tag := range tags {
		if builtin[tag] {
			return tag
		}
	}
	return ""
}
