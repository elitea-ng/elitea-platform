package repos

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/entityfolders"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/folders"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// EntityFoldersRepo stores the project folders of the Agents, Pipelines,
// Skills, Toolkits and MCPs lists (tenant migration 0245).
type EntityFoldersRepo struct {
	pool *pgxpool.Pool
}

func NewEntityFoldersRepo(pool *pgxpool.Pool) *EntityFoldersRepo {
	return &EntityFoldersRepo{pool: pool}
}

// entityFolderOrder is the chat-folder order: highest position first, oldest
// first among equals.
const entityFolderOrder = `ORDER BY f.position DESC, f.created_at ASC, f.id ASC`

const entityFolderColumns = `f.id::text, f.name, f.position, COALESCE(f.created_by::text, ''), f.created_at, f.updated_at,
	COALESCE(ARRAY(SELECT i.entity_id FROM %[1]s.project_folder_items i WHERE i.folder_id = f.id ORDER BY i.entity_id), '{}')::int[]`

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
	q := fmt.Sprintf(`SELECT `+entityFolderColumns+` FROM %[1]s.project_folders f WHERE f.entity_kind = $1 `+entityFolderOrder, s)
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
		SELECT `+entityFolderColumns+` FROM ins f`, s, folders.PositionGap)
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
	if !isNumericRowID(folderID) {
		return entityfolders.Folder{}, apierr.NotFound("folder not found")
	}
	q := fmt.Sprintf(`
		WITH upd AS (
			UPDATE %[1]s.project_folders
			SET name = COALESCE($1::varchar, name), position = COALESCE($2::int, position), updated_at = now()
			WHERE id = $3 AND entity_kind = $4
			RETURNING *
		)
		SELECT `+entityFolderColumns+` FROM upd f`, s)
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
	if !isNumericRowID(folderID) {
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

// entityExistsSQL answers whether $1 is an entity of the kind, by the same rule
// the list uses: an agent is an application with no pipeline version, a
// pipeline one with a pipeline version, an MCP a toolkit of type mcp / mcp_* or
// with meta.mcp = true.
func entityExistsSQL(s string, kind entityfolders.Kind) string {
	switch kind {
	case entityfolders.KindAgent:
		return fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %[1]s.applications a WHERE a.id = $1 AND NOT EXISTS (SELECT 1 FROM %[1]s.application_versions v WHERE v.application_id = a.id AND v.agent_type = 'pipeline'))`, s)
	case entityfolders.KindPipeline:
		return fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %[1]s.applications a WHERE a.id = $1 AND EXISTS (SELECT 1 FROM %[1]s.application_versions v WHERE v.application_id = a.id AND v.agent_type = 'pipeline'))`, s)
	case entityfolders.KindSkill:
		return fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s.skills WHERE id = $1)`, s)
	case entityfolders.KindMCP:
		return fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s.elitea_tools t WHERE t.id = $1 AND `, s) + mcpToolkitSQL + `)`
	default:
		return fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s.elitea_tools t WHERE t.id = $1 AND NOT `, s) + mcpToolkitSQL + `)`
	}
}

const mcpToolkitSQL = `(t.type = 'mcp' OR t.type LIKE 'mcp\_%' OR COALESCE(t.meta->>'mcp', '') = 'true')`

func (r *EntityFoldersRepo) Move(ctx context.Context, projectID string, kind entityfolders.Kind, entityID int, folderID *string) error {
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
	if err := tx.QueryRow(ctx, entityExistsSQL(s, kind), entityID).Scan(&exists); err != nil {
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
	if !isNumericRowID(*folderID) {
		return apierr.NotFound("folder not found")
	}
	id, _ := strconv.Atoi(*folderID)
	// The composite FK (folder_id, entity_kind) refuses a folder of another
	// kind; the lock-free existence probe turns that into a clean 404.
	var folderExists bool
	if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s.project_folders WHERE id = $1 AND entity_kind = $2)`, s), id, string(kind)).Scan(&folderExists); err != nil {
		return fmt.Errorf("entity folders: move: %w", err)
	}
	if !folderExists {
		return apierr.NotFound("folder not found")
	}
	_, err = tx.Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s.project_folder_items (entity_kind, entity_id, folder_id) VALUES ($1, $2, $3)
		ON CONFLICT (entity_kind, entity_id) DO UPDATE SET folder_id = EXCLUDED.folder_id, created_at = now()`, s), string(kind), entityID, id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return apierr.NotFound("folder not found") // deleted between the probe and the write
		}
		return fmt.Errorf("entity folders: move: %w", err)
	}
	return tx.Commit(ctx)
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
