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

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/delegationgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
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
	// ProposalID is the open hire_listing proposal of a workforce manager the
	// hire settles (ADR-0051 decision 6).
	ProposalID string `json:"proposal_id"`
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

// hireFailure is a hire that did not happen: the status and body the route
// answers.
type hireFailure struct {
	status int
	body   any
}

func failHire(status int, code, msg string) *hireFailure {
	return &hireFailure{status: status, body: map[string]any{"error": code, "msg": msg}}
}

// hireInput is one hire, however it was asked for: a person in the wizard, or
// the workforce policy's auto_hire of a listing a workforce manager proposed.
type hireInput struct {
	ProjectIDText string
	ListingID     int64
	VersionID     int64
	Mode          string
	Name          string
	Bindings      []hireBindingBody
	// ActorID is the person the hire is made for: the author of the new
	// agent's versions.
	ActorID int64
	// ProposalID settles the open hire_listing proposal of a workforce manager
	// in the same transaction (0: none).
	ProposalID int64
	// Automatic: the hire is the policy's. It is refused (a person decides)
	// when it would need an approval.
	Automatic bool
}

// MarketplaceHire answers POST /elitea_core/marketplace_hire/prompt_lib/{projectID}.
func (h *Handler) MarketplaceHire(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		hireError(w, http.StatusServiceUnavailable, "unavailable", "The marketplace needs a database.")
		return
	}
	ctx := r.Context()
	projectIDText := chi.URLParam(r, "projectID")
	if _, ok := tenantSchema(w, projectIDText); !ok {
		return
	}
	projectID := parseProjectID(projectIDText)
	_, ownerErr := tenantOwnerID(projectIDText)
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
	var proposalID int64
	if body.ProposalID != "" {
		parsed, err := strconv.ParseInt(body.ProposalID, 10, 64)
		if err != nil || parsed <= 0 {
			hireError(w, http.StatusBadRequest, "invalid_request", "proposal_id must be a workforce proposal id.")
			return
		}
		proposalID = parsed
		// A workforce manager's proposal is the agent managers' to settle: a
		// person who can hire but does not manage agents hires without it.
		manages := false
		if h.holdsWorkforceManage != nil {
			var err error
			if manages, err = h.holdsWorkforceManage(r, projectIDText); err != nil {
				slog.ErrorContext(ctx, "marketplace hire: resolve the agent manager permission failed", "project_id", projectID, "error", err)
				hireError(w, http.StatusInternalServerError, "internal_error", "Failed to hire.")
				return
			}
		}
		if !manages {
			hireError(w, http.StatusForbidden, "workforce_forbidden", "Only an agent manager hires from a workforce manager's proposal.")
			return
		}
	}
	if body.Mode != marketplace.ModeInstall && body.Mode != marketplace.ModeFork {
		hireError(w, http.StatusBadRequest, "invalid_request", "mode must be install or fork.")
		return
	}
	h.ensureListings(ctx)
	result, failure := h.performHire(ctx, nil, hireInput{
		ProjectIDText: projectIDText, ListingID: listingID, VersionID: versionID, Mode: body.Mode, Name: body.Name,
		Bindings: body.Bindings, ActorID: userID.Int64(), ProposalID: proposalID,
	})
	if failure != nil {
		writeJSON(w, failure.status, failure.body)
		return
	}
	if h.events != nil {
		h.events.Emit(ctx, projectIDText, "agent.hired", map[string]any{
			"application_id": result["application_id"], "listing_id": listingID, "version_id": versionID, "mode": body.Mode,
		})
	}
	writeJSON(w, http.StatusCreated, result)
}

