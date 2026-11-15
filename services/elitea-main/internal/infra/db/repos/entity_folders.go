package repos

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/entityfolders"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/folders"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/foldervisibility"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// EntityFoldersRepo stores the project folders of the Agents, Pipelines,
// Skills, Toolkits and MCPs lists (tenant migration 0247).
type EntityFoldersRepo struct {
	pool *pgxpool.Pool
}

func NewEntityFoldersRepo(pool *pgxpool.Pool) *EntityFoldersRepo {
	return &EntityFoldersRepo{pool: pool}
}

// entityFolderOrder is the chat-folder order: highest position first, oldest
// first among equals.
const entityFolderOrder = `ORDER BY f.position DESC, f.created_at ASC, f.id ASC`

// entityFolderColumns selects a folder with the ids of the entities that are
// STILL of the folder's kind: a deleted entity, or one whose kind changed (an
// agent that gained a pipeline version), no longer counts as inside it.
func entityFolderColumns(s string, kind entityfolders.Kind) string {
	return fmt.Sprintf(`f.id::text, f.name, f.position, COALESCE(f.created_by::text, ''), f.created_at, f.updated_at,
	COALESCE(ARRAY(SELECT i.entity_id FROM %[1]s.project_folder_items i WHERE i.folder_id = f.id AND %[2]s ORDER BY i.position DESC, i.entity_id), '{}')::int[]`,
		s, entityLiveSQL(s, kind, "i.entity_id"))
}

func scanEntityFolder(row pgx.Row, kind entityfolders.Kind) (entityfolders.Folder, error) {
	f := entityfolders.Folder{Kind: kind}
	err := row.Scan(&f.ID, &f.Name, &f.Position, &f.CreatedBy, &f.CreatedAt, &f.UpdatedAt, &f.ItemIDs)
	if f.ItemIDs == nil {
		f.ItemIDs = []int{}
	}
	return f, err
}

func (r *EntityFoldersRepo) List(ctx context.Context, projectID string, kind entityfolders.Kind) ([]entityfolders.Folder, error) {
	s, err := tenantSchema(projectID)
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf(`SELECT `+entityFolderColumns(s, kind)+` FROM %[1]s.project_folders f WHERE f.entity_kind = $1 `+entityFolderOrder, s)
	rows, err := r.pool.Query(ctx, q, string(kind))
	if err != nil {
		return nil, fmt.Errorf("entity folders: list: %w", err)
	}
	defer rows.Close()
	items := []entityfolders.Folder{}
	for rows.Next() {
		f, err := scanEntityFolder(rows, kind)
		if err != nil {
			return nil, fmt.Errorf("entity folders: scan: %w", err)
		}
		items = append(items, f)
	}
	return items, rows.Err()
}

func (r *EntityFoldersRepo) Create(ctx context.Context, projectID string, kind entityfolders.Kind, name string, position *int) (entityfolders.Folder, error) {
	s, err := tenantSchema(projectID)
	if err != nil {
		return entityfolders.Folder{}, err
	}
	actor, err := entityFolderActor(ctx)
	if err != nil {
		return entityfolders.Folder{}, err
	}
	// A new folder lands at the TOP: one gap above the current maximum.
	q := fmt.Sprintf(`
		WITH ins AS (
			INSERT INTO %[1]s.project_folders (entity_kind, name, position, created_by)
			VALUES ($1::varchar, $2, COALESCE($3::int, (SELECT COALESCE(MAX(position), 0) + %[2]d FROM %[1]s.project_folders WHERE entity_kind = $1::varchar)), $4)
			RETURNING *
		)
		SELECT `+entityFolderColumns(s, kind)+` FROM ins f`, s, folders.PositionGap)
	f, err := scanEntityFolder(r.pool.QueryRow(ctx, q, string(kind), name, position, actor), kind)
	if err != nil {
		return entityfolders.Folder{}, entityFolderWriteErr("create", err)
	}
	return f, nil
}

func (r *EntityFoldersRepo) Update(ctx context.Context, projectID string, kind entityfolders.Kind, folderID string, name *string, position *int) (entityfolders.Folder, error) {
	s, err := tenantSchema(projectID)
	if err != nil {
		return entityfolders.Folder{}, err
	}
	if _, ok := int32RowID(folderID); !ok {
		return entityfolders.Folder{}, apierr.NotFound("folder not found")
	}
	q := fmt.Sprintf(`
		WITH upd AS (
			UPDATE %[1]s.project_folders
			SET name = COALESCE($1::varchar, name), position = COALESCE($2::int, position), updated_at = now()
			WHERE id = $3 AND entity_kind = $4
			RETURNING *
		)
		SELECT `+entityFolderColumns(s, kind)+` FROM upd f`, s)
	f, err := scanEntityFolder(r.pool.QueryRow(ctx, q, name, position, folderID, string(kind)), kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return entityfolders.Folder{}, apierr.NotFound("folder not found")
	}
	if err != nil {
		return entityfolders.Folder{}, entityFolderWriteErr("update", err)
	}
	return f, nil
}

