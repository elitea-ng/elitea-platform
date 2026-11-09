// Package agentteams serves the saved team routes (ADR-0050 decision 8,
// issue 1620): list, read, save, edit and delete a project's teams, save a
// conversation's agents as a team, and start a conversation from a team.
//
// WHO MAY DO WHAT. The router gates every route on a models.teams.*
// permission (shared 0233). Two routes cross into chat and take the chat
// permission as well:
//
//   - saving a conversation as a team also takes
//     models.chat.conversation.details, and the handler applies the chat
//     detail rule (AuthorizeChatResource) to the conversation, so a team can
//     only be made from a conversation the caller may read;
//   - starting a conversation from a team also takes
//     models.chat.conversations.create, and the conversation is made by the
//     ordinary create path (conversations.Handler.CreateConversation), whose
//     participant path checks the caller may use every agent.
//
// A TEAM ADDS NO CAPABILITY: it lists agents, and starting it adds only what
// the caller could have added by hand. An agent the caller cannot use refuses
// the whole start.
package agentteams

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentteams"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// The route patterns, relative to the /api/v2/elitea_core group.
const (
	TeamsPath            = "/agent_teams/prompt_lib/{projectID}"
	TeamPath             = "/agent_team/prompt_lib/{projectID}/{teamID}"
	FromConversationPath = "/agent_team_from_conversation/prompt_lib/{projectID}/{conversationID}"
	StartPath            = "/agent_team_conversation/prompt_lib/{projectID}/{teamID}"
	maxBody              = 32 << 10
)

// Store is *repos.AgentTeams.
type Store interface {
	ListTeams(ctx context.Context, projectID int64, query string, limit, offset int) ([]agentteams.Stored, int, error)
	GetTeam(ctx context.Context, projectID, teamID int64) (agentteams.Stored, error)
	CreateTeam(ctx context.Context, projectID, actorID int64, team agentteams.Team) (agentteams.Stored, error)
	UpdateTeam(ctx context.Context, projectID, teamID int64, team agentteams.Team) (agentteams.Stored, error)
	DeleteTeam(ctx context.Context, projectID, teamID int64) error
	ConversationRoster(ctx context.Context, projectID int64, conversationID string) ([]agentteams.Member, int, error)
	MissingMembers(ctx context.Context, projectID int64, members []agentteams.Member) (int, error)
}

var _ Store = (*repos.AgentTeams)(nil)

// Conversations is the chat half: the read rule for a source conversation and
// the ordinary create.
type Conversations interface {
	AuthorizeChatResource(ctx context.Context, projectID, resourceKind, resourceID string) error
	CreateConversation(r *http.Request, projectID string, conv conversations.Conversation) (conversations.Conversation, error)
}

// Handler serves the routes.
type Handler struct {
	store Store
	chat  Conversations
}

// NewHandler builds the handler. A nil store answers 503; a nil chat answers
// 503 on the two routes that need it.
func NewHandler(store Store, chat Conversations) *Handler {
	if typed, ok := store.(*repos.AgentTeams); ok && typed == nil {
		store = nil
	}
	return &Handler{store: store, chat: chat}
}

// MemberJSON is a roster member on the wire.
type MemberJSON struct {
	ApplicationID int64  `json:"application_id"`
	ProjectID     int64  `json:"project_id"`
	VersionID     int64  `json:"version_id"`
	Name          string `json:"name"`
}

