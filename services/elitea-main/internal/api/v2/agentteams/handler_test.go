package agentteams_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	v2agentteams "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/agentteams"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentteams"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/turntaking"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

type fakeStore struct {
	teams    map[int64]agentteams.Stored
	created  *agentteams.Team
	roster   []agentteams.Member
	lead     int
	missing  int
	rosterOf string
}

func (s *fakeStore) ListTeams(context.Context, int64, string, int, int) ([]agentteams.Stored, int, error) {
	out := []agentteams.Stored{}
	for _, team := range s.teams {
		out = append(out, team)
	}
	return out, len(out), nil
}

func (s *fakeStore) GetTeam(_ context.Context, _ int64, teamID int64) (agentteams.Stored, error) {
	team, ok := s.teams[teamID]
	if !ok {
		return agentteams.Stored{}, agentteams.ErrNotFound
	}
	return team, nil
}

func (s *fakeStore) CreateTeam(_ context.Context, projectID, actorID int64, team agentteams.Team) (agentteams.Stored, error) {
	s.created = &team
	return agentteams.Stored{Team: team, ID: 9, ProjectID: projectID, CreatedBy: actorID}, nil
}

func (s *fakeStore) UpdateTeam(_ context.Context, projectID, teamID int64, team agentteams.Team) (agentteams.Stored, error) {
	if _, ok := s.teams[teamID]; !ok {
		return agentteams.Stored{}, agentteams.ErrNotFound
	}
	return agentteams.Stored{Team: team, ID: teamID, ProjectID: projectID}, nil
}

func (s *fakeStore) DeleteTeam(_ context.Context, _ int64, teamID int64) error {
	if _, ok := s.teams[teamID]; !ok {
		return agentteams.ErrNotFound
	}
	return nil
}

func (s *fakeStore) ConversationRoster(_ context.Context, _ int64, conversationID string) ([]agentteams.Member, int, error) {
	s.rosterOf = conversationID
	return s.roster, s.lead, nil
}

func (s *fakeStore) MissingMembers(context.Context, int64, []agentteams.Member) (int, error) {
	return s.missing, nil
}

type fakeChat struct {
	authorizeErr error
	createErr    error
	created      *conversations.Conversation
}

func (c *fakeChat) AuthorizeChatResource(context.Context, string, string, string) error {
	return c.authorizeErr
}

func (c *fakeChat) CreateConversation(_ *http.Request, projectID string, conv conversations.Conversation) (conversations.Conversation, error) {
	if c.createErr != nil {
		return conversations.Conversation{}, c.createErr
	}
	c.created = &conv
	conv.ID, conv.ProjectID = "77", projectID
	return conv, nil
}

func newRouter(store *fakeStore, chat *fakeChat) chi.Router {
	handler := v2agentteams.NewHandler(store, chat)
	router := chi.NewRouter()
	router.Get(v2agentteams.TeamsPath, handler.List)
	router.Post(v2agentteams.TeamsPath, handler.Create)
	router.Get(v2agentteams.TeamPath, handler.Get)
	router.Put(v2agentteams.TeamPath, handler.Update)
	router.Delete(v2agentteams.TeamPath, handler.Delete)
	router.Post(v2agentteams.FromConversationPath, handler.CreateFromConversation)
	router.Post(v2agentteams.StartPath, handler.Start)
	return router
}

func serve(router chi.Router, method, path, body string) (int, map[string]any) {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{UserID: "7"}))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	decoded := map[string]any{}
	_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	return recorder.Code, decoded
}

func leadTeam() agentteams.Stored {
	return agentteams.Stored{ID: 3, ProjectID: 1, Team: agentteams.Team{
		Name: "Release crew", Policy: turntaking.PolicyLead, Lead: 2,
		Members: []agentteams.Member{
			{ApplicationID: 5, ProjectID: 1, VersionID: 50, Name: "Mira"},
			{ApplicationID: 6, ProjectID: 2, VersionID: 60, Name: "CI Doctor"},
		},
	}}
}

func TestStartAddsTheTeamsAgentsThroughTheOrdinaryCreate(t *testing.T) {
	store := &fakeStore{teams: map[int64]agentteams.Stored{3: leadTeam()}}
	chat := &fakeChat{}
	code, body := serve(newRouter(store, chat), http.MethodPost, "/agent_team_conversation/prompt_lib/1/3", `{}`)
	if code != http.StatusCreated {
		t.Fatalf("status = %d, body %v", code, body)
	}
	conv := chat.created
	if conv == nil || conv.Name != "Release crew" || conv.LeadParticipant != 2 || len(conv.Participants) != 2 {
		t.Fatalf("created = %+v", conv)
	}
	second := conv.Participants[1]
	if second.EntityName != "application" || second.EntityMeta["id"] != int64(6) || second.EntityMeta["project_id"] != int64(2) ||
		second.EntitySettings["version_id"] != int64(60) {
		t.Fatalf("participant = %+v", second)
	}
}

