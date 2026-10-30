package repos

// The on-ramp to approved_only (ADR-0044 decision 1, #1501). Switching the
// catalogue mode to approved_only refuses every connector no approved request
// names, including every toolkit configured before the switch. The inventory
// finds the connectors in use (the toolkit types, pre-built MCP servers and
// custom MCP server URLs of every project's toolkits), answers how many of
// them no approved request covers, and creates one SUBMITTED request per
// uncovered connector for a connector approver to decide.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/connectorapproval"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workeregress"
)

// ConnectorInUse is one connector configured in at least one project.
type ConnectorInUse struct {
	Kind        connectorapproval.TargetKind
	Target      string
	ToolkitType string
	// Projects are the projects whose toolkits use it, ascending.
	Projects []int64
	// Tools are the tools the toolkits select (a custom MCP server's with
	// the description and input schema its last Load Tools stored, so they
	// can be pinned), at most connectorapproval.MaxTools.
	Tools []connectorapproval.DeclaredTool
	// EgressHosts are the hosts of the toolkits' URL-valued settings.
	EgressHosts []string
}

// ConnectorInventory is the inventory's answer.
type ConnectorInventory struct {
	InUse []ConnectorInUse
	// WithoutApproval are the connectors in use that no approved request
	// covers for every project using them: what approved_only would refuse.
	WithoutApproval []ConnectorInUse
}

type connectorKey struct {
	kind   connectorapproval.TargetKind
	target string
}

type connectorAccumulator struct {
	use       ConnectorInUse
	projects  map[int64]bool
	selected  map[string]bool
	hosts     map[string]bool
	everyTool bool
}

// Inventory scans every project's toolkits.
func (s *ConnectorRequests) Inventory(ctx context.Context) (ConnectorInventory, error) {
	found, err := s.connectorsInUse(ctx)
	if err != nil {
		return ConnectorInventory{}, err
	}
	coverage, err := approvedCoverage(ctx, s.pool)
	if err != nil {
		return ConnectorInventory{}, err
	}
	inventory := ConnectorInventory{InUse: found, WithoutApproval: []ConnectorInUse{}}
	for _, connector := range found {
		if !coverage.covers(connector) {
			inventory.WithoutApproval = append(inventory.WithoutApproval, connector)
		}
	}
	return inventory, nil
}

