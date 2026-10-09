package repos

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/chatauthority"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// CurrentSocialPinsRepository stores one shared pin per project and entity.
// The user column records the last pinner. Project access belongs to the route.
// Conversation writes also require the existing chat detail authority.
type CurrentSocialPinsRepository struct {
	pool *pgxpool.Pool
}

func NewCurrentSocialPinsRepository(pool *pgxpool.Pool) *CurrentSocialPinsRepository {
	return &CurrentSocialPinsRepository{pool: pool}
}

func validateSocialPin(ctx context.Context, projectID, entity, entityID string) (int32, int32, int32, error) {
	user, ok := auth.UserFromContext(ctx)
	if !ok {
		return 0, 0, 0, apierr.Unauthorized("unauthorized")
	}
	actor, ok := user.OwningUserID()
	if !ok || actor > math.MaxInt32 {
		return 0, 0, 0, apierr.Forbidden("forbidden")
	}
	project, err := socialPinID(projectID)
	if err != nil {
		return 0, 0, 0, apierr.BadRequest("invalid project id")
	}
	id, err := socialPinID(entityID)
	if err != nil {
		return 0, 0, 0, apierr.BadRequest("invalid entity id")
	}
	switch entity {
	case "prompt", "collection", "datasource", "application", "toolkit", "configuration", "conversation", "skill", "agent", "pipeline", "mcp":
	default:
		return 0, 0, 0, apierr.BadRequest("invalid entity type")
	}
	return project, id, int32(actor), nil
}

func socialPinID(value string) (int32, error) {
	if !tenantschema.Valid(value) {
		return 0, errors.New("invalid pin identity")
	}
	id, err := strconv.ParseInt(value, 10, 32)
	return int32(id), err
}

// Pin replaces the last pinner without creating another shared row.
//
// A conversation pin also stamps the conversation's sync_at (client contract
// 1.4): the list row carries `is_pinned`, and the `changes_since` delta
// re-delivers a row only when its stamp moves. The pin and the stamp commit in
// one transaction, so a reader that sees the new stamp sees the pin. Only a
// NEW pin stamps; repeating a pin rewrites the last pinner, which no row
// shows.
func (r *CurrentSocialPinsRepository) Pin(ctx context.Context, projectID, entity, entityID string) error {
	project, id, actor, err := validateSocialPin(ctx, projectID, entity, entityID)
	if err != nil {
		return err
	}
	if r.pool == nil {
		return &apierr.APIError{Status: http.StatusServiceUnavailable, Message: "pin storage unavailable"}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	const upsert = ` ON CONFLICT (entity, project_id, entity_id)
	 DO UPDATE SET user_id=EXCLUDED.user_id, updated_at=EXCLUDED.updated_at RETURNING (xmax = 0)`
	if entity != "conversation" {
		var inserted bool
		if err := r.pool.QueryRow(ctx, `INSERT INTO centry.social_pins (entity, project_id, entity_id, user_id, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, NOW(), NOW())`+upsert, entity, project, id, actor).Scan(&inserted); err != nil {
			return fmt.Errorf("pin shared entity: %w", err)
		}
		return nil
	}
	access, err := chatauthority.Load(ctx, r.pool, projectID)
	if err != nil {
		return err
	}
	schema, err := tenantschema.Quote(projectID)
	if err != nil {
		return err
	}
	visible, identity := access.Predicate(schema, "c", 5, chatauthority.Detail)
	// FOR NO KEY UPDATE OF c: the pin holds the conversation row until it commits.
	// A plain read let a concurrent Delete remove the row and run its pin
	// DELETE (which cannot see an uncommitted pin) between this INSERT and
	// the commit, leaving a pin for a conversation that no longer exists.
	// With the lock, a Delete that got there first makes this SELECT skip
	// the gone row (404), and one that comes second waits for the pin and
	// then removes it. The lock is exclusive among pins on purpose: the
	// transaction later UPDATEs the row (stampConversationPin), and two pins
	// each holding FOR SHARE then both upgrading deadlock (HTTP 500). NO KEY
	// is enough because Delete takes FOR UPDATE, which conflicts with it.
	query := fmt.Sprintf(`INSERT INTO centry.social_pins (entity, project_id, entity_id, user_id, created_at, updated_at)
	 SELECT $1, $2, $3, $4, NOW(), NOW() FROM %s.chat_conversations c
	 WHERE c.id=$3 AND %s FOR NO KEY UPDATE OF c`, schema, visible) + upsert
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var inserted bool
		if err := tx.QueryRow(ctx, query, append([]any{entity, project, id, actor}, identity...)...).Scan(&inserted); errors.Is(err, pgx.ErrNoRows) {
			return apierr.NotFound("chat resource not found")
		} else if err != nil {
			return fmt.Errorf("pin shared conversation: %w", err)
		}
		if !inserted {
			return nil
		}
		return stampConversationPin(ctx, tx, schema, id)
	})
}

