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
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentteams"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/tenantschema"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/marketplace"
)

// Saved teams in the Marketplace (ADR-0051 amendment "Teams in the
// Marketplace", issue 3137; shared 0286).
//
// Publish: a saved team is listed (kind `team`) only when every member agent
// is itself listed. The team version pins each member to its listing's
// current version, so a member resolves as its listing. A member with no
// listed, hireable listing refuses the publish with 409
// `team_members_unlisted`, which names each one.
//
// Hire: each member is hired through the agent hire (performHire) in one
// transaction with the saved team: Install reuses an agent the project
// already installed from the same listing version, Fork always makes new
// editable agents. The team is created in the project pinned to the hired
// agents' versions, and an install of entity_kind `team` records it. A member
// refusal (an unbound connection, an unhireable version, an approval a
// personal project cannot give) refuses the whole team and names the member.

type teamPublishBody struct {
	VersionName  string   `json:"version_name"`
	ReleaseNotes string   `json:"release_notes"`
	Category     string   `json:"category"`
	Tags         []string `json:"tags"`
}

type teamHireMemberBody struct {
	Position int               `json:"position"`
	Bindings []hireBindingBody `json:"bindings"`
}

type teamHireBody struct {
	ListingID string               `json:"listing_id"`
	VersionID string               `json:"version_id"`
	Mode      string               `json:"mode"`
	Name      string               `json:"name"`
	Members   []teamHireMemberBody `json:"members"`
}

const maxTeamVersionName = 128

