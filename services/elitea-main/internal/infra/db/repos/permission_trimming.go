package repos

// The permission trimming report (ADR-0044 decision 5, issue #1501): per
// agent, what it is granted (toolkits, the tools selected in each, ADR-0039
// credential use grants and their operation scopes) and per project and
// platform, the worker egress allowlist entries, compared with what the
// step records (elitea_runtime.run_steps, kinds tool_call, retrieval and
// delegation) and the egress run summaries show was used in a window.
// Unused grants are removal candidates; each is removed with one audited
// action through the existing mutation path:
//
//	tool              the agent version's selected tools, written by
//	                  toolrelation.Attach (the toolkit route's relation
//	                  branch), the published-version refusal included
//	toolkit           toolrelation.Detach, the same route's detach
//	credential_grant  the grant service's revoke (GrantRevoker)
//	egress_host       the project's worker_egress configuration row or the
//	                  platform's worker_egress section, the remaining list
//	                  validated by platformconfig.ValidateWorkerEgressEntries
//
// # Which grants are listed
//
// Toolkits and tools are listed for the agent's DRAFT versions only: a
// published version is frozen (its release is what runs) and the toolkit
// route refuses to change it, so its removal is "edit the draft and
// publish". Usage is per agent (run_steps.application_id), whichever
// version ran.
//
// A tool is used when a step of the toolkit named it: the step's name (the
// tool's display name) equal to the tool, or ending in it after a
// separator ("toolkit___tool"), or the step summary's tool_name. A toolkit
// is used when any step carried its id. A toolkit whose agent selected no
// tool (and whose own settings select none) grants every tool it has; its
// tools are not enumerable, so only the toolkit is listed.
//
// A tool is not a removal candidate when it is the version's last selected
// tool in the toolkit: an empty agent selection means "every tool", so
// removing the last would widen the grant. Remove the toolkit instead.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/libs/go/egresslib"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/toolrelation"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// Trimming item kinds.
const (
	TrimTool            = "tool"
	TrimToolkit         = "toolkit"
	TrimCredentialGrant = "credential_grant"
	TrimEgressHost      = "egress_host"
)

// Errors of the trimming report.
var (
	ErrTrimItemNotFound     = errors.New("permission trimming: the item does not exist")
	ErrTrimItemNotRemovable = errors.New("permission trimming: the item cannot be removed")
)

// TrimmingItem is one granted permission in the report.
type TrimmingItem struct {
	Kind            string     `json:"kind"`
	Key             string     `json:"key"`
	ProjectID       int64      `json:"project_id"`
	ApplicationID   int64      `json:"application_id"`
	ApplicationName string     `json:"application_name"`
	VersionID       *int64     `json:"version_id"`
	VersionName     string     `json:"version_name"`
	ToolkitID       *int64     `json:"toolkit_id"`
	ToolkitName     string     `json:"toolkit_name"`
	Tool            string     `json:"tool"`
	GrantID         string     `json:"grant_id"`
	ReferenceID     string     `json:"credential_reference_id"`
	Scopes          []string   `json:"scopes"`
	UnusedScopes    []string   `json:"unused_scopes"`
	Host            string     `json:"host"`
	AllowlistLevel  string     `json:"allowlist_level"`
	Used            bool       `json:"used"`
	Uses            int64      `json:"uses"`
	LastUsedAt      *time.Time `json:"last_used_at"`
	Candidate       bool       `json:"candidate"`
	Removable       bool       `json:"removable"`
	NotRemovable    string     `json:"not_removable_reason"`
	CandidateSince  *time.Time `json:"candidate_since"`
	NotifiedAt      *time.Time `json:"notified_at"`
}

// TrimmingFilter selects the report.
type TrimmingFilter struct {
	// ProjectID 0 is every project (admin only).
	ProjectID int64
	// ApplicationID 0 is every agent of the project(s).
	ApplicationID int64
	// WindowDays 0 is the policy's window.
	WindowDays int
	// CandidatesOnly drops the used grants.
	CandidatesOnly bool
	// IncludePlatform adds the platform allowlist entries (admin view).
	IncludePlatform bool
}

// TrimmingReport is the report.
type TrimmingReport struct {
	WindowDays int            `json:"window_days"`
	Since      time.Time      `json:"since"`
	Items      []TrimmingItem `json:"items"`
}

// PermissionTrimming builds the report and performs removals. It shares
// the credential review store (policy, audit, grant revoker).
type PermissionTrimming struct {
	review *CredentialReview
}

// NewPermissionTrimming builds the store over the credential review store.
func NewPermissionTrimming(review *CredentialReview) (*PermissionTrimming, error) {
	if review == nil {
		return nil, errors.New("permission trimming: the credential review store is required")
	}
	return &PermissionTrimming{review: review}, nil
}

