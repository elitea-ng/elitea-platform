package eliteacore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentrelease"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/marketplace"
)

// The upstream update of a fork (ADR-0051 decision 4). A fork is the project's
// own agent, so a new listing version never changes it by itself, at any tier
// and under any policy. The review shows the version the fork came from (base),
// the new listing version (theirs) and the fork's current draft (ours), field
// by field, conflicts first. The person keeps theirs, keeps ours or edits each
// field; the result is a new draft of the fork and a change request of kind
// `upstream_merge` that a person approves. A fork made before forks recorded
// their origin version has no base, so its review is a two-way diff.
//
// Only the fields of the definition merge. The tools, skills and nested agents
// of a fork are its own: the review lists what upstream now asks for, and the
// person adds it to the fork by hand.

type mergeField struct {
	key, label string
	json       bool
}

// mergeFields are the fields of a version that the review merges. The
// description and the name belong to the agent, not to a version, so a draft
// cannot carry them.
var mergeFields = []mergeField{
	{key: "instructions", label: "Instructions"},
	{key: "welcome_message", label: "Welcome message"},
	{key: "agent_type", label: "Agent type"},
	{key: "conversation_starters", label: "Conversation starters", json: true},
	{key: "llm_settings", label: "Model settings", json: true},
	{key: "pipeline_settings", label: "Pipeline settings", json: true},
	{key: "soul", label: "Soul", json: true},
}

// Statuses of a merged field.
const (
	mergeConflict   = "conflict"    // both sides changed it differently
	mergeTheirsOnly = "theirs_only" // upstream changed it, the fork did not
	mergeOursOnly   = "ours_only"   // the fork changed it, upstream did not
	mergeSame       = "same"
	mergeDiffers    = "differs" // two-way: the sides differ and the base is unknown
)

type mergeFieldView struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Status  string `json:"status"`
	Default string `json:"default"`
	Base    any    `json:"base"`
	Theirs  any    `json:"theirs"`
	Ours    any    `json:"ours"`
}

type upstreamReview struct {
	Mode            string                 `json:"mode"`
	ListingID       string                 `json:"listing_id"`
	ListingName     string                 `json:"listing_name"`
	Base            *planVersion           `json:"base"`
	Theirs          planVersion            `json:"theirs"`
	Fields          []mergeFieldView       `json:"fields"`
	Changes         []agentrelease.Change  `json:"changes"`
	Widening        []marketplace.Widening `json:"widening"`
	NeedsApproval   bool                   `json:"needs_approval"`
	Hireable        bool                   `json:"hireable"`
	OurVersionName  string                 `json:"our_version_name"`
	OpenChangeBlock bool                   `json:"open_change_request"`
}

func decodeRaw(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// definitionValue reads one merge field of a definition.
func definitionValue(def marketplace.VersionDefinition, field mergeField) any {
	switch field.key {
	case "instructions":
		return def.Instructions
	case "welcome_message":
		return def.WelcomeMessage
	case "agent_type":
		return def.AgentType
	case "conversation_starters":
		return decodeRaw(def.ConversationStarters)
	case "llm_settings":
		return decodeRaw(def.LLMSettings)
	case "pipeline_settings":
		return decodeRaw(def.PipelineSettings)
	case "soul":
		return decodeSoul(def.Soul)
	}
	return nil
}

// decodeSoul reads a soul that may be stored as a JSON string of the document.
func decodeSoul(raw json.RawMessage) any {
	value := decodeRaw(raw)
	if text, ok := value.(string); ok {
		var inner any
		if json.Unmarshal([]byte(text), &inner) == nil {
			return inner
		}
	}
	return value
}

func setDefinitionValue(def *marketplace.VersionDefinition, field mergeField, value any) error {
	if !field.json {
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("%s must be text", field.key)
		}
		switch field.key {
		case "instructions":
			def.Instructions = text
		case "welcome_message":
			def.WelcomeMessage = text
		case "agent_type":
			def.AgentType = text
		}
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%s is not valid JSON", field.key)
	}
	raw := json.RawMessage(encoded)
	switch field.key {
	case "conversation_starters":
		def.ConversationStarters = raw
	case "llm_settings":
		def.LLMSettings = raw
	case "pipeline_settings":
		def.PipelineSettings = raw
	case "soul":
		def.Soul = raw
	}
	return nil
}

func emptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// sameValue compares two decoded values by their encoding; an absent value, an
// empty string, an empty list and an empty object are one value.
func sameValue(a, b any) bool {
	if emptyValue(a) && emptyValue(b) {
		return true
	}
	left, errA := json.Marshal(a)
	right, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(left) == string(right)
}

// classify decides the status of one field and the choice that is the default.
// base is nil in a two-way review.
func classify(hasBase bool, base, theirs, ours any) (status, def string) {
	if sameValue(theirs, ours) {
		return mergeSame, "ours"
	}
	if !hasBase {
		return mergeDiffers, "ours"
	}
	switch {
	case sameValue(base, theirs):
		return mergeOursOnly, "ours"
	case sameValue(base, ours):
		return mergeTheirsOnly, "theirs"
	}
	return mergeConflict, "ours"
}

var mergeOrder = map[string]int{mergeConflict: 0, mergeDiffers: 1, mergeTheirsOnly: 2, mergeOursOnly: 3, mergeSame: 4}

// buildFields merges the three definitions field by field, conflicts first.
func buildFields(base *marketplace.VersionDefinition, theirs, ours marketplace.VersionDefinition) []mergeFieldView {
	out := make([]mergeFieldView, 0, len(mergeFields))
	for _, field := range mergeFields {
		view := mergeFieldView{Key: field.key, Label: field.label, Theirs: definitionValue(theirs, field), Ours: definitionValue(ours, field)}
		if base != nil {
			view.Base = definitionValue(*base, field)
		}
		view.Status, view.Default = classify(base != nil, view.Base, view.Theirs, view.Ours)
		out = append(out, view)
	}
	sort.SliceStable(out, func(i, j int) bool { return mergeOrder[out[i].Status] < mergeOrder[out[j].Status] })
	return out
}

// ours is the fork's current definition: its newest draft, else its newest
// version.
type ourVersion struct {
	id     int64
	name   string
	def    marketplace.VersionDefinition
	meta   string
	avatar any
}

func loadOurs(ctx context.Context, q relationQuerier, schema string, applicationID int64) (ourVersion, error) {
	var out ourVersion
	var starters, llm, pipeline, soul, avatar *string
	err := q.QueryRow(ctx, fmt.Sprintf(`
SELECT id, name, COALESCE(agent_type, ''), COALESCE(instructions, ''), COALESCE(welcome_message, ''),
       conversation_starters::text, llm_settings::text, pipeline_settings::text, soul::text, avatar_pack::text, COALESCE(meta::text, '{}')
FROM %s.application_versions
WHERE application_id = $1 AND COALESCE(status, '') <> 'embedded'
ORDER BY (COALESCE(status, '') = 'draft') DESC, id DESC LIMIT 1`, schema), applicationID).
		Scan(&out.id, &out.name, &out.def.AgentType, &out.def.Instructions, &out.def.WelcomeMessage,
			&starters, &llm, &pipeline, &soul, &avatar, &out.meta)
	if err != nil {
		return ourVersion{}, err
	}
	out.def.ConversationStarters = rawOrNullText(starters)
	out.def.LLMSettings = rawOrNullText(llm)
	out.def.PipelineSettings = rawOrNullText(pipeline)
	out.def.Soul = rawOrNullText(soul)
	if avatar != nil {
		out.avatar = *avatar
	}
	return out, nil
}

func rawOrNullText(text *string) json.RawMessage {
	if text == nil || *text == "" {
		return json.RawMessage("null")
	}
	return json.RawMessage(*text)
}

// forkTarget resolves the fork a request is about and the version the review is
// for: version_id when given, else the newest hireable one after the base.
type forkTarget struct {
	origin  marketplace.Origin
	listing int64
	base    int64 // 0 when the fork has no origin version
	target  marketplace.StoredVersion
}

