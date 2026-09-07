package applications

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/ownership"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

type Handler struct {
	repo applications.Repository
	pool *pgxpool.Pool
}

func NewHandler(repo applications.Repository, pool ...*pgxpool.Pool) *Handler {
	h := &Handler{repo: repo}
	if len(pool) > 0 {
		h.pool = pool[0]
	}
	return h
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.List)
	r.Post("/", h.Create)
	r.Get("/{applicationID}", h.Get)
	r.Put("/{applicationID}", h.Update)
	r.Delete("/{applicationID}", h.Delete)
	r.Get("/{applicationID}/versions", h.ListVersions)
	r.Get("/{applicationID}/versions/{versionID}", h.GetVersion)
	return r
}

func (h *Handler) GetDefaultVersion(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	applicationID := chi.URLParam(r, "applicationID")

	versions, err := h.repo.ListVersions(r.Context(), projectID, applicationID)
	if err != nil {
		apierr.Write(w, err)
		return
	}

	// Same resolution order as the UI's own selectDefaultVersion
	// (apps/elitea-web/src/entities/version/model/selectors.ts): the version
	// recorded in the application's meta.default_version_id, else the
	// well-known unnamed-default version "base", else the newest.
	for _, v := range versions {
		if v.IsDefault {
			writeJSON(w, http.StatusOK, v)
			return
		}
	}
	for _, v := range versions {
		if v.Name == defaultVersionName {
			writeJSON(w, http.StatusOK, v)
			return
		}
	}

	if len(versions) > 0 {
		writeJSON(w, http.StatusOK, versions[0])
		return
	}

	apierr.Write(w, apierr.NotFound("no versions found"))
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	// UI sends limit/offset; convert to page/pageSize
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit < 1 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	page := (offset / limit) + 1

	// UI sends "query" for search text
	search := r.URL.Query().Get("query")
	if search == "" {
		search = r.URL.Query().Get("search")
	}

	req := applications.ListRequest{
		ProjectID:  projectID,
		Page:       page,
		PageSize:   limit,
		Search:     search,
		Tags:       r.URL.Query().Get("tags"),
		FolderID:   r.URL.Query().Get("folder_id"),
		AgentsType: r.URL.Query().Get("agents_type"),
	}

	resp, err := h.repo.List(r.Context(), req)
	if err != nil {
		apierr.Write(w, err)
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	applicationID := chi.URLParam(r, "applicationID")

	app, err := h.repo.Get(r.Context(), projectID, applicationID)
	if err != nil {
		apierr.Write(w, err)
		return
	}

	versions, defaultVersionID := h.getVersions(r.Context(), projectID, applicationID)
	result := map[string]any{
		"id":          app.ID,
		"name":        app.Name,
		"description": app.Description,
		"icon":        app.Icon,
		// The owning PROJECT (#533). `applications.owner_id` holds the project,
		// which is what the legacy route answers here as well.
		"owner_id":   app.OwnerID,
		"created_at": app.CreatedAt,
		"versions":   versions,
		// `meta` carries the one key this service actually records on an
		// application: which of its versions is the default
		// (repos/applications.go's defaultVersionMetaKey, written by
		// SetDefaultVersion).
		//
		// The write half of that has existed since #147 and the read half did
		// not, so nothing a client could fetch ever said which version was the
		// default: the version bar in the web app remembered the id it had just
		// set and lost it on reload. Emitted ALWAYS, and as the empty string
		// when no default is recorded, because "" and a missing `meta` are
		// different answers — the first is "no default recorded", the second is
		// "this response cannot tell you", and it was the second that made the
		// affordance guess.
		"meta": map[string]any{"default_version_id": defaultVersionID},
	}

	// Include version_details for the first (latest) version — matches pylon response contract.
	if len(versions) > 0 {
		firstVersionID := versions[0]["id"].(string)
		if vd := h.fetchVersionDetails(r.Context(), projectID, applicationID, firstVersionID); vd != nil {
			result["version_details"] = vd
		}
	}

	writeJSON(w, http.StatusOK, result)
}

// getVersions returns the application's version summaries and the id of the
// version its `meta.default_version_id` names ("" when none is recorded).
//
// The default is joined in rather than fetched separately so the flag on each
// row and the id the caller is told about come from ONE read of the
// applications row: two reads could disagree, and a client that trusted the
// per-row flag over the id (or the other way round) would render a version bar
// that contradicts itself.
//
// `is_default` is derived here, not stored: application_versions has no such
// column, and the fact lives on the owning applications row. That is the same
// derivation repos/applications.go's scanVersion applies — this hand-written
// projection exists only because Get needs the summary shape, not the full
// Version.
//
// LEFT JOIN, not JOIN: this reads a version LIST, and an inner join would make
// a missing applications row silently shorten it. "The default is unknown" is
// the right degradation for that; "these versions do not exist" is not.
func (h *Handler) getVersions(ctx context.Context, projectID, applicationID string) ([]map[string]any, string) {
	s, ok := tenantSchema(projectID)
	if !ok {
		return []map[string]any{}, ""
	}
	q := fmt.Sprintf(`SELECT v.id, v.name, v.status, v.agent_type, v.created_at,
		COALESCE(a.meta->>'`+defaultVersionMetaKey+`', '')
		FROM %s.application_versions v
		LEFT JOIN %s.applications a ON a.id = v.application_id
		WHERE v.application_id = $1 ORDER BY v.id`, s, s)
	rows, err := h.pool.Query(ctx, q, applicationID)
	if err != nil {
		return []map[string]any{}, ""
	}
	defer rows.Close()

	var versions []map[string]any
	var defaultVersionID string
	for rows.Next() {
		var id int
		var name, status, agentType, rowDefaultVersionID string
		var createdAt any
		if err := rows.Scan(&id, &name, &status, &agentType, &createdAt, &rowDefaultVersionID); err != nil {
			continue
		}
		defaultVersionID = rowDefaultVersionID
		versionID := strconv.Itoa(id)
		versions = append(versions, map[string]any{
			"id":         versionID,
			"name":       name,
			"status":     status,
			"agent_type": agentType,
			"created_at": createdAt,
			// Compared against the empty string as well as the row id so an
			// application with no default recorded flags no version, rather
			// than flagging one whose id happens to stringify to "".
			"is_default": rowDefaultVersionID != "" && rowDefaultVersionID == versionID,
		})
	}
	if versions == nil {
		versions = []map[string]any{}
	}
	return versions, defaultVersionID
}

// fetchVersionDetails returns full version details for a single version, or nil on error.
func (h *Handler) fetchVersionDetails(ctx context.Context, projectID, applicationID, versionID string) map[string]any {
	if h.pool == nil {
		version, err := h.repo.GetVersion(ctx, projectID, applicationID, versionID)
		if err != nil {
			return nil
		}
		b, _ := json.Marshal(version)
		var m map[string]any
		// b is from json.Marshal above — cannot fail
		_ = json.Unmarshal(b, &m)
		return m
	}

	s, ok := tenantSchema(projectID)
	if !ok {
		return nil
	}

	var id int
	var appID int
	var name, status, agentType string
	var instructions, welcomeMsg *string
	var llmSettingsJSON, metaJSON, startersJSON, pipelineSettingsJSON []byte
	var createdAt interface{}
	var authorID *int

	err := h.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT v.id, v.application_id, v.name, v.status, v.created_at,
			v.agent_type, v.instructions, v.welcome_message,
			COALESCE(v.llm_settings::text, '{}'), COALESCE(v.meta::text, '{}'),
			COALESCE(v.conversation_starters::text, '[]'),
			COALESCE(v.pipeline_settings::text, '{}'),
			v.author_id
		FROM %s.application_versions v
		WHERE v.application_id = $1 AND v.id = $2`, s), applicationID, versionID).Scan(
		&id, &appID, &name, &status, &createdAt,
		&agentType, &instructions, &welcomeMsg,
		&llmSettingsJSON, &metaJSON, &startersJSON, &pipelineSettingsJSON,
		&authorID,
	)
	if err != nil {
		return nil
	}

	var llmSettings, meta, starters, pipelineSettings any
	// JSON was read from DB via COALESCE — cannot produce invalid JSON
	_ = json.Unmarshal(llmSettingsJSON, &llmSettings)
	_ = json.Unmarshal(metaJSON, &meta)
	_ = json.Unmarshal(startersJSON, &starters)
	_ = json.Unmarshal(pipelineSettingsJSON, &pipelineSettings)

	instrVal := ""
	if instructions != nil {
		instrVal = *instructions
	}
	welcomeVal := ""
	if welcomeMsg != nil {
		welcomeVal = *welcomeMsg
	}

	tools := make([]map[string]any, 0)
	toolRows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT etm.id, etm.tool_id, etm.entity_type, COALESCE(etm.selected_tools::text, '{}'),
			t.name, t.type, t.settings
		FROM %s.entity_tool_mapping etm
		LEFT JOIN %s.elitea_tools t ON t.id = etm.tool_id
		WHERE etm.entity_version_id = $1`, s, s), versionID)
	if err == nil {
		defer toolRows.Close()
		for toolRows.Next() {
			var etmID, toolID int
			var entityType, selectedToolsStr string
			var tName, tType *string
			var tConfig []byte
			if err := toolRows.Scan(&etmID, &toolID, &entityType, &selectedToolsStr, &tName, &tType, &tConfig); err != nil {
				continue
			}
			var selectedTools any
			// selectedToolsStr is COALESCE'd DB JSON — cannot fail
			_ = json.Unmarshal([]byte(selectedToolsStr), &selectedTools)
			var config any
			if tConfig != nil {
				// tConfig is DB-stored JSON — cannot fail
				_ = json.Unmarshal(tConfig, &config)
			}
			tool := map[string]any{
				"id":             etmID,
				"tool_id":        toolID,
				"entity_type":    entityType,
				"selected_tools": selectedTools,
			}
			if tName != nil {
				tool["name"] = *tName
			}
			if tType != nil {
				tool["type"] = *tType
			}
			if config != nil {
				tool["config"] = config
			}
			tools = append(tools, tool)
		}
	}

	authorIDStr := ""
	if authorID != nil {
		authorIDStr = strconv.Itoa(*authorID)
	}

	appToolRows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT id, name, type, settings::text
		FROM %s.application_tools
		WHERE application_version_id = $1`, s), versionID)
	if err == nil {
		defer appToolRows.Close()
		for appToolRows.Next() {
			var atID int
			var atName, atType, settingsStr string
			if err := appToolRows.Scan(&atID, &atName, &atType, &settingsStr); err != nil {
				continue
			}
			var settings any
			// settingsStr is DB-stored JSON — cannot fail
			_ = json.Unmarshal([]byte(settingsStr), &settings)
			tools = append(tools, map[string]any{
				"id":        atID,
				"name":      atName,
				"type":      atType,
				"settings":  settings,
				"author_id": authorIDStr,
			})
		}
	}

	variables := make([]map[string]any, 0)
	varRows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT name, COALESCE(value, '')
		FROM %s.application_variables
		WHERE application_version_id = $1
		ORDER BY id`, s), versionID)
	if err == nil {
		defer varRows.Close()
		for varRows.Next() {
			var vName, vValue string
			if err := varRows.Scan(&vName, &vValue); err != nil {
				continue
			}
			variables = append(variables, map[string]any{
				"name":  vName,
				"value": vValue,
			})
		}
	}

	// #345 — this key was the literal `[]any{}` on every read. The editor
	// reloads through here, so a tag a user saved came back missing and the
	// control blanked itself. The rows exist; nothing read them.
	tags := h.versionTagsOrEmpty(ctx, s, versionID)

	return map[string]any{
		"id":                    strconv.Itoa(id),
		"application_id":        strconv.Itoa(appID),
		"name":                  name,
		"status":                status,
		"created_at":            createdAt,
		"agent_type":            agentType,
		"instructions":          instrVal,
		"welcome_message":       welcomeVal,
		"llm_settings":          llmSettings,
		"meta":                  meta,
		"conversation_starters": starters,
		"pipeline_settings":     pipelineSettings,
		"author_id":             authorIDStr,
		"tools":                 tools,
		"tags":                  tags,
		"variables":             variables,
	}
}

