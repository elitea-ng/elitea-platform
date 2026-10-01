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

	query := `INSERT INTO centry.social_pins (entity, project_id, entity_id, user_id, created_at, updated_at)
	 VALUES ($1, $2, $3, $4, NOW(), NOW())`
	args := []any{entity, project, id, actor}
	if entity == "conversation" {
		access, err := chatauthority.Load(ctx, r.pool, projectID)
		if err != nil {
			return err
		}
		schema, err := tenantschema.Quote(projectID)
		if err != nil {
			return err
		}
		visible, identity := access.Predicate(schema, "c", 5, chatauthority.Detail)
		query = fmt.Sprintf(`INSERT INTO centry.social_pins (entity, project_id, entity_id, user_id, created_at, updated_at)
		 SELECT $1, $2, $3, $4, NOW(), NOW() FROM %s.chat_conversations c
		 WHERE c.id=$3 AND %s`, schema, visible)
		args = append(args, identity...)
	}
	query += ` ON CONFLICT (entity, project_id, entity_id)
	 DO UPDATE SET user_id=EXCLUDED.user_id, updated_at=EXCLUDED.updated_at RETURNING id`
	var pinID int64
	if err := r.pool.QueryRow(ctx, query, args...).Scan(&pinID); errors.Is(err, pgx.ErrNoRows) {
		return apierr.NotFound("chat resource not found")
	} else if err != nil {
		return fmt.Errorf("pin shared entity: %w", err)
	}
	return nil
}

// Unpin removes the shared row regardless of which authorized member pinned it.
// Repeating the operation on an accessible entity succeeds.
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
	) SELECT EXISTS (SELECT 1 FROM visible)`, schema, visible)
	var allowed bool
	if err := r.pool.QueryRow(ctx, query, append([]any{entity, project, id}, identity...)...).Scan(&allowed); err != nil {
		return fmt.Errorf("unpin shared conversation: %w", err)
	}
	if !allowed {
		return apierr.NotFound("chat resource not found")
	}
	return nil
}