// MarketplaceTeamPublish answers POST
// /elitea_core/marketplace_team_publish/prompt_lib/{projectID}/{teamID}.
func (h *Handler) MarketplaceTeamPublish(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		hireError(w, http.StatusServiceUnavailable, "unavailable", "The marketplace needs a database.")
		return
	}
	ctx := r.Context()
	projectID := parseProjectID(chi.URLParam(r, "projectID"))
	teamID, err := strconv.ParseInt(chi.URLParam(r, "teamID"), 10, 64)
	if projectID <= 0 || err != nil || teamID <= 0 {
		hireError(w, http.StatusBadRequest, "invalid_request", "The project and the team must be ids.")
		return
	}
	var body teamPublishBody
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			hireError(w, http.StatusBadRequest, "invalid_request", "The request body is not valid.")
			return
		}
	}
	body.VersionName = strings.TrimSpace(body.VersionName)
	if len([]rune(body.VersionName)) > maxTeamVersionName || len(body.Tags) > 64 || len([]rune(body.Category)) > 64 {
		hireError(w, http.StatusBadRequest, "invalid_request", "version_name is at most 128 characters, category 64, and tags 64.")
		return
	}
	team, err := repos.NewAgentTeams(h.pool).GetTeam(ctx, projectID, teamID)
	if errors.Is(err, agentteams.ErrNotFound) {
		hireError(w, http.StatusNotFound, "not_found", "No such team in this project.")
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "marketplace team publish: read the team failed", "team_id", teamID, "error", err)
		hireError(w, http.StatusInternalServerError, "internal_error", "Failed to publish the team.")
		return
	}
	result, failure := h.publishTeam(ctx, team, body, callerUserID(ctx))
	if failure != nil {
		writeJSON(w, failure.status, failure.body)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (h *Handler) publishTeam(ctx context.Context, team agentteams.Stored, body teamPublishBody, publisher int64) (map[string]any, *hireFailure) {
	fail := func() *hireFailure {
		return failHire(http.StatusInternalServerError, "internal_error", "Failed to publish the team.")
	}
	publicID, _ := strconv.ParseInt(publicProjectIDOrDefault(), 10, 64)
	src := marketplace.TeamSource{
		ProjectID: team.ProjectID, TeamID: team.ID, Name: team.Name, Description: team.Description,
		FloorPolicy: string(team.Policy), LeadPosition: team.Lead, PublicProjectID: publicID,
	}
	for index, member := range team.Members {
		src.Members = append(src.Members, marketplace.TeamMemberSource{
			Position: index + 1, ApplicationID: member.ApplicationID, ProjectID: member.ProjectID, Name: member.Name,
		})
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return nil, fail()
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Publishes of one team run one at a time, so two cannot take the same
	// version number.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"elitea.marketplace_team_publish:"+strconv.FormatInt(team.ID, 10)); err != nil {
		slog.ErrorContext(ctx, "marketplace team publish: take the team lock failed", "team_id", team.ID, "error", err)
		return nil, fail()
	}
	snap, err := marketplace.BuildTeamSnapshot(ctx, tx, src)
	var unlisted *marketplace.ErrMembersUnlisted
	if errors.As(err, &unlisted) {
		names := make([]string, 0, len(unlisted.Members))
		for _, member := range unlisted.Members {
			names = append(names, member.Name)
		}
		return nil, &hireFailure{status: http.StatusConflict, body: map[string]any{
			"error":   "team_members_unlisted",
			"msg":     "Publish these agents to the Marketplace first: " + strings.Join(names, ", ") + ".",
			"members": unlisted.Members,
		}}
	}
	if err != nil {
		slog.ErrorContext(ctx, "marketplace team publish: build the snapshot failed", "team_id", team.ID, "error", err)
		return nil, fail()
	}
	snap.Category = strings.TrimSpace(body.Category)
	for _, tag := range body.Tags {
		if tag = strings.TrimSpace(tag); tag != "" {
			snap.Tags = append(snap.Tags, tag)
		}
	}
	versionID, err := marketplace.NextTeamVersionID(ctx, tx, team.ProjectID, team.ID)
	if err != nil {
		slog.ErrorContext(ctx, "marketplace team publish: number the version failed", "team_id", team.ID, "error", err)
		return nil, fail()
	}
	versionName := body.VersionName
	if versionName == "" {
		versionName = "v" + strconv.FormatInt(versionID, 10)
	}
	listingID, err := marketplace.RecordPublish(ctx, tx, marketplace.PublishRecord{
		Snapshot: snap, SourceProjectID: team.ProjectID, SourceEntityID: team.ID,
		VersionID: versionID, VersionName: versionName, ReleaseNotes: body.ReleaseNotes, PublisherID: publisher,
	})
	if err != nil {
		slog.ErrorContext(ctx, "marketplace team publish: record the listing failed", "team_id", team.ID, "error", err)
		return nil, fail()
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fail()
	}
	return map[string]any{
		"listing_id": strconv.FormatInt(listingID, 10), "version_id": strconv.FormatInt(versionID, 10),
		"version_name": versionName, "digest": hex.EncodeToString(snap.Digest[:]),
	}, nil
}

// MarketplaceTeamHire answers POST /elitea_core/marketplace_team_hire/prompt_lib/{projectID}.
func (h *Handler) MarketplaceTeamHire(w http.ResponseWriter, r *http.Request) {
	if h.pool == nil {
		hireError(w, http.StatusServiceUnavailable, "unavailable", "The marketplace needs a database.")
		return
	}
	ctx := r.Context()
	projectIDText := chi.URLParam(r, "projectID")
	if _, ok := tenantSchema(w, projectIDText); !ok {
		return
	}
	userID, hasUser := importPrincipalUserID(ctx)
	if !hasUser {
		hireError(w, http.StatusUnauthorized, "unauthorized", "An authenticated person is required to hire.")
		return
	}
	var body teamHireBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		hireError(w, http.StatusBadRequest, "invalid_request", "The request body is not valid.")
		return
	}
	listingID, listingErr := strconv.ParseInt(body.ListingID, 10, 64)
	versionID, versionErr := strconv.ParseInt(body.VersionID, 10, 64)
	if parseProjectID(projectIDText) <= 0 || listingErr != nil || versionErr != nil || listingID <= 0 || versionID <= 0 {
		hireError(w, http.StatusBadRequest, "invalid_request", "listing_id, version_id and the project must be ids.")
		return
	}
	if body.Mode != marketplace.ModeInstall && body.Mode != marketplace.ModeFork {
		hireError(w, http.StatusBadRequest, "invalid_request", "mode must be install or fork.")
		return
	}
	h.ensureListings(ctx)
	result, failure := h.performTeamHire(ctx, projectIDText, listingID, versionID, body, userID.Int64())
	if failure != nil {
		writeJSON(w, failure.status, failure.body)
		return
	}
	if h.events != nil {
		h.events.Emit(ctx, projectIDText, "team.hired", map[string]any{
			"team_id": result.TeamID, "listing_id": listingID, "version_id": versionID, "mode": body.Mode,
		})
	}
	writeJSON(w, http.StatusCreated, result)
}