// principal resolves the owning auth_core__user id of the authenticated
// caller. Every route reaching this handler is inside the authenticated group
// (internal/api/router.go), so a missing principal is a composition error, not
// an anonymous caller — it is refused rather than defaulted to user 1, which
// is what made every application in the prototype owned by user 1 regardless
// of who created it. Token principals resolve to their owning user;
// auth.User.OwningUserID never accepts a token id as an author.
func principal(r *http.Request) (auth.User, int64, bool) {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		return auth.User{}, 0, false
	}
	ownerID, ok := user.OwningUserID()
	if !ok {
		return auth.User{}, 0, false
	}
	return user, ownerID, true
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	if _, ok := tenantSchema(projectID); !ok {
		apierr.Write(w, apierr.BadRequest("invalid project id"))
		return
	}
	user, ownerID, ok := principal(r)
	if !ok {
		apierr.Write(w, apierr.Unauthorized("an authenticated principal is required"))
		return
	}
	userID := strconv.FormatInt(ownerID, 10)

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}

	req := applications.CreateRequest{
		ProjectID:   projectID,
		Name:        strVal(body, "name"),
		Description: strVal(body, "description"),
		Type:        strVal(body, "type"),
		Icon:        strVal(body, "icon"),
		AuthorID:    ownership.UserID(ownerID),
	}

	// Pylon creates the first version alongside the application, and so does
	// the repository — in one transaction. An application with no version row
	// is invisible to List (which INNER JOINs application_versions) and cannot
	// be opened in the agent editor, so a half-created agent must not commit.
	var initialVariables []any
	if versions, ok := body["versions"].([]any); ok && len(versions) > 0 {
		if vBody, ok := versions[0].(map[string]any); ok {
			// Validate model_project_id if llm_settings provided
			if llm, ok := vBody["llm_settings"].(map[string]any); ok {
				if mpid, ok := llm["model_project_id"].(string); ok && mpid != "" && mpid != projectID {
					// Non-numeric project IDs are treated as invalid UUIDs
					if _, err := strconv.Atoi(mpid); err != nil {
						apierr.Write(w, apierr.BadRequest("invalid model_project_id: project not found"))
						return
					}
				}
			}
			req.InitialVersion = versionFromBody(vBody, ownerID)
			initialVariables, _ = vBody["variables"].([]any)
		}
	}

	app, err := h.repo.Create(r.Context(), req)
	if err != nil {
		apierr.Write(w, err)
		return
	}

	// #307, create half — versionFromBody folds `variables` into `meta`, and
	// that is all the create paths did, so an agent created WITH variables
	// answered 201 with them echoed back (the echo is built from `meta`) and
	// then read back empty forever: the READ path (fetchVersionDetails, which
	// serves the editor's own reload) SELECTs `application_variables` and
	// ignores `meta`. The rows must be written after repo.Create, not before —
	// application_variables.application_version_id REFERENCES
	// application_versions(id) (migrations/001_initial.sql), so the version row
	// has to exist first, and the repository owns that insert in its own
	// transaction which this handler cannot join.
	//
	// That ordering means the variable write can fail with a version already
	// committed. The application is deleted rather than returning a 500 over a
	// surviving half-created agent: the same rule the InitialVersion comment
	// above states for a missing version row, and the FK's ON DELETE CASCADE
	// takes any rows that did land with it. A create the caller was told
	// failed must leave nothing behind for them to find in the list.
	if len(initialVariables) > 0 && len(app.Versions) > 0 {
		if err := h.replaceVersionVariables(r.Context(), projectID, app.Versions[0].ID, initialVariables); err != nil {
			if delErr := h.repo.Delete(r.Context(), projectID, app.ID); delErr != nil {
				// Nothing left to do for the caller — they get the failure
				// either way — but a stranded agent is worth a trail.
				slog.ErrorContext(r.Context(), "rollback of a half-created application failed",
					"application_id", app.ID, "err", delErr)
			}
			apierr.Write(w, err)
			return
		}
	}

	resp := map[string]any{
		"id":          app.ID,
		"name":        app.Name,
		"description": app.Description,
		"type":        app.Type,
		"icon":        app.Icon,
		// The owning PROJECT, as Get and Update answer it (#533). The create
		// response used to echo the caller, so one route said "user" and the
		// next said "project" for one field of one entity.
		"owner_id":   projectID,
		"created_at": app.CreatedAt,
	}
	if len(app.Versions) > 0 {
		// Create writes no tag association, so the echo says "none" and is
		// true. `tags` on the write body is read by UpdateVersion only.
		versionDetails := versionDetailsResponse(app.Versions[0], user, userID, nil)
		resp["version_details"] = versionDetails
		resp["versions"] = []any{versionDetails}
	}
	writeJSON(w, http.StatusCreated, resp)
}

