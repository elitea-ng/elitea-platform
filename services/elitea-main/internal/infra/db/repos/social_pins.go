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
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/projectaccess"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// CurrentSocialPinsRepository stores one shared pin per project and entity.
// The user column records the last pinner.
//
// Every write decides project membership INSIDE its own statement, with the
// predicate the HTTP gate uses (internal/infra/db/projectaccess). The route
// gate is defence in depth: a caller that reaches this repository without it,
// or a membership revoked between the gate and the write, is refused here and
// the pin table does not change. Conversation writes also require the existing
// chat detail authority.
type CurrentSocialPinsRepository struct {
	pool *pgxpool.Pool
}

func NewCurrentSocialPinsRepository(pool *pgxpool.Pool) *CurrentSocialPinsRepository {
	return &CurrentSocialPinsRepository{pool: pool}
}

// pinTenantTables maps each entity type a NEW pin may name to the tenant table
// that proves the entity exists in the project. Agents, pipelines and
// applications are all rows of `applications`; toolkits and MCP servers are
// rows of `elitea_tools`.
//
// `prompt`, `collection` and `datasource` are accepted by validateSocialPin so
// that a stale pin can still be removed, but the tenant schema has no table
// behind them, so a new pin cannot be proven to name anything. Pin refuses
// them; Unpin accepts them.
var pinTenantTables = map[string]string{
	"application":   "applications",
	"agent":         "applications",
	"pipeline":      "applications",
	"toolkit":       "elitea_tools",
	"mcp":           "elitea_tools",
	"configuration": "configuration",
	"skill":         "skills",
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

// socialPinTimeout bounds one pin or unpin transaction.
const socialPinTimeout = 5 * time.Second

// requirePinProjectAccess is the first statement of every pin transaction. It
// reads only shared auth tables, so a caller who is not a member of the
// project is refused before any tenant table is named, and a project whose
// tenant schema does not exist cannot turn a refusal into a 500. The write
// statement repeats the membership predicate: this call picks the status, the
// write decides.
func requirePinProjectAccess(ctx context.Context, tx pgx.Tx, project, actor int32) error {
	var member, exists bool
	if err := tx.QueryRow(ctx,
		"SELECT "+projectaccess.Membership(1, 2)+", "+projectaccess.ProjectExists(1),
		project, actor).Scan(&member, &exists); err != nil {
		return fmt.Errorf("check pin project access: %w", err)
	}
	// Refuse a non-member BEFORE the existence answer, as the HTTP gate does.
	if !member {
		return apierr.Forbidden("forbidden")
	}
	if !exists {
		return apierr.NotFound("project not found")
	}
	return nil
}

// Pin replaces the last pinner without creating another shared row.
//
// The caller must be a member of the project (see CurrentSocialPinsRepository)
// and, for every entity type except `conversation`, the entity must exist in
// the project's tenant schema; both are decided in the INSERT's own statement.
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
	table, tenantBacked := pinTenantTables[entity]
	if entity != "conversation" && !tenantBacked {
		return apierr.BadRequest("invalid entity type")
	}
	if r.pool == nil {
		return &apierr.APIError{Status: http.StatusServiceUnavailable, Message: "pin storage unavailable"}
	}
	ctx, cancel := context.WithTimeout(ctx, socialPinTimeout)
	defer cancel()

	var access chatauthority.Scope
	if entity == "conversation" {
		if access, err = chatauthority.Load(ctx, r.pool, projectID); err != nil {
			return err
		}
	}
	schema, err := tenantschema.Quote(projectID)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		if err := requirePinProjectAccess(ctx, tx, project, actor); err != nil {
			return err
		}
		if entity != "conversation" {
			return pinEntity(ctx, tx, schema, table, entity, project, id, actor)
		}
		return pinConversation(ctx, tx, schema, access, entity, project, id, actor)
	})
}

// pinUpsert is the shared tail of every pin INSERT.
const pinUpsert = ` ON CONFLICT (entity, project_id, entity_id)
	 DO UPDATE SET user_id=EXCLUDED.user_id, updated_at=EXCLUDED.updated_at`

// pinEntity writes the pin of a non-conversation entity. The membership
// predicate and the entity-exists check are the INSERT's own WHERE clause, so
// neither can change between the decision and the write.
//
// FOR SHARE OF the entity row: the pin holds the row until it commits, so a
// concurrent delete of the entity either finished first (the row is gone and
// the pin answers 404) or waits for the pin.
//
// table comes from pinTenantTables and schema from tenantschema.Quote; neither
// is caller text.
func pinEntity(ctx context.Context, tx pgx.Tx, schema, table, entity string, project, id, actor int32) error {
	query := fmt.Sprintf(`WITH access AS (SELECT %s AS ok),
	 found AS (SELECT 1 FROM %s.%s WHERE id=$3 FOR SHARE),
	 written AS (
	  INSERT INTO centry.social_pins (entity, project_id, entity_id, user_id, created_at, updated_at)
	  SELECT $1, $2, $3, $4, NOW(), NOW()
	  WHERE (SELECT ok FROM access) AND EXISTS (SELECT 1 FROM found)`+pinUpsert+`
	  RETURNING 1)
	 SELECT (SELECT ok FROM access), EXISTS (SELECT 1 FROM found)`,
		projectaccess.Membership(2, 4), schema, table)
	var allowed, found bool
	if err := tx.QueryRow(ctx, query, entity, project, id, actor).Scan(&allowed, &found); err != nil {
		return fmt.Errorf("pin shared entity: %w", err)
	}
	if !allowed {
		return apierr.Forbidden("forbidden")
	}
	if !found {
		return apierr.NotFound("entity not found")
	}
	return nil
}