func (r *EntityFoldersRepo) Rebalance(ctx context.Context, projectID string, kind entityfolders.Kind) ([]entityfolders.Folder, error) {
	s, err := tenantSchema(projectID)
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf(`
		WITH ordered AS (
			SELECT f.id, ROW_NUMBER() OVER (`+entityFolderOrder+`) AS rn, COUNT(*) OVER () AS n
			FROM %[1]s.project_folders f WHERE f.entity_kind = $1
		)
		UPDATE %[1]s.project_folders f SET position = ((o.n - o.rn) + 1) * %[2]d
		FROM ordered o WHERE f.id = o.id`, s, folders.PositionGap)
	if _, err := r.pool.Exec(ctx, q, string(kind)); err != nil {
		return nil, fmt.Errorf("entity folders: rebalance: %w", err)
	}
	return r.List(ctx, projectID, kind)
}

// Delete removes the folder. Its item rows go with it (ON DELETE CASCADE), so
// the entities return to the root; no entity row is touched.
func (r *EntityFoldersRepo) Delete(ctx context.Context, projectID string, kind entityfolders.Kind, folderID string) error {
	s, err := tenantSchema(projectID)
	if err != nil {
		return err
	}
	if _, ok := int32RowID(folderID); !ok {
		return apierr.NotFound("folder not found")
	}
	ct, err := r.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.project_folders WHERE id = $1 AND entity_kind = $2`, s), folderID, string(kind))
	if err != nil {
		return fmt.Errorf("entity folders: delete: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return apierr.NotFound("folder not found")
	}
	return nil
}

// entityLiveSQL is a boolean expression: idExpr is an entity of the kind, by the
// same rule the list uses. An agent is an application with no pipeline version,
// a pipeline one with a pipeline version (applicationIsPipelineSQL, shared with
// the applications list), an MCP a toolkit of type mcp / mcp_* or with
// meta.mcp = true.
func entityLiveSQL(s string, kind entityfolders.Kind, idExpr string) string {
	switch kind {
	case entityfolders.KindAgent:
		return fmt.Sprintf(`EXISTS (SELECT 1 FROM %[1]s.applications a WHERE a.id = %[2]s AND NOT `, s, idExpr) + applicationIsPipelineSQL(s, "a") + `)`
	case entityfolders.KindPipeline:
		return fmt.Sprintf(`EXISTS (SELECT 1 FROM %[1]s.applications a WHERE a.id = %[2]s AND `, s, idExpr) + applicationIsPipelineSQL(s, "a") + `)`
	case entityfolders.KindSkill:
		return fmt.Sprintf(`EXISTS (SELECT 1 FROM %s.skills WHERE id = %s)`, s, idExpr)
	case entityfolders.KindMCP:
		return fmt.Sprintf(`EXISTS (SELECT 1 FROM %s.elitea_tools t WHERE t.id = %s AND `, s, idExpr) + mcpToolkitSQL + `)`
	default:
		return fmt.Sprintf(`EXISTS (SELECT 1 FROM %s.elitea_tools t WHERE t.id = %s AND NOT `, s, idExpr) + mcpToolkitSQL + `)`
	}
}

const mcpToolkitSQL = `(t.type = 'mcp' OR left(t.type, 4) = 'mcp_' OR COALESCE(t.meta->>'mcp', '') = 'true')`

// int32RowID parses a folder id from a path or body. A value outside 32 bits cannot
// name a row, so it is "not found" and never reaches the integer column.
func int32RowID(raw string) (int, bool) {
	if !isNumericRowID(raw) {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 || n > math.MaxInt32 {
		return 0, false
	}
	return n, true
}

func (r *EntityFoldersRepo) Move(ctx context.Context, projectID string, kind entityfolders.Kind, entityID int, folderID *string, place entityfolders.Placement) error {
	s, err := tenantSchema(projectID)
	if err != nil {
		return err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("entity folders: move: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT `+entityLiveSQL(s, kind, "$1::int"), entityID).Scan(&exists); err != nil {
		return fmt.Errorf("entity folders: move: %w", err)
	}
	if !exists {
		return apierr.NotFound(string(kind) + " not found")
	}
	if folderID == nil {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.project_folder_items WHERE entity_kind = $1 AND entity_id = $2`, s), string(kind), entityID); err != nil {
			return fmt.Errorf("entity folders: move: %w", err)
		}
		return tx.Commit(ctx)
	}
	id, ok := int32RowID(*folderID)
	if !ok {
		return apierr.NotFound("folder not found")
	}
	// The composite FK (folder_id, entity_kind) refuses a folder of another
	// kind; the lock-free existence probe turns that into a clean 404.
	var folderExists bool
	if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s.project_folders WHERE id = $1 AND entity_kind = $2)`, s), id, string(kind)).Scan(&folderExists); err != nil {
		return fmt.Errorf("entity folders: move: %w", err)
	}
	if !folderExists {
		return apierr.NotFound("folder not found")
	}
	position, err := itemPosition(ctx, tx, s, kind, id, entityID, place)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s.project_folder_items (entity_kind, entity_id, folder_id, position) VALUES ($1, $2, $3, $4)
		ON CONFLICT (entity_kind, entity_id) DO UPDATE SET folder_id = EXCLUDED.folder_id, position = EXCLUDED.position, created_at = now()`, s),
		string(kind), entityID, id, position)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return apierr.NotFound("folder not found") // deleted between the probe and the write
		}
		return fmt.Errorf("entity folders: move: %w", err)
	}
	return tx.Commit(ctx)
}

// itemPosition is the position that puts the entity where the caller dropped
// it inside the folder: the top when no neighbour is named, otherwise between
// the neighbours (they win over any position, as for the folders). A neighbour
// that is not in the folder is a 400; the entity itself named as its own
// neighbour (a client computing from the displayed list, where it still sits)
// is ignored. When the neighbours are adjacent integers, or the new position
// would leave a 32-bit integer, the folder's other items are respaced by one gap
// first, inside the caller's transaction, keeping their order.
func itemPosition(ctx context.Context, tx pgx.Tx, s string, kind entityfolders.Kind, folderID, entityID int, place entityfolders.Placement) (int, error) {
	siblings, err := folderSiblings(ctx, tx, s, kind, folderID, entityID)
	if err != nil {
		return 0, err
	}
	above, err := neighbourID(place.Above, entityID, siblings)
	if err != nil {
		return 0, err
	}
	below, err := neighbourID(place.Below, entityID, siblings)
	if err != nil {
		return 0, err
	}
	position, rebalance := placedPosition(siblings, above, below)
	if !rebalance && position <= math.MaxInt32 {
		return position, nil
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`
		WITH ordered AS (
			SELECT entity_id, ROW_NUMBER() OVER (ORDER BY position DESC, entity_id ASC) AS rn, COUNT(*) OVER () AS n
			FROM %[1]s.project_folder_items WHERE folder_id = $1 AND entity_kind = $2 AND entity_id <> $3
		)
		UPDATE %[1]s.project_folder_items i SET position = ((o.n - o.rn) + 1) * %[2]d
		FROM ordered o WHERE i.entity_kind = $2 AND i.entity_id = o.entity_id`, s, folders.PositionGap), folderID, string(kind), entityID); err != nil {
		return 0, fmt.Errorf("entity folders: move: %w", err)
	}
	for i := range siblings {
		p := (len(siblings) - i) * folders.PositionGap
		siblings[i].Position = &p
	}
	position, _ = placedPosition(siblings, above, below)
	return position, nil
}

// placedPosition is the position for a drop between `above` and `below` ("" is
// absent), or the top when both are absent. rebalance says the neighbours leave
// no integer between them.
func placedPosition(siblings []folders.Folder, above, below string) (int, bool) {
	if above == "" && below == "" {
		top := 0
		if len(siblings) > 0 {
			top = *siblings[0].Position
		}
		return top + folders.PositionGap, false
	}
	return folders.PositionBetween(siblings, above, below)
}

// folderSiblings is the folder's other items, in display order.
func folderSiblings(ctx context.Context, tx pgx.Tx, s string, kind entityfolders.Kind, folderID, entityID int) ([]folders.Folder, error) {
	rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT entity_id, position FROM %s.project_folder_items
		WHERE folder_id = $1 AND entity_kind = $2 AND entity_id <> $3 ORDER BY position DESC, entity_id ASC`, s),
		folderID, string(kind), entityID)
	if err != nil {
		return nil, fmt.Errorf("entity folders: move: %w", err)
	}
	defer rows.Close()
	var siblings []folders.Folder
	for rows.Next() {
		var id, position int
		if err := rows.Scan(&id, &position); err != nil {
			return nil, fmt.Errorf("entity folders: move: %w", err)
		}
		siblings = append(siblings, folders.Folder{ID: strconv.Itoa(id), Position: &position})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("entity folders: move: %w", err)
	}
	return siblings, nil
}

