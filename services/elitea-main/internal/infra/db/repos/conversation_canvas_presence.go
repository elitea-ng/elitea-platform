package repos

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// ResolveCanvas proves that canvasID names a canvas item INSIDE the tenant
// schema of projectID, and returns that canvas's uuid together with the uuid of
// the message group it belongs to.
//
// This is the cross-project control for #622's presence route, and it is the
// reason the route cannot be served from the {projectID} segment alone. Canvas
// ids are per-tenant-schema SERIAL integers (migrations/tenant/0123's
// chat_message_items), so id 7 exists in almost every project and means a
// different canvas in each. A presence heartbeat that accepted the id without
// resolving it would publish an entry naming another project's canvas.
//
// THE ID PARAMETER TAKES EITHER FORM, which is not an invention here: the
// product UI carries `canvas_uuid` (shared/api/socket/rooms.ts) while the
// canvas READ route is reached with the integer row id, and both land on the
// same {canvasID} segment. A decimal value is matched against `id`, anything
// else against `uuid`. A value that is neither an integer nor a uuid never
// reaches Postgres as a uuid cast — it answers 404, not the 500 that
// "invalid input syntax for type uuid" (22P02) would produce.
//
// The item_type check is load-bearing on its own: chat_message_items holds text,
// tool-call, attachment and context items on the same table and the same id
// sequence, so without it a TEXT item's id would resolve and presence would be
// announced for something that is not a canvas at all. The join to
// chat_messages_canvas is the second half of the same claim — the payload row
// has to exist.
func (r *ConversationsRepo) ResolveCanvas(ctx context.Context, projectID, canvasID string) (string, string, error) {
	s, err := tenantSchema(projectID)
	if err != nil {
		return "", "", err
	}

	var query string
	var arg any
	if rowID, convErr := strconv.ParseInt(canvasID, 10, 64); convErr == nil {
		query = fmt.Sprintf(`
			SELECT mi.uuid::text, mg.uuid::text
			FROM %s.chat_message_items mi
			JOIN %s.chat_message_group mg ON mg.id = mi.message_group_id
			JOIN %s.chat_messages_canvas mc ON mc.id = mi.id
			WHERE mi.id = $1 AND mi.item_type = 'canvas_message'`, s, s, s)
		arg = rowID
	} else {
		if !isCanvasUUID(canvasID) {
			return "", "", apierr.NotFound("canvas not found")
		}
		query = fmt.Sprintf(`
			SELECT mi.uuid::text, mg.uuid::text
			FROM %s.chat_message_items mi
			JOIN %s.chat_message_group mg ON mg.id = mi.message_group_id
			JOIN %s.chat_messages_canvas mc ON mc.id = mi.id
			WHERE mi.uuid = $1::uuid AND mi.item_type = 'canvas_message'`, s, s, s)
		arg = canvasID
	}

	var canvasUUID, messageGroupUUID string
	if err := r.pool.QueryRow(ctx, query, arg).Scan(&canvasUUID, &messageGroupUUID); err != nil {
		if err == pgx.ErrNoRows {
			return "", "", apierr.NotFound("canvas not found")
		}
		return "", "", fmt.Errorf("resolve canvas: %w", err)
	}
	return canvasUUID, messageGroupUUID, nil
}

// isCanvasUUID reports whether value has the 8-4-4-4-12 hexadecimal shape.
// Checked in Go rather than left to the `::uuid` cast: the cast raises 22P02,
// which surfaces as a 500 for what is only an id that cannot exist.
func isCanvasUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}
