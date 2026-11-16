package repos

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// trustEditedForeignBase records the project's trust in the CURRENT base text
// of a skill forked from another project, after a person in this project saved
// that text (ADR-0051 amendment 1, A1.6). It runs in the update's transaction,
// after the new instructions are written, so the revision it hashes is the one
// the next turn will read.
//
// A skill that is not foreign (no parent project, or this one) gets no row; the
// resolver never asks for one. The insert is a no-op when an active row for the
// revision already exists. A schema without the table (a fixture that builds
// its own tenant schema) is left alone rather than failing the edit.
func trustEditedForeignBase(ctx context.Context, tx pgx.Tx, schema, projectID, skillID string) error {
	var present bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+".skill_trust").Scan(&present); err != nil {
		return fmt.Errorf("skills: trust edit: probe: %w", err)
	}
	if !present {
		return nil
	}
	var trustedBy any
	if user, ok := auth.UserFromContext(ctx); ok {
		if id, ok := user.OwningUserID(); ok {
			trustedBy = id
		}
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
INSERT INTO %[1]s.skill_trust (skill_id, revision, publisher_project_id, trusted_by, source)
SELECT sv.skill_id,
       encode(sha256(convert_to(COALESCE(sv.instructions, ''), 'UTF8')), 'hex'),
       CASE WHEN sv.meta ->> 'parent_project_id' ~ '^[0-9]+$' THEN (sv.meta ->> 'parent_project_id')::integer END,
       $3::integer, 'user'
FROM %[1]s.skill_versions sv
WHERE sv.skill_id = $1 AND sv.name = 'base'
  AND COALESCE(sv.meta ->> 'parent_project_id', '') <> ''
  AND sv.meta ->> 'parent_project_id' IS DISTINCT FROM $2
ON CONFLICT DO NOTHING`, schema), skillID, projectID, trustedBy); err != nil {
		return fmt.Errorf("skills: trust edit: %w", err)
	}
	return nil
}