// versionFromBody maps a pylon-shaped version write body onto the columns
// application_versions actually has. `variables` has no column of its own on
// the version row — pylon carries it inside meta alongside step_limit — so it
// is folded into meta here, matching what the prototype's raw SQL did.
func versionFromBody(vBody map[string]any, authorID int64) *applications.Version {
	name, _ := vBody["name"].(string)
	agentType, _ := vBody["agent_type"].(string)
	instructions, _ := vBody["instructions"].(string)
	welcomeMessage, _ := vBody["welcome_message"].(string)
	llmSettings, _ := vBody["llm_settings"].(map[string]any)
	starters, _ := vBody["conversation_starters"].([]any)

	meta, _ := vBody["meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	if _, ok := meta["step_limit"]; !ok {
		meta["step_limit"] = defaultStepLimit
	}
	if vars, ok := vBody["variables"].([]any); ok && len(vars) > 0 {
		meta["variables"] = vars
	}

	return &applications.Version{
		Name:                 name,
		AuthorID:             authorID,
		AgentType:            agentType,
		Instructions:         instructions,
		WelcomeMessage:       welcomeMessage,
		LLMSettings:          llmSettings,
		ConversationStarters: starters,
		Meta:                 meta,
	}
}

const defaultStepLimit = 25

// versionDetailsResponse builds the write echo. `tags` is the version's
// STORED tag list, read back by the caller after its own write — #345: the
// echo used to answer a hardcoded empty list, so a save that persisted tags
// still reported none and the editor blanked the control it had just sent.
// Pass nil from a path that writes no tags; the echo then says "none", which
// is what those paths really store.
func versionDetailsResponse(ver applications.Version, user auth.User, userID string, tags []map[string]any) map[string]any {
	if tags == nil {
		tags = []map[string]any{}
	}
	llm := ver.LLMSettings
	if llm == nil {
		llm = map[string]any{}
	}
	starters := ver.ConversationStarters
	if starters == nil {
		starters = []any{}
	}
	variables, _ := ver.Meta["variables"].([]any)
	if variables == nil {
		variables = []any{}
	}
	return map[string]any{
		"id":                    ver.ID,
		"application_id":        ver.ApplicationID,
		"name":                  ver.Name,
		"status":                ver.Status,
		"author_id":             userID,
		"created_at":            ver.CreatedAt,
		"author":                map[string]any{"id": userID, "email": user.Email, "name": user.Name},
		"meta":                  ver.Meta,
		"is_forked":             false,
		"is_default":            ver.IsDefault,
		"agent_type":            ver.AgentType,
		"instructions":          ver.Instructions,
		"welcome_message":       ver.WelcomeMessage,
		"llm_settings":          llm,
		"conversation_starters": starters,
		"tools":                 []any{},
		"variables":             variables,
		"tags":                  tags,
	}
}

func strVal(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	applicationID := chi.URLParam(r, "applicationID")
	user, ownerID, ok := principal(r)
	if !ok {
		apierr.Write(w, apierr.Unauthorized("an authenticated principal is required"))
		return
	}
	userID := strconv.FormatInt(ownerID, 10)

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}

	// Handle version update if present
	var versionData map[string]any
	if v, ok := body["version"].(map[string]any); ok {
		versionData = v
	}

	// Validate version IDs if provided
	if versionData != nil {
		vAppID := anyStr(versionData, "application_id")
		versionID := anyStr(versionData, "id")

		// application_id is required and must match
		if _, hasAppID := versionData["application_id"]; !hasAppID {
			apierr.Write(w, apierr.BadRequest("application_id is required in version"))
			return
		}
		if vAppID != applicationID {
			apierr.Write(w, apierr.BadRequest("application_id mismatch in version"))
			return
		}

		// version id is required
		if _, hasID := versionData["id"]; !hasID {
			apierr.Write(w, apierr.BadRequest("version id is required"))
			return
		}

		// Validate version_id exists for this application
		versions, _ := h.repo.ListVersions(r.Context(), projectID, applicationID)
		found := false
		for _, ver := range versions {
			if ver.ID == versionID {
				found = true
				break
			}
		}
		if !found {
			apierr.Write(w, apierr.BadRequest("version id mismatch: not found for this application"))
			return
		}

		// Reject renaming protected versions (base, latest)
		if vName := anyStr(versionData, "name"); vName != "" {
			for _, ver := range versions {
				if ver.ID == versionID {
					if (ver.Name == "latest" || ver.Name == "base") && vName != ver.Name {
						apierr.Write(w, apierr.BadRequest("cannot rename base/latest version"))
						return
					}
				}
			}
		}
	}

	// Update application fields
	var req applications.UpdateRequest
	req.ProjectID = projectID
	req.ApplicationID = applicationID
	if n, ok := body["name"].(string); ok {
		req.Name = &n
	}
	if d, ok := body["description"].(string); ok {
		req.Description = &d
	}
	if ic, ok := body["icon"].(string); ok {
		req.Icon = &ic
	}

	app, err := h.repo.Update(r.Context(), req)
	if err != nil {
		apierr.Write(w, err)
		return
	}

	resp := map[string]any{
		"id":          app.ID,
		"name":        app.Name,
		"description": app.Description,
		"icon":        app.Icon,
		// The application's owner is the row's owner_id, not whoever is
		// editing it now — an update does not transfer ownership.
		"owner_id":   app.OwnerID,
		"created_at": app.CreatedAt,
	}

	// Update version if provided
	if versionData != nil {
		versionID := anyStr(versionData, "id")
		if versionID != "" {
			v := applications.Version{
				Name: anyStr(versionData, "name"),
			}
			if instr, ok := versionData["instructions"].(string); ok {
				v.Instructions = instr
			}
			ver, vErr := h.repo.UpdateVersion(r.Context(), projectID, applicationID, versionID, v)
			if vErr == nil {
				// This branch writes name/instructions only, but the echo
				// still reports the version's real tags: they are stored
				// state, not something this request cleared (#345).
				s, _ := tenantSchema(projectID)
				resp["version_details"] = versionDetailsResponse(ver, user, userID, h.versionTagsOrEmpty(r.Context(), s, versionID))
			}
		}
	}

	writeJSON(w, http.StatusCreated, resp)
}

