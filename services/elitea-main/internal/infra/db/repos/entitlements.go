package repos

// The entitlement export (ADR-0044 decision 5, issue 1501).
//
// # Where a grant comes from
//
// Three tables hold every grant an access review must see, and this file
// reads them exactly as the authorisation path does
// (internal/infra/legacyrbac/postgres.go):
//
//   - PLATFORM: auth_core__user_role → auth_core__role (mode administration
//     or developer) → auth_core__role_permission. One row per mode, and the
//     mode is the scope id.
//   - PROJECT: auth_core__project_user_role → auth_core__project_role. The
//     permissions are the project's own rows for the user's roles there
//     (auth_core__project_role_permission); when NONE of the user's roles in
//     that project has a row, the central default-mode role of the same
//     name supplies them instead. That fallback is per user and project, as
//     projectPermissions() applies it, so the export cannot disagree with
//     what a request is actually allowed.
//   - AGENT: elitea_lifecycle.agent_registry business_owner_id and
//     technical_owner_id (shared 0204). Ownership is not a permission; it
//     carries entitlements.OwnerCapabilities.
//
// The permission → capability mapping is entitlements.Capabilities, passed
// to the query as two parallel arrays, so the SQL holds no label of its own.
//
// Suspended accounts and suspended projects are exported, flagged
// (user_active, scope_active): their grants are dormant and return on
// resume, which is exactly what an access review must see.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/entitlements"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/scimdirectory"
)

// ErrEntitlementNotFound reports that no account or role has the id.
var ErrEntitlementNotFound = errors.New("entitlements: not found")

// EntitlementFilter narrows the admin export. Zero fields match everything.
type EntitlementFilter struct {
	UserID    int64
	ProjectID int64
}

// EntitlementExportAudit is the audit_events row of one export.
type EntitlementExportAudit struct {
	// UserID is the administrator; 0 for a SCIM client.
	UserID int64
	// Actor is the account address, or `scim:<client name>`.
	Actor string
	// Format is csv, json or scim.
	Format string
	// Rows is how many grants (csv, json) or resources (scim) left.
	Rows  int
	Route string
}

// Entitlements reads the grant tables for the export.
type Entitlements struct {
	pool *pgxpool.Pool
}

// NewEntitlements returns the store.
func NewEntitlements(pool *pgxpool.Pool) *Entitlements {
	return &Entitlements{pool: pool}
}