func (s *ConnectorRequests) connectorsInUse(ctx context.Context) ([]ConnectorInUse, error) {
	rows, err := s.pool.Query(ctx, `
SELECT p.id FROM centry.project AS p
WHERE to_regclass(format('%I.elitea_tools', 'p_' || p.id)) IS NOT NULL
ORDER BY p.id`)
	if undefinedTable(err) {
		return []ConnectorInUse{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("connector inventory: list the projects: %w", err)
	}
	var projects []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		projects = append(projects, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	connectors := map[connectorKey]*connectorAccumulator{}
	for _, projectID := range projects {
		schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
		if err != nil {
			continue
		}
		toolkits, err := s.pool.Query(ctx, fmt.Sprintf(`SELECT type, COALESCE(settings, '{}'::jsonb) FROM %s.elitea_tools ORDER BY id`, schema))
		if err != nil {
			return nil, fmt.Errorf("connector inventory: read the toolkits of project %d: %w", projectID, err)
		}
		for toolkits.Next() {
			var toolkitType string
			var raw []byte
			if err := toolkits.Scan(&toolkitType, &raw); err != nil {
				toolkits.Close()
				return nil, err
			}
			settings := map[string]any{}
			_ = json.Unmarshal(raw, &settings)
			kind, target, gated := connectorapproval.Connector(toolkitType, settings)
			if !gated || target == "" {
				continue
			}
			normalized, approvedType, err := connectorapproval.NormalizeTarget(kind, target)
			if err != nil {
				continue
			}
			key := connectorKey{kind: kind, target: normalized}
			entry := connectors[key]
			if entry == nil {
				entry = &connectorAccumulator{
					use:      ConnectorInUse{Kind: kind, Target: normalized, ToolkitType: approvedType},
					projects: map[int64]bool{}, selected: map[string]bool{}, hosts: map[string]bool{},
				}
				connectors[key] = entry
			}
			entry.projects[projectID] = true
			selected := stringList(settings["selected_tools"])
			if len(selected) == 0 {
				// No selection is every tool the toolkit offers.
				entry.everyTool = true
			}
			for _, name := range selected {
				entry.selected[name] = true
			}
			hosts, _ := workeregress.HostsFromSettings(raw)
			for _, host := range hosts {
				entry.hosts[host] = true
			}
		}
		toolkits.Close()
		if err := toolkits.Err(); err != nil {
			return nil, err
		}
	}

	out := make([]ConnectorInUse, 0, len(connectors))
	for _, entry := range connectors {
		use := entry.use
		for id := range entry.projects {
			use.Projects = append(use.Projects, id)
		}
		sort.Slice(use.Projects, func(i, j int) bool { return use.Projects[i] < use.Projects[j] })
		for host := range entry.hosts {
			use.EgressHosts = append(use.EgressHosts, host)
		}
		if host, ok := connectorapproval.HostOfTarget(use.Kind, use.Target); ok && !entry.hosts[host] {
			use.EgressHosts = append(use.EgressHosts, host)
		}
		sort.Strings(use.EgressHosts)
		if len(use.EgressHosts) > connectorapproval.MaxEgressHosts {
			use.EgressHosts = use.EgressHosts[:connectorapproval.MaxEgressHosts]
		}
		tools, err := s.inventoryTools(ctx, use, entry)
		if err != nil {
			return nil, err
		}
		use.Tools = tools
		out = append(out, use)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Target < out[j].Target
	})
	return out, nil
}

// inventoryTools answers the tools a connector's request declares: the
// selected tool names, and for a custom MCP server the tools its last Load
// Tools stored for a project using it (their description and input schema
// make the pin), narrowed to the selection unless some toolkit selects every
// tool. The category is left to the ADR-0041 default (write): nothing in a
// toolkit's settings says what a tool does.
func (s *ConnectorRequests) inventoryTools(ctx context.Context, use ConnectorInUse, entry *connectorAccumulator) ([]connectorapproval.DeclaredTool, error) {
	tools := []connectorapproval.DeclaredTool{}
	if use.Kind == connectorapproval.TargetCustomMCP {
		discovered, err := s.registeredMCPTools(ctx, use.Projects, use.Target)
		if err != nil {
			return nil, err
		}
		for _, tool := range discovered {
			if !entry.everyTool && !entry.selected[tool.Name] {
				continue
			}
			tools = append(tools, tool)
		}
	} else {
		names := make([]string, 0, len(entry.selected))
		for name := range entry.selected {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			tools = append(tools, connectorapproval.DeclaredTool{Name: name})
		}
	}
	kept := tools[:0]
	seen := map[string]bool{}
	for _, tool := range tools {
		lower := strings.ToLower(tool.Name)
		if !connectorapproval.ValidToolName(tool.Name) || seen[lower] || len(tool.Description) > connectorapproval.MaxToolDescription {
			continue
		}
		seen[lower] = true
		kept = append(kept, tool)
	}
	if len(kept) > connectorapproval.MaxTools {
		kept = kept[:connectorapproval.MaxTools]
	}
	return kept, nil
}

// registeredMCPTools reads the tools Load Tools stored for a custom MCP
// server (elitea_mcp.registered_tools, shared 0073) in one of the projects,
// matched by the server's canonical URL: the most recently updated
// registration wins.
func (s *ConnectorRequests) registeredMCPTools(ctx context.Context, projects []int64, target string) ([]connectorapproval.DeclaredTool, error) {
	rows, err := s.pool.Query(ctx, `
SELECT s.id, s.server_url FROM elitea_mcp.registered_servers AS s
WHERE s.project_id = ANY($1::bigint[]) AND s.server_url <> ''
ORDER BY s.updated_at DESC, s.id DESC`, projects)
	if undefinedTable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("connector inventory: read the MCP registrations: %w", err)
	}
	var serverID int64
	for rows.Next() {
		var id int64
		var url string
		if err := rows.Scan(&id, &url); err != nil {
			rows.Close()
			return nil, err
		}
		if canonical, err := connectorapproval.CanonicalMCPURL(url); err == nil && canonical == target && serverID == 0 {
			serverID = id
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if serverID == 0 {
		return nil, nil
	}
	tools, err := s.pool.Query(ctx, `
SELECT name, description, input_schema::text FROM elitea_mcp.registered_tools
WHERE server_id = $1 ORDER BY ordinal, id`, serverID)
	if err != nil {
		return nil, fmt.Errorf("connector inventory: read the MCP tools: %w", err)
	}
	defer tools.Close()
	var out []connectorapproval.DeclaredTool
	for tools.Next() {
		var name, description, schema string
		if err := tools.Scan(&name, &description, &schema); err != nil {
			return nil, err
		}
		value, err := connectorapproval.DecodeJSONValue([]byte(schema))
		if err != nil {
			continue
		}
		out = append(out, connectorapproval.DeclaredTool{Name: name, Description: description, InputSchema: value})
	}
	return out, tools.Err()
}

func stringList(value any) []string {
	list, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
			out = append(out, strings.TrimSpace(text))
		}
	}
	return out
}

// connectorCoverage is what the approved requests admit.
type connectorCoverage struct {
	platform map[connectorKey]bool
	projects map[connectorKey]map[int64]bool
}

func (c connectorCoverage) covers(use ConnectorInUse) bool {
	key := connectorKey{kind: use.Kind, target: use.Target}
	if c.platform[key] {
		return true
	}
	for _, id := range use.Projects {
		if !c.projects[key][id] {
			return false
		}
	}
	return true
}