func anyStr(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	if v, ok := m[key].(float64); ok {
		return strconv.FormatInt(int64(v), 10)
	}
	return ""
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	applicationID := chi.URLParam(r, "applicationID")

	s, ok := tenantSchema(projectID)
	if !ok {
		apierr.Write(w, apierr.BadRequest("invalid project id"))
		return
	}

	// Guard: block deletion if any version is published/embedded
	if h.pool != nil {
		var pubCount int
		if err := h.pool.QueryRow(r.Context(), fmt.Sprintf(
			`SELECT COUNT(*) FROM %s.application_versions WHERE application_id = $1 AND status IN ('published','embedded')`, s),
			applicationID).Scan(&pubCount); err != nil {
			apierr.Write(w, apierr.Internal("failed to check published versions"))
			return
		}
		if pubCount > 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "Unpublish first. Cannot delete application with published versions.",
			})
			return
		}
	}

	if err := h.repo.Delete(r.Context(), projectID, applicationID); err != nil {
		// Idempotent delete: return 204 even if not found
		if strings.Contains(err.Error(), "not found") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		apierr.Write(w, err)
		return
	}

	// Clean up application_tools entries on other versions that reference this deleted app
	if h.pool != nil {
		// best-effort cleanup; ignore error so the 204 response is still sent
		_, _ = h.pool.Exec(r.Context(), fmt.Sprintf(`
			DELETE FROM %s.application_tools
			WHERE type = 'application'
			AND settings->>'application_id' = $1`, s), applicationID)
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) ListVersions(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	applicationID := chi.URLParam(r, "applicationID")

	versions, err := h.repo.ListVersions(r.Context(), projectID, applicationID)
	if err != nil {
		apierr.Write(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"items": versions})
}

func (h *Handler) GetVersion(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	applicationID := chi.URLParam(r, "applicationID")
	versionID := chi.URLParam(r, "versionID")

	resp := h.fetchVersionDetails(r.Context(), projectID, applicationID, versionID)
	if resp == nil {
		apierr.Write(w, apierr.NotFound("version not found"))
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) CreateVersion(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	applicationID := chi.URLParam(r, "applicationID")

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}

	name, _ := body["name"].(string)
	if name == "" {
		apierr.Write(w, apierr.BadRequest("version name is required"))
		return
	}

	user, ownerID, ok := principal(r)
	if !ok {
		apierr.Write(w, apierr.Unauthorized("an authenticated principal is required"))
		return
	}
	userID := strconv.FormatInt(ownerID, 10)

	ver, err := h.repo.CreateVersion(r.Context(), projectID, applicationID, *versionFromBody(body, ownerID))
	if err != nil {
		apierr.Write(w, err)
		return
	}

	// #307, the other create entry point ("save as a new version"). Same fold,
	// same gap, same ordering constraint as Create: the version row has to
	// exist before anything can reference it, so the rows go in afterwards and
	// a failure is undone by deleting the version that was just made. Unlike
	// Create there is no application to remove — the application predates this
	// request and keeps its other versions.
	//
	// Note this handler does NOT validate the project id itself; it relies on
	// replaceVersionVariables' own tenantSchema check, which is why that helper
	// returns a BadRequest rather than assuming a caller already screened it.
	if variables, _ := body["variables"].([]any); len(variables) > 0 {
		if err := h.replaceVersionVariables(r.Context(), projectID, ver.ID, variables); err != nil {
			if delErr := h.repo.DeleteVersion(r.Context(), projectID, applicationID, ver.ID); delErr != nil {
				slog.ErrorContext(r.Context(), "rollback of a half-created version failed",
					"application_id", applicationID, "version_id", ver.ID, "err", delErr)
			}
			apierr.Write(w, err)
			return
		}
	}
	// "Save as a new version" clones no tag association — see the `tags`
	// note on VersionWriteRequest in api/openapi/v2.yaml.
	writeJSON(w, http.StatusCreated, versionDetailsResponse(ver, user, userID, nil))
}

