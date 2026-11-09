package api

// Saved teams (ADR-0050 decision 8, #1620): every route is gated on its
// models.teams.* permission, and the two routes that cross into chat on the
// chat permission as well. Each row withholds one permission from an
// otherwise entitled member and expects 403 from the gate; with every
// permission held the request reaches the handler (which, with no database
// in this router, answers 503).

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestAgentTeamRoutesNeedTheirPermissions(t *testing.T) {
	routes := []struct {
		method, path string
		permissions  []string
	}{
		{http.MethodGet, "/api/v2/elitea_core/agent_teams/prompt_lib/7", []string{"models.teams.list"}},
		{http.MethodPost, "/api/v2/elitea_core/agent_teams/prompt_lib/7", []string{"models.teams.create"}},
		{http.MethodGet, "/api/v2/elitea_core/agent_team/prompt_lib/7/3", []string{"models.teams.list"}},
		{http.MethodPut, "/api/v2/elitea_core/agent_team/prompt_lib/7/3", []string{"models.teams.update"}},
		{http.MethodDelete, "/api/v2/elitea_core/agent_team/prompt_lib/7/3", []string{"models.teams.delete"}},
		{http.MethodPost, "/api/v2/elitea_core/agent_team_from_conversation/prompt_lib/7/1",
			[]string{"models.teams.create", "models.chat.conversation.details"}},
		{http.MethodPost, "/api/v2/elitea_core/agent_team_conversation/prompt_lib/7/3",
			[]string{"models.teams.list", "models.chat.conversations.create"}},
	}
	// Every teams string and the two chat strings, so a row that withholds
	// one still holds every other.
	all := []string{
		"models.teams.list", "models.teams.create", "models.teams.update", "models.teams.delete",
		"models.chat.conversation.details", "models.chat.conversations.create",
	}
	serve := func(granted []string, method, path string) *httptest.ResponseRecorder {
		router := newEliteaCoreProjectScopeRouter(&memberOfProject{project: "7"},
			fakePermissionResolver{granted: granted, forProject: "7"})
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, testAuthHeader(httptest.NewRequest(method, path, strings.NewReader("{}"))))
		return recorder
	}
	for _, route := range routes {
		for _, withheld := range route.permissions {
			t.Run(route.method+" "+route.path+" without "+withheld, func(t *testing.T) {
				granted := slices.DeleteFunc(slices.Clone(all), func(permission string) bool { return permission == withheld })
				if recorder := serve(granted, route.method, route.path); recorder.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403 for a member without %q; body=%s", recorder.Code, withheld, recorder.Body.String())
				}
			})
		}
		t.Run(route.method+" "+route.path+" with every permission", func(t *testing.T) {
			if recorder := serve(all, route.method, route.path); recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want the handler's 503 (no database); body=%s", recorder.Code, recorder.Body.String())
			}
		})
		t.Run(route.method+" "+route.path+" in another project", func(t *testing.T) {
			other := strings.Replace(route.path, "/prompt_lib/7", "/prompt_lib/8", 1)
			if recorder := serve(all, route.method, other); recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}