// neighbourID is a neighbour as the id string positions are looked up by: ""
// when absent or the moved entity itself, a 400 when it is not in the folder.
func neighbourID(id *int, entityID int, siblings []folders.Folder) (string, error) {
	if id == nil || *id == entityID {
		return "", nil
	}
	want := strconv.Itoa(*id)
	for i := range siblings {
		if siblings[i].ID == want {
			return want, nil
		}
	}
	return "", apierr.BadRequest("a neighbour is not in this folder")
}

// Pinned is the ids the caller pinned in the list, whose entities still are of
// the kind.
func (r *EntityFoldersRepo) Pinned(ctx context.Context, projectID string, kind entityfolders.Kind) ([]int, error) {
	s, err := tenantSchema(projectID)
	if err != nil {
		return nil, err
	}
	actor, err := entityFolderActor(ctx)
	if err != nil {
		return nil, err
	}
	visible, args, err := r.pinVisibility(ctx, s, kind, "p.entity_id", actor, []any{actor, string(kind)})
	if err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, fmt.Sprintf(`SELECT p.entity_id FROM %[1]s.project_entity_pins p
		WHERE p.user_id = $1 AND p.entity_kind = $2 AND `+entityLiveSQL(s, kind, "p.entity_id")+visible+` ORDER BY p.created_at DESC, p.entity_id`, s), args...)
	if err != nil {
		return nil, fmt.Errorf("entity folders: pinned: %w", err)
	}
	defer rows.Close()
	ids := []int{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("entity folders: pinned: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SetPin pins or unpins the entity for the caller. Pinning an entity that is
// not of the kind is a 404; unpinning never is (a stale pin can always go).
func (r *EntityFoldersRepo) SetPin(ctx context.Context, projectID string, kind entityfolders.Kind, entityID int, pinned bool) error {
	s, err := tenantSchema(projectID)
	if err != nil {
		return err
	}
	actor, err := entityFolderActor(ctx)
	if err != nil {
		return err
	}
	if !pinned {
		if _, err := r.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.project_entity_pins WHERE user_id = $1 AND entity_kind = $2 AND entity_id = $3`, s), actor, string(kind), entityID); err != nil {
			return fmt.Errorf("entity folders: unpin: %w", err)
		}
		return nil
	}
	visible, args, err := r.pinVisibility(ctx, s, kind, "$3::int", actor, []any{actor, string(kind), entityID})
	if err != nil {
		return err
	}
	ct, err := r.pool.Exec(ctx, fmt.Sprintf(`INSERT INTO %[1]s.project_entity_pins (user_id, entity_kind, entity_id)
		SELECT $1, $2::varchar, $3::int WHERE `+entityLiveSQL(s, kind, "$3::int")+visible+`
		ON CONFLICT DO NOTHING`, s), args...)
	if err != nil {
		return fmt.Errorf("entity folders: pin: %w", err)
	}
	if ct.RowsAffected() == 0 {
		var pinned bool
		if err := r.pool.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s.project_entity_pins WHERE user_id = $1 AND entity_kind = $2 AND entity_id = $3)`, s), actor, string(kind), entityID).Scan(&pinned); err != nil {
			return fmt.Errorf("entity folders: pin: %w", err)
		}
		if !pinned {
			// Not there, not already pinned: the entity is not of this kind, or the caller may not see it.
			return apierr.NotFound(string(kind) + " not found")
		}
	}
	return nil
}