func TestStartRefusesWhatTheParticipantPathRefuses(t *testing.T) {
	store := &fakeStore{teams: map[int64]agentteams.Stored{3: leadTeam()}}
	// The participant path refuses an agent of a project the caller is not a
	// member of; the team must not get around it.
	chat := &fakeChat{createErr: apierr.NotFound("The agent or pipeline was not found.")}
	code, _ := serve(newRouter(store, chat), http.MethodPost, "/agent_team_conversation/prompt_lib/1/3", `{}`)
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want the participant path's 404", code)
	}
}

func TestStartNamesAMissingMember(t *testing.T) {
	store := &fakeStore{teams: map[int64]agentteams.Stored{3: leadTeam()}, missing: 1}
	chat := &fakeChat{}
	code, body := serve(newRouter(store, chat), http.MethodPost, "/agent_team_conversation/prompt_lib/1/3", `{"name": "x"}`)
	if code != http.StatusUnprocessableEntity || body["code"] != "team_member_unavailable" || body["position"] != float64(1) {
		t.Fatalf("status = %d, body %v", code, body)
	}
	if chat.created != nil {
		t.Fatal("a conversation was created with a member missing")
	}
}

func TestCreateFromConversationReadsTheRosterAfterTheChatRule(t *testing.T) {
	roster := []agentteams.Member{{ApplicationID: 5, ProjectID: 1, VersionID: 50}, {ApplicationID: 6, ProjectID: 1, VersionID: 60}}
	store := &fakeStore{roster: roster, lead: 2}
	code, body := serve(newRouter(store, &fakeChat{}), http.MethodPost,
		"/agent_team_from_conversation/prompt_lib/1/12", `{"name": "Crew"}`)
	if code != http.StatusCreated || body["floor_policy"] != "lead" || body["lead_position"] != float64(2) {
		t.Fatalf("status = %d, body %v", code, body)
	}
	if store.rosterOf != "12" || store.created == nil || len(store.created.Members) != 2 {
		t.Fatalf("store = %+v", store)
	}

	denied := &fakeStore{roster: roster}
	code, _ = serve(newRouter(denied, &fakeChat{authorizeErr: apierr.NotFound("conversation not found")}), http.MethodPost,
		"/agent_team_from_conversation/prompt_lib/1/12", `{"name": "Crew"}`)
	if code != http.StatusNotFound || denied.rosterOf != "" {
		t.Fatalf("status = %d, roster read %q; a conversation the caller cannot read must not be read", code, denied.rosterOf)
	}
}

func TestCreateFromAConversationWithoutAgentsIsInvalid(t *testing.T) {
	code, body := serve(newRouter(&fakeStore{}, &fakeChat{}), http.MethodPost,
		"/agent_team_from_conversation/prompt_lib/1/12", `{"name": "Crew"}`)
	if code != http.StatusUnprocessableEntity || body["code"] != "team_invalid" {
		t.Fatalf("status = %d, body %v", code, body)
	}
}

func TestCreateValidatesAndRefusesUnknownFields(t *testing.T) {
	router := newRouter(&fakeStore{}, &fakeChat{})
	code, body := serve(router, http.MethodPost, "/agent_teams/prompt_lib/1",
		`{"name": "Crew", "floor_policy": "lead", "lead_position": 3, "members": [{"application_id": 1, "project_id": 1, "version_id": 1}]}`)
	if code != http.StatusUnprocessableEntity || body["code"] != "team_invalid" {
		t.Fatalf("status = %d, body %v", code, body)
	}
	code, _ = serve(router, http.MethodPost, "/agent_teams/prompt_lib/1", `{"name": "Crew", "grants": ["x"]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown field", code)
	}
	code, body = serve(router, http.MethodPost, "/agent_teams/prompt_lib/1",
		`{"name": "Crew", "members": [{"application_id": 1, "project_id": 1, "version_id": 1, "name": "A"}]}`)
	if code != http.StatusCreated || body["floor_policy"] != "addressed" || body["lead_position"] != nil {
		t.Fatalf("status = %d, body %v", code, body)
	}
}

func TestUnknownTeamIsNotFound(t *testing.T) {
	router := newRouter(&fakeStore{teams: map[int64]agentteams.Stored{}}, &fakeChat{})
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		if code, body := serve(router, method, "/agent_team/prompt_lib/1/4", ""); code != http.StatusNotFound || body["code"] != "not_found" {
			t.Fatalf("%s status = %d, body %v", method, code, body)
		}
	}
}

func TestNilStoreAnswers503(t *testing.T) {
	handler := v2agentteams.NewHandler(nil, nil)
	recorder := httptest.NewRecorder()
	handler.List(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", recorder.Code)
	}
}
