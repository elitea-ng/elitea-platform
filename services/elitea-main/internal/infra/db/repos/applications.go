package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

type ApplicationsRepo struct {
	pool *pgxpool.Pool
}

func NewApplicationsRepo(pool *pgxpool.Pool) *ApplicationsRepo {
	return &ApplicationsRepo{pool: pool}
}

const (
	defaultListPageSize = 20
	defaultVersionName  = "base"
	defaultAgentType    = "openai"
	// defaultVersionMetaKey is the applications.meta key that records which
	// version is the application's default. application_versions has no
	// is_default column; see SetDefaultVersion.
	defaultVersionMetaKey = "default_version_id"
)

// versionColumns is the read projection of application_versions. `v` is the
// version alias, `a` the owning applications row (needed for is_default).
const versionColumns = `v.id, v.application_id, v.name, v.status, v.created_at,
	v.author_id, COALESCE(v.agent_type, ''), COALESCE(v.instructions, ''),
	COALESCE(v.welcome_message, ''), COALESCE(v.llm_settings::text, '{}'),
	COALESCE(v.conversation_starters::text, '[]'), COALESCE(v.meta::text, '{}'),
	COALESCE(a.meta->>'` + defaultVersionMetaKey + `', '')`

// rowScanner is satisfied by both pgx.Row and pgx.Rows, so the single-row and
// multi-row read paths share one scan projection.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanVersion(row rowScanner) (applications.Version, error) {
	var (
		ver              applications.Version
		llmJSON          string
		startersJSON     string
		metaJSON         string
		defaultVersionID string
	)
	if err := row.Scan(
		&ver.ID, &ver.ApplicationID, &ver.Name, &ver.Status, &ver.CreatedAt,
		&ver.AuthorID, &ver.AgentType, &ver.Instructions, &ver.WelcomeMessage,
		&llmJSON, &startersJSON, &metaJSON, &defaultVersionID,
	); err != nil {
		return applications.Version{}, err
	}
	ver.LLMSettings = decodeJSONObject(llmJSON)
	ver.ConversationStarters = decodeJSONArray(startersJSON)
	ver.Meta = decodeJSONObject(metaJSON)
	ver.IsDefault = defaultVersionID != "" && defaultVersionID == ver.ID
	ver.Config = configFromColumns(ver.LLMSettings, ver.Instructions)
	return ver, nil
}

func decodeJSONObject(raw string) map[string]any {
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

func decodeJSONArray(raw string) []any {
	var out []any
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return []any{}
	}
	return out
}

// configFromColumns builds the derived VersionConfig projection documented on
// applications.VersionConfig. Fields with no column stay zero.
func configFromColumns(llm map[string]any, instructions string) applications.VersionConfig {
	cfg := applications.VersionConfig{SystemPrompt: instructions}
	if model, ok := llm["model_name"].(string); ok {
		cfg.Model = model
	}
	if temperature, ok := llm["temperature"].(float64); ok {
		cfg.Temperature = temperature
	}
	if maxTokens, ok := llm["max_tokens"].(float64); ok {
		cfg.MaxTokens = int(maxTokens)
	}
	return cfg
}

// rejectDerivedConfig refuses a write that carries VersionConfig. Model,
// Temperature, MaxTokens and SystemPrompt are projections of llm_settings and
// instructions — set those fields instead; Tools, Skills, Datasources and
// Guardrails have no storage at all and would be silently dropped.
func rejectDerivedConfig(cfg applications.VersionConfig) error {
	if cfg.Model == "" && cfg.Temperature == 0 && cfg.MaxTokens == 0 &&
		cfg.SystemPrompt == "" && len(cfg.Tools) == 0 && len(cfg.Skills) == 0 &&
		len(cfg.Datasources) == 0 && cfg.Guardrails == nil {
		return nil
	}
	return apierr.BadRequest(
		"version config is a derived read-only projection: set llm_settings/instructions instead; " +
			"tools, skills, datasources and guardrails have no storage in application_versions")
}

func encodeJSONObject(v map[string]any) (string, error) {
	if v == nil {
		return "{}", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", apierr.BadRequest("version payload is not encodable as JSON")
	}
	return string(b), nil
}

func encodeJSONArray(v []any) (string, error) {
	if v == nil {
		return "[]", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", apierr.BadRequest("version payload is not encodable as JSON")
	}
	return string(b), nil
}