// Report computes the report.
func (t *PermissionTrimming) Report(ctx context.Context, filter TrimmingFilter) (TrimmingReport, error) {
	window := filter.WindowDays
	if window <= 0 {
		policy, err := t.review.policy(ctx)
		if err != nil {
			return TrimmingReport{}, fmt.Errorf("permission trimming: read the policy: %w", err)
		}
		window = policy.TrimmingWindowDays
	}
	window = min(max(window, platformconfig.MinTrimmingWindowDays), platformconfig.MaxTrimmingWindowDays)
	since := t.review.now().Add(-time.Duration(window) * 24 * time.Hour)
	report := TrimmingReport{WindowDays: window, Since: since.UTC(), Items: []TrimmingItem{}}

	projects, err := t.projects(ctx, filter.ProjectID)
	if err != nil {
		return TrimmingReport{}, err
	}
	for _, projectID := range projects {
		items, err := t.projectItems(ctx, projectID, filter.ApplicationID, since)
		if err != nil {
			return TrimmingReport{}, err
		}
		report.Items = append(report.Items, items...)
	}
	if filter.IncludePlatform && filter.ProjectID == 0 && filter.ApplicationID == 0 {
		items, err := t.platformEgressItems(ctx, since)
		if err != nil {
			return TrimmingReport{}, err
		}
		report.Items = append(report.Items, items...)
	}
	if err := t.decorateCandidates(ctx, report.Items); err != nil {
		return TrimmingReport{}, err
	}
	if filter.CandidatesOnly {
		kept := report.Items[:0]
		for _, item := range report.Items {
			if item.Candidate {
				kept = append(kept, item)
			}
		}
		report.Items = kept
	}
	return report, nil
}