func (h *Handler) resolveFork(ctx context.Context, w http.ResponseWriter, schema string, projectID, appID int64, wanted string) (forkTarget, bool) {
	origin, err := marketplace.GetOrigin(ctx, h.pool, schema, appID, marketplace.OriginLookup{
		ProjectID: projectID, PublicProjectID: publicProjectIDOrDefault(),
	})
	if errors.Is(err, marketplace.ErrNotFound) {
		hireError(w, http.StatusNotFound, "not_found", "The agent is not a fork of a Marketplace listing.")
		return forkTarget{}, false
	}
	if err != nil {
		slog.ErrorContext(ctx, "marketplace upstream: read the origin failed", "application_id", appID, "error", err)
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to read the upstream update.")
		return forkTarget{}, false
	}
	if origin.Mode != marketplace.OriginForked {
		hireError(w, http.StatusConflict, "installed_has_no_upstream", "An installed agent is upgraded, not merged.")
		return forkTarget{}, false
	}
	out := forkTarget{origin: origin}
	out.listing, _ = strconv.ParseInt(origin.ListingID, 10, 64)
	out.base, _ = strconv.ParseInt(origin.VersionID, 10, 64)
	targetID, parseErr := strconv.ParseInt(wanted, 10, 64)
	if parseErr != nil || targetID <= 0 {
		if origin.LatestVer == nil {
			hireError(w, http.StatusNotFound, "up_to_date", "The fork is on the newest version.")
			return forkTarget{}, false
		}
		targetID, _ = strconv.ParseInt(origin.LatestVer.ID, 10, 64)
	}
	target, err := marketplace.LoadVersion(ctx, h.pool, out.listing, targetID)
	if errors.Is(err, marketplace.ErrNotFound) {
		hireError(w, http.StatusNotFound, "not_found", "The listing version does not exist.")
		return forkTarget{}, false
	}
	if err != nil {
		slog.ErrorContext(ctx, "marketplace upstream: read the version failed", "version_id", targetID, "error", err)
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to read the upstream update.")
		return forkTarget{}, false
	}
	out.target = target
	return out, true
}

// MarketplaceUpstream answers GET and POST
// /elitea_core/marketplace_upstream/prompt_lib/{projectID}/{applicationID}.
func (h *Handler) MarketplaceUpstream(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		hireError(w, http.StatusServiceUnavailable, "unavailable", "The marketplace needs a database.")
		return
	}
	ctx := r.Context()
	projectID, appID, schema, ok := parseUpgradeIDs(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		h.proposeUpstreamMerge(w, r, schema, projectID, appID)
		return
	}
	fork, ok := h.resolveFork(ctx, w, schema, projectID, appID, r.URL.Query().Get("version_id"))
	if !ok {
		return
	}
	ours, err := loadOurs(ctx, h.pool, schema, appID)
	if err != nil {
		slog.ErrorContext(ctx, "marketplace upstream: read the fork failed", "application_id", appID, "error", err)
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to read the upstream update.")
		return
	}
	review, err := h.buildReview(ctx, projectID, appID, fork, ours)
	if err != nil {
		slog.ErrorContext(ctx, "marketplace upstream: build the review failed", "application_id", appID, "error", err)
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to read the upstream update.")
		return
	}
	writeJSON(w, http.StatusOK, review)
}