// splitTagFilter reads the `tags` query parameter, which is a comma-separated
// list. It removes the empty and blank entries, so `tags=` and `tags=,,` mean
// "no tag filter" and do not produce a condition that matches nothing.
func splitTagFilter(raw string) []string {
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (r *ApplicationsRepo) List(ctx context.Context, req applications.ListRequest) (applications.ListResponse, error) {
	s, err := tenantSchema(req.ProjectID)
	if err != nil {
		return applications.ListResponse{}, err
	}
	page, pageSize := req.Page, req.PageSize
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = defaultListPageSize
	}
	empty := applications.ListResponse{Rows: []applications.Application{}, Page: page, PageSize: pageSize}

	// "pipeline" lists pipeline versions, anything else lists classic agents.
	// Both are INNER JOINs: an application with no version row is not a
	// listable agent (and cannot be opened in the editor either).
	join := fmt.Sprintf(` JOIN %s.application_versions av ON av.application_id = a.id AND av.agent_type %s 'pipeline'`,
		s, map[bool]string{true: "=", false: "!="}[req.AgentsType == "pipeline"])

	args := []any{}
	conditions := []string{}
	if req.Search != "" {
		args = append(args, "%"+req.Search+"%")
		conditions = append(conditions, fmt.Sprintf(`(a.name ILIKE $%d OR a.description ILIKE $%d)`, len(args), len(args)))
	}
	// One condition per requested tag, so the filter is AND and not OR: an
	// application must carry EVERY tag the caller named. That is the rule
	// legacy applies (`get_application_by_tags` counts the distinct matched
	// tags and compares the count to the request, and the list filter appends
	// one `versions.any(tags.any(...))` per tag).
	//
	// A token matches the tag NAME or the tag ID. Legacy accepts ids only
	// (`[int(tag) for tag in tags.split(',')]`), but the web app's tag rail
	// writes NAMES into its `tags[]` search param, and a name is what the row
	// carries back. Both are accepted so neither caller needs a lookup.
	for _, tag := range splitTagFilter(req.Tags) {
		args = append(args, tag)
		conditions = append(conditions, fmt.Sprintf(`EXISTS (
			SELECT 1 FROM %[1]s.application_versions ftv
			JOIN %[1]s.application_version_tag_association fta ON fta.version_id = ftv.id
			JOIN %[1]s.tags ft ON ft.id = fta.tag_id
			WHERE ftv.application_id = a.id AND (ft.name = $%[2]d OR ft.id::text = $%[2]d))`, s, len(args)))
	}
	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}

	var total int
	countQuery := fmt.Sprintf(`SELECT COUNT(DISTINCT a.id) FROM %s.applications a`, s) + join + where
	if err := r.pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return empty, fmt.Errorf("applications: list count: %w", err)
	}

	selectArgs := append([]any{}, args...)
	limitIdx := len(selectArgs) + 1
	// The agent's PUBLISH state, computed per application.
	//
	// `Application.Status` was never selected, so every listed agent carried
	// the empty string and `omitempty` dropped the key entirely. The web app's
	// Drafts/Published/Moderation/Approval/Rejected tabs filter this exact
	// field client-side (`pages/agents/PrivateAgentsList.tsx`), so all five
	// were permanently empty and no publish could ever fill one.
	//
	// It is an EXISTS over the versions, not `av.status`: the row `DISTINCT ON
	// (a.id)` keeps is whichever version the plan happens to reach first, so
	// reading its status would make the tab an agent appears under depend on
	// row order. An agent is published when ANY of its versions is, which is
	// the same rule the editor's own publish/unpublish pair enforces.
	//
	// `embedded` is deliberately not published: those clones exist only to
	// carry a PARENT agent's sub-agents through a publish, and listing them
	// as published would put an agent in the Published tab because something
	// else was published.
	statusExpr := fmt.Sprintf(`CASE WHEN EXISTS (
			SELECT 1 FROM %s.application_versions pv
			WHERE pv.application_id = a.id AND pv.status = 'published'
		) THEN 'published' ELSE 'draft' END`, s)
	// Every tag of every version of the application, by name, deduplicated
	// and sorted.
	//
	// It is a correlated subquery and not a join, for two reasons. `DISTINCT
	// ON (a.id)` keeps ONE version row per application, so a joined
	// aggregate would only ever describe that one version; and legacy takes
	// the UNION of the tags of all versions, keyed by name
	// (`ApplicationListModel.parse_versions_data`).
	//
	// It answers `{}` and never NULL, so a row with no tags carries an empty
	// array rather than a null (#841).
	tagsExpr := fmt.Sprintf(`COALESCE((
			SELECT array_agg(DISTINCT t.name ORDER BY t.name)
			FROM %[1]s.application_versions tv
			JOIN %[1]s.application_version_tag_association ta ON ta.version_id = tv.id
			JOIN %[1]s.tags t ON t.id = ta.tag_id
			WHERE tv.application_id = a.id), '{}')`, s)
	query := fmt.Sprintf(`
		SELECT DISTINCT ON (a.id) a.id, a.name, COALESCE(a.description, ''), COALESCE(a.icon, ''),
			a.owner_id, a.created_at, COALESCE(a.shared_id, 0),
			COALESCE(a.meta, '{}'::jsonb)::text,
			COALESCE(av.agent_type, '`+defaultAgentType+`'),
			COALESCE(u.id, 0), COALESCE(u.email, ''), COALESCE(u.name, ''),
			`+statusExpr+`,
			`+tagsExpr+`
		FROM %s.applications a`, s) + join +
		// The author join reads `av.author_id`, the USER that wrote the
		// version, and not `a.owner_id`, which is the owning PROJECT (#533).
		// The old join gave the list the account whose user id happened to
		// equal the project id — user 1 in nearly every deployment.
		` LEFT JOIN public.auth_core__user u ON u.id = av.author_id` + where +
		// `av.id ASC` picks the FIRST version of each application, whose author
		// is the account that created the agent. DISTINCT ON keeps one row per
		// application, and without this key the row it keeps — and so the
		// author the list shows — is whichever version the plan reaches first.
		fmt.Sprintf(` ORDER BY a.id DESC, av.id ASC LIMIT $%d OFFSET $%d`, limitIdx, limitIdx+1)
	selectArgs = append(selectArgs, pageSize, (page-1)*pageSize)

	rows, err := r.pool.Query(ctx, query, selectArgs...)
	if err != nil {
		return empty, fmt.Errorf("applications: list: %w", err)
	}
	defer rows.Close()

	items := []applications.Application{}
	for rows.Next() {
		var (
			app         applications.Application
			sharedID    int
			metaStr     string
			authorID    int
			authorEmail string
			authorName  string
		)
		if err := rows.Scan(
			&app.ID, &app.Name, &app.Description, &app.Icon,
			&app.OwnerID, &app.CreatedAt, &sharedID,
			&metaStr, &app.AgentType,
			&authorID, &authorEmail, &authorName,
			&app.Status, &app.Tags,
		); err != nil {
			return empty, fmt.Errorf("applications: list scan: %w", err)
		}
		if app.Tags == nil {
			app.Tags = []string{}
		}
		app.ProjectID = req.ProjectID
		app.IsForked = sharedID > 0
		app.Meta = decodeJSONObject(metaStr)
		app.Authors = []applications.Author{}
		if authorID > 0 {
			app.Authors = append(app.Authors, applications.Author{
				ID: strconv.Itoa(authorID), Email: authorEmail, Name: authorName,
			})
		}
		items = append(items, app)
	}
	if err := rows.Err(); err != nil {
		return empty, fmt.Errorf("applications: list rows: %w", err)
	}

	totalPages := total / pageSize
	if total%pageSize > 0 {
		totalPages++
	}
	return applications.ListResponse{
		Rows: items, Total: total, Page: page, PageSize: pageSize, TotalPages: totalPages,
	}, nil
}