func (t *PermissionTrimming) projects(ctx context.Context, projectID int64) ([]int64, error) {
	rows, err := t.review.pool.Query(ctx, `
SELECT p.id FROM centry.project AS p
WHERE ($1::bigint = 0 OR p.id = $1::bigint)
  AND to_regclass(format('%I.entity_tool_mapping', 'p_' || p.id)) IS NOT NULL
  AND to_regclass(format('%I.application_versions', 'p_' || p.id)) IS NOT NULL
ORDER BY p.id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("permission trimming: list projects: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int32
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, int64(id))
	}
	return ids, rows.Err()
}

// usage is what the step records show for one agent.
type usage struct {
	// toolkits[toolkit id] = (count, last)
	toolkits map[string]useCount
	// tools[toolkit id][step name] = (count, last)
	tools map[string]map[string]useCount
}

type useCount struct {
	count int64
	last  time.Time
}

func (u useCount) add(count int64, last time.Time) useCount {
	u.count += count
	if last.After(u.last) {
		u.last = last
	}
	return u
}

func (t *PermissionTrimming) stepUsage(ctx context.Context, projectID, applicationID int64, since time.Time) (map[int64]*usage, error) {
	out := map[int64]*usage{}
	if exists, err := relationExists(ctx, t.review.pool, "elitea_runtime.run_steps"); err != nil || !exists {
		return out, err
	}
	// run_step_heads (project_id, updated_at) narrows to the project's runs
	// touched in the window; a head's updated_at moves with every append.
	rows, err := t.review.pool.Query(ctx, `
SELECT s.application_id, s.toolkit_id, s.name, COALESCE(s.summary->>'tool_name', ''), count(*), max(s.started_at)
FROM elitea_runtime.run_step_heads AS h
JOIN elitea_runtime.run_steps AS s ON s.run_id = h.run_id
WHERE h.project_id = $1 AND h.updated_at >= $2
  AND s.started_at >= $2
  AND s.application_id IS NOT NULL
  AND s.kind IN ('tool_call', 'retrieval', 'delegation')
  AND ($3::bigint = 0 OR s.application_id = $3::bigint)
GROUP BY 1, 2, 3, 4`, projectID, since, applicationID)
	if err != nil {
		return nil, fmt.Errorf("permission trimming: read the step records: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var app int64
		var toolkit, name, toolName string
		var count int64
		var last time.Time
		if err := rows.Scan(&app, &toolkit, &name, &toolName, &count, &last); err != nil {
			return nil, err
		}
		u, ok := out[app]
		if !ok {
			u = &usage{toolkits: map[string]useCount{}, tools: map[string]map[string]useCount{}}
			out[app] = u
		}
		if toolkit == "" {
			continue
		}
		u.toolkits[toolkit] = u.toolkits[toolkit].add(count, last)
		if u.tools[toolkit] == nil {
			u.tools[toolkit] = map[string]useCount{}
		}
		for _, n := range []string{name, toolName} {
			if n != "" {
				u.tools[toolkit][n] = u.tools[toolkit][n].add(count, last)
			}
		}
	}
	return out, rows.Err()
}

// toolUse finds the use of tool among the step names of a toolkit.
func toolUse(names map[string]useCount, tool string) useCount {
	var found useCount
	for name, use := range names {
		if name == tool || toolNameEndsWith(name, tool) {
			found = found.add(use.count, use.last)
		}
	}
	return found
}

func toolNameEndsWith(name, tool string) bool {
	if !strings.HasSuffix(name, tool) || len(name) == len(tool) {
		return false
	}
	separator := name[len(name)-len(tool)-1]
	return !(separator >= 'a' && separator <= 'z' || separator >= 'A' && separator <= 'Z' || separator >= '0' && separator <= '9')
}

type grantedToolkit struct {
	versionID, applicationID, toolkitID int64
	versionName, applicationName        string
	toolkitName                         string
	stored, agent                       []string
}

// effectiveTools is agentrelease.ProjectSettings' selection rule: the
// agent's selection narrowed to the toolkit's own, else the agent's, else
// the toolkit's; empty means every tool.
func (g grantedToolkit) effectiveTools() []string {
	if len(g.agent) > 0 {
		if len(g.stored) > 0 {
			var narrowed []string
			for _, tool := range g.agent {
				for _, allowed := range g.stored {
					if tool == allowed {
						narrowed = append(narrowed, tool)
						break
					}
				}
			}
			if len(narrowed) > 0 {
				return narrowed
			}
		}
		return g.agent
	}
	return g.stored
}

func decodeStrings(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var values []any
	if json.Unmarshal(raw, &values) != nil {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok && text != "" {
			out = append(out, text)
		}
	}
	return out
}

func (t *PermissionTrimming) projectItems(ctx context.Context, projectID, applicationID int64, since time.Time) ([]TrimmingItem, error) {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return nil, err
	}
	pool := t.review.pool
	rows, err := pool.Query(ctx, fmt.Sprintf(`
SELECT v.id, v.name, a.id, a.name, m.tool_id, t.name, t.settings->'selected_tools', m.selected_tools
FROM %[1]s.application_versions AS v
JOIN %[1]s.applications AS a ON a.id = v.application_id
JOIN %[1]s.entity_tool_mapping AS m ON m.entity_version_id = v.id AND m.entity_type IN ('agent', 'application')
JOIN %[1]s.elitea_tools AS t ON t.id = m.tool_id
WHERE v.status NOT IN ('published', 'embedded')
  AND ($1::bigint = 0 OR a.id = $1::bigint)
ORDER BY a.id, v.id, m.tool_id`, schema), applicationID)
	if err != nil {
		return nil, fmt.Errorf("permission trimming: read the toolkits of project %d: %w", projectID, err)
	}
	var granted []grantedToolkit
	for rows.Next() {
		var g grantedToolkit
		var stored, agent []byte
		if err := rows.Scan(&g.versionID, &g.versionName, &g.applicationID, &g.applicationName, &g.toolkitID,
			&g.toolkitName, &stored, &agent); err != nil {
			rows.Close()
			return nil, err
		}
		g.stored, g.agent = decodeStrings(stored), decodeStrings(agent)
		granted = append(granted, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	used, err := t.stepUsage(ctx, projectID, applicationID, since)
	if err != nil {
		return nil, err
	}

	var items []TrimmingItem
	for _, g := range granted {
		u := used[g.applicationID]
		toolkitKey := strconv.FormatInt(g.toolkitID, 10)
		var toolkitUse useCount
		var names map[string]useCount
		if u != nil {
			toolkitUse = u.toolkits[toolkitKey]
			names = u.tools[toolkitKey]
		}
		versionID, toolkitID := g.versionID, g.toolkitID
		base := TrimmingItem{
			ProjectID: projectID, ApplicationID: g.applicationID, ApplicationName: g.applicationName,
			VersionID: &versionID, VersionName: g.versionName, ToolkitID: &toolkitID, ToolkitName: g.toolkitName,
			Scopes: []string{}, UnusedScopes: []string{},
		}
		toolkitItem := base
		toolkitItem.Kind = TrimToolkit
		toolkitItem.Key = fmt.Sprintf("%d/%d", g.versionID, g.toolkitID)
		markUse(&toolkitItem, toolkitUse)
		items = append(items, toolkitItem)

		tools := g.effectiveTools()
		for _, tool := range tools {
			item := base
			item.Kind = TrimTool
			item.Key = fmt.Sprintf("%d/%d/%s", g.versionID, g.toolkitID, tool)
			item.Tool = tool
			markUse(&item, toolUse(names, tool))
			if item.Candidate && len(tools) == 1 {
				item.Removable = false
				item.NotRemovable = "last_selected_tool"
			}
			items = append(items, item)
		}
	}

	grants, err := t.grantItems(ctx, projectID, applicationID, since)
	if err != nil {
		return nil, err
	}
	items = append(items, grants...)
	if applicationID == 0 {
		egress, err := t.projectEgressItems(ctx, projectID, schema, since)
		if err != nil {
			return nil, err
		}
		items = append(items, egress...)
	}
	return items, nil
}

func markUse(item *TrimmingItem, use useCount) {
	item.Uses = use.count
	if use.count > 0 {
		last := use.last
		item.LastUsedAt = &last
		item.Used = true
	}
	item.Candidate = !item.Used
	item.Removable = item.Candidate
}

func (t *PermissionTrimming) grantItems(ctx context.Context, projectID, applicationID int64, since time.Time) ([]TrimmingItem, error) {
	pool := t.review.pool
	if exists, err := relationExists(ctx, pool, "elitea_identity.credential_use_grants"); err != nil || !exists {
		return nil, err
	}
	rows, err := pool.Query(ctx, `
SELECT g.id::text, g.agent_binding_id::text, g.toolkit_id, g.credential_reference_id::text, g.operation_scopes,
       b.application_id, u.last_used_at,
       COALESCE((
           SELECT array_agg(DISTINCT i.operation_id)
           FROM elitea_runtime.bound_tool_invocations AS i
           WHERE i.tenant_project_id = g.tenant_project_id AND i.toolkit_id = g.toolkit_id
             AND i.credential_reference_id = g.credential_reference_id AND i.created_at >= $3
       ), '{}'::text[])
FROM elitea_identity.credential_use_grants AS g
JOIN elitea_identity.agent_identity_bindings AS b ON b.id = g.agent_binding_id
LEFT JOIN elitea_identity.credential_last_use AS u
       ON u.kind = 'credential_grant'
      AND u.credential_id = g.agent_binding_id::text || '/' || g.toolkit_id::text || '/' || g.credential_reference_id::text
WHERE g.tenant_project_id = $1 AND g.state IN ('proposed', 'active', 'suspended')
  AND ($2::bigint = 0 OR b.application_id = $2::bigint)
ORDER BY b.application_id, g.id`, projectID, applicationID, since)
	if err != nil {
		return nil, fmt.Errorf("permission trimming: read the use grants of project %d: %w", projectID, err)
	}
	defer rows.Close()
	var items []TrimmingItem
	for rows.Next() {
		var id, binding, reference string
		var toolkit, app int64
		var scopes, usedOperations []string
		var lastUsed *time.Time
		if err := rows.Scan(&id, &binding, &toolkit, &reference, &scopes, &app, &lastUsed, &usedOperations); err != nil {
			return nil, err
		}
		toolkitID := toolkit
		item := TrimmingItem{
			Kind: TrimCredentialGrant, Key: grantUseKey(binding, toolkit, reference),
			ProjectID: projectID, ApplicationID: app, ToolkitID: &toolkitID, GrantID: id, ReferenceID: reference,
			Scopes: scopes, UnusedScopes: []string{},
		}
		used := map[string]bool{}
		for _, operation := range usedOperations {
			used[operation] = true
		}
		for _, scope := range scopes {
			if !scopeUsed(scope, used) {
				item.UnusedScopes = append(item.UnusedScopes, scope)
			}
		}
		if lastUsed != nil && !lastUsed.Before(since) {
			item.Used = true
			item.LastUsedAt = lastUsed
			item.Uses = int64(len(usedOperations))
		}
		item.Candidate = !item.Used
		item.Removable = item.Candidate
		items = append(items, item)
	}
	return items, rows.Err()
}

// scopeUsed matches an operation scope against the operations invoked; a
// scope ending in "*" covers its prefix.
func scopeUsed(scope string, used map[string]bool) bool {
	if used[scope] {
		return true
	}
	if prefix, ok := strings.CutSuffix(scope, "*"); ok {
		for operation := range used {
			if strings.HasPrefix(operation, prefix) {
				return true
			}
		}
	}
	return false
}

// reachedHosts reads the hosts the project's runs (every project's, for 0)
// reached in the window, as "https://host:port" candidates.
func (t *PermissionTrimming) reachedHosts(ctx context.Context, projectID int64, since time.Time) ([]string, error) {
	pool := t.review.pool
	if exists, err := relationExists(ctx, pool, "elitea_runtime.egress_run_summaries"); err != nil || !exists {
		return nil, err
	}
	rows, err := pool.Query(ctx, `
SELECT DISTINCT lower(host.value->>'host'), COALESCE(host.value->>'port', '')
FROM elitea_runtime.egress_run_summaries AS s,
     jsonb_array_elements(s.hosts_reached) AS host(value)
WHERE s.updated_at >= $2 AND ($1::bigint = 0 OR s.project_id = $1::bigint)
  AND COALESCE(host.value->>'host', '') <> ''`, projectID, since)
	if err != nil {
		return nil, fmt.Errorf("permission trimming: read the egress summaries: %w", err)
	}
	defer rows.Close()
	var hosts []string
	for rows.Next() {
		var host, port string
		if err := rows.Scan(&host, &port); err != nil {
			return nil, err
		}
		if port == "" {
			port = "443"
		}
		hosts = append(hosts, "https://"+hostForURL(host)+":"+port)
	}
	return hosts, rows.Err()
}

func hostForURL(host string) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return "[" + host + "]"
	}
	return host
}

func entryUsed(entry string, reached []string) bool {
	allowlist, err := egresslib.Parse([]string{entry})
	if err != nil || !allowlist.Configured() {
		return false
	}
	for _, candidate := range reached {
		if allowlist.Allows(candidate) {
			return true
		}
	}
	return false
}

func (t *PermissionTrimming) projectEgressItems(ctx context.Context, projectID int64, schema string, since time.Time) ([]TrimmingItem, error) {
	entries, err := (&WorkerEgressSources{pool: t.review.pool}).WorkerEgressProjectEntries(ctx, projectID)
	if err != nil || len(entries) == 0 {
		return nil, err
	}
	reached, err := t.reachedHosts(ctx, projectID, since)
	if err != nil {
		return nil, err
	}
	return egressItems(entries, reached, projectID, "project"), nil
}

func (t *PermissionTrimming) platformEgressItems(ctx context.Context, since time.Time) ([]TrimmingItem, error) {
	settings, err := platformconfig.LoadWorkerEgressSettings(ctx, t.review.pool)
	if err != nil || len(settings.Allowlist) == 0 {
		return nil, err
	}
	reached, err := t.reachedHosts(ctx, 0, since)
	if err != nil {
		return nil, err
	}
	return egressItems(settings.Allowlist, reached, 0, "platform"), nil
}

func egressItems(entries, reached []string, projectID int64, level string) []TrimmingItem {
	items := make([]TrimmingItem, 0, len(entries))
	for _, entry := range entries {
		item := TrimmingItem{
			Kind: TrimEgressHost, Key: level + ":" + entry, ProjectID: projectID, Host: entry,
			AllowlistLevel: level, Scopes: []string{}, UnusedScopes: []string{},
		}
		item.Used = entryUsed(entry, reached)
		item.Candidate = !item.Used
		item.Removable = item.Candidate
		items = append(items, item)
	}
	return items
}

// decorateCandidates joins what the weekly routine stored: since when an
// item is a candidate and when its owners were told.
func (t *PermissionTrimming) decorateCandidates(ctx context.Context, items []TrimmingItem) error {
	if len(items) == 0 {
		return nil
	}
	if exists, err := relationExists(ctx, t.review.pool, "elitea_lifecycle.permission_trimming_candidates"); err != nil || !exists {
		return err
	}
	type key struct {
		project, app int64
		kind, item   string
	}
	rows, err := t.review.pool.Query(ctx, `
SELECT tenant_project_id, application_id, item_kind, item_key, first_seen_at, notified_at
FROM elitea_lifecycle.permission_trimming_candidates WHERE resolved_at IS NULL`)
	if err != nil {
		return fmt.Errorf("permission trimming: read the candidates: %w", err)
	}
	defer rows.Close()
	stored := map[key][2]*time.Time{}
	for rows.Next() {
		var k key
		var first time.Time
		var notified *time.Time
		if err := rows.Scan(&k.project, &k.app, &k.kind, &k.item, &first, &notified); err != nil {
			return err
		}
		firstSeen := first
		stored[k] = [2]*time.Time{&firstSeen, notified}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range items {
		if !items[i].Candidate {
			continue
		}
		if found, ok := stored[key{items[i].ProjectID, items[i].ApplicationID, items[i].Kind, items[i].Key}]; ok {
			items[i].CandidateSince, items[i].NotifiedAt = found[0], found[1]
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Removal

// RemoveRequest names one item of the report.
type RemoveRequest struct {
	ProjectID     int64
	ApplicationID int64
	Kind          string
	Key           string
	ActorID       int64
	// Platform allows a platform egress entry (admin only).
	Platform bool
}

// RemoveResult is the removed item.
type RemoveResult struct {
	Kind      string    `json:"kind"`
	Key       string    `json:"key"`
	RemovedAt time.Time `json:"removed_at"`
}

// Remove removes one item through its existing mutation path, records
// permission.trimmed in the security audit chain and resolves the stored
// candidate. The item must still be granted; it is not required to be a
// candidate (an administrator may remove a used grant on purpose).
func (t *PermissionTrimming) Remove(ctx context.Context, request RemoveRequest) (RemoveResult, error) {
	switch request.Kind {
	case TrimTool, TrimToolkit:
		return t.removeToolOrToolkit(ctx, request)
	case TrimCredentialGrant:
		return t.removeGrant(ctx, request)
	case TrimEgressHost:
		return t.removeEgress(ctx, request)
	default:
		return RemoveResult{}, ErrCredentialReviewInvalid
	}
}

func (t *PermissionTrimming) removeToolOrToolkit(ctx context.Context, request RemoveRequest) (RemoveResult, error) {
	parts := strings.SplitN(request.Key, "/", 3)
	if (request.Kind == TrimToolkit && len(parts) != 2) || (request.Kind == TrimTool && len(parts) != 3) {
		return RemoveResult{}, ErrTrimItemNotFound
	}
	versionID, err1 := strconv.ParseInt(parts[0], 10, 64)
	toolkitID, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil || versionID <= 0 || toolkitID <= 0 {
		return RemoveResult{}, ErrTrimItemNotFound
	}
	schema, err := tenantSchema(strconv.FormatInt(request.ProjectID, 10))
	if err != nil {
		return RemoveResult{}, ErrTrimItemNotFound
	}
	var result RemoveResult
	err = pgx.BeginTxFunc(ctx, t.review.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		var applicationID int64
		var entityType string
		var stored, agent []byte
		err := tx.QueryRow(ctx, fmt.Sprintf(`
SELECT v.application_id, m.entity_type, t.settings->'selected_tools', m.selected_tools
FROM %[1]s.entity_tool_mapping AS m
JOIN %[1]s.application_versions AS v ON v.id = m.entity_version_id
JOIN %[1]s.elitea_tools AS t ON t.id = m.tool_id
WHERE m.entity_version_id = $1 AND m.tool_id = $2 AND m.entity_type IN ('agent', 'application')
ORDER BY m.id LIMIT 1
FOR UPDATE OF m`, schema), versionID, toolkitID).Scan(&applicationID, &entityType, &stored, &agent)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTrimItemNotFound
		}
		if err != nil {
			return err
		}
		if request.ApplicationID > 0 && applicationID != request.ApplicationID {
			return ErrTrimItemNotFound
		}
		if err := toolrelation.CheckVersionEditable(ctx, tx, schema, versionID); err != nil {
			return fmt.Errorf("%w: %v", ErrTrimItemNotRemovable, err)
		}
		if request.Kind == TrimToolkit {
			if _, err := toolrelation.Detach(ctx, tx, schema, versionID, toolkitID); err != nil {
				return err
			}
		} else {
			tool := parts[2]
			granted := grantedToolkit{stored: decodeStrings(stored), agent: decodeStrings(agent)}
			current := granted.effectiveTools()
			remaining := make([]string, 0, len(current))
			found := false
			for _, name := range current {
				if name == tool {
					found = true
					continue
				}
				remaining = append(remaining, name)
			}
			if !found {
				return ErrTrimItemNotFound
			}
			if len(remaining) == 0 {
				// An empty agent selection means every tool: removing the
				// last would widen the grant.
				return fmt.Errorf("%w: it is the last selected tool of the toolkit; remove the toolkit instead", ErrTrimItemNotRemovable)
			}
			encoded, err := json.Marshal(remaining)
			if err != nil {
				return err
			}
			if err := toolrelation.Attach(ctx, tx, schema, versionID, applicationID, entityType, toolkitID, encoded); err != nil {
				return err
			}
		}
		request.ApplicationID = applicationID
		at, err := t.recordTrimmed(ctx, tx, request)
		result = RemoveResult{Kind: request.Kind, Key: request.Key, RemovedAt: at}
		return err
	})
	if err != nil {
		return RemoveResult{}, err
	}
	return result, nil
}

func (t *PermissionTrimming) removeGrant(ctx context.Context, request RemoveRequest) (RemoveResult, error) {
	parts := strings.Split(request.Key, "/")
	if len(parts) != 3 {
		return RemoveResult{}, ErrTrimItemNotFound
	}
	var grantID string
	var applicationID int64
	err := t.review.pool.QueryRow(ctx, `
SELECT g.id::text, b.application_id
FROM elitea_identity.credential_use_grants AS g
JOIN elitea_identity.agent_identity_bindings AS b ON b.id = g.agent_binding_id
WHERE g.tenant_project_id = $1 AND g.agent_binding_id::text = $2 AND g.toolkit_id::text = $3
  AND g.credential_reference_id::text = $4 AND g.state IN ('proposed', 'active', 'suspended')`,
		request.ProjectID, parts[0], parts[1], parts[2]).Scan(&grantID, &applicationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return RemoveResult{}, ErrTrimItemNotFound
	}
	if err != nil {
		return RemoveResult{}, err
	}
	if request.ApplicationID > 0 && applicationID != request.ApplicationID {
		return RemoveResult{}, ErrTrimItemNotFound
	}
	actor := "system:permission_trimming"
	if request.ActorID > 0 {
		actor = strconv.FormatInt(request.ActorID, 10)
	}
	if err := t.review.grants(ctx, request.ProjectID, grantID, actor); err != nil {
		return RemoveResult{}, fmt.Errorf("permission trimming: revoke the use grant: %w", err)
	}
	request.ApplicationID = applicationID
	var result RemoveResult
	err = pgx.BeginTxFunc(ctx, t.review.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		at, err := t.recordTrimmed(ctx, tx, request)
		result = RemoveResult{Kind: request.Kind, Key: request.Key, RemovedAt: at}
		return err
	})
	return result, err
}

func (t *PermissionTrimming) removeEgress(ctx context.Context, request RemoveRequest) (RemoveResult, error) {
	level, entry, ok := strings.Cut(request.Key, ":")
	if !ok || entry == "" {
		return RemoveResult{}, ErrTrimItemNotFound
	}
	request.ApplicationID = 0
	var result RemoveResult
	err := pgx.BeginTxFunc(ctx, t.review.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		switch {
		case level == "project" && request.ProjectID > 0:
			if err := removeProjectEgressEntry(ctx, tx, request.ProjectID, entry); err != nil {
				return err
			}
		case level == "platform" && request.Platform:
			request.ProjectID = 0
			if err := removePlatformEgressEntry(ctx, tx, entry, request.ActorID); err != nil {
				return err
			}
		default:
			return ErrTrimItemNotFound
		}
		at, err := t.recordTrimmed(ctx, tx, request)
		result = RemoveResult{Kind: request.Kind, Key: request.Key, RemovedAt: at}
		return err
	})
	return result, err
}

func withoutEntry(entries []string, entry string) ([]string, bool) {
	remaining := make([]string, 0, len(entries))
	found := false
	for _, existing := range entries {
		if existing == entry {
			found = true
			continue
		}
		remaining = append(remaining, existing)
	}
	return remaining, found
}

// removeProjectEgressEntry rewrites the project's worker_egress
// configuration row without the entry: the row the configuration route
// writes, the remaining list validated as that route's normaliser does.
func removeProjectEgressEntry(ctx context.Context, tx pgx.Tx, projectID int64, entry string) error {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return ErrTrimItemNotFound
	}
	var id int64
	var raw []byte
	err = tx.QueryRow(ctx, fmt.Sprintf(`
SELECT id, data FROM %s.configuration WHERE type = 'worker_egress' ORDER BY id LIMIT 1 FOR UPDATE`, schema)).Scan(&id, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrTrimItemNotFound
	}
	if err != nil {
		return err
	}
	data := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &data); err != nil {
			return err
		}
	}
	var entries []string
	if list, ok := data["allowlist"].([]any); ok {
		for _, item := range list {
			if text, ok := item.(string); ok {
				entries = append(entries, text)
			}
		}
	}
	remaining, found := withoutEntry(entries, entry)
	if !found {
		return ErrTrimItemNotFound
	}
	if err := platformconfig.ValidateWorkerEgressEntries(remaining, platformconfig.MaxWorkerEgressProjectEntries); err != nil {
		return fmt.Errorf("%w: %v", ErrTrimItemNotRemovable, err)
	}
	data["allowlist"] = remaining
	encoded, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.configuration SET data = $2::jsonb WHERE id = $1`, schema), id, encoded)
	return err
}