// performHire makes one hire (ADR-0051 decision 3). With a transaction it
// writes inside it and leaves the commit to the caller (the policy's hire is
// one transaction with its probation record); without one it opens its own and
// commits.
func (h *Handler) performHire(ctx context.Context, external pgx.Tx, in hireInput) (map[string]any, *hireFailure) {
	projectIDText := in.ProjectIDText
	schema, err := tenantschema.Quote(projectIDText)
	if err != nil {
		return nil, failHire(http.StatusBadRequest, "invalid_request", "The project id is not valid.")
	}
	projectID := parseProjectID(projectIDText)
	owner, ownerErr := tenantOwnerID(projectIDText)
	if ownerErr != nil {
		return nil, failHire(http.StatusBadRequest, "invalid_request", "The project id is not valid.")
	}
	listingID, versionID := in.ListingID, in.VersionID
	version, err := marketplace.LoadVersion(ctx, h.pool, listingID, versionID)
	if errors.Is(err, marketplace.ErrNotFound) {
		return nil, failHire(http.StatusNotFound, "not_found", "The listing version does not exist.")
	}
	if err != nil {
		slog.ErrorContext(ctx, "marketplace hire: read the version failed", "listing_id", listingID, "error", err)
		return nil, failHire(http.StatusInternalServerError, "internal_error", "Failed to hire.")
	}
	if version.ListingKind == marketplace.KindTeam {
		return nil, failHire(http.StatusBadRequest, "invalid_request", "A team is hired with marketplace_team_hire.")
	}
	if version.ListingKind != marketplace.KindAgent {
		return nil, failHire(http.StatusBadRequest, "invalid_request", "A skill is added to an agent, not hired.")
	}
	if !version.Hireable() {
		return nil, failHire(http.StatusConflict, "version_not_hireable", "This version was yanked, withdrawn or delisted. Choose another version.")
	}
	node, err := marketplace.ParseDefinition(version.Definition)
	if err != nil {
		slog.ErrorContext(ctx, "marketplace hire: read the definition failed", "listing_id", listingID, "error", err)
		return nil, failHire(http.StatusInternalServerError, "internal_error", "Failed to hire.")
	}
	bindings, code, msg := h.checkBindings(ctx, schema, version.Requirements, in.Bindings)
	if code != "" {
		status := http.StatusConflict
		switch code {
		case "binding_invalid":
			status = http.StatusBadRequest
		case "internal_error":
			status = http.StatusInternalServerError
		}
		return nil, failHire(status, code, msg)
	}

	// Above low tier, or with a write tool, a person approves the hire.
	tier := agentlifecycle.Tier(version.Requirements.Tier)
	if !tier.Valid() {
		tier = agentlifecycle.TierLow
	}
	needsApproval := tier.Above(agentlifecycle.TierLow) || version.Requirements.HasWriteTools
	if in.Automatic && needsApproval {
		return nil, failHire(http.StatusConflict, "hire_needs_approval", "This agent needs a person's approval.")
	}
	selfApprove := false
	if needsApproval {
		personal, perr := marketplace.IsPersonalProject(ctx, h.pool, projectID)
		if perr != nil {
			slog.ErrorContext(ctx, "marketplace hire: read the project failed", "project_id", projectID, "error", perr)
			return nil, failHire(http.StatusInternalServerError, "internal_error", "Failed to hire.")
		}
		if personal {
			allowed, cerr := platformconfig.LoadMarketplacePersonalSelfApprove(ctx, h.pool)
			if cerr != nil {
				slog.ErrorContext(ctx, "marketplace hire: read the policy failed", "error", cerr)
				return nil, failHire(http.StatusInternalServerError, "internal_error", "Failed to hire.")
			}
			if !allowed {
				return nil, failHire(http.StatusConflict, "hire_needs_shared_project",
					"This agent needs an approval, and a personal project has no second approver. Hire it in a shared project.")
			}
			selfApprove = true
		}
	}
	governor, hasGovernor := h.governor.(hireGovernor)
	if needsApproval && !hasGovernor {
		return nil, failHire(http.StatusServiceUnavailable, "unavailable", "Approvals are not available on this deployment.")
	}

	// The avatar files are copied into this project before the transaction.
	var metaMap map[string]any
	_ = json.Unmarshal(node.Version.Meta, &metaMap) // the snapshot's meta is an object
	if metaMap == nil {
		metaMap = map[string]any{}
	}
	// A person's hire reads the source files as that person. The policy's hire
	// has no request, so it reads them as the person it acts for: the same
	// project-membership rule, and the same look for a hire of either kind.
	mediaCtx := ctx
	if in.Automatic {
		mediaCtx = auth.ContextWithUser(ctx, auth.User{ID: strconv.FormatInt(in.ActorID, 10)})
	}
	var packDoc any
	if len(node.Version.AvatarPack) > 0 {
		_ = json.Unmarshal(node.Version.AvatarPack, &packDoc) // null reads as no pack
	}
	avatar, err := h.carriedAvatarPack(mediaCtx, map[string]any{"avatar_pack": packDoc, "meta": metaMap}, projectIDText)
	if err != nil {
		slog.WarnContext(ctx, "marketplace hire: the avatar could not be copied", "listing_id", listingID, "error", err)
		avatar = nil
	}

	name := node.Name
	if in.Name != "" {
		name = in.Name
	}
	name = entitylimits.Truncate(name, entitylimits.MaxNameLength)
	description := entitylimits.Truncate(node.Description, entitylimits.MaxDescriptionLength)

	origin := marketplace.OriginInstalled
	if in.Mode == marketplace.ModeFork {
		origin = marketplace.OriginForked
	}
	metaMap[marketplace.MetaOriginKind] = origin
	metaMap[marketplace.MetaOriginListingID] = strconv.FormatInt(listingID, 10)
	metaMap[marketplace.MetaOriginVersionID] = strconv.FormatInt(versionID, 10)
	metaMap[marketplace.MetaOriginDigest] = hex.EncodeToString(version.Digest[:])
	metaMap[marketplace.MetaOriginVersionName] = version.Name
	metaMap[marketplace.MetaOriginListingName] = version.ListingName
	if in.Mode == marketplace.ModeFork {
		// The existing fork surfaces read these two keys.
		if version.EntityID > 0 {
			metaMap["parent_entity_id"] = strconv.FormatInt(version.EntityID, 10)
		} else {
			metaMap["parent_entity_id"] = strconv.FormatInt(listingID, 10)
		}
		metaMap["parent_project_id"] = publicProjectIDOrDefault()
		metaMap["parent_author_id"] = strconv.FormatInt(in.ActorID, 10)
	}
	metaJSON, err := json.Marshal(metaMap)
	if err != nil {
		return nil, failHire(http.StatusInternalServerError, "internal_error", "Failed to hire.")
	}

	// Inside the caller's transaction the hire is a savepoint: a refused hire
	// leaves nothing of itself behind, and the caller's other writes stay.
	var tx pgx.Tx
	if external != nil {
		tx, err = external.Begin(ctx)
	} else {
		tx, err = h.pool.Begin(ctx)
	}
	if err != nil {
		return nil, failHire(http.StatusInternalServerError, "internal_error", "Failed to hire.")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	fail := func() *hireFailure {
		return failHire(http.StatusInternalServerError, "internal_error", "Failed to hire.")
	}

	var appID int64
	if err := tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.applications (name, description, icon, owner_id, meta, created_at, updated_at)
VALUES ($1, $2, $3, $4, '{}'::jsonb, now() AT TIME ZONE 'UTC', now() AT TIME ZONE 'UTC') RETURNING id`, schema),
		name, description, node.Icon, owner.Int64()).Scan(&appID); err != nil {
		slog.ErrorContext(ctx, "marketplace hire: insert the application failed", "schema", schema, "error", err)
		return nil, fail()
	}
	versionRow, err := insertDefinitionVersion(ctx, tx, schema, appID, "base", "draft", in.ActorID,
		node.Version, string(metaJSON), avatar)
	if err != nil {
		slog.ErrorContext(ctx, "marketplace hire: insert the version failed", "schema", schema, "error", err)
		return nil, fail()
	}
	for _, tag := range node.Tags {
		var tagID int64
		if err := tx.QueryRow(ctx, fmt.Sprintf(`
INSERT INTO %s.tags (name, data) VALUES ($1, '{}'::jsonb)
ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name RETURNING id`, schema), tag).Scan(&tagID); err != nil {
			slog.ErrorContext(ctx, "marketplace hire: insert a tag failed", "tag", tag, "error", err)
			return nil, fail()
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
INSERT INTO %s.application_version_tag_association (version_id, tag_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, schema),
			versionRow, tagID); err != nil {
			return nil, fail()
		}
	}
	if err := installNested(ctx, tx, schema, int(owner.Int64()), in.ActorID, versionRow, node); err != nil {
		slog.ErrorContext(ctx, "marketplace hire: write the nested agents failed", "schema", schema, "error", err)
		return nil, fail()
	}

	toolsEnabled := !needsApproval || selfApprove
	if toolsEnabled {
		if err := marketplace.AttachTools(ctx, tx, schema, appID, versionRow, node, marketplace.BindingMap(bindings)); err != nil {
			slog.ErrorContext(ctx, "marketplace hire: attach the tools failed", "schema", schema, "error", err)
			return nil, fail()
		}
		// The hire is the approval of the agent's internal edges (a hire that needs
		// a person approves them in its change request instead).
		if in.ActorID > 0 {
			if err := h.reconcileComposition(ctx, tx, repos.CompositionApproval{
				ProjectID: projectID, RootApplicationID: appID, Origin: delegationgrant.OriginMarketplaceHire,
				ProposerID: in.ActorID, ApproverIDs: []int64{in.ActorID},
			}); err != nil {
				slog.ErrorContext(ctx, "marketplace hire: approve the internal edges failed", "schema", schema, "error", err)
				return nil, fail()
			}
		}
	}
	bindingsJSON, err := json.Marshal(bindings)
	if err != nil {
		return nil, fail()
	}
	if err := marketplace.RecordInstall(ctx, tx, marketplace.Install{
		ProjectID: projectID, EntityKind: marketplace.KindAgent, EntityID: appID, ListingID: listingID, VersionID: versionID,
		Digest: version.Digest, Mode: in.Mode, Bindings: bindingsJSON, ToolsOn: toolsEnabled, InstalledBy: in.ActorID,
	}); err != nil {
		slog.ErrorContext(ctx, "marketplace hire: record the hire failed", "error", err)
		return nil, fail()
	}

	result := map[string]any{
		"application_id": strconv.FormatInt(appID, 10), "version_id": strconv.FormatInt(versionRow, 10),
		"mode": in.Mode, "tools_enabled": toolsEnabled, "awaiting_approval": needsApproval && !selfApprove,
	}
	if hasGovernor {
		change, err := governor.RecordMarketplaceChangeTx(ctx, tx, agentlifecycle.NewMarketplaceChange{
			ProjectID: projectID, ApplicationID: appID, Tier: tier, ProposedBy: in.ActorID,
			NeedsChange: needsApproval, Kind: agentlifecycle.KindHire,
			Summary:   "Hire " + version.ListingName + " " + version.Name + " (" + in.Mode + ").",
			ListingID: listingID, VersionID: versionID, Digest: version.Digest, SelfApprove: selfApprove,
		})
		if err != nil {
			if refusal, isRefusal := agentlifecycle.AsRefusal(err); isRefusal {
				return nil, &hireFailure{status: refusal.Status(), body: refusal.Body()}
			}
			slog.ErrorContext(ctx, "marketplace hire: record the change request failed", "error", err)
			return nil, fail()
		}
		if change.ID > 0 {
			result["change_request_id"] = strconv.FormatInt(change.ID, 10)
		}
	}
	// A hire made from a workforce manager's proposal settles it in the same
	// transaction: the person who hires is the one who approves it.
	if in.ProposalID > 0 {
		if err := marketplace.SettleHireProposalTx(ctx, tx, projectID, in.ProposalID, listingID, 0, in.ActorID, map[string]any{
			"application_id": appID, "version_id": versionID, "mode": in.Mode, "automatic": in.Automatic,
		}); err != nil {
			switch {
			case errors.Is(err, marketplace.ErrNotFound):
				return nil, failHire(http.StatusNotFound, "proposal_not_found", "No such hire proposal in this project.")
			case errors.Is(err, marketplace.ErrProposalNotOpen):
				return nil, failHire(http.StatusConflict, "proposal_not_open", "The hire proposal was decided already.")
			case errors.Is(err, marketplace.ErrInvalidRequest):
				return nil, failHire(http.StatusBadRequest, "invalid_request", "The proposal is for another listing.")
			}
			slog.ErrorContext(ctx, "marketplace hire: settle the proposal failed", "error", err)
			return nil, fail()
		}
		result["proposal_id"] = strconv.FormatInt(in.ProposalID, 10)
	}
	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "marketplace hire: commit failed", "error", err)
		return nil, fail()
	}
	return result, nil
}