// grantsSQL produces one row per (user, scope, role, capability, granted_via).
//
// $1 permissions, $2 capabilities (parallel), $3 owner capabilities,
// $4 user id (0 = all), $5 project id (0 = all), $6 user ids (NULL = all).
const grantsSQL = `
WITH cap_map AS (
    SELECT m.permission, m.capability FROM unnest($1::text[], $2::text[]) AS m(permission, capability)
),
platform_assignment AS (
    SELECT ur.user_id::bigint AS user_id, r.id::bigint AS role_id, r.name AS role_name, r.mode
    FROM public.auth_core__user_role AS ur
    JOIN public.auth_core__role AS r ON r.id = ur.role_id
    WHERE r.mode <> 'default'
      AND ($4::bigint = 0 OR ur.user_id = $4::bigint)
      AND ($6::bigint[] IS NULL OR ur.user_id = ANY($6::bigint[]))
      AND $5::bigint = 0
),
project_assignment AS (
    SELECT a.user_id::bigint AS user_id, a.project_id::bigint AS project_id, pr.id::bigint AS role_id, pr.name AS role_name
    FROM public.auth_core__project_user_role AS a
    JOIN public.auth_core__project_role AS pr ON pr.id = a.role_id AND pr.project_id = a.project_id
    WHERE ($4::bigint = 0 OR a.user_id = $4::bigint)
      AND ($6::bigint[] IS NULL OR a.user_id = ANY($6::bigint[]))
      AND ($5::bigint = 0 OR a.project_id = $5::bigint)
),
own_rows AS (
    SELECT DISTINCT a.user_id, a.project_id
    FROM project_assignment AS a
    JOIN public.auth_core__project_role_permission AS g ON g.project_id = a.project_id AND g.role_id = a.role_id
),
project_permission AS (
    SELECT a.user_id, a.project_id, a.role_id, g.permission
    FROM project_assignment AS a
    JOIN public.auth_core__project_role_permission AS g ON g.project_id = a.project_id AND g.role_id = a.role_id
    UNION
    SELECT a.user_id, a.project_id, a.role_id, cg.permission
    FROM project_assignment AS a
    JOIN public.auth_core__role AS cr ON cr.name = a.role_name AND cr.mode = 'default'
    JOIN public.auth_core__role_permission AS cg ON cg.role_id = cr.id
    WHERE NOT EXISTS (SELECT 1 FROM own_rows AS o WHERE o.user_id = a.user_id AND o.project_id = a.project_id)
),
grants AS (
    SELECT a.user_id, 1 AS scope_order, 'platform'::text AS scope_type, a.mode::text AS scope_id, 0::bigint AS scope_sort,
           ('Platform (' || a.mode || ')')::text AS scope_name, 0::bigint AS project_id, true AS scope_active,
           a.role_name::text AS role, a.role_id, m.capability, rp.permission::text AS permission, 'role'::text AS granted_via
    FROM platform_assignment AS a
    LEFT JOIN (public.auth_core__role_permission AS rp JOIN cap_map AS m ON m.permission = rp.permission)
      ON rp.role_id = a.role_id
    UNION ALL
    SELECT a.user_id, 2, 'project', a.project_id::text, a.project_id,
           COALESCE(p.name, '')::text, a.project_id, COALESCE(NOT p.suspended, false),
           a.role_name::text, a.role_id, m.capability, pp.permission::text, 'role'
    FROM project_assignment AS a
    LEFT JOIN centry.project AS p ON p.id = a.project_id
    LEFT JOIN (project_permission AS pp JOIN cap_map AS m ON m.permission = pp.permission)
      ON pp.user_id = a.user_id AND pp.project_id = a.project_id AND pp.role_id = a.role_id
    UNION ALL
    SELECT o.user_id, 3, 'agent', reg.application_id::text, reg.application_id,
           '', reg.tenant_project_id, COALESCE(NOT p.suspended, false),
           o.role, 0::bigint, oc.capability, NULL::text, 'owner'
    FROM elitea_lifecycle.agent_registry AS reg
    LEFT JOIN centry.project AS p ON p.id = reg.tenant_project_id
    CROSS JOIN LATERAL (VALUES (reg.business_owner_id, 'business_owner'::text),
                               (reg.technical_owner_id, 'technical_owner'::text)) AS o(user_id, role)
    CROSS JOIN unnest($3::text[]) AS oc(capability)
    WHERE o.user_id IS NOT NULL
      AND ($4::bigint = 0 OR o.user_id = $4::bigint)
      AND ($6::bigint[] IS NULL OR o.user_id = ANY($6::bigint[]))
      AND ($5::bigint = 0 OR reg.tenant_project_id = $5::bigint)
)
SELECT g.user_id, COALESCE(u.email, ''), COALESCE(u.name, ''), NOT u.suspended,
       g.scope_type, g.scope_id, g.scope_name, g.project_id, g.scope_active,
       g.role, g.role_id, COALESCE(g.capability, ''),
       COALESCE(array_agg(DISTINCT g.permission ORDER BY g.permission) FILTER (WHERE g.permission IS NOT NULL), '{}'::text[]),
       g.granted_via
FROM grants AS g
JOIN public.auth_core__user AS u ON u.id = g.user_id
GROUP BY g.user_id, u.email, u.name, u.suspended, g.scope_order, g.scope_type, g.scope_id, g.scope_sort,
         g.scope_name, g.project_id, g.scope_active, g.role, g.role_id, g.capability, g.granted_via
ORDER BY g.user_id, g.scope_order, g.project_id, g.scope_sort, g.scope_id, g.role, COALESCE(g.capability, '')`

type entitlementQueryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func grantArgs(filter EntitlementFilter, userIDs []int64) []any {
	permissions, capabilities := entitlements.PermissionCapabilityPairs()
	owner := make([]string, 0, len(entitlements.OwnerCapabilities))
	for _, capability := range entitlements.OwnerCapabilities {
		owner = append(owner, string(capability))
	}
	var ids any // NULL: every account
	if userIDs != nil {
		ids = userIDs
	}
	return []any{permissions, capabilities, owner, filter.UserID, filter.ProjectID, ids}
}

func scanEntitlementGrant(rows pgx.Rows) (entitlements.Grant, error) {
	var g entitlements.Grant
	err := rows.Scan(&g.UserID, &g.UserName, &g.DisplayName, &g.UserActive,
		&g.ScopeType, &g.ScopeID, &g.ScopeName, &g.ProjectID, &g.ScopeActive,
		&g.Role, &g.RoleID, &g.Capability, &g.Permissions, &g.GrantedVia)
	return g, err
}