// removePlatformEgressEntry rewrites the platform's worker_egress allowlist
// without the entry, as the Configuration page's save does
// (centry.platform_config upsert with updated_by).
func removePlatformEgressEntry(ctx context.Context, tx pgx.Tx, entry string, actorID int64) error {
	var raw []byte
	err := tx.QueryRow(ctx, `
SELECT value FROM centry.platform_config WHERE section = $1 AND key = $2 FOR UPDATE`,
		platformconfig.SectionWorkerEgress, platformconfig.KeyWorkerEgressAllowlist).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrTrimItemNotFound
	}
	if err != nil {
		return err
	}
	var entries []string
	if err := json.Unmarshal(raw, &entries); err != nil {
		return err
	}
	remaining, found := withoutEntry(entries, entry)
	if !found {
		return ErrTrimItemNotFound
	}
	if err := platformconfig.ValidateWorkerEgressEntries(remaining, platformconfig.MaxWorkerEgressPlatformEntries); err != nil {
		return fmt.Errorf("%w: %v", ErrTrimItemNotRemovable, err)
	}
	encoded, err := json.Marshal(remaining)
	if err != nil {
		return err
	}
	author := "permission_trimming"
	if actorID > 0 {
		author = "user:" + strconv.FormatInt(actorID, 10)
	}
	_, err = tx.Exec(ctx, `
UPDATE centry.platform_config SET value = $3::jsonb, updated_at = (now() AT TIME ZONE 'UTC'), updated_by = $4
WHERE section = $1 AND key = $2`, platformconfig.SectionWorkerEgress, platformconfig.KeyWorkerEgressAllowlist, string(encoded), author)
	return err
}