const applicationColumns = `id, name, COALESCE(description, ''), COALESCE(icon, ''),
	owner_id, created_at, COALESCE(uuid::text, '')`

func scanApplication(row rowScanner, projectID string) (applications.Application, error) {
	var app applications.Application
	if err := row.Scan(
		&app.ID, &app.Name, &app.Description, &app.Icon,
		&app.OwnerID, &app.CreatedAt, &app.UUID,
	); err != nil {
		return applications.Application{}, err
	}
	// CreatedBy stays empty. It used to mirror OwnerID, which was true only
	// while every writer in this service put the caller user id into
	// `owner_id`. That column holds the owning PROJECT (#533), so the mirror
	// now says that a project created the agent. The author of a version is
	// application_versions.author_id, which the version reads carry.
	app.ProjectID = projectID
	return app, nil
}

func (r *ApplicationsRepo) Get(ctx context.Context, projectID, applicationID string) (applications.Application, error) {
	s, err := tenantSchema(projectID)
	if err != nil {
		return applications.Application{}, err
	}
	if !isNumericRowID(applicationID) {
		return applications.Application{}, apierr.NotFound("application not found")
	}
	query := fmt.Sprintf(`SELECT `+applicationColumns+` FROM %s.applications WHERE id = $1`, s)
	app, err := scanApplication(r.pool.QueryRow(ctx, query, applicationID), projectID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return applications.Application{}, apierr.NotFound("application not found")
		}
		return applications.Application{}, fmt.Errorf("applications: get: %w", err)
	}
	return app, nil
}

