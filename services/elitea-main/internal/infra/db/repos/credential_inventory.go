package repos

// The credential inventory (ADR-0044 decision 5, issue #1501): one admin
// view of every credential an agent can use, its owner, project, scope,
// age, expiry and last use, the agents that use it and whether it is stale;
// the admin revoke of a personal access token or an ADR-0039 use grant; and
// the daily main.credential_staleness pass.
//
// # What is listed, per kind
//
//	pat                   public.auth_core__token rows with a uuid (the
//	                      tokens Settings › Tokens lists, and the system and
//	                      actor tokens minted the same way). created_at is
//	                      token_lifecycle.issued_at (NULL for a token minted
//	                      before shared 0125 or by legacy). A user credential:
//	                      no agent uses it directly.
//	toolkit_secret        a {{secret.NAME}} placeholder in a toolkit's
//	                      settings or in a configuration a toolkit names, per
//	                      project. The vault is one encrypted blob per
//	                      project, so a secret has no row, no creator and no
//	                      created_at: its age is first_seen_at, when the
//	                      staleness routine first listed it. The owner is the
//	                      project's owner.
//	mcp_oauth_token       elitea_auth.mcp_oauth_tokens, unrevoked and unexpired:
//	                      the delegated token of a remote MCP toolkit.
//	provider_oauth_grant  elitea_identity.provider_oauth_grants not revoked:
//	                      a user's consent to an OAuth provider for an agent.
//	credential_reference  elitea_identity.credential_references not retired.
//	credential_grant      elitea_identity.credential_use_grants proposed,
//	                      active or suspended.
//
// No query reads a secret value: no encrypted column, no vault blob and no
// configuration value is selected; a toolkit secret is found by matching
// the placeholder's NAME in the settings text, and only the name leaves.
//
// # Revoking
//
// Only a personal access token and a credential use grant are revoked here
// (from the inventory, or by the routine under stale_action revoke). The
// other kinds are issued and rotated outside the platform (the external
// secret manager, the OAuth provider): they are flagged, never revoked
// (ADR-0044 "Delegated outside the platform").

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialuse"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// Notification event types of credential and access review. The web
// client renders meta.message, so no client change is needed to show them.
const (
	CredentialStaleNotificationEventType    = "credential_stale"
	CredentialRevokedNotificationEventType  = "credential_revoked_stale"
	PermissionTrimmingNotificationEventType = "permission_trimming_candidates"
)

// Errors of the credential review store.
var (
	ErrCredentialNotFound      = errors.New("credential review: the credential does not exist")
	ErrCredentialNotRevocable  = errors.New("credential review: this kind of credential is flagged only, never revoked by the platform")
	ErrCredentialReviewInvalid = errors.New("credential review: invalid request")
)

// InventoryAgent is one agent that uses a credential.
type InventoryAgent struct {
	ProjectID     int64  `json:"project_id"`
	ApplicationID int64  `json:"application_id"`
	Name          string `json:"name"`
}

// InventoryCredential is one row of the inventory. It never carries a
// secret value.
type InventoryCredential struct {
	Kind         credentialuse.Kind `json:"kind"`
	CredentialID string             `json:"credential_id"`
	Name         string             `json:"name"`
	OwnerID      *int64             `json:"owner_id"`
	ProjectID    *int64             `json:"project_id"`
	Scope        string             `json:"scope"`
	State        string             `json:"state"`
	CreatedAt    *time.Time         `json:"created_at"`
	ExpiresAt    *time.Time         `json:"expires_at"`
	LastUsedAt   *time.Time         `json:"last_used_at"`
	FirstSeenAt  *time.Time         `json:"first_seen_at"`
	FlaggedAt    *time.Time         `json:"flagged_at"`
	Agents       []InventoryAgent   `json:"agents"`
	Stale        bool               `json:"stale"`
	Revocable    bool               `json:"revocable"`
}

// referenceTime is what staleness is measured from: the last use, else the
// later of the creation and when the routine first saw it.
//
// Without a recorded use the clock starts at the later of creation and the
// first staleness pass that saw the credential: last use is only recorded
// from migration 0208 on, so a token created a year ago and used every day
// must not read as stale (and be revoked under the revoke policy) on the
// first pass after the upgrade. A credential no pass has seen yet is not
// judged at all.
func (c InventoryCredential) referenceTime() *time.Time {
	switch {
	case c.LastUsedAt != nil:
		return c.LastUsedAt
	case c.FirstSeenAt == nil:
		return nil
	case c.CreatedAt != nil && c.CreatedAt.After(*c.FirstSeenAt):
		return c.CreatedAt
	default:
		return c.FirstSeenAt
	}
}

// Revocable reports whether the platform revokes the kind.
func Revocable(kind credentialuse.Kind) bool {
	return kind == credentialuse.KindPAT || kind == credentialuse.KindCredentialGrant
}

// InventoryFilter narrows the inventory. Zero values are "any".
type InventoryFilter struct {
	Kind      credentialuse.Kind
	ProjectID int64
	// Stale: nil is any, true only stale, false only fresh.
	Stale  *bool
	Limit  int
	Offset int
}

// GrantRevoker revokes an ADR-0039 use grant through the grant service, so
// the grant's own transition rules, epoch bump, security audit event and
// broker cache invalidation apply (credentialgrant.Service.TransitionGrant
// with ActionRevoke).
type GrantRevoker func(ctx context.Context, tenantProjectID int64, grantID, actor string) error

// CredentialReview is the inventory, revoke and staleness store.
type CredentialReview struct {
	pool   *pgxpool.Pool
	audit  securityaudit.Appender
	grants GrantRevoker
	policy func(context.Context) (platformconfig.CredentialReviewPolicy, error)
	now    func() time.Time
}

// NewCredentialReview builds the store. grants may be nil: a use grant is
// then revoked with the grant store directly (its audit event included).
func NewCredentialReview(pool *pgxpool.Pool, audit securityaudit.Appender, grants GrantRevoker) (*CredentialReview, error) {
	if pool == nil {
		return nil, errors.New("credential review: the database pool is required")
	}
	if audit == nil {
		return nil, errors.New("credential review: the security audit appender is required")
	}
	store := &CredentialReview{pool: pool, audit: audit, grants: grants, now: time.Now}
	store.policy = func(ctx context.Context) (platformconfig.CredentialReviewPolicy, error) {
		return platformconfig.LoadCredentialReviewPolicy(ctx, pool)
	}
	if store.grants == nil {
		store.grants = directGrantRevoker(pool, audit)
	}
	return store, nil
}