// recordTrimmed appends permission.trimmed and resolves the candidate, in
// the removal's transaction.
func (t *PermissionTrimming) recordTrimmed(ctx context.Context, tx pgx.Tx, request RemoveRequest) (time.Time, error) {
	var at time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at); err != nil {
		return at, err
	}
	if _, err := tx.Exec(ctx, `
UPDATE elitea_lifecycle.permission_trimming_candidates SET resolved_at = $5
WHERE tenant_project_id = $1 AND application_id = $2 AND item_kind = $3 AND item_key = $4 AND resolved_at IS NULL`,
		request.ProjectID, request.ApplicationID, request.Kind, request.Key, at); err != nil {
		return at, err
	}
	err := t.review.audit.Append(ctx, tx, securityaudit.SecurityEvent{
		Type: securityaudit.TypeCredentialReview, TenantProjectID: request.ProjectID,
		Action: "permission.trimmed", Reasons: []string{"kind:" + request.Kind},
		PrincipalRef: principalRef(request.ActorID, "permission_trimming"),
		ResourceType: "agent." + request.Kind, ResourceID: trimResourceID(request),
	})
	return at, err
}

func trimResourceID(request RemoveRequest) string {
	id := strconv.FormatInt(request.ApplicationID, 10) + ":" + request.Key
	if len(id) > 512 {
		id = id[:512]
	}
	return id
}