// agentNames reads the names of the registry agents that have an owner, by
// (project, application), narrowed to one project and to the agents the
// listed accounts own (nil: every account). A project whose tenant schema is
// missing is skipped: the name is a display field.
func agentNames(ctx context.Context, q entitlementQueryer, filter EntitlementFilter, userIDs []int64) (map[[2]int64]string, error) {
	var owners any // NULL: every owner
	if userIDs != nil {
		owners = userIDs
	}
	if filter.UserID > 0 {
		owners = []int64{filter.UserID}
	}
	rows, err := q.Query(ctx, `
SELECT tenant_project_id, array_agg(application_id ORDER BY application_id)
FROM elitea_lifecycle.agent_registry
WHERE (business_owner_id IS NOT NULL OR technical_owner_id IS NOT NULL)
  AND ($1::bigint = 0 OR tenant_project_id = $1::bigint)
  AND ($2::bigint[] IS NULL OR business_owner_id = ANY($2::bigint[]) OR technical_owner_id = ANY($2::bigint[]))
GROUP BY tenant_project_id`, filter.ProjectID, owners)
	if err != nil {
		return nil, fmt.Errorf("entitlements: registry agents: %w", err)
	}
	byProject := map[int64][]int64{}
	for rows.Next() {
		var (
			project int64
			ids     []int64
		)
		if err := rows.Scan(&project, &ids); err != nil {
			rows.Close()
			return nil, fmt.Errorf("entitlements: registry agents: %w", err)
		}
		byProject[project] = ids
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("entitlements: registry agents: %w", err)
	}
	names := map[[2]int64]string{}
	for project, ids := range byProject {
		schema, err := tenantschema.QuoteInt(project)
		if err != nil {
			continue
		}
		// Probed first: inside a transaction a failed statement aborts the
		// rest, so a missing table must never reach the SELECT.
		var present *string
		if err := q.QueryRow(ctx, `SELECT to_regclass($1)::text`, schema+".applications").Scan(&present); err != nil {
			return nil, fmt.Errorf("entitlements: probe the tenant schema: %w", err)
		}
		if present == nil {
			continue
		}
		result, err := q.Query(ctx, fmt.Sprintf(`SELECT id::bigint, COALESCE(name, '') FROM %s.applications WHERE id = ANY($1::bigint[])`, schema), ids)
		if err != nil {
			return nil, fmt.Errorf("entitlements: agent names: %w", err)
		}
		for result.Next() {
			var (
				id   int64
				name string
			)
			if err := result.Scan(&id, &name); err != nil {
				result.Close()
				return nil, fmt.Errorf("entitlements: agent names: %w", err)
			}
			names[[2]int64{project, id}] = name
		}
		result.Close()
		if err := result.Err(); err != nil {
			return nil, fmt.Errorf("entitlements: agent names: %w", err)
		}
	}
	return names, nil
}

func nameAgent(g *entitlements.Grant, names map[[2]int64]string) {
	if g.ScopeType != entitlements.ScopeAgent {
		return
	}
	var id int64
	_, _ = fmt.Sscan(g.ScopeID, &id)
	g.ScopeName = names[[2]int64{g.ProjectID, id}]
}

func (s *Entitlements) ready() error {
	if s == nil || s.pool == nil {
		return errors.New("entitlements: no database pool")
	}
	return nil
}

// Stream writes every grant matching the filter to emit, in a single
// read-only snapshot, without holding the export in memory.
//
// before receives the exact number of rows emit will be called with, before
// the first one, so the caller can record the export (and refuse it when the
// record cannot be written) before any byte leaves.
func (s *Entitlements) Stream(
	ctx context.Context, filter EntitlementFilter,
	before func(rows int) error, emit func(entitlements.Grant) error,
) error {
	if err := s.ready(); err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("entitlements: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	names, err := agentNames(ctx, tx, filter, nil)
	if err != nil {
		return err
	}
	args := grantArgs(filter, nil)
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM (`+grantsSQL+`) AS export`, args...).Scan(&total); err != nil {
		return fmt.Errorf("entitlements: count: %w", err)
	}
	if err := before(total); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, grantsSQL, args...)
	if err != nil {
		return fmt.Errorf("entitlements: query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		grant, err := scanEntitlementGrant(rows)
		if err != nil {
			return fmt.Errorf("entitlements: scan: %w", err)
		}
		nameAgent(&grant, names)
		if err := emit(grant); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("entitlements: rows: %w", err)
	}
	return nil
}

// Page answers one page of grants and the total, for the admin JSON view.
func (s *Entitlements) Page(ctx context.Context, filter EntitlementFilter, limit, offset int) ([]entitlements.Grant, int, error) {
	if err := s.ready(); err != nil {
		return nil, 0, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, fmt.Errorf("entitlements: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	names, err := agentNames(ctx, tx, filter, nil)
	if err != nil {
		return nil, 0, err
	}
	args := grantArgs(filter, nil)
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM (`+grantsSQL+`) AS export`, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("entitlements: count: %w", err)
	}
	args = append(args, limit, offset)
	rows, err := tx.Query(ctx, grantsSQL+` LIMIT $7 OFFSET $8`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("entitlements: query: %w", err)
	}
	defer rows.Close()
	out := []entitlements.Grant{}
	for rows.Next() {
		grant, err := scanEntitlementGrant(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("entitlements: scan: %w", err)
		}
		nameAgent(&grant, names)
		out = append(out, grant)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("entitlements: rows: %w", err)
	}
	return out, total, nil
}