func (h *Handler) UpdateVersion(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	applicationID := chi.URLParam(r, "applicationID")
	versionID := chi.URLParam(r, "versionID")

	if _, ok := tenantSchema(projectID); !ok {
		apierr.Write(w, apierr.BadRequest("invalid project id"))
		return
	}

	// Guard: block update of published/embedded versions
	if h.pool != nil {
		s, _ := tenantSchema(projectID)
		var status string
		err := h.pool.QueryRow(r.Context(), fmt.Sprintf(
			`SELECT status FROM %s.application_versions WHERE application_id = $1 AND id = $2`, s),
			applicationID, versionID).Scan(&status)
		if err == nil && (status == "published" || status == "embedded") {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "Published version can not be updated. Unpublish first.",
			})
			return
		}
	}

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		apierr.Write(w, apierr.BadRequest("invalid request body"))
		return
	}

	user, ownerID, ok := principal(r)
	if !ok {
		apierr.Write(w, apierr.Unauthorized("an authenticated principal is required"))
		return
	}

	// Only the keys the caller actually sent are written: the repository
	// leaves an unset field alone rather than blanking it. The previous
	// implementation opened its SET list with `updated_at = now()` —
	// application_versions has no updated_at column (migrations/001_initial
	// .sql), so every version update failed with 42P01 and returned a 500.
	v := applications.Version{}
	if name, ok := body["name"].(string); ok {
		v.Name = name
	}
	if instructions, ok := body["instructions"].(string); ok {
		v.Instructions = instructions
	}
	if welcomeMessage, ok := body["welcome_message"].(string); ok {
		v.WelcomeMessage = welcomeMessage
	}
	if agentType, ok := body["agent_type"].(string); ok {
		v.AgentType = agentType
	}
	if llm, ok := body["llm_settings"].(map[string]any); ok {
		v.LLMSettings = llm
	}
	if starters, ok := body["conversation_starters"].([]any); ok {
		v.ConversationStarters = starters
	}
	if meta, ok := body["meta"].(map[string]any); ok {
		v.Meta = meta
	}
	// #307 — `variables` had NO branch here at all, so the agent editor's
	// variables were accepted with a 201 and silently discarded on every
	// save; only CreateVersion persisted them, and only into `meta`.
	//
	// Presence-based, not `len(vars) > 0` as on the create path: on an
	// update, an empty array is a real, distinguishable user action
	// ("I deleted my last variable"), and dropping it would leave exactly
	// the silent no-op this branch exists to fix. A body with no
	// `variables` key at all still leaves the stored value alone.
	variables, hasVariables := body["variables"].([]any)
	// #345 — `tags` had no branch here either, and the two read paths
	// answered a hardcoded empty list, so a tag edit was accepted with a 201
	// and lost twice over. Presence-based for the same reason `variables`
	// above is: an empty array is a real user action ("I removed my last
	// tag") and must delete the association rows, while a body with no
	// `tags` key leaves the stored set alone.
	tags, hasTags := body["tags"].([]any)
	// The write-side fold `versionFromBody` performs (variables have no
	// column on application_versions), kept so this endpoint's own 201 echo
	// — built by `versionDetailsResponse`, which reads `meta["variables"]`
	// — reports what was actually saved. It must run AFTER the `meta`
	// assignment above or the client's stale `meta.variables` (which it
	// spreads from the stored blob) would win over the edit.
	if hasVariables {
		if v.Meta == nil {
			v.Meta = map[string]any{}
		}
		v.Meta["variables"] = variables
	}
	// Pipeline flow-graph layout. Without this the pipeline editor's Save
	// returned 200 while silently discarding every node/edge edit (#135) —
	// the graph itself round-trips through `instructions` (the pipeline
	// YAML), this column carries the laid-out node/edge positions the
	// editor restores on reload.
	if pipelineSettings, ok := body["pipeline_settings"].(map[string]any); ok {
		v.PipelineSettings = pipelineSettings
	}

	ver, err := h.repo.UpdateVersion(r.Context(), projectID, applicationID, versionID, v)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	// #307, second half — the meta fold above is NOT enough on its own, and a
	// green "the row's meta now holds my variables" assertion hides why: the
	// two sides of the round trip read different stores. Every write path
	// puts variables in `meta`, but the READ path (`fetchVersionDetails`,
	// which serves GET /version/... and therefore the editor's own reload)
	// SELECTs them from the `application_variables` table and ignores `meta`
	// entirely. So a save whose only effect was on `meta` still came back
	// empty on reload — the same 200-that-lies shape the field already had.
	// This replaces the version's rows in that table, the pylon-shaped store
	// `eliteacore`'s own version-create path already writes
	// (eliteacore/handler.go:2485-2497).
	if hasVariables {
		if err := h.replaceVersionVariables(r.Context(), projectID, versionID, variables); err != nil {
			apierr.Write(w, err)
			return
		}
	}
	if hasTags {
		if err := h.replaceVersionTags(r.Context(), projectID, versionID, tags); err != nil {
			apierr.Write(w, err)
			return
		}
	}
	// The echo reports the tags the database holds AFTER the write, not the
	// list the caller sent: a duplicate name collapses to one row and a
	// blank name is dropped, so echoing the request would over-report.
	s, _ := tenantSchema(projectID)
	writeJSON(w, http.StatusCreated, versionDetailsResponse(ver, user, strconv.FormatInt(ownerID, 10), h.versionTagsOrEmpty(r.Context(), s, versionID)))
}