func (h *Handler) buildReview(ctx context.Context, projectID, appID int64, fork forkTarget, ours ourVersion) (upstreamReview, error) {
	theirsNode, err := marketplace.ParseDefinition(fork.target.Definition)
	if err != nil {
		return upstreamReview{}, err
	}
	targetInfo, err := marketplace.LoadVersionInfo(ctx, h.pool, fork.listing, fork.target.VersionID)
	if err != nil {
		return upstreamReview{}, err
	}
	review := upstreamReview{
		Mode: "two_way", ListingID: marketplace.IDText(fork.listing), ListingName: fork.target.ListingName,
		Theirs: toPlanVersion(targetInfo), Hireable: fork.target.Hireable(), OurVersionName: ours.name,
		Changes: []agentrelease.Change{}, Widening: []marketplace.Widening{},
		NeedsApproval: marketplace.NeedsApproval(registryTier(ctx, h.pool, projectID, appID), marketplace.Requirements{}),
	}
	var baseDef *marketplace.VersionDefinition
	if fork.base > 0 {
		if base, err := marketplace.LoadVersion(ctx, h.pool, fork.listing, fork.base); err == nil {
			baseNode, perr := marketplace.ParseDefinition(base.Definition)
			if perr != nil {
				return upstreamReview{}, perr
			}
			baseDef = &baseNode.Version
			review.Mode = "three_way"
			if info, ierr := marketplace.LoadVersionInfo(ctx, h.pool, fork.listing, fork.base); ierr == nil {
				view := toPlanVersion(info)
				review.Base = &view
			}
			if changes, cerr := marketplace.Changes(base, fork.target); cerr == nil {
				review.Changes = changes
			}
			review.Widening = marketplace.WideningOf(base.Requirements, fork.target.Requirements)
		}
	}
	review.Fields = buildFields(baseDef, theirsNode.Version, ours.def)
	var open bool
	_ = h.pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM elitea_lifecycle.change_requests
               WHERE tenant_project_id = $1 AND application_id = $2 AND state IN ('draft', 'submitted'))`,
		projectID, appID).Scan(&open) // a failed read shows no banner; the proposal itself is refused by the one-open rule
	review.OpenChangeBlock = open
	return review, nil
}

type resolutionBody struct {
	Choice string `json:"choice"`
	Value  any    `json:"value"`
}

type upstreamBody struct {
	VersionID   string                    `json:"version_id"`
	Resolutions map[string]resolutionBody `json:"resolutions"`
}

func (h *Handler) proposeUpstreamMerge(w http.ResponseWriter, r *http.Request, schema string, projectID, appID int64) {
	ctx := r.Context()
	userID, hasUser := importPrincipalUserID(ctx)
	if !hasUser {
		hireError(w, http.StatusUnauthorized, "unauthorized", "An authenticated person is required to merge.")
		return
	}
	var body upstreamBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		hireError(w, http.StatusBadRequest, "invalid_request", "The request body is not valid.")
		return
	}
	fork, ok := h.resolveFork(ctx, w, schema, projectID, appID, body.VersionID)
	if !ok {
		return
	}
	if !fork.target.Hireable() {
		hireError(w, http.StatusConflict, "version_not_hireable", "This version was yanked, withdrawn or delisted. Choose another version.")
		return
	}
	// One read of the fork's draft serves both the review and the merge, so the
	// field statuses are computed against the draft that is merged.
	ours, err := loadOurs(ctx, h.pool, schema, appID)
	if err != nil {
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to merge.")
		return
	}
	review, err := h.buildReview(ctx, projectID, appID, fork, ours)
	if err != nil {
		slog.ErrorContext(ctx, "marketplace upstream: build the review failed", "application_id", appID, "error", err)
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to merge.")
		return
	}
	merged := ours.def
	resolutions := map[string]string{}
	var unresolved []string
	for _, view := range review.Fields {
		field := fieldByKey(view.Key)
		given, has := body.Resolutions[view.Key]
		choice := given.Choice
		if !has {
			if view.Status == mergeConflict {
				unresolved = append(unresolved, view.Key)
				continue
			}
			choice = view.Default
		}
		var value any
		switch choice {
		case "ours":
			value = view.Ours
		case "theirs":
			value = view.Theirs
		case "edit":
			value = given.Value
		default:
			hireError(w, http.StatusBadRequest, "invalid_request", "choice for "+view.Key+" must be ours, theirs or edit.")
			return
		}
		if err := setDefinitionValue(&merged, field, value); err != nil {
			hireError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		resolutions[view.Key] = choice
	}
	if len(unresolved) > 0 {
		sort.Strings(unresolved)
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "unresolved_conflicts", "msg": "Choose theirs, ours or an edit for every conflict.", "fields": unresolved,
		})
		return
	}
	result, status, code, msg := h.writeMerge(ctx, mergeRequest{
		Schema: schema, ProjectID: projectID, ApplicationID: appID, Fork: fork, Ours: ours, Merged: merged,
		Resolutions: resolutions, UserID: userID.Int64(),
	})
	if code != "" {
		hireError(w, status, code, msg)
		return
	}
	if h.events != nil {
		h.events.Emit(ctx, chi.URLParam(r, "projectID"), "agent.upstream_merged", map[string]any{
			"application_id": appID, "listing_id": fork.listing, "version_id": fork.target.VersionID, "applied": result["applied"],
		})
	}
	writeJSON(w, http.StatusCreated, result)
}

func fieldByKey(key string) mergeField {
	for _, field := range mergeFields {
		if field.key == key {
			return field
		}
	}
	return mergeField{key: key}
}

type mergeRequest struct {
	Schema                   string
	ProjectID, ApplicationID int64
	Fork                     forkTarget
	Ours                     ourVersion
	Merged                   marketplace.VersionDefinition
	Resolutions              map[string]string
	UserID                   int64
}

// writeMerge writes the draft and the change request in one transaction, and
// applies the move of the origin at once when the fork is low tier. The draft
// is written in every case; it is the fork's own to edit and publish.
func (h *Handler) writeMerge(ctx context.Context, req mergeRequest) (map[string]any, int, string, string) {
	internal := func() (map[string]any, int, string, string) {
		return nil, http.StatusInternalServerError, "internal_error", "Failed to merge."
	}
	tier := registryTier(ctx, h.pool, req.ProjectID, req.ApplicationID)
	needsApproval := marketplace.NeedsApproval(tier, marketplace.Requirements{})
	selfApprove, reason := !needsApproval, "direct_low_tier"
	if needsApproval {
		personal, err := marketplace.IsPersonalProject(ctx, h.pool, req.ProjectID)
		if err != nil {
			return internal()
		}
		if personal {
			allowed, status, code, msg := h.personalSelfApprove(ctx)
			if code != "" {
				return nil, status, code, msg
			}
			if !allowed {
				return nil, http.StatusConflict, "hire_needs_shared_project",
					"This merge needs an approval, and a personal project has no second approver. Use a shared project."
			}
			selfApprove, reason = true, ""
		}
	}
	governor, hasGovernor := h.governor.(hireGovernor)
	if needsApproval && !selfApprove && !hasGovernor {
		return nil, http.StatusServiceUnavailable, "unavailable", "Approvals are not available on this deployment."
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return internal()
	}
	defer func() { _ = tx.Rollback(ctx) }()

	name, err := freeVersionName(ctx, tx, req.Schema, req.ApplicationID, "upstream-"+req.Fork.target.Name)
	if err != nil {
		return internal()
	}
	draftID, err := insertDefinitionVersion(ctx, tx, req.Schema, req.ApplicationID, name, "draft", req.UserID,
		req.Merged, req.Ours.meta, req.Ours.avatar)
	if err != nil {
		slog.ErrorContext(ctx, "marketplace upstream: write the draft failed", "application_id", req.ApplicationID, "error", err)
		return internal()
	}
	if err := copyVersionLinks(ctx, tx, req.Schema, req.Ours.id, draftID); err != nil {
		slog.ErrorContext(ctx, "marketplace upstream: copy the links failed", "application_id", req.ApplicationID, "error", err)
		return internal()
	}
	payload, err := json.Marshal(marketplace.MergePayload{DraftVersionID: draftID, Resolutions: req.Resolutions})
	if err != nil {
		return internal()
	}
	result := map[string]any{
		"application_id": marketplace.IDText(req.ApplicationID), "draft_version_id": marketplace.IDText(draftID),
		"version_id": marketplace.IDText(req.Fork.target.VersionID), "applied": selfApprove, "awaiting_approval": !selfApprove,
	}
	if hasGovernor {
		change, err := governor.RecordMarketplaceChangeTx(ctx, tx, agentlifecycle.NewMarketplaceChange{
			ProjectID: req.ProjectID, ApplicationID: req.ApplicationID, Tier: agentlifecycle.Tier(tier),
			ProposedBy: req.UserID, NeedsChange: true, Kind: agentlifecycle.KindUpstreamMerge,
			Summary:   fmt.Sprintf("Merge %s %s into this fork.", req.Fork.target.ListingName, req.Fork.target.Name),
			ListingID: req.Fork.listing, VersionID: req.Fork.target.VersionID, Digest: req.Fork.target.Digest,
			SelfApprove: selfApprove, ApprovalReason: reason, Payload: payload,
		})
		if err != nil {
			if refusal, isRefusal := agentlifecycle.AsRefusal(err); isRefusal {
				return nil, refusal.Status(), refusal.Code, refusal.Message
			}
			slog.ErrorContext(ctx, "marketplace upstream: record the change request failed", "error", err)
			return internal()
		}
		if change.ID > 0 {
			result["change_request_id"] = marketplace.IDText(change.ID)
		}
	}
	if selfApprove {
		if err := h.ApplyMarketplaceTx(ctx, tx, agentlifecycle.MarketplaceApply{
			ProjectID: req.ProjectID, ApplicationID: req.ApplicationID, Kind: agentlifecycle.KindUpstreamMerge,
			ListingID: req.Fork.listing, VersionID: req.Fork.target.VersionID, Digest: req.Fork.target.Digest,
			Payload: payload, ActorID: req.UserID,
		}); err != nil {
			if refusal, isRefusal := agentlifecycle.AsRefusal(err); isRefusal {
				return nil, refusal.Status(), refusal.Code, refusal.Message
			}
			slog.ErrorContext(ctx, "marketplace upstream: apply failed", "application_id", req.ApplicationID, "error", err)
			return internal()
		}
	}
	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "marketplace upstream: commit failed", "error", err)
		return internal()
	}
	return result, http.StatusCreated, "", ""
}

// freeVersionName finds a version name of the agent that no version holds.
func freeVersionName(ctx context.Context, q relationQuerier, schema string, applicationID int64, base string) (string, error) {
	if len([]rune(base)) > 120 {
		base = string([]rune(base)[:120])
	}
	name := base
	for n := 2; n < 100; n++ {
		var taken bool
		if err := q.QueryRow(ctx, fmt.Sprintf(
			`SELECT EXISTS (SELECT 1 FROM %s.application_versions WHERE application_id = $1 AND name = $2)`, schema),
			applicationID, name).Scan(&taken); err != nil {
			return "", err
		}
		if !taken {
			return name, nil
		}
		name = fmt.Sprintf("%s-%d", base, n)
	}
	return "", errors.New("no free version name")
}

// copyVersionLinks gives the draft the tool, skill and tag links of the version
// it was made from.
func copyVersionLinks(ctx context.Context, q relationQuerier, schema string, from, to int64) error {
	statements := []string{
		`INSERT INTO %[1]s.entity_tool_mapping (entity_version_id, entity_id, entity_type, tool_id, selected_tools, created_at)
		 SELECT $2, entity_id, entity_type, tool_id, selected_tools, now() AT TIME ZONE 'UTC'
		 FROM %[1]s.entity_tool_mapping WHERE entity_version_id = $1 AND entity_type = 'agent'`,
		`INSERT INTO %[1]s.entity_skill_mapping (entity_version_id, entity_type, skill_id, skill_version_id, created_at)
		 SELECT $2, entity_type, skill_id, skill_version_id, now() AT TIME ZONE 'UTC'
		 FROM %[1]s.entity_skill_mapping WHERE entity_version_id = $1 AND entity_type = 'agent'`,
		`INSERT INTO %[1]s.application_version_tag_association (version_id, tag_id)
		 SELECT $2, tag_id FROM %[1]s.application_version_tag_association WHERE version_id = $1`,
	}
	for _, statement := range statements {
		if _, err := q.Exec(ctx, fmt.Sprintf(statement, schema), from, to); err != nil {
			return err
		}
	}
	return nil
}