// RecordExport writes the audit_events row of one export, synchronously:
// an export the trail cannot record is refused by the caller.
func (s *Entitlements) RecordExport(ctx context.Context, record EntitlementExportAudit) error {
	if err := s.ready(); err != nil {
		return err
	}
	var user any
	if record.UserID > 0 {
		user = int32(record.UserID)
	}
	action := fmt.Sprintf("export entitlements %s: %d rows", record.Format, record.Rows)
	if _, err := s.pool.Exec(ctx, `
INSERT INTO centry.audit_events
    (timestamp, user_id, user_email, event_type, action, http_method, http_route, status_code, duration_ms, is_error, entity_type, entity_name)
VALUES (clock_timestamp(), $1, $2, 'api', $3, 'GET', $4, 200, 0, false, 'entitlements', $5)`,
		user, nullableText(truncateEvidence(record.Actor, 256)), truncateEvidence(action, 512),
		truncateEvidence(record.Route, 512), record.Format); err != nil {
		return fmt.Errorf("entitlements: record the export: %w", err)
	}
	return nil
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

/* ── SCIM ──────────────────────────────────────────────────────────────── */

// ListUsers answers one page of accounts, each with its grants, and the
// number of accounts the filter matches. startIndex is one-based.
func (s *Entitlements) ListUsers(ctx context.Context, filter scimdirectory.Filter, startIndex, count int) ([]entitlements.UserEntitlements, int, error) {
	if err := s.ready(); err != nil {
		return nil, 0, err
	}
	condition, args := filter.Condition(1)
	var total int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM public.auth_core__user AS account WHERE `+condition, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("entitlements: count users: %w", err)
	}
	if count == 0 {
		return []entitlements.UserEntitlements{}, total, nil
	}
	position := len(args) + 1
	args = append(args, startIndex-1, count)
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT account.id::bigint, COALESCE(account.email, ''), COALESCE(account.name, ''), NOT account.suspended
FROM public.auth_core__user AS account
WHERE %s
ORDER BY account.id
OFFSET $%d LIMIT $%d`, condition, position, position+1), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("entitlements: list users: %w", err)
	}
	users := []entitlements.UserEntitlements{}
	for rows.Next() {
		var user entitlements.UserEntitlements
		if err := rows.Scan(&user.UserID, &user.UserName, &user.DisplayName, &user.Active); err != nil {
			rows.Close()
			return nil, 0, fmt.Errorf("entitlements: list users: %w", err)
		}
		user.Grants = []entitlements.Grant{}
		users = append(users, user)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("entitlements: list users: %w", err)
	}
	if err := s.attachGrants(ctx, users); err != nil {
		return nil, 0, err
	}
	return users, total, nil
}

// GetUser answers one account and its grants.
func (s *Entitlements) GetUser(ctx context.Context, id int64) (entitlements.UserEntitlements, error) {
	if err := s.ready(); err != nil {
		return entitlements.UserEntitlements{}, err
	}
	var user entitlements.UserEntitlements
	err := s.pool.QueryRow(ctx, `
SELECT id::bigint, COALESCE(email, ''), COALESCE(name, ''), NOT suspended
FROM public.auth_core__user WHERE id = $1`, id).Scan(&user.UserID, &user.UserName, &user.DisplayName, &user.Active)
	if errors.Is(err, pgx.ErrNoRows) {
		return entitlements.UserEntitlements{}, ErrEntitlementNotFound
	}
	if err != nil {
		return entitlements.UserEntitlements{}, fmt.Errorf("entitlements: get user: %w", err)
	}
	user.Grants = []entitlements.Grant{}
	users := []entitlements.UserEntitlements{user}
	if err := s.attachGrants(ctx, users); err != nil {
		return entitlements.UserEntitlements{}, err
	}
	return users[0], nil
}