// WithClock replaces the clock (tests).
func (s *CredentialReview) WithClock(now func() time.Time) *CredentialReview {
	s.now = now
	return s
}

// Policy reads the credential review section.
func (s *CredentialReview) Policy(ctx context.Context) (platformconfig.CredentialReviewPolicy, error) {
	return s.policy(ctx)
}

// WriteLastUse is the recorder's store (credentialuse.Store): one batched
// upsert. The WHERE keeps a stored value less than an hour older than the
// new one, so another replica's write inside the hour is not repeated.
func (s *CredentialReview) WriteLastUse(ctx context.Context, uses []credentialuse.Use) (int64, error) {
	return WriteCredentialLastUse(ctx, s.pool, uses)
}

// WriteCredentialLastUse is WriteLastUse over a pool.
func WriteCredentialLastUse(ctx context.Context, pool *pgxpool.Pool, uses []credentialuse.Use) (int64, error) {
	if len(uses) == 0 {
		return 0, nil
	}
	kinds := make([]string, 0, len(uses))
	ids := make([]string, 0, len(uses))
	projects := make([]*int64, 0, len(uses))
	ats := make([]time.Time, 0, len(uses))
	for _, use := range uses {
		if !use.Kind.Valid() || use.CredentialID == "" {
			continue
		}
		kinds = append(kinds, string(use.Kind))
		ids = append(ids, use.CredentialID)
		if use.ProjectID > 0 {
			project := use.ProjectID
			projects = append(projects, &project)
		} else {
			projects = append(projects, nil)
		}
		ats = append(ats, use.At)
	}
	tag, err := pool.Exec(ctx, `
INSERT INTO elitea_identity.credential_last_use AS stored (kind, credential_id, project_id, last_used_at)
SELECT DISTINCT ON (u.kind, u.credential_id) u.kind, u.credential_id, u.project_id, u.at
FROM unnest($1::text[], $2::text[], $3::bigint[], $4::timestamptz[]) AS u(kind, credential_id, project_id, at)
ORDER BY u.kind, u.credential_id, u.at DESC
ON CONFLICT (kind, credential_id) DO UPDATE
SET last_used_at = EXCLUDED.last_used_at,
    project_id = COALESCE(EXCLUDED.project_id, stored.project_id)
WHERE stored.last_used_at < EXCLUDED.last_used_at - interval '1 hour'`, kinds, ids, projects, ats)
	if err != nil {
		return 0, fmt.Errorf("credential review: write last use: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ---------------------------------------------------------------------------
// The inventory

// Inventory lists the credentials matching the filter, ordered by kind,
// project and id, with the total before paging.
func (s *CredentialReview) Inventory(ctx context.Context, filter InventoryFilter) ([]InventoryCredential, int, error) {
	if filter.Kind != "" && !filter.Kind.Valid() {
		return nil, 0, ErrCredentialReviewInvalid
	}
	policy, err := s.policy(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("credential review: read the policy: %w", err)
	}
	all, err := s.collect(ctx, filter.Kind, filter.ProjectID, policy)
	if err != nil {
		return nil, 0, err
	}
	matched := all[:0]
	for _, credential := range all {
		if filter.Stale != nil && credential.Stale != *filter.Stale {
			continue
		}
		matched = append(matched, credential)
	}
	total := len(matched)
	if filter.Offset > 0 {
		if filter.Offset >= len(matched) {
			return []InventoryCredential{}, total, nil
		}
		matched = matched[filter.Offset:]
	}
	if filter.Limit > 0 && len(matched) > filter.Limit {
		matched = matched[:filter.Limit]
	}
	return matched, total, nil
}

// collect reads every credential of the kind (all when empty) in the
// project (all when 0) with its last use, review state and agents, and
// marks staleness under the policy.
func (s *CredentialReview) collect(ctx context.Context, kind credentialuse.Kind, projectID int64, policy platformconfig.CredentialReviewPolicy) ([]InventoryCredential, error) {
	want := func(k credentialuse.Kind) bool { return kind == "" || kind == k }
	var out []InventoryCredential
	var tenants *tenantIndex
	tenantsFor := func() (*tenantIndex, error) {
		if tenants != nil {
			return tenants, nil
		}
		index, err := s.loadTenants(ctx, projectID)
		if err != nil {
			return nil, err
		}
		tenants = index
		return tenants, nil
	}
	if want(credentialuse.KindPAT) {
		rows, err := s.collectPATs(ctx, projectID)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	if want(credentialuse.KindToolkitSecret) {
		index, err := tenantsFor()
		if err != nil {
			return nil, err
		}
		out = append(out, index.secrets...)
	}
	if want(credentialuse.KindMCPOAuthToken) {
		index, err := tenantsFor()
		if err != nil {
			return nil, err
		}
		rows, err := s.collectMCPTokens(ctx, projectID, index)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	if want(credentialuse.KindProviderOAuthGrant) {
		rows, err := s.collectProviderGrants(ctx, projectID)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	if want(credentialuse.KindCredentialReference) {
		rows, err := s.collectReferences(ctx, projectID)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	if want(credentialuse.KindCredentialGrant) {
		rows, err := s.collectGrants(ctx, projectID)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	if err := s.decorate(ctx, out, policy); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return kindOrder(out[i].Kind) < kindOrder(out[j].Kind)
		}
		pi, pj := derefInt(out[i].ProjectID), derefInt(out[j].ProjectID)
		if pi != pj {
			return pi < pj
		}
		return out[i].CredentialID < out[j].CredentialID
	})
	return out, nil
}

func kindOrder(kind credentialuse.Kind) int {
	for i, known := range credentialuse.Kinds {
		if known == kind {
			return i
		}
	}
	return len(credentialuse.Kinds)
}

func derefInt(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func int64Ptr(value int64) *int64 {
	if value <= 0 {
		return nil
	}
	return &value
}

// decorate joins last use, review state, agent names and staleness.
func (s *CredentialReview) decorate(ctx context.Context, rows []InventoryCredential, policy platformconfig.CredentialReviewPolicy) error {
	if len(rows) == 0 {
		return nil
	}
	type stateKey struct {
		kind string
		id   string
	}
	lastUse := map[stateKey]time.Time{}
	if exists, err := relationExists(ctx, s.pool, "elitea_identity.credential_last_use"); err != nil {
		return err
	} else if exists {
		result, err := s.pool.Query(ctx, `SELECT kind, credential_id, last_used_at FROM elitea_identity.credential_last_use`)
		if err != nil {
			return fmt.Errorf("credential review: read last use: %w", err)
		}
		for result.Next() {
			var key stateKey
			var at time.Time
			if err := result.Scan(&key.kind, &key.id, &at); err != nil {
				result.Close()
				return err
			}
			lastUse[key] = at
		}
		result.Close()
		if err := result.Err(); err != nil {
			return err
		}
	}
	type reviewState struct {
		firstSeen time.Time
		flagged   *time.Time
	}
	states := map[stateKey]reviewState{}
	if exists, err := relationExists(ctx, s.pool, "elitea_identity.credential_review_state"); err != nil {
		return err
	} else if exists {
		result, err := s.pool.Query(ctx, `
SELECT kind, credential_id, first_seen_at, flagged_at FROM elitea_identity.credential_review_state
WHERE revoked_at IS NULL`)
		if err != nil {
			return fmt.Errorf("credential review: read the review state: %w", err)
		}
		for result.Next() {
			var key stateKey
			var state reviewState
			if err := result.Scan(&key.kind, &key.id, &state.firstSeen, &state.flagged); err != nil {
				result.Close()
				return err
			}
			states[key] = state
		}
		result.Close()
		if err := result.Err(); err != nil {
			return err
		}
	}
	cutoff := s.now().Add(-time.Duration(policy.StaleAfterDays) * 24 * time.Hour)
	for i := range rows {
		row := &rows[i]
		key := stateKey{kind: string(row.Kind), id: row.CredentialID}
		if at, ok := lastUse[key]; ok {
			at := at
			row.LastUsedAt = &at
		}
		if state, ok := states[key]; ok {
			firstSeen := state.firstSeen
			row.FirstSeenAt = &firstSeen
			row.FlaggedAt = state.flagged
		}
		if row.Agents == nil {
			row.Agents = []InventoryAgent{}
		}
		row.Revocable = Revocable(row.Kind)
		if reference := row.referenceTime(); reference != nil {
			row.Stale = reference.Before(cutoff)
		}
	}
	return nil
}

func (s *CredentialReview) collectPATs(ctx context.Context, projectID int64) ([]InventoryCredential, error) {
	if exists, err := relationExists(ctx, s.pool, "public.auth_core__token"); err != nil || !exists {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
SELECT token.id,
       COALESCE(token.name, '')::text,
       token.user_id,
       binding.project_id,
       lifecycle.issued_at,
       token.expires AT TIME ZONE 'UTC'
FROM public.auth_core__token AS token
LEFT JOIN elitea_identity.token_project_binding AS binding ON binding.token_id = token.id
LEFT JOIN elitea_identity.token_lifecycle AS lifecycle ON lifecycle.token_id = token.id
WHERE token.uuid IS NOT NULL
  AND token.user_id IS NOT NULL
  AND ($1::bigint = 0 OR binding.project_id = $1::bigint)
ORDER BY token.id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("credential review: read personal access tokens: %w", err)
	}
	defer rows.Close()
	var out []InventoryCredential
	for rows.Next() {
		var id, userID int32
		var name string
		var project *int32
		var issued, expires *time.Time
		if err := rows.Scan(&id, &name, &userID, &project, &issued, &expires); err != nil {
			return nil, err
		}
		credential := InventoryCredential{
			Kind: credentialuse.KindPAT, CredentialID: strconv.FormatInt(int64(id), 10), Name: name,
			OwnerID: int64Ptr(int64(userID)), CreatedAt: issued, ExpiresAt: expires, State: "active",
			Scope: "all projects of the owner",
		}
		if project != nil && *project > 0 {
			credential.ProjectID = int64Ptr(int64(*project))
			credential.Scope = "project " + strconv.FormatInt(int64(*project), 10)
		}
		if expires != nil && !expires.After(s.now()) {
			credential.State = "expired"
		}
		out = append(out, credential)
	}
	return out, rows.Err()
}

// tenantIndex is what the per-project tenant tables answer: the toolkit
// secrets, and per toolkit the agents attached to it.
type tenantIndex struct {
	secrets []InventoryCredential
	// toolkitAgents[project][toolkit] = agents
	toolkitAgents map[int64]map[int64][]InventoryAgent
	agentNames    map[int64]map[int64]string
	owners        map[int64]int64
}

var secretPlaceholder = regexp.MustCompile(`\{\{secret\.([A-Za-z0-9_]+)\}\}`)

// loadTenants reads each project's toolkits, their agents and the secret
// names their settings and named configurations hold. Only names leave the
// database query: settings text is scanned for the placeholder pattern in
// SQL, never returned.
func (s *CredentialReview) loadTenants(ctx context.Context, projectID int64) (*tenantIndex, error) {
	index := &tenantIndex{
		toolkitAgents: map[int64]map[int64][]InventoryAgent{},
		agentNames:    map[int64]map[int64]string{},
		owners:        map[int64]int64{},
	}
	projects, err := s.pool.Query(ctx, `
SELECT p.id, p.owner_id FROM centry.project AS p
WHERE ($1::bigint = 0 OR p.id = $1::bigint)
  AND to_regclass(format('%I.elitea_tools', 'p_' || p.id)) IS NOT NULL
  AND to_regclass(format('%I.entity_tool_mapping', 'p_' || p.id)) IS NOT NULL
  AND to_regclass(format('%I.applications', 'p_' || p.id)) IS NOT NULL
ORDER BY p.id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("credential review: list projects: %w", err)
	}
	var ids []int64
	for projects.Next() {
		var id int32
		var owner int32
		if err := projects.Scan(&id, &owner); err != nil {
			projects.Close()
			return nil, err
		}
		ids = append(ids, int64(id))
		index.owners[int64(id)] = int64(owner)
	}
	projects.Close()
	if err := projects.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if err := s.loadTenant(ctx, id, index); err != nil {
			return nil, err
		}
	}
	return index, nil
}

func (s *CredentialReview) loadTenant(ctx context.Context, projectID int64, index *tenantIndex) error {
	schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
	if err != nil {
		return err
	}
	agents := map[int64][]InventoryAgent{}
	names := map[int64]string{}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT DISTINCT m.tool_id, a.id, a.name
FROM %[1]s.entity_tool_mapping AS m
JOIN %[1]s.applications AS a ON a.id = m.entity_id
WHERE m.entity_type IN ('agent', 'application')
ORDER BY m.tool_id, a.id`, schema))
	if err != nil {
		return fmt.Errorf("credential review: read the agents of project %d: %w", projectID, err)
	}
	for rows.Next() {
		var toolID, appID int64
		var name string
		if err := rows.Scan(&toolID, &appID, &name); err != nil {
			rows.Close()
			return err
		}
		agents[toolID] = append(agents[toolID], InventoryAgent{ProjectID: projectID, ApplicationID: appID, Name: name})
		names[appID] = name
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	index.toolkitAgents[projectID] = agents
	index.agentNames[projectID] = names

	// The secret names a toolkit's settings hold, and those of the
	// configurations it names by title. The settings text is matched in
	// SQL; only toolkit ids, names and secret names are returned.
	configurationJoin := ""
	if exists, err := relationExists(ctx, s.pool, strings.Trim(schema, `"`)+".configuration"); err != nil {
		return err
	} else if exists {
		configurationJoin = fmt.Sprintf(`
UNION
SELECT t.id, t.name, (regexp_matches(c.data::text, '\{\{secret\.([A-Za-z0-9_]+)\}\}', 'g'))[1]
FROM %[1]s.elitea_tools AS t
JOIN %[1]s.configuration AS c
  ON strpos(t.settings::text, '"elitea_title": ' || to_jsonb(c.elitea_title)::text) > 0`, schema)
	}
	secrets, err := s.pool.Query(ctx, fmt.Sprintf(`
SELECT toolkit_id, toolkit_name, secret_name FROM (
SELECT t.id AS toolkit_id, t.name AS toolkit_name,
       (regexp_matches(t.settings::text, '\{\{secret\.([A-Za-z0-9_]+)\}\}', 'g'))[1] AS secret_name
FROM %[1]s.elitea_tools AS t%[2]s
) AS found
ORDER BY secret_name, toolkit_id`, schema, configurationJoin))
	if err != nil {
		return fmt.Errorf("credential review: read the toolkit secrets of project %d: %w", projectID, err)
	}
	type secretUse struct {
		toolkits []string
		agents   map[int64]InventoryAgent
	}
	found := map[string]*secretUse{}
	var order []string
	for secrets.Next() {
		var toolkitID int64
		var toolkitName, secretName string
		if err := secrets.Scan(&toolkitID, &toolkitName, &secretName); err != nil {
			secrets.Close()
			return err
		}
		if !secretPlaceholder.MatchString("{{secret." + secretName + "}}") {
			continue
		}
		use, ok := found[secretName]
		if !ok {
			use = &secretUse{agents: map[int64]InventoryAgent{}}
			found[secretName] = use
			order = append(order, secretName)
		}
		use.toolkits = appendUnique(use.toolkits, toolkitName)
		for _, agent := range agents[toolkitID] {
			use.agents[agent.ApplicationID] = agent
		}
	}
	secrets.Close()
	if err := secrets.Err(); err != nil {
		return err
	}
	owner := index.owners[projectID]
	for _, name := range order {
		use := found[name]
		credential := InventoryCredential{
			Kind:         credentialuse.KindToolkitSecret,
			CredentialID: strconv.FormatInt(projectID, 10) + ":" + name,
			Name:         name,
			OwnerID:      int64Ptr(owner),
			ProjectID:    int64Ptr(projectID),
			Scope:        "toolkits: " + strings.Join(use.toolkits, ", "),
			State:        "active",
			Agents:       sortedAgents(use.agents),
		}
		index.secrets = append(index.secrets, credential)
	}
	return nil
}

func appendUnique(list []string, value string) []string {
	for _, existing := range list {
		if existing == value {
			return list
		}
	}
	return append(list, value)
}

func sortedAgents(agents map[int64]InventoryAgent) []InventoryAgent {
	out := make([]InventoryAgent, 0, len(agents))
	for _, agent := range agents {
		out = append(out, agent)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProjectID != out[j].ProjectID {
			return out[i].ProjectID < out[j].ProjectID
		}
		return out[i].ApplicationID < out[j].ApplicationID
	})
	return out
}

func (s *CredentialReview) collectMCPTokens(ctx context.Context, projectID int64, index *tenantIndex) ([]InventoryCredential, error) {
	if exists, err := relationExists(ctx, s.pool, "elitea_auth.mcp_oauth_tokens"); err != nil || !exists {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
SELECT id, project_id, actor_id, toolkit_id, resource, expires_at
FROM elitea_auth.mcp_oauth_tokens
WHERE revoked_at IS NULL AND expires_at > now()
  AND ($1::bigint = 0 OR project_id = $1::bigint)
ORDER BY project_id, id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("credential review: read MCP OAuth tokens: %w", err)
	}
	defer rows.Close()
	var out []InventoryCredential
	for rows.Next() {
		var id, resource string
		var project, actor int32
		var toolkit int64
		var expires time.Time
		if err := rows.Scan(&id, &project, &actor, &toolkit, &resource, &expires); err != nil {
			return nil, err
		}
		expiresAt := expires
		credential := InventoryCredential{
			Kind: credentialuse.KindMCPOAuthToken, CredentialID: id, Name: resource,
			OwnerID: int64Ptr(int64(actor)), ProjectID: int64Ptr(int64(project)),
			Scope: "toolkit " + strconv.FormatInt(toolkit, 10) + ": " + resource, State: "active",
			ExpiresAt: &expiresAt,
		}
		if index != nil {
			credential.Agents = append([]InventoryAgent(nil), index.toolkitAgents[int64(project)][toolkit]...)
		}
		out = append(out, credential)
	}
	return out, rows.Err()
}

// bindingAgents names the agents behind agent identity bindings, by
// project and application id, from the tenant applications table.
func (s *CredentialReview) agentName(ctx context.Context, cache map[int64]map[int64]string, projectID, applicationID int64) string {
	names, ok := cache[projectID]
	if !ok {
		names = map[int64]string{}
		cache[projectID] = names
		schema, err := tenantSchema(strconv.FormatInt(projectID, 10))
		if err == nil {
			if exists, err := relationExists(ctx, s.pool, strings.Trim(schema, `"`)+".applications"); err == nil && exists {
				rows, err := s.pool.Query(ctx, fmt.Sprintf(`SELECT id, name FROM %s.applications`, schema))
				if err == nil {
					for rows.Next() {
						var id int64
						var name string
						if rows.Scan(&id, &name) == nil {
							names[id] = name
						}
					}
					rows.Close()
				}
			}
		}
	}
	return names[applicationID]
}

func (s *CredentialReview) collectProviderGrants(ctx context.Context, projectID int64) ([]InventoryCredential, error) {
	if exists, err := relationExists(ctx, s.pool, "elitea_identity.provider_oauth_grants"); err != nil || !exists {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
SELECT g.id::text, g.tenant_project_id, g.subject_user_id, g.provider_type, g.granted_scopes, g.state,
       g.created_at, g.access_token_expires_at, b.application_id
FROM elitea_identity.provider_oauth_grants AS g
LEFT JOIN elitea_identity.agent_identity_bindings AS b ON b.id = g.agent_binding_id
WHERE g.state <> 'revoked'
  AND ($1::bigint = 0 OR g.tenant_project_id = $1::bigint)
ORDER BY g.tenant_project_id, g.id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("credential review: read provider OAuth grants: %w", err)
	}
	type row struct {
		credential InventoryCredential
		app        *int64
	}
	var read []row
	for rows.Next() {
		var id, provider, state string
		var project, subject int64
		var scopes []string
		var created time.Time
		var expires *time.Time
		var app *int64
		if err := rows.Scan(&id, &project, &subject, &provider, &scopes, &state, &created, &expires, &app); err != nil {
			rows.Close()
			return nil, err
		}
		createdAt := created
		read = append(read, row{credential: InventoryCredential{
			Kind: credentialuse.KindProviderOAuthGrant, CredentialID: id, Name: provider,
			OwnerID: int64Ptr(subject), ProjectID: int64Ptr(project),
			Scope: provider + scopeSuffix(scopes), State: state, CreatedAt: &createdAt, ExpiresAt: expires,
		}, app: app})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	names := map[int64]map[int64]string{}
	out := make([]InventoryCredential, 0, len(read))
	for _, r := range read {
		if r.app != nil {
			project := derefInt(r.credential.ProjectID)
			r.credential.Agents = []InventoryAgent{{ProjectID: project, ApplicationID: *r.app, Name: s.agentName(ctx, names, project, *r.app)}}
		}
		out = append(out, r.credential)
	}
	return out, nil
}

func scopeSuffix(scopes []string) string {
	if len(scopes) == 0 {
		return ""
	}
	return ": " + strings.Join(scopes, " ")
}

func (s *CredentialReview) collectReferences(ctx context.Context, projectID int64) ([]InventoryCredential, error) {
	if exists, err := relationExists(ctx, s.pool, "elitea_identity.credential_references"); err != nil || !exists {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
SELECT r.id::text, r.tenant_project_id, r.staged_by, r.class, r.provider_type, r.state, r.created_at,
       COALESCE((
           SELECT jsonb_agg(DISTINCT jsonb_build_object('project_id', b.tenant_project_id, 'application_id', b.application_id))
           FROM elitea_identity.credential_use_grants AS g
           JOIN elitea_identity.agent_identity_bindings AS b ON b.id = g.agent_binding_id
           WHERE g.credential_reference_id = r.id AND g.state IN ('proposed', 'active', 'suspended')
       ), '[]'::jsonb)
FROM elitea_identity.credential_references AS r
WHERE r.state <> 'retired'
  AND ($1::bigint = 0 OR r.tenant_project_id = $1::bigint)
ORDER BY r.tenant_project_id, r.id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("credential review: read credential references: %w", err)
	}
	type row struct {
		credential InventoryCredential
		agents     []byte
	}
	var read []row
	for rows.Next() {
		var id, class, provider, state string
		var project int64
		var staged *int64
		var created time.Time
		var agents []byte
		if err := rows.Scan(&id, &project, &staged, &class, &provider, &state, &created, &agents); err != nil {
			rows.Close()
			return nil, err
		}
		createdAt := created
		credential := InventoryCredential{
			Kind: credentialuse.KindCredentialReference, CredentialID: id, Name: provider,
			ProjectID: int64Ptr(project), Scope: class + " " + provider, State: state, CreatedAt: &createdAt,
		}
		if staged != nil {
			credential.OwnerID = int64Ptr(*staged)
		}
		read = append(read, row{credential: credential, agents: agents})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	names := map[int64]map[int64]string{}
	out := make([]InventoryCredential, 0, len(read))
	for _, r := range read {
		var refs []InventoryAgent
		if err := json.Unmarshal(r.agents, &refs); err != nil {
			return nil, err
		}
		for i := range refs {
			refs[i].Name = s.agentName(ctx, names, refs[i].ProjectID, refs[i].ApplicationID)
		}
		r.credential.Agents = refs
		out = append(out, r.credential)
	}
	return out, nil
}

func (s *CredentialReview) collectGrants(ctx context.Context, projectID int64) ([]InventoryCredential, error) {
	if exists, err := relationExists(ctx, s.pool, "elitea_identity.credential_use_grants"); err != nil || !exists {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
SELECT g.id::text, g.tenant_project_id, g.agent_binding_id::text, g.toolkit_id, g.credential_reference_id::text,
       g.operation_scopes, g.state, g.expires_at, g.proposed_by, g.created_at, b.application_id
FROM elitea_identity.credential_use_grants AS g
LEFT JOIN elitea_identity.agent_identity_bindings AS b ON b.id = g.agent_binding_id
WHERE g.state IN ('proposed', 'active', 'suspended')
  AND ($1::bigint = 0 OR g.tenant_project_id = $1::bigint)
ORDER BY g.tenant_project_id, g.id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("credential review: read credential use grants: %w", err)
	}
	type row struct {
		credential InventoryCredential
		app        *int64
	}
	var read []row
	for rows.Next() {
		var id, binding, reference, state, proposedBy string
		var project, toolkit int64
		var scopes []string
		var expires *time.Time
		var created time.Time
		var app *int64
		if err := rows.Scan(&id, &project, &binding, &toolkit, &reference, &scopes, &state, &expires, &proposedBy, &created, &app); err != nil {
			rows.Close()
			return nil, err
		}
		createdAt := created
		credential := InventoryCredential{
			// The id is the live-grant key the broker records use under;
			// the grant row id is the Name, which the revoke reads back.
			Kind: credentialuse.KindCredentialGrant, CredentialID: grantUseKey(binding, toolkit, reference),
			Name: id, ProjectID: int64Ptr(project), State: state, ExpiresAt: expires, CreatedAt: &createdAt,
			Scope: "toolkit " + strconv.FormatInt(toolkit, 10) + ", reference " + reference + scopeSuffix(scopes),
		}
		if owner, err := strconv.ParseInt(proposedBy, 10, 64); err == nil {
			credential.OwnerID = int64Ptr(owner)
		}
		read = append(read, row{credential: credential, app: app})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	names := map[int64]map[int64]string{}
	out := make([]InventoryCredential, 0, len(read))
	for _, r := range read {
		if r.app != nil {
			project := derefInt(r.credential.ProjectID)
			r.credential.Agents = []InventoryAgent{{ProjectID: project, ApplicationID: *r.app, Name: s.agentName(ctx, names, project, *r.app)}}
		}
		out = append(out, r.credential)
	}
	return out, nil
}

// grantUseKey mirrors credentialbroker.CredentialGrantUseID (repos may not
// import the broker).
func grantUseKey(agentBindingID string, toolkitID int64, referenceID string) string {
	return agentBindingID + "/" + strconv.FormatInt(toolkitID, 10) + "/" + referenceID
}

func relationExists(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, name string) (bool, error) {
	var exists bool
	if err := q.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
		return false, fmt.Errorf("credential review: look up %s: %w", name, err)
	}
	return exists, nil
}

// ---------------------------------------------------------------------------
// Revoking

// RevokeRequest is one revoke from the inventory or the routine.
type RevokeRequest struct {
	Kind         credentialuse.Kind
	CredentialID string
	// ActorID is the administrator; 0 is the staleness routine.
	ActorID int64
	Reason  string
}

// RevokeResult is what a revoke did.
type RevokeResult struct {
	Kind         credentialuse.Kind `json:"kind"`
	CredentialID string             `json:"credential_id"`
	ProjectID    *int64             `json:"project_id"`
	OwnerID      *int64             `json:"owner_id"`
	RevokedAt    time.Time          `json:"revoked_at"`
}

// Revoke revokes a personal access token (deleted with its project
// binding, as the owner's own delete does) or a credential use grant
// (through the grant service). It records revoked_at in the review state
// and appends credential.revoked to the security audit chain.
func (s *CredentialReview) Revoke(ctx context.Context, request RevokeRequest) (RevokeResult, error) {
	if len(request.Reason) > 500 {
		return RevokeResult{}, ErrCredentialReviewInvalid
	}
	switch request.Kind {
	case credentialuse.KindPAT:
		return s.revokePAT(ctx, request)
	case credentialuse.KindCredentialGrant:
		return s.revokeGrant(ctx, request)
	default:
		if !request.Kind.Valid() {
			return RevokeResult{}, ErrCredentialReviewInvalid
		}
		return RevokeResult{}, ErrCredentialNotRevocable
	}
}

func principalRef(actorID int64, system string) string {
	if actorID > 0 {
		return "user:" + strconv.FormatInt(actorID, 10)
	}
	return "system:" + system
}

func reviewReasons(request RevokeRequest) []string {
	reasons := []string{"kind:" + string(request.Kind)}
	if request.ActorID == 0 {
		reasons = append(reasons, "stale")
	}
	return reasons
}

func (s *CredentialReview) revokePAT(ctx context.Context, request RevokeRequest) (RevokeResult, error) {
	id, err := strconv.ParseInt(request.CredentialID, 10, 32)
	if err != nil || id <= 0 {
		return RevokeResult{}, ErrCredentialNotFound
	}
	var result RevokeResult
	err = pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		var owner *int32
		var project *int32
		err := tx.QueryRow(ctx, `
SELECT token.user_id, binding.project_id
FROM public.auth_core__token AS token
LEFT JOIN elitea_identity.token_project_binding AS binding ON binding.token_id = token.id
WHERE token.id = $1 AND token.uuid IS NOT NULL
FOR UPDATE OF token`, int32(id)).Scan(&owner, &project)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCredentialNotFound
		}
		if err != nil {
			return err
		}
		// The owner's own delete (auth tokens.go DeleteOwned): the binding
		// explicitly, then the token, in one transaction.
		queries := sqlcgen.New(tx)
		if err := queries.DeleteTokenProjectBinding(ctx, int32(id)); err != nil {
			return err
		}
		deleted, err := queries.DeletePATByID(ctx, int32(id))
		if err != nil {
			return err
		}
		if deleted != 1 {
			return ErrCredentialNotFound
		}
		result = RevokeResult{Kind: credentialuse.KindPAT, CredentialID: request.CredentialID}
		if owner != nil {
			result.OwnerID = int64Ptr(int64(*owner))
		}
		if project != nil {
			result.ProjectID = int64Ptr(int64(*project))
		}
		return s.recordRevoked(ctx, tx, request, &result)
	})
	if err != nil {
		return RevokeResult{}, err
	}
	return result, nil
}

func (s *CredentialReview) revokeGrant(ctx context.Context, request RevokeRequest) (RevokeResult, error) {
	parts := strings.Split(request.CredentialID, "/")
	if len(parts) != 3 {
		return RevokeResult{}, ErrCredentialNotFound
	}
	var grantID, proposedBy string
	var project int64
	err := s.pool.QueryRow(ctx, `
SELECT id::text, tenant_project_id, proposed_by FROM elitea_identity.credential_use_grants
WHERE agent_binding_id::text = $1 AND toolkit_id::text = $2 AND credential_reference_id::text = $3
  AND state IN ('proposed', 'active', 'suspended')`, parts[0], parts[1], parts[2]).Scan(&grantID, &project, &proposedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return RevokeResult{}, ErrCredentialNotFound
	}
	if err != nil {
		return RevokeResult{}, fmt.Errorf("credential review: read the use grant: %w", err)
	}
	// The grant service's actor is the plain user id, as its own routes
	// pass it; the routine is "system:credential_staleness".
	grantActor := "system:credential_staleness"
	if request.ActorID > 0 {
		grantActor = strconv.FormatInt(request.ActorID, 10)
	}
	if err := s.grants(ctx, project, grantID, grantActor); err != nil {
		return RevokeResult{}, fmt.Errorf("credential review: revoke the use grant: %w", err)
	}
	result := RevokeResult{Kind: credentialuse.KindCredentialGrant, CredentialID: request.CredentialID, ProjectID: int64Ptr(project)}
	if owner, err := strconv.ParseInt(proposedBy, 10, 64); err == nil {
		result.OwnerID = int64Ptr(owner)
	}
	err = pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		return s.recordRevoked(ctx, tx, request, &result)
	})
	if err != nil {
		return RevokeResult{}, err
	}
	return result, nil
}

// recordRevoked stamps the review state and appends the audit event in the
// revoke's transaction.
func (s *CredentialReview) recordRevoked(ctx context.Context, tx pgx.Tx, request RevokeRequest, result *RevokeResult) error {
	var revokedBy *int64
	if request.ActorID > 0 {
		revokedBy = &request.ActorID
	}
	if err := tx.QueryRow(ctx, `
INSERT INTO elitea_identity.credential_review_state AS state
    (kind, credential_id, project_id, owner_id, revoked_at, revoked_by, revoke_reason)
VALUES ($1, $2, $3, $4, clock_timestamp(), $5, $6)
ON CONFLICT (kind, credential_id) DO UPDATE
SET revoked_at = clock_timestamp(), revoked_by = EXCLUDED.revoked_by, revoke_reason = EXCLUDED.revoke_reason,
    project_id = COALESCE(EXCLUDED.project_id, state.project_id), owner_id = COALESCE(EXCLUDED.owner_id, state.owner_id)
RETURNING revoked_at`, string(request.Kind), request.CredentialID, result.ProjectID, result.OwnerID,
		revokedBy, request.Reason).Scan(&result.RevokedAt); err != nil {
		return fmt.Errorf("credential review: record the revoke: %w", err)
	}
	event := securityaudit.SecurityEvent{
		Type: securityaudit.TypeCredentialReview, TenantProjectID: derefInt(result.ProjectID),
		Action: "credential.revoked", Reasons: reviewReasons(request),
		PrincipalRef: principalRef(request.ActorID, "credential_staleness"),
		ResourceType: "credential." + string(request.Kind), ResourceID: request.CredentialID,
	}
	if err := s.audit.Append(ctx, tx, event); err != nil {
		return fmt.Errorf("credential review: append the audit event: %w", err)
	}
	return nil
}

// directGrantRevoker revokes a use grant with the grant store and the
// domain transition rules when no service was composed (tests, a process
// without credential governance): CredentialGrants.TransitionGrant writes
// the grant's own security audit event in its transaction.
func directGrantRevoker(pool *pgxpool.Pool, audit securityaudit.Appender) GrantRevoker {
	store := NewCredentialGrants(pool, audit)
	return func(ctx context.Context, tenantProjectID int64, grantID, actor string) error {
		_, err := store.TransitionGrant(ctx, tenantProjectID, grantID, revokeTransition(actor, time.Now().UTC()))
		return err
	}
}

// ---------------------------------------------------------------------------
// The staleness routine

// StalenessSummary is what one pass did.
type StalenessSummary struct {
	Credentials int
	Stale       int
	Flagged     int
	Revoked     int
	Unflagged   int
	Notified    int
	Errors      int
}

// String is the summary for the log.
func (s StalenessSummary) String() string {
	return fmt.Sprintf("credentials=%d stale=%d flagged=%d revoked=%d unflagged=%d notified=%d errors=%d",
		s.Credentials, s.Stale, s.Flagged, s.Revoked, s.Unflagged, s.Notified, s.Errors)
}

// StalenessPass records every credential it sees (first_seen_at), flags
// the stale ones and notifies their owners once per flag, revokes a stale
// personal access token or use grant under stale_action revoke (audited,
// the owner notified), and clears the flag of a credential used again.
func (s *CredentialReview) StalenessPass(ctx context.Context) (StalenessSummary, error) {
	var summary StalenessSummary
	policy, err := s.policy(ctx)
	if err != nil {
		return summary, fmt.Errorf("credential review: read the policy: %w", err)
	}
	all, err := s.collect(ctx, "", 0, policy)
	if err != nil {
		return summary, err
	}
	summary.Credentials = len(all)
	if err := s.recordSeen(ctx, all); err != nil {
		return summary, err
	}
	// first_seen_at may have been set just now; read the state again so a
	// credential with no other age is measured from it.
	if err := s.decorate(ctx, all, policy); err != nil {
		return summary, err
	}
	for _, credential := range all {
		if !credential.Stale {
			if credential.FlaggedAt != nil {
				if err := s.unflag(ctx, credential); err != nil {
					summary.Errors++
					continue
				}
				summary.Unflagged++
			}
			continue
		}
		summary.Stale++
		if policy.StaleAction == platformconfig.StaleActionRevoke && credential.Revocable {
			result, err := s.Revoke(ctx, RevokeRequest{Kind: credential.Kind, CredentialID: credential.CredentialID,
				Reason: fmt.Sprintf("unused for %d days", policy.StaleAfterDays)})
			if err != nil {
				summary.Errors++
				continue
			}
			summary.Revoked++
			if s.notifyCredential(ctx, credential, result.RevokedAt, CredentialRevokedNotificationEventType,
				fmt.Sprintf("Your %s %q was not used for %d days and was revoked.", kindLabel(credential.Kind),
					displayName(credential), policy.StaleAfterDays)) {
				summary.Notified++
			}
			continue
		}
		if credential.FlaggedAt != nil {
			continue
		}
		flaggedAt, err := s.flag(ctx, credential, policy)
		if err != nil {
			summary.Errors++
			continue
		}
		summary.Flagged++
		if s.notifyCredential(ctx, credential, flaggedAt, CredentialStaleNotificationEventType,
			fmt.Sprintf("Your %s %q was not used for %d days. Revoke it if it is no longer needed.",
				kindLabel(credential.Kind), displayName(credential), policy.StaleAfterDays)) {
			summary.Notified++
			if _, err := s.pool.Exec(ctx, `
UPDATE elitea_identity.credential_review_state SET notified_at = clock_timestamp()
WHERE kind = $1 AND credential_id = $2`, string(credential.Kind), credential.CredentialID); err != nil {
				summary.Errors++
			}
		}
	}
	return summary, nil
}

func kindLabel(kind credentialuse.Kind) string {
	switch kind {
	case credentialuse.KindPAT:
		return "personal access token"
	case credentialuse.KindToolkitSecret:
		return "toolkit secret"
	case credentialuse.KindMCPOAuthToken:
		return "MCP OAuth token"
	case credentialuse.KindProviderOAuthGrant:
		return "OAuth grant"
	case credentialuse.KindCredentialReference:
		return "credential reference"
	case credentialuse.KindCredentialGrant:
		return "credential use grant"
	}
	return "credential"
}

func displayName(credential InventoryCredential) string {
	if credential.Name != "" {
		return credential.Name
	}
	return credential.CredentialID
}

func (s *CredentialReview) recordSeen(ctx context.Context, all []InventoryCredential) error {
	if len(all) == 0 {
		return nil
	}
	kinds := make([]string, len(all))
	ids := make([]string, len(all))
	projects := make([]*int64, len(all))
	owners := make([]*int64, len(all))
	for i, credential := range all {
		kinds[i], ids[i], projects[i], owners[i] = string(credential.Kind), credential.CredentialID, credential.ProjectID, credential.OwnerID
	}
	_, err := s.pool.Exec(ctx, `
INSERT INTO elitea_identity.credential_review_state (kind, credential_id, project_id, owner_id, first_seen_at)
SELECT u.kind, u.credential_id, u.project_id, u.owner_id, $5::timestamptz
FROM unnest($1::text[], $2::text[], $3::bigint[], $4::bigint[]) AS u(kind, credential_id, project_id, owner_id)
ON CONFLICT (kind, credential_id) DO UPDATE
SET project_id = EXCLUDED.project_id, owner_id = EXCLUDED.owner_id,
    -- A revoked id that is listed again (a new grant on the same key) is
    -- a new credential.
    first_seen_at = CASE WHEN credential_review_state.revoked_at IS NULL
                         THEN credential_review_state.first_seen_at ELSE EXCLUDED.first_seen_at END,
    flagged_at = CASE WHEN credential_review_state.revoked_at IS NULL THEN credential_review_state.flagged_at END,
    notified_at = CASE WHEN credential_review_state.revoked_at IS NULL THEN credential_review_state.notified_at END,
    revoked_at = NULL, revoked_by = NULL, revoke_reason = ''`, kinds, ids, projects, owners, s.now())
	if err != nil {
		return fmt.Errorf("credential review: record the credentials seen: %w", err)
	}
	return nil
}

func (s *CredentialReview) flag(ctx context.Context, credential InventoryCredential, policy platformconfig.CredentialReviewPolicy) (time.Time, error) {
	var flaggedAt time.Time
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
UPDATE elitea_identity.credential_review_state SET flagged_at = clock_timestamp(), notified_at = NULL
WHERE kind = $1 AND credential_id = $2 AND flagged_at IS NULL
RETURNING flagged_at`, string(credential.Kind), credential.CredentialID).Scan(&flaggedAt); err != nil {
			return err
		}
		return s.audit.Append(ctx, tx, securityaudit.SecurityEvent{
			Type: securityaudit.TypeCredentialReview, TenantProjectID: derefInt(credential.ProjectID),
			Action: "credential.stale.flagged", PrincipalRef: "system:credential_staleness",
			Reasons:      []string{"kind:" + string(credential.Kind), "stale_after_days:" + strconv.Itoa(policy.StaleAfterDays)},
			ResourceType: "credential." + string(credential.Kind), ResourceID: credential.CredentialID,
		})
	})
	return flaggedAt, err
}

func (s *CredentialReview) unflag(ctx context.Context, credential InventoryCredential) error {
	_, err := s.pool.Exec(ctx, `
UPDATE elitea_identity.credential_review_state SET flagged_at = NULL, notified_at = NULL
WHERE kind = $1 AND credential_id = $2`, string(credential.Kind), credential.CredentialID)
	return err
}

// notifyCredential writes one centry.notifications row to the
// credential's owner (else the project's owner). The uuid is derived from
// the credential and the flag time, so a retried pass does not repeat it.
func (s *CredentialReview) notifyCredential(ctx context.Context, credential InventoryCredential, at time.Time, eventType, message string) bool {
	recipient := derefInt(credential.OwnerID)
	project := derefInt(credential.ProjectID)
	if recipient == 0 && project > 0 {
		_ = s.pool.QueryRow(ctx, `SELECT owner_id FROM centry.project WHERE id = $1`, project).Scan(&recipient)
	}
	if recipient <= 0 {
		return false
	}
	meta := map[string]any{
		"message":       message,
		"kind":          string(credential.Kind),
		"credential_id": credential.CredentialID,
		"name":          credential.Name,
	}
	key := fmt.Sprintf("%s-%s-%s-%d", eventType, credential.Kind, credential.CredentialID, at.UTC().Unix())
	return insertReviewNotification(ctx, s.pool, key, project, recipient, eventType, meta) == nil
}

// insertReviewNotification writes one notification. The uuid is supplied
// (gen_random_uuid() is not the column default on a legacy-created
// database: chat_mention_notification.go).
func insertReviewNotification(ctx context.Context, pool *pgxpool.Pool, key string, projectID, userID int64, eventType string, meta map[string]any) error {
	encoded, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if projectID <= 0 {
		// notifications.project_id is NOT NULL; the owner's own project
		// answers it, as the PAT expiry notice does.
		_ = pool.QueryRow(ctx, `
SELECT COALESCE((SELECT id FROM centry.project WHERE owner_id = $1 ORDER BY id LIMIT 1), 0)`, userID).Scan(&projectID)
	}
	_, err = pool.Exec(ctx, `
INSERT INTO centry.notifications (uuid, is_seen, project_id, user_id, meta, event_type, created_at, updated_at)
VALUES ($1::uuid, FALSE, $2, $3, $4::jsonb, $5, now() AT TIME ZONE 'UTC', now() AT TIME ZONE 'UTC')
ON CONFLICT (uuid) DO NOTHING`, reviewNotificationUUID(key), int32(projectID), int32(userID), encoded, eventType)
	return err
}

// revokeTransition is the revoke step of credentialgrant.Service
// .TransitionGrant: the domain's state machine decides.
func revokeTransition(actor string, now time.Time) func(credentialgrant.CredentialUseGrant) (credentialgrant.CredentialUseGrant, error) {
	return func(current credentialgrant.CredentialUseGrant) (credentialgrant.CredentialUseGrant, error) {
		return current.Transition(credentialgrant.ActionRevoke, actor, now)
	}
}

// reviewNotificationUUID derives a deterministic UUIDv4-shaped id from key,
// as artifactBucketExpiryNotificationUUID does.
func reviewNotificationUUID(key string) string {
	digest := sha256.Sum256([]byte(key))
	value := append([]byte(nil), digest[:16]...)
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}
