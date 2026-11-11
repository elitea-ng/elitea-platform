package repos

// The references of agent-media files (ADR-0050 decision 6, #1855): which
// `/api/v2/agent_media/{project}/{sha256}.{ext}` files something still
// names, so the orphan sweep can tell a draft nobody saved from a file a
// pack shows.
//
// THE SET IS GLOBAL. Publish, the catalogue mirror and the MCP copy carry a
// pack into another project with its source URLs, so a file of project 7 may
// be named only by a version in the public project. A sweep that judged
// project 7's bucket by project 7's own rows would delete a published agent's
// avatar.
//
// EVERY PLACE A URL IS KEPT IS READ, not only the live pack: a version's
// meta.icon_meta (old clients, and an icon picked on its own), a
// conversation participant's settings (its icon snapshot), and the
// lifecycle store's revision snapshots and release definitions, which a
// restore or a pinned release brings back. A column a tenant does not have
// is skipped; a read that fails fails the whole set, and the sweep then
// deletes nothing (fail closed).

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// agentMediaURLPattern is agentmedia's URL shape as a PostgreSQL regular
// expression: group 1 the project, group 2 the file.
const agentMediaURLPattern = `/api/v2/agent_media/([1-9][0-9]{0,17})/([0-9a-f]{64}\.(?:png|jpg|webp|svg|mp4|webm))`

// agentMediaLike prefilters rows before the regular expression runs.
const agentMediaLike = `%/api/v2/agent_media/%`

// agentMediaTenantColumns are the per-tenant columns that can hold a media URL.
var agentMediaTenantColumns = [][2]string{
	{"application_versions", "avatar_pack"},
	{"application_versions", "meta"},
	{"chat_participant_mapping", "entity_settings"},
	{"chat_participants", "meta"},
}

// agentMediaSharedColumns are the lifecycle store's columns that can.
var agentMediaSharedColumns = [][3]string{
	{"elitea_lifecycle", "version_revisions", "snapshot"},
	{"elitea_lifecycle", "application_releases", "definition"},
}

// AgentMediaReferencesRepository reads the global reference set.
type AgentMediaReferencesRepository struct {
	pool *pgxpool.Pool
}

// NewAgentMediaReferencesRepository builds the repository.
func NewAgentMediaReferencesRepository(pool *pgxpool.Pool) (*AgentMediaReferencesRepository, error) {
	if pool == nil {
		return nil, errors.New("agent media references: pool is required")
	}
	return &AgentMediaReferencesRepository{pool: pool}, nil
}

// TenantProjects lists the projects whose tenant schema has agent versions,
// in id order: the buckets the sweep looks in.
func (r *AgentMediaReferencesRepository) TenantProjects(ctx context.Context) ([]int64, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT substring(namespace.nspname FROM 3)::bigint
		  FROM pg_class AS relation
		  JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
		 WHERE relation.relname = 'application_versions'
		   AND relation.relkind = 'r'
		   AND namespace.nspname ~ '^p_[0-9]+$'
		 ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("agent media references: list tenants: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// Referenced answers every file any project still names, keyed
// `{project}/{file}`.
func (r *AgentMediaReferencesRepository) Referenced(ctx context.Context) (map[string]struct{}, error) {
	sources, err := r.sources(ctx)
	if err != nil {
		return nil, err
	}
	referenced := map[string]struct{}{}
	for _, source := range sources {
		rows, err := r.pool.Query(ctx, fmt.Sprintf(`
			SELECT DISTINCT match[1] || '/' || match[2]
			  FROM (SELECT %[2]s::text AS body FROM %[1]s WHERE %[2]s::text LIKE $1) AS source,
			       regexp_matches(source.body, $2, 'g') AS match`, source.table, source.column),
			agentMediaLike, agentMediaURLPattern)
		if err != nil {
			return nil, fmt.Errorf("agent media references: read %s.%s: %w", source.table, source.column, err)
		}
		keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, fmt.Errorf("agent media references: read %s.%s: %w", source.table, source.column, err)
		}
		for _, key := range keys {
			referenced[key] = struct{}{}
		}
	}
	return referenced, nil
}

type agentMediaSource struct {
	table  string // quoted schema.table
	column string // quoted column
}

// sources lists the (table, column) pairs present in this database.
func (r *AgentMediaReferencesRepository) sources(ctx context.Context) ([]agentMediaSource, error) {
	tables := make([]string, 0, len(agentMediaTenantColumns))
	columns := make([]string, 0, len(agentMediaTenantColumns))
	for _, pair := range agentMediaTenantColumns {
		tables = append(tables, pair[0])
		columns = append(columns, pair[1])
	}
	sharedSchemas := make([]string, 0, len(agentMediaSharedColumns))
	sharedTables := make([]string, 0, len(agentMediaSharedColumns))
	sharedColumns := make([]string, 0, len(agentMediaSharedColumns))
	for _, triple := range agentMediaSharedColumns {
		sharedSchemas = append(sharedSchemas, triple[0])
		sharedTables = append(sharedTables, triple[1])
		sharedColumns = append(sharedColumns, triple[2])
	}
	rows, err := r.pool.Query(ctx, `
		SELECT c.table_schema, c.table_name, c.column_name
		  FROM information_schema.columns AS c
		  JOIN unnest($1::text[], $2::text[]) AS wanted(table_name, column_name)
		    ON wanted.table_name = c.table_name AND wanted.column_name = c.column_name
		 WHERE c.table_schema ~ '^p_[0-9]+$'
		UNION
		SELECT c.table_schema, c.table_name, c.column_name
		  FROM information_schema.columns AS c
		  JOIN unnest($3::text[], $4::text[], $5::text[]) AS wanted(table_schema, table_name, column_name)
		    ON wanted.table_schema = c.table_schema AND wanted.table_name = c.table_name AND wanted.column_name = c.column_name
		 ORDER BY 1, 2, 3`, tables, columns, sharedSchemas, sharedTables, sharedColumns)
	if err != nil {
		return nil, fmt.Errorf("agent media references: list sources: %w", err)
	}
	defer rows.Close()
	var sources []agentMediaSource
	for rows.Next() {
		var schemaName, table, column string
		if err := rows.Scan(&schemaName, &table, &column); err != nil {
			return nil, fmt.Errorf("agent media references: list sources: %w", err)
		}
		sources = append(sources, agentMediaSource{
			table:  pgx.Identifier{schemaName, table}.Sanitize(),
			column: pgx.Identifier{column}.Sanitize(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agent media references: list sources: %w", err)
	}
	return sources, nil
}

// AgentMediaReferenceKey is the key Referenced answers for one file.
func AgentMediaReferenceKey(projectID int64, file string) string {
	return strconv.FormatInt(projectID, 10) + "/" + strings.TrimSpace(file)
}