// TeamJSON is a team on the wire.
type TeamJSON struct {
	ID          int64        `json:"id"`
	ProjectID   int64        `json:"project_id"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	FloorPolicy string       `json:"floor_policy"`
	Lead        *int         `json:"lead_position"`
	Members     []MemberJSON `json:"members"`
	CreatedBy   int64        `json:"created_by"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

func teamJSON(team agentteams.Stored) TeamJSON {
	out := TeamJSON{
		ID: team.ID, ProjectID: team.ProjectID, Name: team.Name, Description: team.Description,
		FloorPolicy: string(team.Policy), CreatedBy: team.CreatedBy, CreatedAt: team.CreatedAt, UpdatedAt: team.UpdatedAt,
		Members: make([]MemberJSON, 0, len(team.Members)),
	}
	if team.Lead > 0 {
		lead := team.Lead
		out.Lead = &lead
	}
	for _, member := range team.Members {
		out.Members = append(out.Members, MemberJSON(member))
	}
	return out
}

// teamBody is the save and edit body.
type teamBody struct {
	Name        string       `json:"name"`
	Description string       `json:"description"`
	FloorPolicy string       `json:"floor_policy"`
	Lead        *int         `json:"lead_position"`
	Members     []MemberJSON `json:"members"`
}

func (b teamBody) team() agentteams.Team {
	team := agentteams.Team{Name: b.Name, Description: b.Description, Policy: turntaking.Policy(b.FloorPolicy)}
	if b.Lead != nil {
		team.Lead = *b.Lead
	}
	for _, member := range b.Members {
		team.Members = append(team.Members, agentteams.Member(member))
	}
	return team
}

func (h *Handler) ready(w http.ResponseWriter) bool {
	if h == nil || h.store == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return false
	}
	return true
}

// List answers the project's teams.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	projectID, ok := pathID(w, r, "projectID")
	if !ok {
		return
	}
	query := r.URL.Query()
	limit, offset := 50, 0
	for name, target := range map[string]*int{"limit": &limit, "offset": &offset} {
		if raw := query.Get(name); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 0 || (name == "limit" && (value < 1 || value > 200)) {
				apierr.Write(w, apierr.BadRequest(name+" is not valid"))
				return
			}
			*target = value
		}
	}
	search := strings.TrimSpace(query.Get("query"))
	if len(search) > 128 {
		apierr.Write(w, apierr.BadRequest("query is longer than 128 characters"))
		return
	}
	teams, total, err := h.store.ListTeams(r.Context(), projectID, search, limit, offset)
	if err != nil {
		writeError(w, r, err)
		return
	}
	rows := make([]TeamJSON, 0, len(teams))
	for _, team := range teams {
		rows = append(rows, teamJSON(team))
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "total": total})
}

// Get answers one team.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	projectID, teamID, ok := h.teamPath(w, r)
	if !ok {
		return
	}
	team, err := h.store.GetTeam(r.Context(), projectID, teamID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, teamJSON(team))
}

// Create saves a team.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	projectID, ok := pathID(w, r, "projectID")
	if !ok {
		return
	}
	actorID, ok := actor(w, r)
	if !ok {
		return
	}
	var body teamBody
	if !decode(w, r, &body) {
		return
	}
	team := body.team()
	if err := team.Normalize(); err != nil {
		writeError(w, r, err)
		return
	}
	stored, err := h.store.CreateTeam(r.Context(), projectID, actorID, team)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, teamJSON(stored))
}

// Update replaces a team's fields and roster.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	projectID, teamID, ok := h.teamPath(w, r)
	if !ok {
		return
	}
	var body teamBody
	if !decode(w, r, &body) {
		return
	}
	team := body.team()
	if err := team.Normalize(); err != nil {
		writeError(w, r, err)
		return
	}
	stored, err := h.store.UpdateTeam(r.Context(), projectID, teamID, team)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, teamJSON(stored))
}

// Delete deletes a team. Conversations started from it are not touched.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	projectID, teamID, ok := h.teamPath(w, r)
	if !ok {
		return
	}
	if err := h.store.DeleteTeam(r.Context(), projectID, teamID); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// CreateFromConversation saves a conversation's agents, floor and lead as a
// team.
func (h *Handler) CreateFromConversation(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	if h.chat == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "chat unavailable")
		return
	}
	projectID, ok := pathID(w, r, "projectID")
	if !ok {
		return
	}
	actorID, ok := actor(w, r)
	if !ok {
		return
	}
	conversationID := chi.URLParam(r, "conversationID")
	if err := h.chat.AuthorizeChatResource(r.Context(), strconv.FormatInt(projectID, 10), "conversation", conversationID); err != nil {
		apierr.Write(w, err)
		return
	}
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !decode(w, r, &body) {
		return
	}
	members, lead, err := h.store.ConversationRoster(r.Context(), projectID, conversationID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	team := agentteams.Team{Name: body.Name, Description: body.Description, Policy: turntaking.PolicyAddressed, Members: members}
	if lead > 0 {
		team.Policy, team.Lead = turntaking.PolicyLead, lead
	}
	if err := team.Normalize(); err != nil {
		writeError(w, r, err)
		return
	}
	stored, err := h.store.CreateTeam(r.Context(), projectID, actorID, team)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, teamJSON(stored))
}