// pinConversation writes a conversation pin: the membership predicate, the
// chat detail authority and the row lock are one statement.
//
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
// Pin, Unpin and Delete all lock the conversation row before the pin
// row, so they queue on the conversation instead of deadlocking.
func pinConversation(ctx context.Context, tx pgx.Tx, schema string, access chatauthority.Scope, entity string, project, id, actor int32) error {
	visible, identity := access.Predicate(schema, "c", 5, chatauthority.Detail)
	query := fmt.Sprintf(`WITH access AS (SELECT %s AS ok),
	 visible AS (SELECT c.id FROM %s.chat_conversations c WHERE c.id=$3 AND %s FOR NO KEY UPDATE OF c),
	 written AS (
	  INSERT INTO centry.social_pins (entity, project_id, entity_id, user_id, created_at, updated_at)
	  SELECT $1, $2, $3, $4, NOW(), NOW()
	  WHERE (SELECT ok FROM access) AND EXISTS (SELECT 1 FROM visible)`+pinUpsert+`
	  RETURNING (xmax = 0) AS inserted)
	 SELECT (SELECT ok FROM access), EXISTS (SELECT 1 FROM visible), COALESCE((SELECT inserted FROM written), false)`,
		projectaccess.Membership(2, 4), schema, visible)
	var allowed, found, inserted bool
	if err := tx.QueryRow(ctx, query, append([]any{entity, project, id, actor}, identity...)...).Scan(&allowed, &found, &inserted); err != nil {
		return fmt.Errorf("pin shared conversation: %w", err)
	}
	if !allowed {
		return apierr.Forbidden("forbidden")
	}
	if !found {
		return apierr.NotFound("chat resource not found")
	}
	if !inserted {
		return nil
	}
	return stampConversationPin(ctx, tx, schema, id)
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
// The caller must be a member of the project; the DELETE carries the same
// predicate. Repeating the operation on an accessible entity succeeds, and so
// does removing a pin that is absent. Removing a conversation pin stamps the
// conversation as Pin does; a repeated unpin removes nothing and stamps
// nothing.
func (r *CurrentSocialPinsRepository) Unpin(ctx context.Context, projectID, entity, entityID string) error {
	project, id, actor, err := validateSocialPin(ctx, projectID, entity, entityID)
	if err != nil {
		return err
	}
	if r.pool == nil {
		return &apierr.APIError{Status: http.StatusServiceUnavailable, Message: "pin storage unavailable"}
	}
	ctx, cancel := context.WithTimeout(ctx, socialPinTimeout)
	defer cancel()
	if entity != "conversation" {
		return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
			if err := requirePinProjectAccess(ctx, tx, project, actor); err != nil {
				return err
			}
			return unpinEntity(ctx, tx, entity, project, id, actor)
		})
	}
	access, err := chatauthority.Load(ctx, r.pool, projectID)
	if err != nil {
		return err
	}
	schema, err := tenantschema.Quote(projectID)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		if err := requirePinProjectAccess(ctx, tx, project, actor); err != nil {
			return err
		}
		return unpinConversation(ctx, tx, schema, access, entity, project, id, actor)
	})
}

// unpinEntity deletes the pin of a non-conversation entity when the actor is a
// member. A pin that is not there is a success, not a refusal.
func unpinEntity(ctx context.Context, tx pgx.Tx, entity string, project, id, actor int32) error {
	query := `WITH access AS (SELECT ` + projectaccess.Membership(2, 4) + ` AS ok),
	 removed AS (
	  DELETE FROM centry.social_pins WHERE entity=$1 AND project_id=$2 AND entity_id=$3
	  AND (SELECT ok FROM access) RETURNING id)
	 SELECT (SELECT ok FROM access), EXISTS (SELECT 1 FROM removed)`
	var allowed, removed bool
	if err := tx.QueryRow(ctx, query, entity, project, id, actor).Scan(&allowed, &removed); err != nil {
		return fmt.Errorf("unpin shared entity: %w", err)
	}
	if !allowed {
		return apierr.Forbidden("forbidden")
	}
	return nil
}

// unpinConversation removes a conversation pin. The DELETE runs only once
// `visible` has answered, so the conversation row is locked before the pin
// row, in Pin's and Delete's order (see pinConversation). Taking the pin row
// first deadlocked against a Delete or a Pin already holding the conversation.
func unpinConversation(ctx context.Context, tx pgx.Tx, schema string, access chatauthority.Scope, entity string, project, id, actor int32) error {
	visible, identity := access.Predicate(schema, "c", 5, chatauthority.Detail)
	query := fmt.Sprintf(`WITH access AS (SELECT %s AS ok),
	 visible AS (
	  SELECT c.id FROM %s.chat_conversations c WHERE c.id=$3 AND %s FOR NO KEY UPDATE OF c
	 ), removed AS (
	  DELETE FROM centry.social_pins WHERE entity=$1 AND project_id=$2 AND entity_id=$3
	  AND (SELECT ok FROM access) AND EXISTS (SELECT 1 FROM visible) RETURNING id
	 ) SELECT (SELECT ok FROM access), EXISTS (SELECT 1 FROM visible), EXISTS (SELECT 1 FROM removed)`,
		projectaccess.Membership(2, 4), schema, visible)
	var member, allowed, removed bool
	if err := tx.QueryRow(ctx, query, append([]any{entity, project, id, actor}, identity...)...).Scan(&member, &allowed, &removed); err != nil {
		return fmt.Errorf("unpin shared conversation: %w", err)
	}
	if !member {
		return apierr.Forbidden("forbidden")
	}
	if !allowed {
		return apierr.NotFound("chat resource not found")
	}
	if !removed {
		return nil
	}
	return stampConversationPin(ctx, tx, schema, id)
}