// teamHireMember is what the team hire did for one member.
type teamHireMember struct {
	Position         int      `json:"position"`
	ApplicationID    string   `json:"application_id"`
	VersionID        string   `json:"version_id"`
	Reused           bool     `json:"reused"`
	ToolsEnabled     bool     `json:"tools_enabled"`
	AwaitingApproval bool     `json:"awaiting_approval"`
	ChangeRequestID  string   `json:"change_request_id,omitempty"`
	Unbound          []string `json:"unbound"`
}

// teamHireResult is the team hire's answer.
type teamHireResult struct {
	TeamID  string           `json:"team_id"`
	Name    string           `json:"name"`
	Mode    string           `json:"mode"`
	Members []teamHireMember `json:"members"`
}

// memberFailure names the member a refusal is about.
func memberFailure(failure *hireFailure, ref marketplace.TeamMemberRef) *hireFailure {
	body, ok := failure.body.(map[string]any)
	if !ok {
		return failure
	}
	out := map[string]any{"position": ref.Position, "member": ref.Name}
	for key, value := range body {
		out[key] = value
	}
	if msg, ok := out["msg"].(string); ok {
		out["msg"] = ref.Name + ": " + msg
	}
	return &hireFailure{status: failure.status, body: out}
}

func (h *Handler) performTeamHire(ctx context.Context, projectIDText string, listingID, versionID int64,
	body teamHireBody, actorID int64) (teamHireResult, *hireFailure) {
	fail := func() *hireFailure {
		return failHire(http.StatusInternalServerError, "internal_error", "Failed to hire the team.")
	}
	schema, err := tenantschema.Quote(projectIDText)
	if err != nil {
		return teamHireResult{}, failHire(http.StatusBadRequest, "invalid_request", "The project id is not valid.")
	}
	projectID := parseProjectID(projectIDText)
	version, err := marketplace.LoadVersion(ctx, h.pool, listingID, versionID)
	if errors.Is(err, marketplace.ErrNotFound) {
		return teamHireResult{}, failHire(http.StatusNotFound, "not_found", "The listing version does not exist.")
	}
	if err != nil {
		slog.ErrorContext(ctx, "marketplace team hire: read the version failed", "listing_id", listingID, "error", err)
		return teamHireResult{}, fail()
	}
	if version.ListingKind != marketplace.KindTeam {
		return teamHireResult{}, failHire(http.StatusBadRequest, "invalid_request", "This listing is not a team; hire it with marketplace_hire.")
	}
	if !version.Hireable() {
		return teamHireResult{}, failHire(http.StatusConflict, "version_not_hireable", "This version was yanked, withdrawn or delisted. Choose another version.")
	}
	def, err := marketplace.ParseTeamDefinition(version.Definition)
	if err != nil {
		slog.ErrorContext(ctx, "marketplace team hire: read the definition failed", "listing_id", listingID, "error", err)
		return teamHireResult{}, fail()
	}
	bindingsByPosition := map[int][]hireBindingBody{}
	for _, member := range body.Members {
		bindingsByPosition[member.Position] = member.Bindings
	}

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return teamHireResult{}, fail()
	}
	defer func() { _ = tx.Rollback(ctx) }()

	result := teamHireResult{Mode: body.Mode, Members: make([]teamHireMember, 0, len(def.Members))}
	team := agentteams.Team{
		Name: def.Name, Description: def.Description, Policy: turntaking.Policy(def.FloorPolicy),
		Members: make([]agentteams.Member, 0, len(def.Members)),
	}
	if strings.TrimSpace(body.Name) != "" {
		team.Name = body.Name
	}
	for _, ref := range def.Members {
		member, failure := h.hireTeamMember(ctx, tx, schema, projectIDText, ref, body.Mode, bindingsByPosition[ref.Position], actorID)
		if failure != nil {
			return teamHireResult{}, memberFailure(failure, ref)
		}
		appID, _ := strconv.ParseInt(member.ApplicationID, 10, 64)
		agentVersion, _ := strconv.ParseInt(member.VersionID, 10, 64)
		team.Members = append(team.Members, agentteams.Member{
			ApplicationID: appID, ProjectID: projectID, VersionID: agentVersion, Name: ref.Name,
		})
		if def.LeadPosition == ref.Position {
			team.Lead = len(team.Members)
		}
		result.Members = append(result.Members, member)
	}
	if err := team.Normalize(); err != nil {
		return teamHireResult{}, failHire(http.StatusUnprocessableEntity, "team_invalid", err.Error())
	}
	teamID, err := repos.InsertTeam(ctx, tx, projectID, actorID, team)
	if errors.Is(err, agentteams.ErrNameTaken) {
		return teamHireResult{}, failHire(http.StatusConflict, "team_name_taken",
			"A team named "+team.Name+" already exists in this project. Choose another name.")
	}
	if err != nil {
		slog.ErrorContext(ctx, "marketplace team hire: write the team failed", "project_id", projectID, "error", err)
		return teamHireResult{}, fail()
	}
	if err := marketplace.RecordInstall(ctx, tx, marketplace.Install{
		ProjectID: projectID, EntityKind: marketplace.KindTeam, EntityID: teamID, ListingID: listingID, VersionID: versionID,
		Digest: version.Digest, Mode: body.Mode, ToolsOn: true, InstalledBy: actorID,
	}); err != nil {
		slog.ErrorContext(ctx, "marketplace team hire: record the hire failed", "error", err)
		return teamHireResult{}, fail()
	}
	if err := tx.Commit(ctx); err != nil {
		slog.ErrorContext(ctx, "marketplace team hire: commit failed", "error", err)
		return teamHireResult{}, fail()
	}
	result.TeamID = strconv.FormatInt(teamID, 10)
	result.Name = team.Name
	return result, nil
}

