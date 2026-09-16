package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/storage"
)

// CurrentRuntimeEntityBuilderRepository is the tenant-side half of the two
// chat-authored builder modules (#940 A8): the statements that actually write
// a Skill or the Project Context for a project the CLAIM already authorized.
//
// It lives in this package, next to SkillsRepo, so the skill write reuses that
// repository's own statements — createSkillSQL and upsertBaseSkillVersion —
// rather than a second, parallel spelling of "create a skill". The two must
// agree about owner_id, about the reserved `base` version, and about tag
// replacement; a copy in another package is how they would stop agreeing (the
// owner_id defect createSkillSQL's own doc comment records is exactly that
// failure, found once already).
//
// The project id arrives as an int64 the caller resolved from the durable
// claim. It is never read from a request: see
// internal/infra/storage/runtime_entity_builder.go's module doc comment.
type CurrentRuntimeEntityBuilderRepository struct {
	pool   *pgxpool.Pool
	skills *SkillsRepo
}

func NewCurrentRuntimeEntityBuilderRepository(
	pool *pgxpool.Pool,
) (*CurrentRuntimeEntityBuilderRepository, error) {
	if pool == nil {
		return nil, errors.New("runtime entity builder repository requires a pool")
	}
	return &CurrentRuntimeEntityBuilderRepository{pool: pool, skills: NewSkillsRepo(pool)}, nil
}

var (
	_ storage.RuntimeSkillSink          = (*CurrentRuntimeEntityBuilderRepository)(nil)
	_ storage.RuntimeProjectContextSink = (*CurrentRuntimeEntityBuilderRepository)(nil)
)