// Start creates a conversation with the team's agents, floor and lead, by the
// ordinary create path, and answers it as the create route does (201).
func (h *Handler) Start(w http.ResponseWriter, r *http.Request) {
	projectID, teamID, ok := h.teamPath(w, r)
	if !ok {
		return
	}
	if h.chat == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "chat unavailable")
		return
	}
	var body struct {
		Name      string `json:"name"`
		IsPrivate *bool  `json:"is_private"`
	}
	if !decode(w, r, &body) {
		return
	}
	team, err := h.store.GetTeam(r.Context(), projectID, teamID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// A member of the team's own project that is gone is named; a member of
	// another project is left to the participant path, which refuses it
	// without saying whether it exists.
	if position, err := h.store.MissingMembers(r.Context(), projectID, team.Members); err != nil {
		writeError(w, r, err)
		return
	} else if position > 0 {
		writeError(w, r, &agentteams.MemberUnavailable{Position: position})
		return
	}
	conv := conversations.Conversation{Name: strings.TrimSpace(body.Name), IsPrivate: body.IsPrivate}
	if conv.Name == "" {
		conv.Name = team.Name
	}
	for _, member := range team.Members {
		meta := map[string]any{"id": member.ApplicationID, "project_id": member.ProjectID}
		if member.Name != "" {
			meta["name"] = member.Name
		}
		conv.Participants = append(conv.Participants, conversations.Participant{
			EntityName: "application", EntityMeta: meta,
			EntitySettings: map[string]any{"version_id": member.VersionID},
		})
	}
	if team.Policy == turntaking.PolicyLead {
		conv.LeadParticipant = team.Lead
	}
	created, err := h.chat.CreateConversation(r, strconv.FormatInt(projectID, 10), conv)
	if err != nil {
		conversations.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *Handler) teamPath(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	if !h.ready(w) {
		return 0, 0, false
	}
	projectID, ok := pathID(w, r, "projectID")
	if !ok {
		return 0, 0, false
	}
	teamID, ok := pathID(w, r, "teamID")
	return projectID, teamID, ok
}

func actor(w http.ResponseWriter, r *http.Request) (int64, bool) {
	principal, _ := auth.UserFromContext(r.Context())
	userID, ok := principal.OwningUserID()
	if !ok || userID <= 0 {
		apierr.WriteStatus(w, http.StatusUnauthorized, "this action needs a signed-in user")
		return 0, false
	}
	return userID, true
}

// Refusal codes.
const (
	CodeNotFound          = "not_found"
	CodeNameTaken         = "team_name_taken"
	CodeInvalid           = "team_invalid"
	CodeMemberUnavailable = "team_member_unavailable"
)

func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var unavailable *agentteams.MemberUnavailable
	switch {
	case errors.Is(err, agentteams.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "The team does not exist.", "code": CodeNotFound})
	case errors.Is(err, agentteams.ErrNameTaken):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": CodeNameTaken})
	case errors.Is(err, agentteams.ErrInvalid):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error(), "code": CodeInvalid})
	case errors.As(err, &unavailable):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": unavailable.Error(), "code": CodeMemberUnavailable, "position": unavailable.Position,
		})
	default:
		var apiErr *apierr.APIError
		if errors.As(err, &apiErr) {
			apierr.Write(w, err)
			return
		}
		slog.ErrorContext(r.Context(), "agent teams", "error", err)
		apierr.WriteStatus(w, http.StatusInternalServerError, "could not read or save the team")
	}
}

func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	value, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil || value <= 0 || value > 1<<53 {
		apierr.Write(w, apierr.BadRequest("the path's "+name+" is not a valid id"))
		return 0, false
	}
	return value, true
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(raw) > maxBody {
		apierr.Write(w, apierr.BadRequest("the request body is not readable or too large"))
		return false
	}
	if strings.TrimSpace(string(raw)) == "" {
		raw = []byte("{}")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		apierr.Write(w, apierr.BadRequest("the request body is not valid JSON for this route"))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