func (r *ApplicationsRepo) Create(ctx context.Context, req applications.CreateRequest) (applications.Application, error) {
	s, err := tenantSchema(req.ProjectID)
	if err != nil {
		return applications.Application{}, err
	}
	if req.AuthorID <= 0 {
		return applications.Application{}, apierr.Unauthorized("an authenticated owner is required to create an application")
	}
	// `applications.owner_id` is the owning PROJECT and not the caller (#533).
	// This statement wrote the principal, which made the agent invisible to
	// every legacy read: the legacy runtime filters
	// `Application.owner_id == project_id`. The principal is the author of the
	// first version, below.
	ownerID, err := tenantschema.OwnerID(req.ProjectID)
	if err != nil {
		return applications.Application{}, err
	}
	if req.Config != nil {
		if err := rejectDerivedConfig(*req.Config); err != nil {
			return applications.Application{}, err
		}
	}
	if req.InitialVersion != nil {
		if err := rejectDerivedConfig(req.InitialVersion.Config); err != nil {
			return applications.Application{}, err
		}
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return applications.Application{}, fmt.Errorf("applications: create: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	query := fmt.Sprintf(`
		INSERT INTO %s.applications (name, description, icon, owner_id)
		VALUES ($1, $2, $3, $4)
		RETURNING `+applicationColumns, s)
	app, err := scanApplication(
		tx.QueryRow(ctx, query, req.Name, req.Description, req.Icon, ownerID.Int64()),
		req.ProjectID,
	)
	if err != nil {
		return applications.Application{}, fmt.Errorf("applications: create: %w", err)
	}

	if req.InitialVersion != nil {
		version := *req.InitialVersion
		if version.AuthorID <= 0 {
			version.AuthorID = req.AuthorID.Int64()
		}
		created, err := insertVersion(ctx, tx, s, app.ID, version)
		if err != nil {
			return applications.Application{}, err
		}
		app.Versions = []applications.Version{created}
	}

	if err := tx.Commit(ctx); err != nil {
		return applications.Application{}, fmt.Errorf("applications: create: commit: %w", err)
	}
	return app, nil
}

func (r *ApplicationsRepo) Update(ctx context.Context, req applications.UpdateRequest) (applications.Application, error) {
	s, err := tenantSchema(req.ProjectID)
	if err != nil {
		return applications.Application{}, err
	}
	if !isNumericRowID(req.ApplicationID) {
		return applications.Application{}, apierr.NotFound("application not found")
	}

	setClauses := []string{}
	args := []any{}
	appendSet := func(column string, value any) {
		args = append(args, value)
		setClauses = append(setClauses, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	if req.Name != nil {
		appendSet("name", *req.Name)
	}
	if req.Description != nil {
		appendSet("description", *req.Description)
	}
	if req.Icon != nil {
		appendSet("icon", *req.Icon)
	}
	if len(setClauses) == 0 {
		return r.Get(ctx, req.ProjectID, req.ApplicationID)
	}

	args = append(args, req.ApplicationID)
	query := fmt.Sprintf(`UPDATE %s.applications SET %s WHERE id = $%d RETURNING `+applicationColumns,
		s, strings.Join(setClauses, ", "), len(args))
	app, err := scanApplication(r.pool.QueryRow(ctx, query, args...), req.ProjectID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return applications.Application{}, apierr.NotFound("application not found")
		}
		return applications.Application{}, fmt.Errorf("applications: update: %w", err)
	}
	return app, nil
}

func (r *ApplicationsRepo) Delete(ctx context.Context, projectID, applicationID string) error {
	s, err := tenantSchema(projectID)
	if err != nil {
		return err
	}
	if !isNumericRowID(applicationID) {
		return apierr.NotFound("application not found")
	}

	// application_versions, application_variables and
	// application_version_tag_association all cascade from applications
	// (migrations/001_initial.sql), so one DELETE is enough for the schema
	// this service creates. application_tools exists only in pylon-migrated
	// databases and has no cascade there, so it is cleared first when
	// present. The previous unconditional DELETE FROM ...application_tools
	// made every Delete fail with 42P01 on a schema created from 001_initial.
	var applicationTools *string
	if err := r.pool.QueryRow(ctx, `SELECT to_regclass($1)::text`,
		"p_"+projectID+".application_tools").Scan(&applicationTools); err != nil {
		return fmt.Errorf("applications: delete: probe application_tools: %w", err)
	}
	if applicationTools != nil {
		if _, err := r.pool.Exec(ctx, fmt.Sprintf(
			`DELETE FROM %s.application_tools WHERE application_version_id IN
				(SELECT id FROM %s.application_versions WHERE application_id = $1)`, s, s),
			applicationID); err != nil {
			return fmt.Errorf("applications: delete tools: %w", err)
		}
	}

	ct, err := r.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.applications WHERE id = $1`, s), applicationID)
	if err != nil {
		return fmt.Errorf("applications: delete: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return apierr.NotFound("application not found")
	}
	return nil
}

func (r *ApplicationsRepo) GetVersion(ctx context.Context, projectID, applicationID, versionID string) (applications.Version, error) {
	s, err := tenantSchema(projectID)
	if err != nil {
		return applications.Version{}, err
	}
	if !isNumericRowID(applicationID) || !isNumericRowID(versionID) {
		return applications.Version{}, apierr.NotFound("version not found")
	}
	query := fmt.Sprintf(`SELECT `+versionColumns+`
		FROM %s.application_versions v
		JOIN %s.applications a ON a.id = v.application_id
		WHERE v.application_id = $1 AND v.id = $2`, s, s)
	ver, err := scanVersion(r.pool.QueryRow(ctx, query, applicationID, versionID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return applications.Version{}, apierr.NotFound("version not found")
		}
		return applications.Version{}, fmt.Errorf("applications: get version: %w", err)
	}
	return ver, nil
}

func (r *ApplicationsRepo) ListVersions(ctx context.Context, projectID, applicationID string) ([]applications.Version, error) {
	s, err := tenantSchema(projectID)
	if err != nil {
		return nil, err
	}
	if !isNumericRowID(applicationID) {
		return []applications.Version{}, nil
	}
	query := fmt.Sprintf(`SELECT `+versionColumns+`
		FROM %s.application_versions v
		JOIN %s.applications a ON a.id = v.application_id
		WHERE v.application_id = $1
		ORDER BY v.created_at DESC, v.id DESC`, s, s)

	rows, err := r.pool.Query(ctx, query, applicationID)
	if err != nil {
		return nil, fmt.Errorf("applications: list versions: %w", err)
	}
	defer rows.Close()

	versions := []applications.Version{}
	for rows.Next() {
		ver, err := scanVersion(rows)
		if err != nil {
			return nil, fmt.Errorf("applications: list versions scan: %w", err)
		}
		versions = append(versions, ver)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("applications: list versions rows: %w", err)
	}
	return versions, nil
}

func (r *ApplicationsRepo) CreateVersion(ctx context.Context, projectID, applicationID string, v applications.Version) (applications.Version, error) {
	s, err := tenantSchema(projectID)
	if err != nil {
		return applications.Version{}, err
	}
	if !isNumericRowID(applicationID) {
		return applications.Version{}, apierr.NotFound("application not found")
	}
	if err := rejectDerivedConfig(v.Config); err != nil {
		return applications.Version{}, err
	}
	if v.AuthorID <= 0 {
		return applications.Version{}, apierr.Unauthorized("an authenticated author is required to create a version")
	}
	return insertVersion(ctx, r.pool, s, applicationID, v)
}

// querier is the subset of pgxpool.Pool / pgx.Tx insertVersion needs, so the
// same statement runs inside Create's transaction and standalone.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func insertVersion(ctx context.Context, q querier, s, applicationID string, v applications.Version) (applications.Version, error) {
	name := v.Name
	if name == "" {
		name = defaultVersionName
	}
	agentType := v.AgentType
	if agentType == "" {
		agentType = defaultAgentType
	}
	status := v.Status
	if status == "" {
		status = "draft"
	}
	llmJSON, err := encodeJSONObject(v.LLMSettings)
	if err != nil {
		return applications.Version{}, err
	}
	startersJSON, err := encodeJSONArray(v.ConversationStarters)
	if err != nil {
		return applications.Version{}, err
	}
	metaJSON, err := encodeJSONObject(v.Meta)
	if err != nil {
		return applications.Version{}, err
	}
	pipelineSettingsJSON, err := encodeJSONObject(v.PipelineSettings)
	if err != nil {
		return applications.Version{}, err
	}

	// The INSERT ... RETURNING is wrapped in a CTE so the read projection —
	// which needs the owning applications row for is_default — is the same
	// SQL as GetVersion's.
	query := fmt.Sprintf(`
		WITH v AS (
			INSERT INTO %s.application_versions
				(application_id, name, status, author_id, agent_type, instructions,
				 welcome_message, llm_settings, conversation_starters, meta, pipeline_settings)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9::jsonb, $10::jsonb, $11::jsonb)
			RETURNING *
		)
		SELECT `+versionColumns+` FROM v JOIN %s.applications a ON a.id = v.application_id`, s, s)

	ver, err := scanVersion(q.QueryRow(ctx, query,
		applicationID, name, status, v.AuthorID, agentType, v.Instructions,
		v.WelcomeMessage, llmJSON, startersJSON, metaJSON, pipelineSettingsJSON,
	))
	if err != nil {
		return applications.Version{}, fmt.Errorf("applications: create version: %w", err)
	}
	return ver, nil
}