// UpsertRuntimeSkillByName creates the named skill, or updates the one that
// already carries that name in this project.
//
// NAME, not id, is the selector — see RuntimeSkillWriteRequest's own doc
// comment for why. The lookup is SkillsRepo.GetByName's own statement rather
// than a new one, so "which row does this name mean" has one answer in this
// service (lowest id wins on a duplicate, which a project can hold: skills.name
// carries no unique constraint).
//
// The whole write runs in ONE transaction. A create that inserted the skill and
// then failed to write its `base` version would leave a skill whose every read
// renders blank — the exact shape of the skills-read gap #611 recorded — and a
// user would have no way to tell that from a skill that was never created.
func (r *CurrentRuntimeEntityBuilderRepository) UpsertRuntimeSkillByName(
	ctx context.Context,
	projectID int64,
	name string,
	description string,
	instructions string,
) (storage.RuntimeSkillRecord, error) {
	projectKey := strconv.FormatInt(projectID, 10)
	tenantSchema := schema(projectKey)
	ownerID, err := tenantschema.OwnerID(projectKey)
	if err != nil {
		return storage.RuntimeSkillRecord{}, fmt.Errorf("runtime skill write: owner: %w", err)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return storage.RuntimeSkillRecord{}, fmt.Errorf("runtime skill write: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var skillID string
	created := false
	err = tx.QueryRow(
		ctx,
		fmt.Sprintf(`SELECT id FROM %s.skills WHERE name = $1 ORDER BY id LIMIT 1`, tenantSchema),
		name,
	).Scan(&skillID)
	switch {
	case err == nil:
		if _, updateErr := tx.Exec(
			ctx,
			fmt.Sprintf(`UPDATE %s.skills SET name = $1, description = $2 WHERE id = $3`, tenantSchema),
			name, description, skillID,
		); updateErr != nil {
			return storage.RuntimeSkillRecord{}, fmt.Errorf("runtime skill write: update: %w", updateErr)
		}
	case errors.Is(err, pgx.ErrNoRows):
		created = true
		var insertedName, insertedDescription string
		var createdAt any
		if insertErr := tx.QueryRow(
			ctx, createSkillSQL(tenantSchema), name, description, ownerID.Int64(),
		).Scan(&skillID, &insertedName, &insertedDescription, &createdAt); insertErr != nil {
			return storage.RuntimeSkillRecord{}, fmt.Errorf("runtime skill write: create: %w", insertErr)
		}
	default:
		return storage.RuntimeSkillRecord{}, fmt.Errorf("runtime skill write: lookup: %w", err)
	}

	// Tags are deliberately left alone: the builder tool writes a skill's
	// CONTENT, and passing nil here replaces an existing skill's tags with an
	// empty set. `replaceVersionTags` is a replace, not a merge, so an update
	// that meant only to rewrite the instructions would silently strip every
	// tag a user had put on the skill by hand.
	existingTags, err := readBaseSkillVersionTags(ctx, tx, tenantSchema, skillID)
	if err != nil {
		return storage.RuntimeSkillRecord{}, err
	}
	if _, err := upsertBaseSkillVersion(ctx, tx, tenantSchema, skillID, instructions, existingTags); err != nil {
		return storage.RuntimeSkillRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return storage.RuntimeSkillRecord{}, fmt.Errorf("runtime skill write: commit: %w", err)
	}
	return storage.RuntimeSkillRecord{SkillID: skillID, Name: name, Created: created}, nil
}

// readBaseSkillVersionTags returns the tag NAMES already attached to a skill's
// `base` version, so an update can hand them straight back to
// upsertBaseSkillVersion and leave them unchanged. Returns nil for a skill with
// no base version yet (a create, on the row this transaction just inserted).
func readBaseSkillVersionTags(
	ctx context.Context,
	tx pgx.Tx,
	tenantSchema string,
	skillID string,
) ([]string, error) {
	rows, err := tx.Query(ctx, fmt.Sprintf(`
		SELECT t.name
		FROM %[1]s.skill_versions sv
		JOIN %[1]s.skill_version_tag_association a ON a.version_id = sv.id
		JOIN %[1]s.tags t ON t.id = a.tag_id
		WHERE sv.skill_id = $1 AND sv.name = 'base'
		ORDER BY t.name`, tenantSchema), skillID)
	if err != nil {
		return nil, fmt.Errorf("runtime skill write: read tags: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if scanErr := rows.Scan(&name); scanErr != nil {
			return nil, fmt.Errorf("runtime skill write: read tags: %w", scanErr)
		}
		names = append(names, name)
	}
	if rows.Err() != nil {
		return nil, fmt.Errorf("runtime skill write: read tags: %w", rows.Err())
	}
	return names, nil
}

// ReadRuntimeProjectContext reads the project's single context row.
//
// A missing row is `Found: false`, not an error: a project that has never had
// a context is the normal precondition for the CREATE case (ELITEA-2786), and
// turning it into a failure is the "absent reads as failure" shape that has
// already cost this service two 500-class defects.
func (r *CurrentRuntimeEntityBuilderRepository) ReadRuntimeProjectContext(
	ctx context.Context,
	projectID int64,
) (storage.RuntimeProjectContextRecord, error) {
	tenantSchema := schema(strconv.FormatInt(projectID, 10))
	var data []byte
	err := r.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT data FROM %s.configuration WHERE type = 'project_context' LIMIT 1`, tenantSchema,
	)).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return storage.RuntimeProjectContextRecord{}, nil
	}
	if err != nil {
		return storage.RuntimeProjectContextRecord{}, fmt.Errorf("runtime project context read: %w", err)
	}
	record := storage.RuntimeProjectContextRecord{Found: true}
	if len(data) == 0 {
		return record, nil
	}
	var stored struct {
		Content string `json:"content"`
		Enabled bool   `json:"enabled"`
	}
	// A malformed row reads as an empty-but-present context rather than an
	// error, matching eliteacore's own handler: the write that follows replaces
	// it wholesale anyway.
	_ = json.Unmarshal(data, &stored)
	record.Content = stored.Content
	record.Enabled = stored.Enabled
	return record, nil
}

// WriteRuntimeProjectContext upserts the project's context row.
//
// UPDATE-first, then INSERT with the project_id column named — the exact shape
// eliteacore's writeProjectContext settled on (#888), restated here rather than
// called because that method hangs off the HTTP handler's own struct. All three
// properties its doc comment records are load-bearing and all three are kept:
// project_id is NOT NULL on this table, the conflict target is the bare
// elitea_title column (its UNIQUE constraint is not partial), and the UPDATE's
// zero-row case falls through to the INSERT instead of being discarded.
func (r *CurrentRuntimeEntityBuilderRepository) WriteRuntimeProjectContext(
	ctx context.Context,
	projectID int64,
	content string,
	enabled bool,
) error {
	tenantSchema := schema(strconv.FormatInt(projectID, 10))
	data, err := json.Marshal(map[string]any{"content": content, "enabled": enabled})
	if err != nil {
		return fmt.Errorf("runtime project context write: encode: %w", err)
	}
	tag, err := r.pool.Exec(ctx, fmt.Sprintf(
		`UPDATE %s.configuration SET data = $1, updated_at = NOW() WHERE type = 'project_context'`,
		tenantSchema,
	), data)
	if err != nil {
		return fmt.Errorf("runtime project context write: update: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	if _, err := r.pool.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s.configuration
			(project_id, label, elitea_title, type, section, data, status_ok, created_at)
		VALUES ($1, 'Project Context', $2, 'project_context', 'project_context', $3, true, NOW())
		ON CONFLICT (elitea_title) DO UPDATE
			SET data = EXCLUDED.data, updated_at = NOW()`, tenantSchema),
		projectID, fmt.Sprintf("project_context_%d", projectID), data,
	); err != nil {
		return fmt.Errorf("runtime project context write: insert: %w", err)
	}
	return nil
}