// ---------------------------------------------------------------------------
// The weekly routine

// TrimmingSummary is what one pass did.
type TrimmingSummary struct {
	Items      int
	Candidates int
	New        int
	Resolved   int
	Notified   int
	Errors     int
}

// String is the summary for the log.
func (s TrimmingSummary) String() string {
	return fmt.Sprintf("items=%d candidates=%d new=%d resolved=%d notified=%d errors=%d",
		s.Items, s.Candidates, s.New, s.Resolved, s.Notified, s.Errors)
}

// TrimmingPass recomputes the report for every project and the platform,
// stores the candidates, resolves the ones that are used again (or gone),
// and notifies the owners of each agent (the registry's business and
// technical owners, else the agent's owner; the project's owner for a
// project egress entry) once per NEW candidate set. A candidate already
// notified is never notified again while it stays a candidate.
func (t *PermissionTrimming) TrimmingPass(ctx context.Context) (TrimmingSummary, error) {
	var summary TrimmingSummary
	report, err := t.Report(ctx, TrimmingFilter{IncludePlatform: true})
	if err != nil {
		return summary, err
	}
	summary.Items = len(report.Items)
	pool := t.review.pool
	var projects, apps []int64
	var kinds, keys []string
	for _, item := range report.Items {
		if !item.Candidate {
			continue
		}
		summary.Candidates++
		projects = append(projects, item.ProjectID)
		apps = append(apps, item.ApplicationID)
		kinds = append(kinds, item.Kind)
		keys = append(keys, item.Key)
	}
	now := t.review.now()
	// Upsert the candidates seen now; a resolved row that is a candidate
	// again starts over (a new candidate, notified again).
	tag, err := pool.Exec(ctx, `
INSERT INTO elitea_lifecycle.permission_trimming_candidates AS c
    (tenant_project_id, application_id, item_kind, item_key, first_seen_at, last_seen_at)
SELECT u.p, u.a, u.k, u.key, $5, $5
FROM unnest($1::bigint[], $2::bigint[], $3::text[], $4::text[]) AS u(p, a, k, key)
ON CONFLICT (tenant_project_id, application_id, item_kind, item_key) DO UPDATE
SET last_seen_at = EXCLUDED.last_seen_at,
    first_seen_at = CASE WHEN c.resolved_at IS NULL THEN c.first_seen_at ELSE EXCLUDED.first_seen_at END,
    notified_at = CASE WHEN c.resolved_at IS NULL THEN c.notified_at END,
    resolved_at = NULL`, projects, apps, kinds, keys, now)
	if err != nil {
		return summary, fmt.Errorf("permission trimming: store the candidates: %w", err)
	}
	_ = tag
	// Resolve what is no longer a candidate (used again, removed elsewhere).
	tag, err = pool.Exec(ctx, `
UPDATE elitea_lifecycle.permission_trimming_candidates SET resolved_at = $1
WHERE resolved_at IS NULL AND last_seen_at < $1`, now)
	if err != nil {
		return summary, fmt.Errorf("permission trimming: resolve the candidates: %w", err)
	}
	summary.Resolved = int(tag.RowsAffected())

	// Notify per agent (or per project for egress entries) the candidates
	// never notified.
	rows, err := pool.Query(ctx, `
SELECT tenant_project_id, application_id, item_kind, item_key
FROM elitea_lifecycle.permission_trimming_candidates
WHERE resolved_at IS NULL AND notified_at IS NULL
ORDER BY tenant_project_id, application_id, item_kind, item_key`)
	if err != nil {
		return summary, fmt.Errorf("permission trimming: read the new candidates: %w", err)
	}
	type group struct{ project, app int64 }
	pending := map[group][]string{}
	var order []group
	for rows.Next() {
		var g group
		var kind, key string
		if err := rows.Scan(&g.project, &g.app, &kind, &key); err != nil {
			rows.Close()
			return summary, err
		}
		if _, ok := pending[g]; !ok {
			order = append(order, g)
		}
		pending[g] = append(pending[g], kind+" "+key)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return summary, err
	}
	names := map[string]string{}
	for _, item := range report.Items {
		names[strconv.FormatInt(item.ProjectID, 10)+"/"+strconv.FormatInt(item.ApplicationID, 10)] = item.ApplicationName
	}
	for _, g := range order {
		summary.New += len(pending[g])
		recipients, err := t.owners(ctx, g.project, g.app)
		if err != nil {
			summary.Errors++
			continue
		}
		name := names[strconv.FormatInt(g.project, 10)+"/"+strconv.FormatInt(g.app, 10)]
		message := fmt.Sprintf("Agent %q has permissions not used in %d days (%d). Review them under Governance › Permission trimming.",
			name, report.WindowDays, len(pending[g]))
		if g.app == 0 {
			message = fmt.Sprintf("Worker egress allowlist entries not reached in %d days can be removed (%d).",
				report.WindowDays, len(pending[g]))
		}
		sent := false
		for _, recipient := range recipients {
			meta := map[string]any{
				"message": message, "project_id": g.project, "application_id": g.app,
				"items": pending[g], "window_days": report.WindowDays,
			}
			// The key names the candidates themselves: a retried pass does
			// not repeat a notice, a pass with other new candidates does.
			key := fmt.Sprintf("trim-%d-%d-%d-%s", g.project, g.app, recipient, strings.Join(pending[g], "|"))
			if insertReviewNotification(ctx, pool, key, g.project, recipient, PermissionTrimmingNotificationEventType, meta) == nil {
				summary.Notified++
				sent = true
			} else {
				summary.Errors++
			}
		}
		// Mark them notified even with no recipient: the report shows them
		// and a later pass must not notify them as new.
		_ = sent
		if _, err := pool.Exec(ctx, `
UPDATE elitea_lifecycle.permission_trimming_candidates SET notified_at = $3
WHERE tenant_project_id = $1 AND application_id = $2 AND resolved_at IS NULL AND notified_at IS NULL`,
			g.project, g.app, now); err != nil {
			summary.Errors++
		}
	}
	return summary, nil
}