// hireTeamMember hires one member inside the team's transaction, or reuses the
// agent the project installed from the same listing version (Install only).
func (h *Handler) hireTeamMember(ctx context.Context, tx pgx.Tx, schema, projectIDText string,
	ref marketplace.TeamMemberRef, mode string, bindings []hireBindingBody, actorID int64) (teamHireMember, *hireFailure) {
	member := teamHireMember{Position: ref.Position, Unbound: []string{}}
	if mode == marketplace.ModeInstall {
		appID, agentVersion, found, err := reusableMemberAgent(ctx, tx, schema, parseProjectID(projectIDText), ref)
		if err != nil {
			slog.ErrorContext(ctx, "marketplace team hire: find a reusable agent failed", "listing_id", ref.ListingID, "error", err)
			return member, failHire(http.StatusInternalServerError, "internal_error", "Failed to hire the team.")
		}
		if found {
			member.ApplicationID = strconv.FormatInt(appID, 10)
			member.VersionID = strconv.FormatInt(agentVersion, 10)
			member.Reused = true
			member.ToolsEnabled = true
			return member, nil
		}
	}
	hired, failure := h.performHire(ctx, tx, hireInput{
		ProjectIDText: projectIDText, ListingID: ref.ListingID, VersionID: ref.VersionID, Mode: mode,
		Bindings: bindings, ActorID: actorID,
	})
	if failure != nil {
		return member, failure
	}
	member.ApplicationID, _ = hired["application_id"].(string)
	member.VersionID, _ = hired["version_id"].(string)
	member.ToolsEnabled, _ = hired["tools_enabled"].(bool)
	member.AwaitingApproval, _ = hired["awaiting_approval"].(bool)
	member.ChangeRequestID, _ = hired["change_request_id"].(string)
	for _, binding := range bindings {
		if binding.Disabled {
			member.Unbound = append(member.Unbound, binding.RequirementKey)
		}
	}
	return member, nil
}

// reusableMemberAgent is the project's live installed agent of the member's
// listing version and its base version: what a team install pins instead of
// hiring the same snapshot again.
func reusableMemberAgent(ctx context.Context, tx pgx.Tx, schema string, projectID int64, ref marketplace.TeamMemberRef) (int64, int64, bool, error) {
	appID, found, err := marketplace.ReusableInstall(ctx, tx, projectID, ref.ListingID, ref.VersionID)
	if err != nil || !found {
		return 0, 0, false, err
	}
	var versionID int64
	err = tx.QueryRow(ctx, fmt.Sprintf(`
SELECT v.id FROM %s.application_versions v
WHERE v.application_id = $1 AND v.name = 'base'
ORDER BY v.id LIMIT 1`, schema), appID).Scan(&versionID)
	if errors.Is(err, pgx.ErrNoRows) {
		// The agent was deleted and the hire not yet marked dismissed.
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return appID, versionID, true, nil
}
