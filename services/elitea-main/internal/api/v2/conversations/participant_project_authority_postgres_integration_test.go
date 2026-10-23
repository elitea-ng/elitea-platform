package conversations_test

// FU-244, review item 1: the participant composition check reads only a project
// the caller may use for a conversation participant. The conversation's own
// project is read (the route authorized the caller for it); the catalogue
// project is read only for a version it publishes; any other project is refused
// as not found before anything is read in it, and nothing of it is named.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

func TestParticipantProjectIsAuthorizedBeforeItsCompositionIsRead(t *testing.T) {
	pool := newChatAuthorityPool(t)
	ctx := context.Background()
	// p_1: the conversation's project. p_2: the catalogue (ELITEA_AI_PROJECT_ID=2).
	// p_3: another project, whose agent with the same ids closes a cycle named "Secret".
	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.applications (id, name) VALUES (1, 'Leaf');
INSERT INTO p_1.application_versions (id, application_id, agent_type, instructions) VALUES (1, 1, 'agent', '');
CREATE SCHEMA p_2;
CREATE TABLE p_2.applications(id integer PRIMARY KEY, name text);
CREATE TABLE p_2.application_versions(id integer PRIMARY KEY, application_id integer, agent_type text, instructions text, status text);
CREATE TABLE p_2.elitea_tools(id serial PRIMARY KEY, name text, type text, settings jsonb);
CREATE TABLE p_2.entity_tool_mapping(id serial PRIMARY KEY, entity_version_id integer, entity_type text, tool_id integer);
INSERT INTO p_2.applications VALUES (50, 'Catalogue agent'), (51, 'Catalogue draft');
INSERT INTO p_2.application_versions VALUES (50, 50, 'agent', '', 'published'), (51, 51, 'agent', '', 'draft');
CREATE SCHEMA p_3;
CREATE TABLE p_3.applications(id integer PRIMARY KEY, name text);
CREATE TABLE p_3.application_versions(id integer PRIMARY KEY, application_id integer, agent_type text, instructions text);
CREATE TABLE p_3.elitea_tools(id serial PRIMARY KEY, name text, type text, settings jsonb);
CREATE TABLE p_3.entity_tool_mapping(id serial PRIMARY KEY, entity_version_id integer, entity_type text, tool_id integer);
INSERT INTO p_3.applications VALUES (1, 'Secret');
INSERT INTO p_3.application_versions VALUES (1, 1, 'agent', '');
INSERT INTO p_3.elitea_tools (id, name, type, settings) VALUES (1, 'Secret', 'application', '{"application_id": 1, "application_version_id": 1}');
INSERT INTO p_3.entity_tool_mapping (entity_version_id, entity_type, tool_id) VALUES (1, 'agent', 1);
`); err != nil {
		t.Fatal(err)
	}
	repo := repos.NewConversationsRepo(pool)
	conv, err := repo.Create(chatActor("7"), "1", conversations.Conversation{Name: "Participants"})
	if err != nil {
		t.Fatal(err)
	}
	router := chatAuthorityRouter(pool)
	path := "/1/conversations/" + conv.ID + "/participants"
	body := func(project string, application, version int) string {
		meta := fmt.Sprintf(`"id": %d, "name": "agent"`, application)
		if project != "" {
			meta += `, "project_id": ` + project
		}
		return fmt.Sprintf(`{"entity_name": "application", "entity_meta": {%s}, "entity_settings": {"version_id": %d}}`, meta, version)
	}
	for _, tc := range []struct {
		name   string
		actor  string
		body   string
		status int
	}{
		// The route's own answers, before any participant is read.
		{"unauthenticated", "", body("1", 1, 1), http.StatusForbidden},
		{"another user, not in the conversation's project", "10", body("1", 1, 1), http.StatusNotFound},
		{"a project member who cannot see the private conversation", "8", body("1", 1, 1), http.StatusNotFound},
		{"a foreign project's agent", "7", body("3", 1, 1), http.StatusNotFound},
		{"a project that does not exist", "7", body("99", 1, 1), http.StatusNotFound},
		{"a malformed project", "7", body(`"x"`, 1, 1), http.StatusBadRequest},
		{"an unpublished catalogue version", "7", body("2", 51, 51), http.StatusNotFound},
		{"the conversation's own project", "7", body("1", 1, 1), http.StatusOK},
		{"no project: the conversation's own", "7", body("", 1, 1), http.StatusOK},
		{"a published catalogue version", "7", body("2", 50, 50), http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := callChatAuthority(t, router, tc.actor, "POST", path, tc.body, tc.status)
			if strings.Contains(w.Body.String(), "Secret") {
				t.Fatalf("a foreign project's application was named: %s", w.Body.String())
			}
		})
	}
	// Nothing of the refused projects was mapped.
	var mapped int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM p_1.chat_participants WHERE entity_name = 'application'
		AND entity_meta->>'project_id' IN ('3', '99', 'x') OR (entity_meta->>'project_id' = '2' AND entity_meta->>'id' = '51')`).Scan(&mapped); err != nil {
		t.Fatal(err)
	}
	if mapped != 0 {
		t.Fatalf("%d refused participants were written", mapped)
	}
}