// owners are the agent's registry owners (ADR-0040), else its owner; the
// project's owner for a project-level item; nobody for the platform.
func (t *PermissionTrimming) owners(ctx context.Context, projectID, applicationID int64) ([]int64, error) {
	pool := t.review.pool
	if projectID == 0 {
		return nil, nil
	}
	seen := map[int64]bool{}
	var out []int64
	add := func(id *int64) {
		if id != nil && *id > 0 && !seen[*id] {
			seen[*id] = true
			out = append(out, *id)
		}
	}
	if applicationID > 0 {
		if exists, err := relationExists(ctx, pool, "elitea_lifecycle.agent_registry"); err != nil {
			return nil, err
		} else if exists {
			var business, technical *int64
			err := pool.QueryRow(ctx, `
SELECT business_owner_id, technical_owner_id FROM elitea_lifecycle.agent_registry
WHERE tenant_project_id = $1 AND application_id = $2`, projectID, applicationID).Scan(&business, &technical)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return nil, err
			}
			add(business)
			add(technical)
		}
		if len(out) == 0 {
			schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
			if err != nil {
				return nil, err
			}
			var owner int64
			err = pool.QueryRow(ctx, fmt.Sprintf(`SELECT owner_id FROM %s.applications WHERE id = $1`, schema), applicationID).Scan(&owner)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return nil, err
			}
			add(&owner)
		}
		return out, nil
	}
	var owner int64
	if err := pool.QueryRow(ctx, `SELECT owner_id FROM centry.project WHERE id = $1`, projectID).Scan(&owner); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	add(&owner)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}