// stampConversationPin moves the conversation's sync_at so the list delta
// re-delivers the row with its new `is_pinned`. The tenant's BEFORE UPDATE
// trigger (tenant/0144 chat_sync_stamp) writes clock_timestamp() whatever the
// statement sets, and no other column changes: a pin is not an edit, so
// updated_at (the row's "last modified") and the lost-access markers stay as
// they are.
func stampConversationPin(ctx context.Context, tx pgx.Tx, schema string, id int32) error {
	tag, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.chat_conversations SET sync_at = clock_timestamp() WHERE id = $1`, schema), id)
	if err != nil {
		return fmt.Errorf("stamp pinned conversation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// The conversation went away under the pin; never commit a pin
		// (or an unpin's stamp) for a row that is gone.
		return apierr.NotFound("chat resource not found")
	}
	return nil
}

// Unpin removes the shared row regardless of which authorized member pinned it.
// Repeating the operation on an accessible entity succeeds. Removing a
// conversation pin stamps the conversation as Pin does; a repeated unpin
// removes nothing and stamps nothing.
func (r *CurrentSocialPinsRepository) Unpin(ctx context.Context, projectID, entity, entityID string) error {
	project, id, _, err := validateSocialPin(ctx, projectID, entity, entityID)
	if err != nil {
		return err
	}
	if r.pool == nil {
		return &apierr.APIError{Status: http.StatusServiceUnavailable, Message: "pin storage unavailable"}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if entity != "conversation" {
		_, err := r.pool.Exec(ctx, `DELETE FROM centry.social_pins WHERE entity=$1 AND project_id=$2 AND entity_id=$3`, entity, project, id)
		if err != nil {
			return fmt.Errorf("unpin shared entity: %w", err)
		}
		return nil
	}
	access, err := chatauthority.Load(ctx, r.pool, projectID)
	if err != nil {
		return err
	}
	schema, err := tenantschema.Quote(projectID)
	if err != nil {
		return err
	}
	visible, identity := access.Predicate(schema, "c", 4, chatauthority.Detail)
	query := fmt.Sprintf(`WITH visible AS (
	 SELECT c.id FROM %s.chat_conversations c WHERE c.id=$3 AND %s
	), removed AS (
	 DELETE FROM centry.social_pins WHERE entity=$1 AND project_id=$2 AND entity_id=$3
	 AND EXISTS (SELECT 1 FROM visible) RETURNING id
	) SELECT EXISTS (SELECT 1 FROM visible), EXISTS (SELECT 1 FROM removed)`, schema, visible)
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var allowed, removed bool
		if err := tx.QueryRow(ctx, query, append([]any{entity, project, id}, identity...)...).Scan(&allowed, &removed); err != nil {
			return fmt.Errorf("unpin shared conversation: %w", err)
		}
		if !allowed {
			return apierr.NotFound("chat resource not found")
		}
		if !removed {
			return nil
		}
		return stampConversationPin(ctx, tx, schema, id)
	})
}