// versionTagsOrEmpty reads the version's tags in the shape pylon's
// TagListModel serializes — {id, name, data} — which is what the agent
// editor's tag control binds to
// (legacy/plugins/elitea_core/models/pd/version.py:73-74).
//
// It answers an empty list, never nil, so the JSON always carries `[]`
// rather than `null`. A nil pool (the repository-only unit wiring) answers
// an empty list too, matching every other pool-guarded read in this file. A
// query failure is logged and answered as "no tags": the caller is a
// response builder with no error channel, and a tag read must not turn a
// readable version into a 500.
func (h *Handler) versionTagsOrEmpty(ctx context.Context, schema, versionID string) []map[string]any {
	tags := make([]map[string]any, 0)
	if h.pool == nil || schema == "" {
		return tags
	}
	rows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT t.id, t.name, COALESCE(t.data::text, 'null')
		FROM %s.application_version_tag_association a
		JOIN %s.tags t ON t.id = a.tag_id
		WHERE a.version_id = $1
		ORDER BY t.name`, schema, schema), versionID)
	if err != nil {
		slog.ErrorContext(ctx, "could not read the tags of a version",
			"schema", schema, "version_id", versionID, "err", err)
		return tags
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		var name, dataText string
		if err := rows.Scan(&id, &name, &dataText); err != nil {
			slog.ErrorContext(ctx, "could not scan a tag of a version",
				"schema", schema, "version_id", versionID, "err", err)
			return tags
		}
		var data any
		// dataText is DB-stored JSON — cannot fail.
		_ = json.Unmarshal([]byte(dataText), &data)
		tags = append(tags, map[string]any{"id": id, "name": name, "data": data})
	}
	if err := rows.Err(); err != nil {
		slog.ErrorContext(ctx, "could not read every tag of a version",
			"schema", schema, "version_id", versionID, "err", err)
	}
	return tags
}

// tagNameFrom reads the name out of one entry of a `tags` write list.
//
// The wire shape is the object VersionTag declares ({id, name, data}), which
// is what pylon's PromptTagUpdateModel accepts and what the editor sends. A
// bare string is accepted as well, because the skills write path speaks that
// shape (repos/skills.go upsertBaseSkillVersion takes []string) and a client
// that reuses it must not have its tags silently dropped.
func tagNameFrom(raw any) (string, any) {
	if name, ok := raw.(string); ok {
		return strings.TrimSpace(name), nil
	}
	entry, _ := raw.(map[string]any)
	if entry == nil {
		return "", nil
	}
	name, _ := entry["name"].(string)
	return strings.TrimSpace(name), entry["data"]
}

// replaceVersionTags makes `application_version_tag_association` match the
// list the caller sent, exactly — deleting the associations they dropped,
// not only adding the ones they kept. It mirrors
// repos/skills.go's upsertBaseSkillVersion, which does the same job for a
// skill version, and pylon's own `version.tags.clear()` then re-append
// (legacy/plugins/elitea_core/utils/application_utils.py:214-227).
//
// Transactional: a partial application would leave the version showing a
// mixture of the old and the new list with nothing reporting a failure. A
// nil pool is a no-op, matching replaceVersionVariables.
//
// Tags are keyed by name — `tags.name` is UNIQUE per tenant schema, so one
// name is one row shared by every version that carries it. An entry's `id`
// is therefore ignored, exactly as pylon ignores it. `data` is written only
// when the name is NEW: an existing tag keeps the data it already has,
// because it belongs to every other version using that name too.
func (h *Handler) replaceVersionTags(ctx context.Context, projectID, versionID string, tags []any) error {
	if h.pool == nil {
		return nil
	}
	s, ok := tenantSchema(projectID)
	if !ok {
		return apierr.BadRequest("invalid project id")
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return apierr.Internal("could not save tags")
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`DELETE FROM %s.application_version_tag_association WHERE version_id = $1`, s), versionID); err != nil {
		return apierr.Internal("could not save tags")
	}
	seen := make(map[string]bool, len(tags))
	for _, raw := range tags {
		name, data := tagNameFrom(raw)
		// A nameless tag cannot be stored (`tags.name` is NOT NULL) and the
		// editor emits one the moment a user opens the control, so it is
		// skipped rather than failing the whole save — the same rule
		// replaceVersionVariables applies to a nameless variable.
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true

		var dataJSON []byte
		if data != nil {
			encoded, err := json.Marshal(data)
			if err != nil {
				return apierr.BadRequest("invalid tag data")
			}
			dataJSON = encoded
		}
		var tagID int
		if err := tx.QueryRow(ctx, fmt.Sprintf(`
			INSERT INTO %s.tags (name, data) VALUES ($1, $2::jsonb)
			ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
			RETURNING id`, s), name, dataJSON).Scan(&tagID); err != nil {
			return apierr.Internal("could not save tags")
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s.application_version_tag_association (version_id, tag_id) VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, s), versionID, tagID); err != nil {
			return apierr.Internal("could not save tags")
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return apierr.Internal("could not save tags")
	}
	return nil
}

// replaceVersionVariables makes `application_variables` match the list the
// caller sent, exactly — deleting the rows they dropped, not only upserting
// the ones they kept. Transactional: a partial application would leave the
// version showing a mixture of the old and new lists with nothing reporting
// a failure. A nil pool (the unit-test/repo-only wiring) is a no-op, matching
// every other pool-guarded branch in this file.
func (h *Handler) replaceVersionVariables(ctx context.Context, projectID, versionID string, variables []any) error {
	if h.pool == nil {
		return nil
	}
	s, ok := tenantSchema(projectID)
	if !ok {
		return apierr.BadRequest("invalid project id")
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return apierr.Internal("could not save variables")
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`DELETE FROM %s.application_variables WHERE application_version_id = $1`, s), versionID); err != nil {
		return apierr.Internal("could not save variables")
	}
	for _, raw := range variables {
		entry, _ := raw.(map[string]any)
		if entry == nil {
			continue
		}
		name, _ := entry["name"].(string)
		if name == "" {
			// The table's own UNIQUE (application_version_id, name) makes a
			// nameless variable meaningless, and the editor emits a blank
			// row the moment a user clicks "add" — skipping it here is what
			// keeps that half-typed row from failing the whole save.
			continue
		}
		value, _ := entry["value"].(string)
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			`INSERT INTO %s.application_variables (application_version_id, name, value) VALUES ($1, $2, $3)
			 ON CONFLICT (application_version_id, name) DO UPDATE SET value = EXCLUDED.value`, s),
			versionID, name, value); err != nil {
			return apierr.Internal("could not save variables")
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return apierr.Internal("could not save variables")
	}
	return nil
}

func (h *Handler) DeleteVersion(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	applicationID := chi.URLParam(r, "applicationID")
	versionID := chi.URLParam(r, "versionID")

	if _, ok := tenantSchema(projectID); !ok {
		apierr.Write(w, apierr.BadRequest("invalid project id"))
		return
	}

	// Guard: block deletion of published/embedded versions
	if h.pool != nil {
		s, _ := tenantSchema(projectID)
		var status string
		err := h.pool.QueryRow(r.Context(), fmt.Sprintf(
			`SELECT status FROM %s.application_versions WHERE application_id = $1 AND id = $2`, s),
			applicationID, versionID).Scan(&status)
		if err == nil && (status == "published" || status == "embedded") {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "Unpublish first. Cannot delete a published version.",
			})
			return
		}
	}

	if err := h.repo.DeleteVersion(r.Context(), projectID, applicationID, versionID); err != nil {
		apierr.Write(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) SetDefaultVersion(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	applicationID := chi.URLParam(r, "applicationID")

	// UI sends PATCH with body {"version_id": 123}; fall back to URL path param for backward compat.
	versionID := chi.URLParam(r, "versionID")
	var body struct {
		VersionID string `json:"version_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err == nil && body.VersionID != "" {
		versionID = body.VersionID
	}

	if versionID == "" {
		apierr.Write(w, apierr.BadRequest("version_id is required"))
		return
	}

	if err := h.repo.SetDefaultVersion(r.Context(), projectID, applicationID, versionID); err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) BatchReplaceVersion(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	oldVersionID := chi.URLParam(r, "oldVersionID")
	newVersionID := chi.URLParam(r, "newVersionID")
	deleteOld := r.URL.Query().Get("delete_old") == "true"

	if err := h.repo.BatchReplaceVersion(r.Context(), projectID, oldVersionID, newVersionID, deleteOld); err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// secretHeaderRefusal reports why the `X-SECRET` request header cannot pass the
// check pylon's `check_secret_header` makes, or nil when it passes.
//
// THE CHECK IS CLOSED. Pylon reads
// `secrets.get("secrets_header_value", "secret")`
// (legacy/plugins/elitea_core/utils/secrets.py:4-9), so a project whose vault
// holds no value accepts the literal string "secret" — a value every reader of
// that source knows. This handler refuses such a project instead (#408 step 3).
//
// The refusal is safe now because the value always exists and always travels:
//   - provisioning seals a random value into every new project vault
//     (internal/application/projectprovisioning/steps.go, #408 step 1);
//   - the start-up backfill gives every older project one
//     (internal/api/v2/secrets/secrets_header_value.go, #408 step 2);
//   - the runtime hands the project's own value to the worker, which puts it on
//     every SDK call (internal/infra/storage/index_runtime_context.go and
//     services/elitea-worker-python/src/elitea_worker/agents/client_context.py).
//
// So a project with no value is a FAULT, not a state a caller may authenticate
// in, and the three causes answer differently on purpose:
//
//	no value in a readable vault   403 — provisioning or the backfill did not
//	                                     reach this project. Nothing can pass.
//	the vault will not open        403 — the same for the caller, and it is
//	                                     already the pre-#408 behaviour.
//	a value that does not match    400 — pylon's exact status and body.
//
// A caller here is already authenticated and already holds the
// `models.applications.version.details` project permission, so a message that
// names the missing secret discloses nothing the caller may not read, and it is
// the one message that names the repair.
func (h *Handler) secretHeaderRefusal(
	ctx context.Context,
	secretsReader secretsHeaderReader,
	projectID, received string,
) error {
	expected, err := secretsReader.ResolveSecretValue(ctx, projectID, currentSecretsHeaderName)
	switch {
	case err == nil && expected != "":
	case err == nil,
		errors.Is(err, secrets.ErrSecretNotFound),
		errors.Is(err, secrets.ErrVaultAbsent):
		// `err == nil` with an empty value is the same fault: a vault that
		// holds the key with no text would authenticate an empty header, and
		// an empty header is what a caller that sets none sends.
		slog.WarnContext(ctx,
			"the project has no X-SECRET value, so the version details route refuses every caller",
			"project_id", projectID,
			"secret", currentSecretsHeaderName)
		return apierr.Forbidden(
			"This project has no " + currentSecretsHeaderName + " secret. " +
				"The version details route cannot authenticate a caller until the project has one.")
	default:
		// A vault that exists and will not open. To treat it as "no secret set"
		// would make a broken vault an open door, so it refuses.
		slog.WarnContext(ctx,
			"the project vault will not open, so the version details route refuses every caller",
			"project_id", projectID)
		return apierr.Forbidden("This project vault cannot be read.")
	}
	// Constant-time comparison: the header is attacker-supplied and the vault
	// value is a shared credential.
	if subtle.ConstantTimeCompare([]byte(received), []byte(expected)) != 1 {
		// The exact pylon status and body for this failure.
		return apierr.BadRequest("Invalid secret header")
	}
	return nil
}

// secretsHeaderReader is the one vault operation the `X-SECRET` check makes.
//
// It is an interface because every branch above is decided by what this call
// returns. A test that must start PostgreSQL to reach those branches proves the
// wiring and not the rule, and the rule is the part that refuses.
type secretsHeaderReader interface {
	ResolveSecretValue(ctx context.Context, projectID, secretRef string) (string, error)
}

// currentSecretsHeaderName is the project-vault secret pylon compares the
// `X-SECRET` request header against.
const currentSecretsHeaderName = "secrets_header_value"

// GetVersionExpanded returns version details with expanded and unsecreted
// toolkit configurations.
//
// It serves the SDK's `get_app_version_details`
// (elitea_sdk/runtime/clients/client.py:681-688), which issues a body-less
// PATCH. The handler therefore never reads the request body: a PATCH on this
// path is a READ, not a partial update, and pylon's handler
// (legacy/plugins/elitea_core/api/v2/version.py:107-156) reads no body either.
//
// Authentication follows pylon: the `X-SECRET` header must equal the PROJECT
// vault's `secrets_header_value`. The retired implementation compared it to the
// `APPLICATION_SECRET_KEY` process environment variable — one value for every
// project, which no deployment sets — so the route could never have
// authenticated a real SDK caller.
func (h *Handler) GetVersionExpanded(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	applicationID := chi.URLParam(r, "applicationID")
	versionID := chi.URLParam(r, "versionID")

	if h.pool == nil {
		apierr.Write(w, apierr.Internal("database pool not available"))
		return
	}

	ctx := r.Context()
	s, ok := tenantSchema(projectID)
	if !ok {
		apierr.Write(w, apierr.BadRequest("invalid project id"))
		return
	}

	secretsHandler := secrets.NewHandler(h.pool)
	if refusal := h.secretHeaderRefusal(ctx, secretsHandler, projectID, r.Header.Get("X-SECRET")); refusal != nil {
		apierr.Write(w, refusal)
		return
	}

	var id int
	var appID int
	var name, status, agentType string
	var instructions, welcomeMsg *string
	var llmSettingsJSON, metaJSON, startersJSON, pipelineSettingsJSON []byte
	var createdAt interface{}
	var authorID *int

	err := h.pool.QueryRow(ctx, fmt.Sprintf(`
		SELECT v.id, v.application_id, v.name, v.status, v.created_at,
			v.agent_type, v.instructions, v.welcome_message,
			COALESCE(v.llm_settings::text, '{}'), COALESCE(v.meta::text, '{}'),
			COALESCE(v.conversation_starters::text, '[]'),
			COALESCE(v.pipeline_settings::text, '{}'),
			v.author_id
		FROM %s.application_versions v
		WHERE v.application_id = $1 AND v.id = $2`, s), applicationID, versionID).Scan(
		&id, &appID, &name, &status, &createdAt,
		&agentType, &instructions, &welcomeMsg,
		&llmSettingsJSON, &metaJSON, &startersJSON, &pipelineSettingsJSON,
		&authorID,
	)
	if err != nil {
		apierr.Write(w, apierr.NotFound("version not found"))
		return
	}

	var llmSettings, meta, starters, pipelineSettings any
	// JSON was read from DB via COALESCE — cannot be invalid JSON
	_ = json.Unmarshal(llmSettingsJSON, &llmSettings)
	_ = json.Unmarshal(metaJSON, &meta)
	_ = json.Unmarshal(startersJSON, &starters)
	_ = json.Unmarshal(pipelineSettingsJSON, &pipelineSettings)

	instrVal := ""
	if instructions != nil {
		instrVal = *instructions
	}
	welcomeVal := ""
	if welcomeMsg != nil {
		welcomeVal = *welcomeMsg
	}

	// Fetch tools from entity_tool_mapping
	tools := make([]map[string]any, 0)
	toolRows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT etm.id, etm.tool_id, etm.entity_type, COALESCE(etm.selected_tools::text, '{}'),
			t.name, t.type, t.settings
		FROM %s.entity_tool_mapping etm
		LEFT JOIN %s.elitea_tools t ON t.id = etm.tool_id
		WHERE etm.entity_version_id = $1`, s, s), versionID)
	if err == nil {
		defer toolRows.Close()
		for toolRows.Next() {
			var etmID, toolID int
			var entityType, selectedToolsStr string
			var tName, tType *string
			var tConfig []byte
			if err := toolRows.Scan(&etmID, &toolID, &entityType, &selectedToolsStr, &tName, &tType, &tConfig); err != nil {
				continue
			}
			var selectedTools any
			// selectedToolsStr is COALESCE'd DB JSON — cannot fail
			_ = json.Unmarshal([]byte(selectedToolsStr), &selectedTools)
			var config map[string]any
			if tConfig != nil {
				// tConfig is DB-stored JSON — cannot fail
				_ = json.Unmarshal(tConfig, &config)
			}
			if config == nil {
				config = map[string]any{}
			}

			// Expand configurations in settings
			expandedSettings := h.expandToolSettings(ctx, s, projectID, config, secretsHandler)

			tool := map[string]any{
				"id":             etmID,
				"tool_id":        toolID,
				"entity_type":    entityType,
				"selected_tools": selectedTools,
				"settings":       expandedSettings,
			}
			if tName != nil {
				tool["name"] = *tName
			}
			if tType != nil {
				tool["type"] = *tType
			}
			tools = append(tools, tool)
		}
	}

	authorIDStr := ""
	if authorID != nil {
		authorIDStr = strconv.Itoa(*authorID)
	}

	// Also fetch from application_tools (sub-agent references)
	projIDInt, _ := strconv.Atoi(projectID)
	appToolRows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT id, name, type, settings::text
		FROM %s.application_tools
		WHERE application_version_id = $1`, s), versionID)
	if err == nil {
		defer appToolRows.Close()
		for appToolRows.Next() {
			var atID int
			var atName, atType, settingsStr string
			if err := appToolRows.Scan(&atID, &atName, &atType, &settingsStr); err != nil {
				continue
			}
			var settings any
			// settingsStr is DB-stored JSON — cannot fail
			_ = json.Unmarshal([]byte(settingsStr), &settings)
			tools = append(tools, map[string]any{
				"id":         atID,
				"name":       atName,
				"type":       atType,
				"settings":   settings,
				"author_id":  authorIDStr,
				"project_id": projIDInt,
			})
		}
	}

	variables, err := h.versionVariables(ctx, s, versionID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	attachedSkills, err := h.versionAttachedSkills(ctx, s, versionID, agentType)
	if err != nil {
		apierr.Write(w, err)
		return
	}

	metaMap, _ := meta.(map[string]any)

	resp := map[string]any{
		"id":                    strconv.Itoa(id),
		"application_id":        strconv.Itoa(appID),
		"name":                  name,
		"status":                status,
		"created_at":            createdAt,
		"agent_type":            agentType,
		"instructions":          instrVal,
		"welcome_message":       welcomeVal,
		"llm_settings":          llmSettings,
		"meta":                  meta,
		"conversation_starters": starters,
		"pipeline_settings":     pipelineSettings,
		"author_id":             authorIDStr,
		"tools":                 tools,
		// #345 — the SDK reads this expanded detail too, and it answered a
		// hardcoded empty list here as well.
		"tags":            h.versionTagsOrEmpty(ctx, s, versionID),
		"variables":       variables,
		"icon_meta":       iconMetaFromMeta(metaMap),
		"is_forked":       isForkedFromMeta(metaMap),
		"attached_skills": attachedSkills,
	}
	writeJSON(w, http.StatusOK, resp)
}