func approvedCoverage(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) (connectorCoverage, error) {
	coverage := connectorCoverage{platform: map[connectorKey]bool{}, projects: map[connectorKey]map[int64]bool{}}
	rows, err := q.Query(ctx, `
SELECT target_kind, target, platform_wide, project_ids FROM centry.connector_requests WHERE state = 'approved'`)
	if undefinedTable(err) {
		return coverage, nil
	}
	if err != nil {
		return coverage, fmt.Errorf("connector inventory: read the approved requests: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, target string
		var platform bool
		var projects []int64
		if err := rows.Scan(&kind, &target, &platform, &projects); err != nil {
			return coverage, err
		}
		key := connectorKey{kind: connectorapproval.TargetKind(kind), target: target}
		if platform {
			coverage.platform[key] = true
			continue
		}
		if coverage.projects[key] == nil {
			coverage.projects[key] = map[int64]bool{}
		}
		for _, id := range projects {
			coverage.projects[key][id] = true
		}
	}
	return coverage, rows.Err()
}

// InventoryOutcome is what CreateInventoryRequests wrote.
type InventoryOutcome struct {
	Created []connectorapproval.Request
	// Skipped counts the connectors in use that already have an open
	// (draft or submitted) or an approved request.
	Skipped int
}

// CreateInventoryRequests creates one SUBMITTED inventory request per
// connector in use that has no open or approved request: origin inventory,
// no requester, created_by the administrator, the projects using it (the
// whole platform when more than connectorapproval.MaxProjects do), the
// selected or discovered tools and the configured hosts. Each is audited as
// a member's request is (created, then submitted). Idempotent: a second run
// skips what the first created; concurrent runs serialise on a transaction
// lock.
func (s *ConnectorRequests) CreateInventoryRequests(ctx context.Context, adminID int64) (InventoryOutcome, error) {
	if adminID <= 0 {
		return InventoryOutcome{}, connectorapproval.Refuse(connectorapproval.InvalidCode, "Creating requests needs a signed-in administrator.")
	}
	found, err := s.connectorsInUse(ctx)
	if err != nil {
		return InventoryOutcome{}, err
	}
	outcome := InventoryOutcome{Created: []connectorapproval.Request{}}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('connector_requests:inventory'))`); err != nil {
			return fmt.Errorf("connector inventory: lock: %w", err)
		}
		live := map[connectorKey]bool{}
		rows, err := tx.Query(ctx, `
SELECT DISTINCT target_kind, target FROM centry.connector_requests WHERE state IN ('draft', 'submitted', 'approved')`)
		if err != nil {
			return fmt.Errorf("connector inventory: read the live requests: %w", err)
		}
		for rows.Next() {
			var kind, target string
			if err := rows.Scan(&kind, &target); err != nil {
				rows.Close()
				return err
			}
			live[connectorKey{kind: connectorapproval.TargetKind(kind), target: target}] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		now := s.now()
		for _, use := range found {
			if live[connectorKey{kind: use.Kind, target: use.Target}] {
				outcome.Skipped++
				continue
			}
			draft := connectorapproval.Draft{TargetKind: use.Kind, Target: use.Target, Tools: use.Tools,
				EgressHosts: use.EgressHosts, DataClasses: []string{}}
			if len(use.Projects) > connectorapproval.MaxProjects {
				draft.PlatformWide = true
				draft.Justification = fmt.Sprintf("Created from the connectors in use: configured in %d projects "+
					"(more than %d, so requested platform-wide) before the catalogue mode switched to approved only.",
					len(use.Projects), connectorapproval.MaxProjects)
			} else {
				draft.ProjectIDs = use.Projects
				draft.Justification = fmt.Sprintf("Created from the connectors in use: configured in %d project(s) "+
					"before the catalogue mode switched to approved only.", len(use.Projects))
			}
			normalized, toolkitType, err := connectorapproval.NormalizeInventoryDraft(draft)
			if err != nil {
				return fmt.Errorf("connector inventory: %s %s: %w", use.Kind, use.Target, err)
			}
			tools, err := json.Marshal(normalized.Tools)
			if err != nil {
				return err
			}
			created, err := scanConnectorRequest(tx.QueryRow(ctx, `
INSERT INTO centry.connector_requests
    (project_id, target_kind, target, toolkit_type, platform_wide, project_ids, declared_tools,
     egress_hosts, data_classes, justification, state, origin, created_by, submitted_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'submitted', 'inventory', $11, $12)
RETURNING `+connectorRequestColumns,
				use.Projects[0], string(normalized.TargetKind), normalized.Target, toolkitType, normalized.PlatformWide,
				normalized.ProjectIDs, tools, normalized.EgressHosts, normalized.DataClasses, normalized.Justification,
				adminID, now))
			if err != nil {
				return fmt.Errorf("connector inventory: create: %w", err)
			}
			for _, action := range []string{connectorapproval.ActionCreated, connectorapproval.ActionSubmitted} {
				if err := s.append(ctx, tx, connectorEvent(created, action, adminID, "origin:inventory")); err != nil {
					return err
				}
			}
			outcome.Created = append(outcome.Created, created)
		}
		return nil
	})
	if err != nil {
		return InventoryOutcome{}, err
	}
	return outcome, nil
}
