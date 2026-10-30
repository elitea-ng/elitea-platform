// Package toolrelation is the one writer of an agent version's toolkit
// attachments (tenant entity_tool_mapping): the toolkit update route's
// relation branch (api/v2/toolkits, has_relation) and the permission
// trimming report's one-click removals (ADR-0044 decision 5, #1501) both go
// through it, so the published-version refusal and the selection rules are
// the same on both paths.
package toolrelation

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrPublishedVersion refuses a change to a published or embedded version:
// what runs is its release, changed by editing the draft and publishing.
var ErrPublishedVersion = errors.New("cannot change tools on a published version")

// PublishedVersionMessage is the toolkit route's wire text for
// ErrPublishedVersion (clients match it; the web app's ToolMenu shows it).
const PublishedVersionMessage = "Cannot change tools on a published version. Unpublish first."

// DB is a pool or a transaction.
type DB interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// CheckVersionEditable refuses a published or embedded version. A version
// whose status cannot be read is not refused here (the route's historical
// behaviour); the write that follows names no row then.
//
// schema is the QUOTED tenant schema.
func CheckVersionEditable(ctx context.Context, db DB, schema string, versionID any) error {
	var status string
	err := db.QueryRow(ctx, fmt.Sprintf(`SELECT status FROM %s.application_versions WHERE id = $1`, schema), versionID).Scan(&status)
	if err == nil && (status == "published" || status == "embedded") {
		return ErrPublishedVersion
	}
	return nil
}

// Attach upserts the version's mapping to the toolkit. selected is the
// JSON array of the agent's selected tools; nil leaves an existing
// selection alone (an attach with nothing to say about it, #248).
func Attach(ctx context.Context, db DB, schema string, versionID, entityID any, entityType string, toolID any, selected []byte) error {
	args := []any{versionID, entityID, entityType, toolID}
	if selected != nil {
		args = append(args, selected)
	}
	_, err := db.Exec(ctx, AttachSQL(schema, selected != nil), args...)
	return err
}

// AttachSQL builds the attach statement. `_entity_tool_unique
// (entity_version_id, tool_id, entity_type)` is the upsert target, so a
// selection edit on an ALREADY-attached toolkit updates the existing
// mapping row instead of conflicting into a no-op.
func AttachSQL(schema string, withSelectedTools bool) string {
	if !withSelectedTools {
		return fmt.Sprintf(`INSERT INTO %s.entity_tool_mapping (entity_version_id, entity_id, entity_type, tool_id, created_at) VALUES ($1, $2, $3, $4, now() AT TIME ZONE 'UTC') ON CONFLICT (entity_version_id, tool_id, entity_type) DO NOTHING`, schema)
	}
	return fmt.Sprintf(`INSERT INTO %s.entity_tool_mapping (entity_version_id, entity_id, entity_type, tool_id, selected_tools, created_at) VALUES ($1, $2, $3, $4, $5::jsonb, now() AT TIME ZONE 'UTC') ON CONFLICT (entity_version_id, tool_id, entity_type) DO UPDATE SET selected_tools = EXCLUDED.selected_tools, updated_at = (now() AT TIME ZONE 'UTC')`, schema)
}

// Detach removes the version's mapping to the toolkit and reports how many
// rows it removed.
func Detach(ctx context.Context, db DB, schema string, versionID, toolID any) (int64, error) {
	tag, err := db.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.entity_tool_mapping WHERE entity_version_id = $1 AND tool_id = $2`, schema), versionID, toolID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