// iconMetaFromMeta mirrors ApplicationVersionDetailModel.set_icon_meta
// (legacy/plugins/elitea_core/models/pd/version.py:288-295): the value lives
// inside the `meta` JSONB under `icon_meta`, and a missing or falsy value
// becomes an empty object rather than null. `meta` keeps its own copy — pylon
// does not strip the key.
func iconMetaFromMeta(meta map[string]any) map[string]any {
	value, ok := meta["icon_meta"].(map[string]any)
	if !ok || value == nil {
		return map[string]any{}
	}
	return value
}

// isForkedFromMeta mirrors ApplicationVersionDetailModel.set_is_forked
// (legacy/plugins/elitea_core/models/pd/version.py:282-287): true when `meta`
// carries BOTH `parent_entity_id` and `parent_project_id`. Pylon tests key
// presence and never inspects the values, so a null value still yields true.
func isForkedFromMeta(meta map[string]any) bool {
	_, hasEntity := meta["parent_entity_id"]
	_, hasProject := meta["parent_project_id"]
	return hasEntity && hasProject
}

// versionVariables reads the version's variables in the shape the SDK iterates.
//
// The SDK indexes `var['name']` directly
// (elitea_sdk/runtime/clients/client.py:819-821), so a missing `name` key
// raises there rather than degrading. `value` is emitted as "" and never null
// for the same reason, which also matches pylon: its
// ApplicationVariableDetailedModel types `value` as a non-optional str.
func (h *Handler) versionVariables(ctx context.Context, schema, versionID string) ([]map[string]any, error) {
	rows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT id, name, COALESCE(value, '')
		FROM %s.application_variables
		WHERE application_version_id = $1
		ORDER BY id`, schema), versionID)
	if err != nil {
		return nil, apierr.Internal("could not read the version variables")
	}
	defer rows.Close()

	variables := make([]map[string]any, 0)
	for rows.Next() {
		var variableID int
		var variableName, variableValue string
		if err := rows.Scan(&variableID, &variableName, &variableValue); err != nil {
			return nil, apierr.Internal("could not read the version variables")
		}
		variables = append(variables, map[string]any{
			"id":    variableID,
			"name":  variableName,
			"value": variableValue,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, apierr.Internal("could not read the version variables")
	}
	return variables, nil
}

// versionAttachedSkills reads the skill registry the SDK binds `load_skill`
// against.
//
// The key is `attached_skills`, NOT `skills`. Pylon builds a 7-key `skills`
// list, then `apply_runtime_skills`
// (legacy/plugins/elitea_core/utils/skill_utils.py:1547-1567) replaces it: it
// appends this 5-key projection under `attached_skills` and POPS `skills`
// entirely. The SDK reads only the new key
// (elitea_sdk/runtime/tools/application.py:453-454), so emitting `skills`
// would serve a key no consumer reads and omit the one that binds the tools.
//
// Two pylon filter rules are reproduced:
//   - a pipeline contributes no skills at all;
//   - a skill with no name, or with blank instructions, is dropped, because
//     `load_skill` could neither name nor serve it.
func (h *Handler) versionAttachedSkills(
	ctx context.Context,
	schema, versionID, agentType string,
) ([]map[string]any, error) {
	attached := make([]map[string]any, 0)
	if agentType == "pipeline" {
		return attached, nil
	}

	rows, err := h.pool.Query(ctx, fmt.Sprintf(`
		SELECT mapping.skill_id,
			skill.name,
			skill.description,
			COALESCE(version.instructions, ''),
			COALESCE(version.meta -> 'icon_meta', 'null'::jsonb)::text
		FROM %s.entity_skill_mapping AS mapping
		JOIN %s.skills AS skill ON skill.id = mapping.skill_id
		LEFT JOIN %s.skill_versions AS version ON version.id = mapping.skill_version_id
		WHERE mapping.entity_version_id = $1
			AND mapping.entity_type = 'agent'
		ORDER BY mapping.id`, schema, schema, schema), versionID)
	if err != nil {
		return nil, apierr.Internal("could not read the attached skills")
	}
	defer rows.Close()

	for rows.Next() {
		var skillID int
		var skillName, skillDescription, instructions, iconMetaJSON string
		if err := rows.Scan(&skillID, &skillName, &skillDescription, &instructions, &iconMetaJSON); err != nil {
			return nil, apierr.Internal("could not read the attached skills")
		}
		if skillName == "" || strings.TrimSpace(instructions) == "" {
			continue
		}
		var iconMeta any
		// The column is DB-stored JSON, coalesced to 'null' — it cannot be invalid.
		_ = json.Unmarshal([]byte(iconMetaJSON), &iconMeta)
		attached = append(attached, map[string]any{
			"skill_id":     skillID,
			"name":         skillName,
			"description":  skillDescription,
			"icon_meta":    iconMeta,
			"instructions": instructions,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, apierr.Internal("could not read the attached skills")
	}
	return attached, nil
}

// expandToolSettings expands configuration references in tool settings.
// For fields like "github_configuration": {"elitea_title": "...", "private": ...},
// it fetches the full configuration from the database and resolves secrets.
func (h *Handler) expandToolSettings(ctx context.Context, schema, projectID string, settings map[string]any, secretsHandler *secrets.Handler) map[string]any {
	expanded := make(map[string]any, len(settings))
	for k, v := range settings {
		expanded[k] = v
	}

	for key, val := range settings {
		if !strings.HasSuffix(key, "_configuration") {
			continue
		}
		ref, ok := val.(map[string]any)
		if !ok {
			continue
		}
		title, _ := ref["elitea_title"].(string)
		if title == "" {
			continue
		}
		private, _ := ref["private"].(bool)

		// Look up configuration by elitea_title
		var configID int
		var configUUID, configType, configProjectID string
		var configData []byte
		var configShared bool

		err := h.pool.QueryRow(ctx, fmt.Sprintf(`
			SELECT id, COALESCE(uuid::text, ''), type, project_id, data, shared
			FROM %s.configuration
			WHERE elitea_title = $1
			LIMIT 1`, schema), title).Scan(
			&configID, &configUUID, &configType, &configProjectID, &configData, &configShared,
		)
		if err != nil {
			// Try shared configs if private lookup fails
			if private {
				continue
			}
			err = h.pool.QueryRow(ctx, fmt.Sprintf(`
				SELECT id, COALESCE(uuid::text, ''), type, project_id, data, shared
				FROM %s.configuration
				WHERE elitea_title = $1 AND shared = true
				LIMIT 1`, schema), title).Scan(
				&configID, &configUUID, &configType, &configProjectID, &configData, &configShared,
			)
			if err != nil {
				continue
			}
		}

		var data map[string]any
		// configData is DB-stored JSON from the configuration table — cannot be invalid
		_ = json.Unmarshal(configData, &data)
		if data == nil {
			data = map[string]any{}
		}

		// Resolve secrets in data
		for dk, dv := range data {
			sv, ok := dv.(string)
			if !ok {
				continue
			}
			if strings.HasPrefix(sv, "{{secret.") {
				resolved, err := secretsHandler.ResolveSecretValue(ctx, projectID, sv)
				if err == nil {
					data[dk] = resolved
				}
			}
		}

		// Build expanded configuration
		expandedConfig := make(map[string]any)
		expandedConfig["private"] = private
		expandedConfig["elitea_title"] = title
		expandedConfig["configuration_uuid"] = configUUID
		expandedConfig["configuration_project_id"] = configProjectID
		expandedConfig["configuration_type"] = configType
		for dk, dv := range data {
			expandedConfig[dk] = dv
		}
		expanded[key] = expandedConfig
	}

	return expanded
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	// connection already committed; ignore write error
	_ = json.NewEncoder(w).Encode(v)
}
