package conversations_test

// FU-244, review item 1: the participant composition check reads only a project
// the caller may use for a conversation participant. The conversation's own
// project is read (the route authorized the caller for it); another project only
// when the caller may act inside it, or for a version the catalogue project
// publishes; anything else is refused as not found before anything is read in
// it, and nothing of it is named.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

func TestParticipantProjectIsAuthorizedBeforeItsCompositionIsRead(t *testing.T) {
	pool := newChatAuthorityPool(t)
	ctx := context.Background()
	// p_1: the conversation's project. p_2: the catalogue (ELITEA_AI_PROJECT_ID=2).
	// p_3: another project, whose agent with the same ids closes a cycle named "Secret".
	// The shared tables projectaccess.Membership reads. Actor 8 is a member of
	// the catalogue project (role 3), actor 7 is not.
	if _, err := pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS public.auth_core__user(id integer PRIMARY KEY, name text, email text, suspended boolean NOT NULL DEFAULT false);
INSERT INTO public.auth_core__user (id) VALUES (7), (8), (9), (10) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS centry.project(id integer PRIMARY KEY, suspended boolean NOT NULL DEFAULT false);
INSERT INTO centry.project (id) VALUES (1), (2), (3) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS public.auth_core__role(id integer PRIMARY KEY, name text, mode text);
CREATE TABLE IF NOT EXISTS public.auth_core__user_role(user_id integer, role_id integer);
-- Actor 9 is a platform administrator: membership admits it everywhere, but a
-- project that does not exist is still not found.
INSERT INTO public.auth_core__role VALUES (100, 'super_admin', 'administration') ON CONFLICT DO NOTHING;
INSERT INTO public.auth_core__user_role VALUES (9, 100);
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
	memberConv, err := repo.Create(chatActor("8"), "1", conversations.Conversation{Name: "Member participants"})
	if err != nil {
		t.Fatal(err)
	}
	adminConv, err := repo.Create(chatActor("9"), "1", conversations.Conversation{Name: "Administrator participants"})
	if err != nil {
		t.Fatal(err)
	}
	router := chatAuthorityRouter(pool)
	path := "/1/conversations/" + conv.ID + "/participants"
	memberPath := "/1/conversations/" + memberConv.ID + "/participants"
	adminPath := "/1/conversations/" + adminConv.ID + "/participants"
	body := func(project string, application, version int) string {
		meta := fmt.Sprintf(`"id": %d, "name": "agent"`, application)
		if project != "" {
			meta += `, "project_id": ` + project
		}
		return fmt.Sprintf(`{"entity_name": "application", "entity_meta": {%s}, "entity_settings": {"version_id": %d}}`, meta, version)
	}
	// A token acts as its owning user (or not at all); the same answers as a session.
	tokenPath := path
	for _, tc := range []struct {
		name   string
		user   auth.User
		body   string
		status int
	}{
		{"token of the conversation's author: own project", auth.User{ID: "7", UserID: "7", TokenID: "71", AuthType: "token"}, body("1", 1, 1), http.StatusOK},
		{"token of the conversation's author: foreign project", auth.User{ID: "7", UserID: "7", TokenID: "71", AuthType: "token"}, body("3", 1, 1), http.StatusNotFound},
		{"token without an owning user", auth.User{TokenID: "72", AuthType: "token"}, body("1", 1, 1), http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			callAsChatPrincipal(t, router, tc.user, "POST", tokenPath, tc.body, tc.status)
		})
	}
	for _, tc := range []struct {
		name   string
		actor  string
		body   string
		status int
		path   string
	}{
		// The route's own answers, before any participant is read.
		{"unauthenticated", "", body("1", 1, 1), http.StatusForbidden, ""},
		{"another user, not in the conversation's project", "10", body("1", 1, 1), http.StatusNotFound, ""},
		{"a project member who cannot see the private conversation", "8", body("1", 1, 1), http.StatusNotFound, ""},
		{"a foreign project's agent", "7", body("3", 1, 1), http.StatusNotFound, ""},
		{"a project that does not exist", "7", body("99", 1, 1), http.StatusNotFound, ""},
		{"a malformed project", "7", body(`"x"`, 1, 1), http.StatusBadRequest, ""},
		{"an unpublished catalogue version", "7", body("2", 51, 51), http.StatusNotFound, ""},
		{"the conversation's own project", "7", body("1", 1, 1), http.StatusOK, ""},
		{"no project: the conversation's own", "7", body("", 1, 1), http.StatusOK, ""},
		{"a published catalogue version", "7", body("2", 50, 50), http.StatusOK, ""},
		// A member of the participant's project may use its unpublished agents,
		// as on every other surface of that project; not another project's.
		{"a member of the catalogue project: an unpublished version", "8", body("2", 51, 51), http.StatusOK, memberPath},
		{"a member of the catalogue project: a project it is not in", "8", body("3", 1, 1), http.StatusNotFound, memberPath},
		{"a platform administrator: a project that does not exist", "9", body("99", 1, 1), http.StatusNotFound, adminPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := path
			if tc.path != "" {
				target = tc.path
			}
			w := callChatAuthority(t, router, tc.actor, "POST", target, tc.body, tc.status)
			if strings.Contains(w.Body.String(), "Secret") {
				t.Fatalf("a foreign project's application was named: %s", w.Body.String())
			}
		})
	}
	// Nothing of the refused projects was mapped.
	var mapped int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM p_1.chat_participants WHERE entity_name = 'application'
		AND entity_meta->>'project_id' IN ('3', '99', 'x')`).Scan(&mapped); err != nil {
		t.Fatal(err)
	}
	if mapped != 0 {
		t.Fatalf("%d refused participants were written", mapped)
	}
}

// callAsChatPrincipal is callChatAuthority for an arbitrary principal (a token).
func callAsChatPrincipal(t *testing.T, router http.Handler, user auth.User, method, path, body string, want int) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(auth.ContextWithUser(context.Background(), user))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, w.Code, want, w.Body.String())
	}
	return w
}

// Review round 2 (A): switching a participant's version and creating a
// conversation with participants authorize the participant's project the same
// way, inside the write's own transaction; a refused switch changes nothing and
// a refused create creates nothing.
func TestParticipantVersionSwitchAndCreateAuthorizeTheParticipantsProject(t *testing.T) {
	pool := newChatAuthorityPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS public.auth_core__user(id integer PRIMARY KEY, name text, email text, suspended boolean NOT NULL DEFAULT false);
INSERT INTO public.auth_core__user (id) VALUES (7), (8) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS centry.project(id integer PRIMARY KEY, suspended boolean NOT NULL DEFAULT false);
INSERT INTO centry.project (id) VALUES (1), (2), (3) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS public.auth_core__role(id integer PRIMARY KEY, name text, mode text);
CREATE TABLE IF NOT EXISTS public.auth_core__user_role(user_id integer, role_id integer);
CREATE SCHEMA p_2;
CREATE TABLE p_2.applications(id integer PRIMARY KEY, name text);
CREATE TABLE p_2.application_versions(id integer PRIMARY KEY, application_id integer, agent_type text, instructions text, status text);
CREATE TABLE p_2.elitea_tools(id serial PRIMARY KEY, name text, type text, settings jsonb);
CREATE TABLE p_2.entity_tool_mapping(id serial PRIMARY KEY, entity_version_id integer, entity_type text, tool_id integer);
INSERT INTO p_2.applications VALUES (50, 'Catalogue agent');
INSERT INTO p_2.application_versions VALUES (50, 50, 'agent', '', 'published'), (52, 50, 'agent', '', 'draft');
`); err != nil {
		t.Fatal(err)
	}
	repo := repos.NewConversationsRepo(pool)
	conv, err := repo.Create(chatActor("7"), "1", conversations.Conversation{Name: "Switches"})
	if err != nil {
		t.Fatal(err)
	}
	// Participants stored before the add check existed (written directly).
	stored := func(project, application, version int) string {
		var participant int
		if err := pool.QueryRow(ctx, `
WITH participant AS (
  INSERT INTO p_1.chat_participants (uuid, entity_name, entity_meta, meta)
  VALUES (gen_random_uuid(), 'application', jsonb_build_object('id', $1::int, 'project_id', $2::int, 'name', 'stored'), '{"name": "stored"}')
  RETURNING id)
INSERT INTO p_1.chat_participant_mapping (conversation_id, participant_id, entity_settings)
SELECT c.id, participant.id, jsonb_build_object('version_id', $3::int) FROM participant, p_1.chat_conversations c WHERE c.uuid::text = $4 OR c.id::text = $4
RETURNING participant_id`, application, project, version, conv.ID).Scan(&participant); err != nil {
			t.Fatalf("store participant: %v", err)
		}
		return fmt.Sprint(participant)
	}
	foreign, missing, catalogue := stored(3, 1, 1), stored(99, 1, 1), stored(2, 50, 50)
	router := chatAuthorityRouter(pool)
	put := func(participant string, version int, want int) {
		t.Helper()
		callChatAuthority(t, router, "7", "PUT", "/1/conversations/"+conv.ID+"/participants/"+participant,
			fmt.Sprintf(`{"version_id": %d}`, version), want)
	}
	put(foreign, 2, http.StatusNotFound)
	put(missing, 2, http.StatusNotFound)
	put(catalogue, 52, http.StatusNotFound) // an unpublished catalogue version, actor 7 not a member
	put(catalogue, 50, http.StatusOK)
	var versions []string
	if err := pool.QueryRow(ctx, `SELECT array_agg(entity_settings->>'version_id' ORDER BY participant_id) FROM p_1.chat_participant_mapping
		WHERE participant_id::text = ANY($1)`, []string{foreign, missing}).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if strings.Join(versions, ",") != "1,1" {
		t.Fatalf("a refused switch changed the stored versions: %v", versions)
	}

	create := func(name, project string, application, version int, want int) {
		t.Helper()
		callChatAuthority(t, router, "7", "POST", "/1/conversations", fmt.Sprintf(
			`{"name": %q, "participants": [{"entity_name": "application", "entity_meta": {"id": %d, "name": "agent", "project_id": %s}, "entity_settings": {"version_id": %d}}]}`,
			name, application, project, version), want)
	}
	create("create foreign", "3", 1, 1, http.StatusNotFound)
	create("create missing", "99", 1, 1, http.StatusNotFound)
	create("create catalogue draft", "2", 50, 52, http.StatusNotFound)
	create("create catalogue", "2", 50, 50, http.StatusCreated)
	var created []string
	if err := pool.QueryRow(ctx, `SELECT coalesce(array_agg(name ORDER BY name), '{}') FROM p_1.chat_conversations WHERE name LIKE 'create %'`).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if strings.Join(created, ",") != "create catalogue" {
		t.Fatalf("a refused create left conversations: %v", created)
	}
}
