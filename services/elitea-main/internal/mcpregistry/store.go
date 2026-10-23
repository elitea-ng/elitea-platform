package mcpregistry

// The PostgreSQL store behind `elitea_mcp.registered_servers` and
// `elitea_mcp.registered_tools` (shared migration 0073).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoPool is returned instead of dereferencing a nil pool. A store built
// without a database is a composition fault, and the callers turn it into a 503
// rather than a panic that takes the process down.
var ErrNoPool = errors.New("mcpregistry: no database pool configured")

// Store reads and writes the registered MCP servers of a project.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Save writes one server and the tools it publishes.
//
// The write REPLACES the server's tool set rather than merging into it. A tool
// that the server no longer publishes must disappear from the listing, because
// a worker that still sees it builds a tool that cannot run. Merging would keep
// removed tools forever, and nothing else deletes them.
//
// Server and tools are written in one transaction, so a reader never sees a
// server whose tools are half replaced.
//
// A registration with no tools is rejected. An MCP server that publishes
// nothing is a failed discovery, not an empty server, and storing it would
// replace a working tool set with an empty one on the next transient error.
func (s *Store) Save(ctx context.Context, registration Registration) error {
	if s == nil || s.pool == nil {
		return ErrNoPool
	}
	name := strings.TrimSpace(SanitizeName(registration.Name))
	if name == "" {
		return fmt.Errorf("mcpregistry: server name is empty")
	}
	if registration.ProjectID <= 0 {
		return fmt.Errorf("mcpregistry: project id is not positive")
	}
	if len(registration.Tools) == 0 {
		return fmt.Errorf("mcpregistry: server %q published no tools", name)
	}

	group := registration.Group
	if strings.TrimSpace(group) == "" {
		group = DefaultGroup
	}
	listTimeout := registration.TimeoutToolsList
	if listTimeout <= 0 {
		listTimeout = DefaultTimeoutSeconds
	}
	callTimeout := registration.TimeoutToolsCall
	if callTimeout <= 0 {
		callTimeout = DefaultTimeoutSeconds
	}

	transaction, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback(ctx) }()

	var serverID int64
	if err := transaction.QueryRow(ctx, `
		INSERT INTO elitea_mcp.registered_servers
			(project_id, name, server_url, server_group, timeout_tools_list, timeout_tools_call)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (project_id, name) DO UPDATE SET
			server_url         = EXCLUDED.server_url,
			server_group       = EXCLUDED.server_group,
			timeout_tools_list = EXCLUDED.timeout_tools_list,
			timeout_tools_call = EXCLUDED.timeout_tools_call,
			updated_at         = now()
		RETURNING id`,
		registration.ProjectID, name, registration.ServerURL, group, listTimeout, callTimeout,
	).Scan(&serverID); err != nil {
		return err
	}

	if _, err := transaction.Exec(ctx,
		`DELETE FROM elitea_mcp.registered_tools WHERE server_id = $1`, serverID); err != nil {
		return err
	}

	for ordinal, tool := range registration.Tools {
		toolName := strings.TrimSpace(tool.Name)
		if toolName == "" {
			// One unnamed entry must not fail the whole registration. The
			// server is still usable through its other tools, and a tool with
			// no name is not addressable anyway.
			continue
		}
		schema := tool.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{}}
		}
		encoded, err := json.Marshal(schema)
		if err != nil {
			return fmt.Errorf("mcpregistry: encode input schema of tool %q: %w", toolName, err)
		}
		if _, err := transaction.Exec(ctx, `
			INSERT INTO elitea_mcp.registered_tools (server_id, name, description, input_schema, ordinal)
			VALUES ($1, $2, $3, $4::jsonb, $5)
			ON CONFLICT (server_id, name) DO UPDATE SET
				description  = EXCLUDED.description,
				input_schema = EXCLUDED.input_schema,
				ordinal      = EXCLUDED.ordinal`,
			serverID, toolName, tool.Description, string(encoded), ordinal); err != nil {
			return err
		}
	}

	return transaction.Commit(ctx)
}

// ListForProject returns every server registered in one project, with its
// tools.
//
// The projection is the wire shape, so the caller marshals the result without
// translating it again. `sio_sid` is null and `project_id` is the project the
// row belongs to.
func (s *Store) ListForProject(ctx context.Context, projectID int64) ([]Server, error) {
	if s == nil || s.pool == nil {
		return nil, ErrNoPool
	}
	if projectID <= 0 {
		return nil, nil
	}

	// One query, not one per server. The LEFT JOIN keeps a server that has no
	// tool rows, which cannot happen through Save but can happen if a tool row
	// is deleted by hand; the server then reports an empty tool list rather
	// than vanishing.
	rows, err := s.pool.Query(ctx, `
		SELECT server.id, server.name, server.server_group,
		       server.timeout_tools_list, server.timeout_tools_call,
		       tool.name, tool.description, tool.input_schema
		FROM elitea_mcp.registered_servers AS server
		LEFT JOIN elitea_mcp.registered_tools AS tool ON tool.server_id = server.id
		WHERE server.project_id = $1
		ORDER BY server.name, tool.ordinal, tool.name`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	servers := make([]Server, 0)
	indexByID := make(map[int64]int)
	for rows.Next() {
		var (
			serverID                  int64
			serverName, serverGroup   string
			listTimeout, callTimeout  int
			toolName, toolDescription *string
			inputSchema               []byte
		)
		if err := rows.Scan(&serverID, &serverName, &serverGroup,
			&listTimeout, &callTimeout,
			&toolName, &toolDescription, &inputSchema); err != nil {
			return nil, err
		}
		index, seen := indexByID[serverID]
		if !seen {
			owner := projectID
			servers = append(servers, Server{
				Name:             serverName,
				Tools:            []Tool{},
				ProjectID:        &owner,
				SioSID:           nil,
				TimeoutToolsList: listTimeout,
				TimeoutToolsCall: callTimeout,
				Group:            serverGroup,
			})
			index = len(servers) - 1
			indexByID[serverID] = index
		}
		if toolName == nil {
			continue
		}
		tool := Tool{Name: *toolName}
		if toolDescription != nil {
			tool.Description = *toolDescription
		}
		if len(inputSchema) > 0 {
			if err := json.Unmarshal(inputSchema, &tool.InputSchema); err != nil {
				// A schema that no longer parses must not remove the whole
				// listing. An open object is what the legacy runtime emits when its own
				// lookup misses.
				tool.InputSchema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
		}
		if tool.InputSchema == nil {
			tool.InputSchema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		servers[index].Tools = append(servers[index].Tools, tool)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return servers, nil
}

// ServerURL reports where a named server in a project lives, and how long a
// call to it may take.
func (s *Store) ServerURL(ctx context.Context, projectID int64, name string) (string, int, error) {
	if s == nil || s.pool == nil {
		return "", 0, ErrNoPool
	}
	var address string
	var timeout int
	err := s.pool.QueryRow(ctx, `
		SELECT server_url, timeout_tools_call
		FROM elitea_mcp.registered_servers
		WHERE project_id = $1 AND name = $2`, projectID, name).Scan(&address, &timeout)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	return address, timeout, nil
}
