package eliteacore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/marketplace"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/entitylimits"
)

// The hire (ADR-0051 decision 3). Install builds a project agent of origin
// `installed` from the listing version's snapshot, never from the public twin;
// its definition is read-only (the applications handler refuses its edits).
// Fork builds an editable agent that records its origin. Both write the agent,
// its nested agents, its tags, its Marketplace record and its registry row in
// one transaction.
//
// An agent above low tier, or with a write tool, is created with every tool
// disabled and a change request of kind `hire` (ADR-0040). The bindings the
// person chose wait in the Marketplace record, and the final approval attaches
// the tools (repos.AgentLifecycle.finalizeMarketplace).

// hireGovernor is the lifecycle side of a hire. *repos.AgentLifecycle
// satisfies it.
type hireGovernor interface {
	RecordMarketplaceChangeTx(ctx context.Context, tx pgx.Tx, change agentlifecycle.NewMarketplaceChange) (agentlifecycle.ChangeRequest, error)
}

type hireBindingBody struct {
	RequirementKey string `json:"requirement_key"`
	ToolkitID      string `json:"toolkit_id"`
	Disabled       bool   `json:"disabled"`
}

type hireBody struct {
	ListingID string            `json:"listing_id"`
	VersionID string            `json:"version_id"`
	Mode      string            `json:"mode"`
	Name      string            `json:"name"`
	Bindings  []hireBindingBody `json:"bindings"`
}

func hireError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": code, "msg": msg})
}

// checkBindings validates the bindings against the requirements and answers the
// ones to keep. A required toolkit or MCP type needs a binding to a project
// toolkit of that type, or `disabled`. A skill requirement may stay unbound.
func (h *Handler) checkBindings(ctx context.Context, schema string, reqs marketplace.Requirements, given []hireBindingBody) ([]marketplace.Binding, string, string) {
	byKey := map[string]hireBindingBody{}
	for _, binding := range given {
		byKey[binding.RequirementKey] = binding
	}
	out := []marketplace.Binding{}
	for _, item := range reqs.Items {
		if item.Kind == marketplace.RequirementAgent {
			continue
		}
		binding, ok := byKey[item.Key]
		needed := item.Kind == marketplace.RequirementToolkit || item.Kind == marketplace.RequirementMCP
		if !ok || (!binding.Disabled && binding.ToolkitID == "") {
			if needed {
				return nil, "binding_missing", "Bind " + item.Type + " to a project toolkit, or hire it without it."
			}
			out = append(out, marketplace.Binding{RequirementKey: item.Key, Disabled: true})
			continue
		}
		if binding.Disabled {
			out = append(out, marketplace.Binding{RequirementKey: item.Key, Disabled: true})
			continue
		}
		id, err := strconv.ParseInt(binding.ToolkitID, 10, 64)
		if err != nil || id <= 0 {
			return nil, "binding_invalid", "The binding for " + item.Type + " names no project entity."
		}
		var found string
		if needed {
			err = h.pool.QueryRow(ctx, fmt.Sprintf(`SELECT type FROM %s.elitea_tools WHERE id = $1`, schema), id).Scan(&found)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && found != item.Type) {
				return nil, "binding_invalid", "The toolkit bound to " + item.Type + " must be a project toolkit of that type."
			}
		} else {
			err = h.pool.QueryRow(ctx, fmt.Sprintf(`SELECT name FROM %s.skills WHERE id = $1`, schema), id).Scan(&found)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, "binding_invalid", "The skill bound to " + item.Name + " is not a skill of this project."
			}
		}
		if err != nil {
			slog.ErrorContext(ctx, "marketplace hire: check a binding failed", "key", item.Key, "error", err)
			return nil, "internal_error", "Failed to check the bindings."
		}
		out = append(out, marketplace.Binding{RequirementKey: item.Key, ToolkitID: id})
	}
	return out, "", ""
}

func jsonText(raw json.RawMessage, fallback string) string {
	if len(raw) == 0 || string(raw) == "null" {
		return fallback
	}
	return string(raw)
}

func jsonOrNil(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return string(raw)
}

// insertDefinitionVersion writes one version of an installed or forked agent
// from a snapshot definition.
func insertDefinitionVersion(ctx context.Context, q relationQuerier, schema string, applicationID int64, name, status string,
	authorID int64, version marketplace.VersionDefinition, metaJSON string, avatar any) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.application_versions
    (application_id, name, status, author_id, llm_settings, instructions, conversation_starters, welcome_message,
     agent_type, meta, pipeline_settings, soul, avatar_pack, created_at)
VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7::jsonb, $8, $9, $10::jsonb, $11::jsonb, $12::jsonb, $13::jsonb,
        now() AT TIME ZONE 'UTC')
RETURNING id`, schema),
		applicationID, name, status, authorID, jsonText(version.LLMSettings, "{}"), version.Instructions,
		jsonText(version.ConversationStarters, "[]"), version.WelcomeMessage, version.AgentType, metaJSON,
		jsonText(version.PipelineSettings, "{}"), jsonOrNil(version.Soul), avatar).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert the version: %w", err)
	}
	return id, nil
}

// installNested writes the agents a snapshot's agent uses as tools: each as an
// embedded application and version, linked to its parent as a sub-agent
// reference, as a publish embeds them.
func installNested(ctx context.Context, q relationQuerier, schema string, ownerProject int, authorID int64,
	parentVersionID int64, node marketplace.DefinitionNode) error {
	for _, nested := range node.NestedAgents {
		child := nested.Node
		var childApp int64
		if err := q.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.applications (name, description, owner_id, created_at, updated_at)
VALUES ($1, $2, $3, now() AT TIME ZONE 'UTC', now() AT TIME ZONE 'UTC') RETURNING id`, schema),
			entitylimits.Truncate(child.Name, entitylimits.MaxNameLength),
			entitylimits.Truncate(child.Description, entitylimits.MaxDescriptionLength), ownerProject).Scan(&childApp); err != nil {
			return fmt.Errorf("insert the nested agent %q: %w", nested.ToolName, err)
		}
		childVersion, err := insertDefinitionVersion(ctx, q, schema, childApp, "base", "embedded", authorID,
			child.Version, jsonText(child.Version.Meta, "{}"), jsonOrNil(child.Version.AvatarPack))
		if err != nil {
			return err
		}
		if err := insertApplicationToolReference(ctx, q, schema, parentVersionID, nested.ToolName,
			childApp, childVersion, ownerProject, int(authorID)); err != nil {
			return err
		}
		if err := installNested(ctx, q, schema, ownerProject, authorID, childVersion, child); err != nil {
			return err
		}
	}
	return nil
}