func (s *Entitlements) attachGrants(ctx context.Context, users []entitlements.UserEntitlements) error {
	if len(users) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(users))
	index := map[int64]int{}
	for i, user := range users {
		ids = append(ids, user.UserID)
		index[user.UserID] = i
	}
	names, err := agentNames(ctx, s.pool, EntitlementFilter{}, ids)
	if err != nil {
		return err
	}
	rows, err := s.pool.Query(ctx, grantsSQL, grantArgs(EntitlementFilter{}, ids)...)
	if err != nil {
		return fmt.Errorf("entitlements: user grants: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		grant, err := scanEntitlementGrant(rows)
		if err != nil {
			return fmt.Errorf("entitlements: user grants: %w", err)
		}
		nameAgent(&grant, names)
		if i, ok := index[grant.UserID]; ok {
			users[i].Grants = append(users[i].Grants, grant)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("entitlements: user grants: %w", err)
	}
	return nil
}

// groupsSQL lists every role as a group: the platform roles (one per mode
// other than default) and every project role.
const groupsSQL = `
WITH grp AS (
    SELECT ('platform-role-' || r.id)::text AS id, r.id::bigint AS role_id, 'platform'::text AS scope_type,
           r.mode::text AS scope_id, ('Platform (' || r.mode || ')')::text AS scope_name, 0::bigint AS project_id,
           true AS scope_active, r.name::text AS role,
           ('Platform (' || r.mode || ') / ' || r.name)::text AS display_name, 1 AS kind
    FROM public.auth_core__role AS r
    WHERE r.mode <> 'default'
    UNION ALL
    SELECT ('project-role-' || pr.id)::text, pr.id::bigint, 'project', pr.project_id::text,
           COALESCE(p.name, '')::text, pr.project_id::bigint, COALESCE(NOT p.suspended, false), pr.name::text,
           (COALESCE(NULLIF(p.name, ''), 'Project ' || pr.project_id) || ' / ' || pr.name)::text, 2
    FROM public.auth_core__project_role AS pr
    LEFT JOIN centry.project AS p ON p.id = pr.project_id
)`

// ListGroups answers one page of role groups with their members and
// permissions, and the number the filter matches.
func (s *Entitlements) ListGroups(ctx context.Context, filter scimdirectory.Filter, startIndex, count int) ([]entitlements.Group, int, error) {
	if err := s.ready(); err != nil {
		return nil, 0, err
	}
	condition, args := filter.Condition(1)
	var total int
	if err := s.pool.QueryRow(ctx, groupsSQL+` SELECT count(*) FROM grp WHERE `+condition, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("entitlements: count groups: %w", err)
	}
	if count == 0 {
		return []entitlements.Group{}, total, nil
	}
	position := len(args) + 1
	args = append(args, startIndex-1, count)
	groups, err := s.readGroups(ctx, fmt.Sprintf(groupsSQL+`
SELECT id, role_id, scope_type, scope_id, scope_name, project_id, scope_active, role, display_name
FROM grp WHERE %s ORDER BY kind, role_id OFFSET $%d LIMIT $%d`, condition, position, position+1), args...)
	if err != nil {
		return nil, 0, err
	}
	return groups, total, nil
}

// GetGroup answers one role group by its SCIM id.
func (s *Entitlements) GetGroup(ctx context.Context, id string) (entitlements.Group, error) {
	if err := s.ready(); err != nil {
		return entitlements.Group{}, err
	}
	if _, _, ok := entitlements.ParseGroupID(id); !ok {
		return entitlements.Group{}, ErrEntitlementNotFound
	}
	groups, err := s.readGroups(ctx, groupsSQL+`
SELECT id, role_id, scope_type, scope_id, scope_name, project_id, scope_active, role, display_name
FROM grp WHERE id = $1`, id)
	if err != nil {
		return entitlements.Group{}, err
	}
	if len(groups) == 0 {
		return entitlements.Group{}, ErrEntitlementNotFound
	}
	return groups[0], nil
}

func (s *Entitlements) readGroups(ctx context.Context, sql string, args ...any) ([]entitlements.Group, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("entitlements: groups: %w", err)
	}
	groups := []entitlements.Group{}
	for rows.Next() {
		var g entitlements.Group
		if err := rows.Scan(&g.ID, &g.RoleID, &g.ScopeType, &g.ScopeID, &g.ScopeName, &g.ProjectID,
			&g.ScopeActive, &g.Role, &g.DisplayName); err != nil {
			rows.Close()
			return nil, fmt.Errorf("entitlements: groups: %w", err)
		}
		g.Permissions, g.Capabilities, g.Members = []string{}, []string{}, []entitlements.Member{}
		groups = append(groups, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("entitlements: groups: %w", err)
	}
	if len(groups) == 0 {
		return groups, nil
	}
	var platformIDs, projectIDs []int64
	byKey := map[string]int{}
	for i, g := range groups {
		byKey[g.ID] = i
		if g.ScopeType == entitlements.ScopePlatform {
			platformIDs = append(platformIDs, g.RoleID)
		} else {
			projectIDs = append(projectIDs, g.RoleID)
		}
	}
	// One query for members and one for permissions, both keyed by the
	// SCIM group id.
	members, err := s.pool.Query(ctx, `
SELECT 'platform-role-' || ur.role_id, u.id::bigint, COALESCE(u.email, '')
FROM public.auth_core__user_role AS ur JOIN public.auth_core__user AS u ON u.id = ur.user_id
WHERE ur.role_id = ANY($1::bigint[])
UNION ALL
SELECT 'project-role-' || a.role_id, u.id::bigint, COALESCE(u.email, '')
FROM public.auth_core__project_user_role AS a
JOIN public.auth_core__project_role AS pr ON pr.id = a.role_id AND pr.project_id = a.project_id
JOIN public.auth_core__user AS u ON u.id = a.user_id
WHERE a.role_id = ANY($2::bigint[])
ORDER BY 1, 2`, platformIDs, projectIDs)
	if err != nil {
		return nil, fmt.Errorf("entitlements: group members: %w", err)
	}
	for members.Next() {
		var (
			key    string
			member entitlements.Member
		)
		if err := members.Scan(&key, &member.UserID, &member.UserName); err != nil {
			members.Close()
			return nil, fmt.Errorf("entitlements: group members: %w", err)
		}
		if i, ok := byKey[key]; ok {
			groups[i].Members = append(groups[i].Members, member)
		}
	}
	members.Close()
	if err := members.Err(); err != nil {
		return nil, fmt.Errorf("entitlements: group members: %w", err)
	}
	// A project role's permissions are its own rows in its project when it
	// has any, else the central default-mode role of the same name.
	permissions, err := s.pool.Query(ctx, `
SELECT 'platform-role-' || rp.role_id, rp.permission::text
FROM public.auth_core__role_permission AS rp
WHERE rp.role_id = ANY($1::bigint[]) AND rp.permission IS NOT NULL
UNION
SELECT 'project-role-' || pr.id, g.permission
FROM public.auth_core__project_role AS pr
JOIN public.auth_core__project_role_permission AS g ON g.project_id = pr.project_id AND g.role_id = pr.id
WHERE pr.id = ANY($2::bigint[])
UNION
SELECT 'project-role-' || pr.id, cg.permission::text
FROM public.auth_core__project_role AS pr
JOIN public.auth_core__role AS cr ON cr.name = pr.name AND cr.mode = 'default'
JOIN public.auth_core__role_permission AS cg ON cg.role_id = cr.id
WHERE pr.id = ANY($2::bigint[]) AND cg.permission IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM public.auth_core__project_role_permission AS own
                  WHERE own.project_id = pr.project_id AND own.role_id = pr.id)
ORDER BY 1, 2`, platformIDs, projectIDs)
	if err != nil {
		return nil, fmt.Errorf("entitlements: group permissions: %w", err)
	}
	for permissions.Next() {
		var key, permission string
		if err := permissions.Scan(&key, &permission); err != nil {
			permissions.Close()
			return nil, fmt.Errorf("entitlements: group permissions: %w", err)
		}
		if i, ok := byKey[key]; ok {
			groups[i].Permissions = append(groups[i].Permissions, permission)
		}
	}
	permissions.Close()
	if err := permissions.Err(); err != nil {
		return nil, fmt.Errorf("entitlements: group permissions: %w", err)
	}
	for i := range groups {
		groups[i].Capabilities = entitlements.CapabilitiesOf(groups[i].Permissions)
	}
	return groups, nil
}