func (r *ApplicationsRepo) UpdateVersion(ctx context.Context, projectID, applicationID, versionID string, v applications.Version) (applications.Version, error) {
	s, err := tenantSchema(projectID)
	if err != nil {
		return applications.Version{}, err
	}
	if !isNumericRowID(applicationID) || !isNumericRowID(versionID) {
		return applications.Version{}, apierr.NotFound("version not found")
	}
	if err := rejectDerivedConfig(v.Config); err != nil {
		return applications.Version{}, err
	}

	setClauses := []string{}
	args := []any{}
	appendSet := func(clause string, value any) {
		args = append(args, value)
		setClauses = append(setClauses, fmt.Sprintf(clause, len(args)))
	}
	// Presence, not emptiness, decides whether a string column joins the SET
	// list. The `|| value != ""` half keeps every caller that fills the value
	// without setting the flag; the flag half is what lets an explicit ""
	// CLEAR the column. Before #824 the test was emptiness alone, so clearing
	// a welcome message answered 201 and read the old text back.
	if v.Present.Name || v.Name != "" {
		appendSet("name = $%d", v.Name)
	}
	if v.Present.AgentType || v.AgentType != "" {
		appendSet("agent_type = $%d", v.AgentType)
	}
	if v.Present.Instructions || v.Instructions != "" {
		appendSet("instructions = $%d", v.Instructions)
	}
	if v.Present.WelcomeMessage || v.WelcomeMessage != "" {
		appendSet("welcome_message = $%d", v.WelcomeMessage)
	}
	if v.LLMSettings != nil {
		encoded, err := encodeJSONObject(v.LLMSettings)
		if err != nil {
			return applications.Version{}, err
		}
		appendSet("llm_settings = $%d::jsonb", encoded)
	}
	if v.ConversationStarters != nil {
		encoded, err := encodeJSONArray(v.ConversationStarters)
		if err != nil {
			return applications.Version{}, err
		}
		appendSet("conversation_starters = $%d::jsonb", encoded)
	}
	if v.Meta != nil {
		encoded, err := encodeJSONObject(v.Meta)
		if err != nil {
			return applications.Version{}, err
		}
		appendSet("meta = $%d::jsonb", encoded)
	}
	if v.PipelineSettings != nil {
		encoded, err := encodeJSONObject(v.PipelineSettings)
		if err != nil {
			return applications.Version{}, err
		}
		appendSet("pipeline_settings = $%d::jsonb", encoded)
	}
	if len(setClauses) == 0 {
		return r.GetVersion(ctx, projectID, applicationID, versionID)
	}

	args = append(args, applicationID, versionID)
	query := fmt.Sprintf(`
		WITH v AS (
			UPDATE %s.application_versions SET %s
			WHERE application_id = $%d AND id = $%d
			RETURNING *
		)
		SELECT `+versionColumns+` FROM v JOIN %s.applications a ON a.id = v.application_id`,
		s, strings.Join(setClauses, ", "), len(args)-1, len(args), s)

	ver, err := scanVersion(r.pool.QueryRow(ctx, query, args...))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return applications.Version{}, apierr.NotFound("version not found")
		}
		return applications.Version{}, fmt.Errorf("applications: update version: %w", err)
	}
	return ver, nil
}