// pinVisibility is the extra predicate that keeps a pin to what the caller may
// see: where a deployment restricts access to agent folders (the optional access
// overlay), a pin of an agent or pipeline the caller has no access to is neither
// accepted nor counted, as the lists themselves apply it. `args` is `args` with
// the placeholders of that predicate appended.
func (r *EntityFoldersRepo) pinVisibility(ctx context.Context, s string, kind entityfolders.Kind, idExpr string, actor int64, args []any) (string, []any, error) {
	if kind != entityfolders.KindAgent && kind != entityfolders.KindPipeline {
		return "", args, nil
	}
	access, err := foldervisibility.Resolve(ctx, r.pool, s)
	if err != nil {
		return "", nil, fmt.Errorf("entity folders: pin visibility: %w", err)
	}
	if !access.FolderRestrictions {
		return "", args, nil
	}
	args = append(args, []string{string(kind)}, actor)
	return " AND " + foldervisibility.ExclusionSQL(s, idExpr, fmt.Sprintf("$%d", len(args)-1), fmt.Sprintf("$%d", len(args))), args, nil
}

func entityFolderActor(ctx context.Context) (int64, error) {
	user, ok := auth.UserFromContext(ctx)
	if !ok {
		return 0, apierr.Forbidden("folders require an authenticated user")
	}
	id, ok := user.OwningUserID()
	if !ok {
		return 0, apierr.Forbidden("folders require an authenticated user")
	}
	return id, nil
}

func entityFolderWriteErr(op string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return apierr.Conflict("a folder with this name already exists in this list")
	}
	return fmt.Errorf("entity folders: %s: %w", op, err)
}