// MarketplaceHire answers POST /elitea_core/marketplace_hire/prompt_lib/{projectID}.
func (h *Handler) MarketplaceHire(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		hireError(w, http.StatusServiceUnavailable, "unavailable", "The marketplace needs a database.")
		return
	}
	ctx := r.Context()
	projectIDText := chi.URLParam(r, "projectID")
	schema, ok := tenantSchema(w, projectIDText)
	if !ok {
		return
	}
	projectID := parseProjectID(projectIDText)
	owner, ownerErr := tenantOwnerID(projectIDText)
	userID, hasUser := importPrincipalUserID(ctx)
	if !hasUser {
		hireError(w, http.StatusUnauthorized, "unauthorized", "An authenticated person is required to hire.")
		return
	}
	var body hireBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		hireError(w, http.StatusBadRequest, "invalid_request", "The request body is not valid.")
		return
	}
	listingID, listingErr := strconv.ParseInt(body.ListingID, 10, 64)
	versionID, versionErr := strconv.ParseInt(body.VersionID, 10, 64)
	if ownerErr != nil || projectID <= 0 || listingErr != nil || versionErr != nil || listingID <= 0 || versionID <= 0 {
		hireError(w, http.StatusBadRequest, "invalid_request", "listing_id, version_id and the project must be ids.")
		return
	}
	if body.Mode != marketplace.ModeInstall && body.Mode != marketplace.ModeFork {
		hireError(w, http.StatusBadRequest, "invalid_request", "mode must be install or fork.")
		return
	}
	h.ensureListings(ctx)
	version, err := marketplace.LoadVersion(ctx, h.pool, listingID, versionID)
	if errors.Is(err, marketplace.ErrNotFound) {
		hireError(w, http.StatusNotFound, "not_found", "The listing version does not exist.")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "marketplace hire: read the version failed", "listing_id", listingID, "error", err)
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to hire.")
		return
	}
	if version.ListingKind != marketplace.KindAgent {
		hireError(w, http.StatusBadRequest, "invalid_request", "A skill is added to an agent, not hired.")
		return
	}
	if !version.Hireable() {
		hireError(w, http.StatusConflict, "version_not_hireable", "This version was yanked, withdrawn or delisted. Choose another version.")
		return
	}
	node, err := marketplace.ParseDefinition(version.Definition)
	if err != nil {
		slog.ErrorContext(ctx, "marketplace hire: read the definition failed", "listing_id", listingID, "error", err)
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to hire.")
		return
	}
	bindings, code, msg := h.checkBindings(ctx, schema, version.Requirements, body.Bindings)
	if code != "" {
		status := http.StatusConflict
		switch code {
		case "binding_invalid":
			status = http.StatusBadRequest
		case "internal_error":
			status = http.StatusInternalServerError
		}
		hireError(w, status, code, msg)
		return
	}

	// Above low tier, or with a write tool, a person approves the hire.
	tier := agentlifecycle.Tier(version.Requirements.Tier)
	if !tier.Valid() {
		tier = agentlifecycle.TierLow
	}
	needsApproval := tier.Above(agentlifecycle.TierLow) || version.Requirements.HasWriteTools
	selfApprove := false
	if needsApproval {
		personal, perr := marketplace.IsPersonalProject(ctx, h.pool, projectID)
		if perr != nil {
			slog.ErrorContext(ctx, "marketplace hire: read the project failed", "project_id", projectID, "error", perr)
			hireError(w, http.StatusInternalServerError, "internal_error", "Failed to hire.")
			return
		}
		if personal {
			allowed, cerr := platformconfig.LoadMarketplacePersonalSelfApprove(ctx, h.pool)
			if cerr != nil {
				slog.ErrorContext(ctx, "marketplace hire: read the policy failed", "error", cerr)
				hireError(w, http.StatusInternalServerError, "internal_error", "Failed to hire.")
				return
			}
			if !allowed {
				hireError(w, http.StatusConflict, "hire_needs_shared_project",
					"This agent needs an approval, and a personal project has no second approver. Hire it in a shared project.")
				return
			}
			selfApprove = true
		}
	}
	governor, hasGovernor := h.governor.(hireGovernor)
	if needsApproval && !hasGovernor {
		hireError(w, http.StatusServiceUnavailable, "unavailable", "Approvals are not available on this deployment.")
		return
	}

	// The avatar files are copied into this project before the transaction.
	var metaMap map[string]any
	_ = json.Unmarshal(node.Version.Meta, &metaMap) // the snapshot's meta is an object
	if metaMap == nil {
		metaMap = map[string]any{}
	}
	var packDoc any
	if len(node.Version.AvatarPack) > 0 {
		_ = json.Unmarshal(node.Version.AvatarPack, &packDoc) // null reads as no pack
	}
	avatar, err := h.carriedAvatarPack(ctx, map[string]any{"avatar_pack": packDoc, "meta": metaMap}, projectIDText)
	if err != nil {
		slog.WarnContext(ctx, "marketplace hire: the avatar could not be copied", "listing_id", listingID, "error", err)
		avatar = nil
	}

	name := node.Name
	if body.Name != "" {
		name = body.Name
	}
	name = entitylimits.Truncate(name, entitylimits.MaxNameLength)
	description := entitylimits.Truncate(node.Description, entitylimits.MaxDescriptionLength)

	origin := marketplace.OriginInstalled
	if body.Mode == marketplace.ModeFork {
		origin = marketplace.OriginForked
	}
	metaMap[marketplace.MetaOriginKind] = origin
	metaMap[marketplace.MetaOriginListingID] = strconv.FormatInt(listingID, 10)
	metaMap[marketplace.MetaOriginVersionID] = strconv.FormatInt(versionID, 10)
	metaMap[marketplace.MetaOriginDigest] = hex.EncodeToString(version.Digest[:])
	metaMap[marketplace.MetaOriginVersionName] = version.Name
	metaMap[marketplace.MetaOriginListingName] = version.ListingName
	if body.Mode == marketplace.ModeFork {
		// The existing fork surfaces read these two keys.
		if version.EntityID > 0 {
			metaMap["parent_entity_id"] = strconv.FormatInt(version.EntityID, 10)
		} else {
			metaMap["parent_entity_id"] = strconv.FormatInt(listingID, 10)
		}
		metaMap["parent_project_id"] = publicProjectIDOrDefault()
		metaMap["parent_author_id"] = userID.String()
	}
	metaJSON, err := json.Marshal(metaMap)
	if err != nil {
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to hire.")
		return
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to hire.")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var appID int64
	if err := tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.applications (name, description, icon, owner_id, meta, created_at, updated_at)
VALUES ($1, $2, $3, $4, '{}'::jsonb, now() AT TIME ZONE 'UTC', now() AT TIME ZONE 'UTC') RETURNING id`, schema),
		name, description, node.Icon, owner.Int64()).Scan(&appID); err != nil {
		slog.ErrorContext(ctx, "marketplace hire: insert the application failed", "schema", schema, "error", err)
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to hire.")
		return
	}
	versionRow, err := insertDefinitionVersion(ctx, tx, schema, appID, "base", "draft", userID.Int64(),
		node.Version, string(metaJSON), avatar)
	if err != nil {
		slog.ErrorContext(ctx, "marketplace hire: insert the version failed", "schema", schema, "error", err)
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to hire.")
		return
	}
	for _, tag := range node.Tags {
		var tagID int64
		if err := tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.tags (name, data) VALUES ($1, '{}'::jsonb)
ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name RETURNING id`, schema), tag).Scan(&tagID); err != nil {
			slog.ErrorContext(ctx, "marketplace hire: insert a tag failed", "tag", tag, "error", err)
			hireError(w, http.StatusInternalServerError, "internal_error", "Failed to hire.")
			return
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.application_version_tag_association (version_id, tag_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, schema),
			versionRow, tagID); err != nil {
			hireError(w, http.StatusInternalServerError, "internal_error", "Failed to hire.")
			return
		}
	}
	if err := installNested(ctx, tx, schema, int(owner.Int64()), userID.Int64(), versionRow, node); err != nil {
		slog.ErrorContext(ctx, "marketplace hire: write the nested agents failed", "schema", schema, "error", err)
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to hire.")
		return
	}

	toolsEnabled := !needsApproval || selfApprove
	if toolsEnabled {
		if err := marketplace.AttachTools(ctx, tx, schema, appID, versionRow, node, marketplace.BindingMap(bindings)); err != nil {
			slog.ErrorContext(ctx, "marketplace hire: attach the tools failed", "schema", schema, "error", err)
			hireError(w, http.StatusInternalServerError, "internal_error", "Failed to hire.")
			return
		}
	}
	bindingsJSON, err := json.Marshal(bindings)
	if err != nil {
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to hire.")
		return
	}
	if err := marketplace.RecordInstall(ctx, tx, marketplace.Install{
		ProjectID: projectID, EntityKind: marketplace.KindAgent, EntityID: appID, ListingID: listingID, VersionID: versionID,
		Digest: version.Digest, Mode: body.Mode, Bindings: bindingsJSON, ToolsOn: toolsEnabled, InstalledBy: userID.Int64(),
	}); err != nil {
		slog.ErrorContext(ctx, "marketplace hire: record the hire failed", "error", err)
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to hire.")
		return
	}

	result := map[string]any{
		"application_id": strconv.FormatInt(appID, 10), "version_id": strconv.FormatInt(versionRow, 10),
		"mode": body.Mode, "tools_enabled": toolsEnabled, "awaiting_approval": needsApproval && !selfApprove,
	}
	if hasGovernor {
		change, err := governor.RecordMarketplaceChangeTx(ctx, tx, agentlifecycle.NewMarketplaceChange{
			ProjectID: projectID, ApplicationID: appID, Tier: tier, ProposedBy: userID.Int64(),
			NeedsChange: needsApproval, Kind: agentlifecycle.KindHire,
			Summary:   "Hire " + version.ListingName + " " + version.Name + " (" + body.Mode + ").",
			ListingID: listingID, VersionID: versionID, Digest: version.Digest, SelfApprove: selfApprove,
		})
		if err != nil {
			if refusal, isRefusal := agentlifecycle.AsRefusal(err); isRefusal {
				writeJSON(w, refusal.Status(), refusal.Body())
				return
			}
			slog.ErrorContext(ctx, "marketplace hire: record the change request failed", "error", err)
			hireError(w, http.StatusInternalServerError, "internal_error", "Failed to hire.")
			return
		}
		if change.ID > 0 {
			result["change_request_id"] = strconv.FormatInt(change.ID, 10)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "marketplace hire: commit failed", "error", err)
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to hire.")
		return
	}
	if h.events != nil {
		h.events.Emit(ctx, projectIDText, "agent.hired", map[string]any{
			"application_id": appID, "listing_id": listingID, "version_id": versionID, "mode": body.Mode,
		})
	}
	writeJSON(w, http.StatusCreated, result)
}