func (r *ApplicationsRepo) DeleteVersion(ctx context.Context, projectID, applicationID, versionID string) error {
	s, err := tenantSchema(projectID)
	if err != nil {
		return err
	}
	if !isNumericRowID(applicationID) || !isNumericRowID(versionID) {
		return apierr.NotFound("version not found")
	}
	ct, err := r.pool.Exec(ctx,
		fmt.Sprintf(`DELETE FROM %s.application_versions WHERE application_id = $1 AND id = $2`, s),
		applicationID, versionID)
	if err != nil {
		return fmt.Errorf("applications: delete version: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return apierr.NotFound("version not found")
	}
	return nil
}

// SetDefaultVersion records the application's default version in
// applications.meta->>'default_version_id'.
//
// application_versions has no is_default column and its only state column,
// status, is the publish lifecycle (draft/published/embedded) that the
// transport layer's publish guards read — overloading it would conflate two
// independent state machines. meta is the schema's own extension point, the
// cardinality is right (one default per application, not a flag per version),
// and it is already the contract the UI reads: apps/elitea-web/src/entities/
// version/model/selectors.ts resolves the default from the owning entity's
// meta.default_version_id, falling back to the version named "base".
func (r *ApplicationsRepo) SetDefaultVersion(ctx context.Context, projectID, applicationID, versionID string) error {
	s, err := tenantSchema(projectID)
	if err != nil {
		return err
	}
	if !isNumericRowID(applicationID) || !isNumericRowID(versionID) {
		return apierr.NotFound("version not found")
	}

	// The version must exist and belong to this application; otherwise the
	// previous implementation reported success while writing a dangling id.
	var exists bool
	if err := r.pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT EXISTS (SELECT 1 FROM %s.application_versions WHERE application_id = $1 AND id = $2)`, s),
		applicationID, versionID).Scan(&exists); err != nil {
		return fmt.Errorf("applications: set default version: %w", err)
	}
	if !exists {
		return apierr.NotFound("version not found")
	}

	ct, err := r.pool.Exec(ctx, fmt.Sprintf(`
		UPDATE %s.applications
		SET meta = jsonb_set(COALESCE(meta, '{}'::jsonb), '{`+defaultVersionMetaKey+`}', to_jsonb($1::text))
		WHERE id = $2`, s), versionID, applicationID)
	if err != nil {
		return fmt.Errorf("applications: set default version: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return apierr.NotFound("application not found")
	}
	return nil
}

func (r *ApplicationsRepo) BatchReplaceVersion(ctx context.Context, projectID, oldVersionID, newVersionID string, deleteOld bool) error {
	s, err := tenantSchema(projectID)
	if err != nil {
		return err
	}
	if !isNumericRowID(oldVersionID) || !isNumericRowID(newVersionID) {
		return apierr.NotFound("version not found")
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("applications: batch replace version: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var bothExist bool
	if err := tx.QueryRow(ctx, fmt.Sprintf(
		`SELECT COUNT(*) = 2 FROM %s.application_versions WHERE id = ANY($1::int[])`, s),
		[]string{oldVersionID, newVersionID}).Scan(&bothExist); err != nil {
		return fmt.Errorf("applications: batch replace version: %w", err)
	}
	if !bothExist {
		return apierr.NotFound("version not found")
	}

	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`UPDATE %s.entity_tool_mapping SET entity_version_id = $1 WHERE entity_version_id = $2`, s),
		newVersionID, oldVersionID); err != nil {
		return fmt.Errorf("applications: batch replace version: %w", err)
	}
	if deleteOld {
		if _, err := tx.Exec(ctx,
			fmt.Sprintf(`DELETE FROM %s.application_versions WHERE id = $1`, s), oldVersionID); err != nil {
			return fmt.Errorf("applications: batch replace version delete old: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("applications: batch replace version: commit: %w", err)
	}
	return nil
}
