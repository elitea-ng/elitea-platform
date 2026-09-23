package conversations_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/folders"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newChatAuthorityPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("ELITEA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set ELITEA_TEST_DATABASE_URL for isolated chat authority PostgreSQL tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("elitea_chat_auth_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	for _, key := range []string{"ELITEA_AI_PROJECT_ID", "AI_PROJECT_ID", "PUBLIC_PROJECT_ID", "SHARED_PROJECT_ID"} {
		t.Setenv(key, "2")
	}
	_, err = pool.Exec(ctx, `
CREATE SCHEMA p_1;
CREATE SCHEMA centry;
CREATE TABLE centry.platform_config(section text,key text,value jsonb);
CREATE TABLE public.auth_core__project_role(id integer PRIMARY KEY,project_id integer,name text);
CREATE TABLE public.auth_core__project_user_role(project_id integer,user_id integer,role_id integer);
INSERT INTO public.auth_core__project_role VALUES (1,1,'admin'),(2,1,'viewer'),(3,2,'admin');
INSERT INTO public.auth_core__project_user_role VALUES (1,9,1),(1,7,2),(1,8,2),(2,8,3);
CREATE TABLE p_1.chat_conversation_folders(id serial PRIMARY KEY,name text,owner_id integer,position integer,uuid uuid,meta jsonb,created_at timestamp DEFAULT now(),updated_at timestamp);
CREATE TABLE p_1.chat_conversations(id serial PRIMARY KEY,uuid uuid UNIQUE DEFAULT gen_random_uuid(),name text NOT NULL,author_id integer NOT NULL,is_private boolean NOT NULL DEFAULT true,meta jsonb NOT NULL DEFAULT '{}',source text DEFAULT 'elitea',instructions text,folder_id integer REFERENCES p_1.chat_conversation_folders(id),created_at timestamp DEFAULT now(),updated_at timestamp);
CREATE TABLE p_1.chat_participants(id serial PRIMARY KEY,uuid uuid UNIQUE,entity_name text,entity_meta jsonb,meta jsonb,created_at timestamp DEFAULT now(),updated_at timestamp);
ALTER TABLE p_1.chat_conversations ADD COLUMN attachment_participant_id integer REFERENCES p_1.chat_participants(id);
CREATE TABLE p_1.chat_participant_mapping(id serial PRIMARY KEY,conversation_id integer REFERENCES p_1.chat_conversations(id),participant_id integer REFERENCES p_1.chat_participants(id),entity_settings jsonb DEFAULT '{}',created_at timestamp DEFAULT now(),updated_at timestamp,UNIQUE(participant_id,conversation_id));
CREATE TABLE p_1.chat_message_group(id serial PRIMARY KEY,uuid uuid UNIQUE DEFAULT gen_random_uuid(),conversation_id integer REFERENCES p_1.chat_conversations(id),author_participant_id integer,meta jsonb DEFAULT '{}',created_at timestamp DEFAULT now());
CREATE TABLE p_1.chat_message_items(id serial PRIMARY KEY,uuid uuid UNIQUE DEFAULT gen_random_uuid(),message_group_id integer REFERENCES p_1.chat_message_group(id),item_type text);
CREATE TABLE p_1.chat_messages_canvas(id integer PRIMARY KEY REFERENCES p_1.chat_message_items(id));
CREATE SCHEMA IF NOT EXISTS centry;
CREATE TABLE centry.social_pins(entity text,project_id integer,entity_id integer,user_id integer);
CREATE TABLE p_1.chat_selected_conversations(conversation_id integer,user_id integer);
`)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func chatActor(id string) context.Context {
	return auth.ContextWithUser(context.Background(), auth.User{ID: id})
}

func chatAuthorityRouter(pool *pgxpool.Pool) http.Handler {
	repo := repos.NewConversationsRepo(pool)
	h := conversations.NewHandler(repo).WithPool(pool)
	f := folders.NewHandler(repos.NewFoldersRepo(pool)).WithPool(pool)
	r := chi.NewRouter()
	r.Post("/{projectID}/conversations", h.Create)
	r.Get("/{projectID}/conversations", h.List)
	r.Get("/{projectID}/conversations/{conversationID}", h.Get)
	r.Put("/{projectID}/conversations/{conversationID}", h.Update)
	r.Delete("/{projectID}/conversations/{conversationID}", h.Delete)
	r.Get("/{projectID}/conversations/{conversationID}/messages", h.ListMessages)
	r.Post("/{projectID}/conversations/{conversationID}/participants", h.AddParticipant)
	r.Delete("/{projectID}/conversations/{conversationID}/participants/{participantID}", h.RemoveParticipant)
	r.Put("/{projectID}/conversations/{conversationID}/participants/{participantID}", h.UpdateEntitySettings)
	r.Get("/{projectID}/messages/{messageID}", h.GetMessage)
	r.Get("/{projectID}/canvas/{canvasID}", h.GetCanvas)
	r.Get("/{projectID}/folders", f.List)
	r.Post("/{projectID}/folders", f.Create)
	r.Put("/{projectID}/folders/{folderID}", f.Update)
	r.Delete("/{projectID}/folders/{folderID}", f.Delete)
	return r
}

func callChatAuthority(t *testing.T, router http.Handler, actor, method, path, body string, want int) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if actor != "" {
		r = r.WithContext(chatActor(actor))
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s %s actor=%s: status=%d want=%d body=%s", method, path, actor, w.Code, want, w.Body.String())
	}
	return w
}

func TestChatAuthorityAtomicCreationAndActor(t *testing.T) {
	pool := newChatAuthorityPool(t)
	router := chatAuthorityRouter(pool)
	w := callChatAuthority(t, router, "7", "POST", "/1/conversations", `{"name":"Created","author_id":8,"is_private":false,"source":" Elitea ","instructions":"Keep this","meta":{"steps_limit":12},"participants":[{"entity_name":"application","entity_meta":{"id":41,"name":"Agent"},"entity_settings":{"version_id":3}}]}`, 201)
	var created conversations.Conversation
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	var author, count int
	var private bool
	var source, instructions string
	if err := pool.QueryRow(context.Background(), `SELECT author_id,is_private,source,instructions FROM p_1.chat_conversations WHERE id=$1`, created.ID).Scan(&author, &private, &source, &instructions); err != nil {
		t.Fatal(err)
	}
	if author != 7 || private || source != "elitea" || instructions != "Keep this" {
		t.Fatalf("stored identity/settings: %d %v %s %s", author, private, source, instructions)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM p_1.chat_participant_mapping m JOIN p_1.chat_participants p ON p.id=m.participant_id WHERE m.conversation_id=$1 AND ((p.entity_name='user' AND p.entity_meta->>'id'='7') OR p.entity_name='dummy' OR (p.entity_name='application' AND p.entity_meta->>'project_id'='1' AND m.entity_settings->>'version_id'='3'))`, created.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("required and initial participants: %d", count)
	}
	if len(created.Participants) != 3 || created.Meta["single_participant"] == nil {
		t.Fatalf("creation response loses persisted participants: %+v", created)
	}
	callChatAuthority(t, router, "", "POST", "/1/conversations", `{"name":"Unauthenticated","author_id":7}`, 403)
	callChatAuthority(t, router, "7", "POST", "/1/conversations", `{"name":"Rollback","participants":[{"entity_name":"unknown","entity_meta":{}}]}`, 400)
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM p_1.chat_conversations`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("failed creation persisted: count=%d error=%v", count, err)
	}
	callChatAuthority(t, router, "7", "POST", "/2/conversations", `{"name":"Public refusal","is_private":false}`, 400)
}

func TestChatAuthorityPrivateReadsMutationsAndFolders(t *testing.T) {
	pool := newChatAuthorityPool(t)
	router := chatAuthorityRouter(pool)
	ctx := context.Background()
	repo := repos.NewConversationsRepo(pool)
	private, err := repo.Create(chatActor("7"), "1", conversations.Conversation{Name: "Private owner"})
	if err != nil {
		t.Fatal(err)
	}
	publicFlag := false
	public, err := repo.Create(chatActor("7"), "1", conversations.Conversation{Name: "Public chat", IsPrivate: &publicFlag})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := repos.NewFoldersRepo(pool).Create(chatActor("7"), "1", folders.Folder{Name: "Private folder"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE p_1.chat_conversations SET folder_id=$1 WHERE id=$2`, foreign.ID, private.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{private.ID, private.UUID} {
		callChatAuthority(t, router, "8", "GET", "/1/conversations/"+id, "", 404)
	}
	callChatAuthority(t, router, "7", "GET", "/1/conversations/"+private.ID, "", 200)
	callChatAuthority(t, router, "8", "GET", "/1/conversations/"+public.ID, "", 200)
	for _, test := range []struct{ method, path, body string }{
		{"PUT", "/1/conversations/" + private.ID, `{"name":"Stolen"}`},
		{"DELETE", "/1/conversations/" + private.ID, ""},
		{"GET", "/1/conversations/" + private.ID + "/messages", ""},
		{"POST", "/1/conversations/" + private.ID + "/participants", `[{"entity_name":"user","entity_meta":{"id":8}}]`},
		{"DELETE", "/1/conversations/" + private.ID + "/participants/1", ""},
		{"PUT", "/1/conversations/" + private.ID + "/participants/1", `{"settings":{}}`},
		{"PUT", "/1/folders/" + foreign.ID, `{"name":"Stolen folder"}`},
		{"DELETE", "/1/folders/" + foreign.ID, ""},
	} {
		callChatAuthority(t, router, "8", test.method, test.path, test.body, 404)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO p_1.chat_selected_conversations(conversation_id,user_id) VALUES ($1,8)`, private.ID); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/1/conversations", "/1/folders", "/1/folders?grouped=true"} {
		w := callChatAuthority(t, router, "8", "GET", path, "", 200)
		if path == "/1/folders?grouped=true" {
			var sidebar map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &sidebar); err != nil {
				t.Fatal(err)
			}
			if sidebar["selected_conversation_id"] != nil {
				t.Fatal("inaccessible selected conversation leaked")
			}
		}
		if strings.Contains(w.Body.String(), "Private owner") || strings.Contains(w.Body.String(), "Private folder") {
			t.Fatalf("private data leaked at %s: %s", path, w.Body.String())
		}
	}
	callChatAuthority(t, router, "8", "GET", "/1/folders?folder_id="+foreign.ID, "", 404)
	// Participation grants visibility even when the actor is not the author.
	if err := repo.AddParticipant(ctx, "1", private.ID, map[string]any{"entity_name": "user", "entity_meta": map[string]any{"id": 8}}); err != nil {
		t.Fatal(err)
	}
	callChatAuthority(t, router, "8", "GET", "/1/conversations/"+private.UUID, "", 200)
	sidebarResponse := callChatAuthority(t, router, "8", "GET", "/1/folders?grouped=true", "", 200)
	var sidebar map[string]any
	if err := json.Unmarshal(sidebarResponse.Body.Bytes(), &sidebar); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(sidebar["selected_conversation_id"]) != private.ID {
		t.Fatal("visible selected conversation lost")
	}

	w := callChatAuthority(t, router, "8", "GET", "/1/folders", "", 200)
	if !strings.Contains(w.Body.String(), "Private folder") {
		t.Fatal("participant cannot see the containing folder")
	}
	callChatAuthority(t, router, "8", "PUT", "/1/folders/"+foreign.ID, `{"name":"Still forbidden"}`, 404)
}

func TestChatAuthorityAdminExceptionsAndFailures(t *testing.T) {
	pool := newChatAuthorityPool(t)
	router := chatAuthorityRouter(pool)
	repo := repos.NewConversationsRepo(pool)
	ordinary, err := repo.Create(chatActor("7"), "1", conversations.Conversation{Name: "Ordinary private"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := repo.Create(chatActor("7"), "1", conversations.Conversation{Name: "Run private", Meta: map[string]any{"single_participant": map[string]any{"entity_name": "application"}}})
	if err != nil {
		t.Fatal(err)
	}
	w := callChatAuthority(t, router, "9", "GET", "/1/conversations", "", 200)
	if !strings.Contains(w.Body.String(), "Ordinary private") {
		t.Fatal("admin list exception missing")
	}
	callChatAuthority(t, router, "9", "GET", "/1/conversations/"+ordinary.ID, "", 404)
	callChatAuthority(t, router, "9", "GET", "/1/conversations/"+run.ID, "", 200)
	callChatAuthority(t, router, "8", "GET", "/1/conversations/"+run.ID, "", 404)
	callChatAuthority(t, router, "", "GET", "/1/conversations", "", 403)
	if _, err := pool.Exec(context.Background(), `ALTER TABLE public.auth_core__project_role RENAME COLUMN name TO broken_name`); err != nil {
		t.Fatal(err)
	}
	w = callChatAuthority(t, router, "9", "GET", "/1/conversations", "", 500)
	if strings.Contains(w.Body.String(), "broken_name") || strings.Contains(w.Body.String(), "SQLSTATE") {
		t.Fatal("database failure leaked")
	}
}

func TestChatAuthoritySettingsRequireMappedParticipant(t *testing.T) {
	pool := newChatAuthorityPool(t)
	repo := repos.NewConversationsRepo(pool)
	conversation, err := repo.Create(chatActor("7"), "1", conversations.Conversation{Name: "Settings owner"})
	if err != nil {
		t.Fatal(err)
	}
	router := chatAuthorityRouter(pool)
	callChatAuthority(t, router, "7", "PUT", "/1/conversations/"+conversation.ID+"/participants/2147483647", `{"steps_limit":12}`, 404)
	var participantID string
	if err := pool.QueryRow(context.Background(), `SELECT participant_id::text FROM p_1.chat_participant_mapping WHERE conversation_id=$1 ORDER BY participant_id LIMIT 1`, conversation.ID).Scan(&participantID); err != nil {
		t.Fatal(err)
	}
	callChatAuthority(t, router, "7", "PUT", "/1/conversations/"+conversation.ID+"/participants/"+participantID, `{"steps_limit":12}`, 200)
	var limit int
	if err := pool.QueryRow(context.Background(), `SELECT (entity_settings->>'steps_limit')::integer FROM p_1.chat_participant_mapping WHERE conversation_id=$1 AND participant_id=$2`, conversation.ID, participantID).Scan(&limit); err != nil || limit != 12 {
		t.Fatalf("mapped settings were not persisted: limit=%d error=%v", limit, err)
	}
}

func TestChatAuthorityMessageCanvasAndTokenOwner(t *testing.T) {
	pool := newChatAuthorityPool(t)
	repo := repos.NewConversationsRepo(pool)
	tokenContext := auth.ContextWithUser(context.Background(), auth.User{ID: "99", TokenID: "99", UserID: "7", AuthType: "token"})
	conversation, err := repo.Create(tokenContext, "1", conversations.Conversation{Name: "Token owner", CreatedBy: "99"})
	if err != nil {
		t.Fatal(err)
	}
	if conversation.CreatedBy != "7" {
		t.Fatalf("token row became author: %s", conversation.CreatedBy)
	}
	var messageID, messageUUID, canvasID, canvasUUID string
	if err := pool.QueryRow(context.Background(), `INSERT INTO p_1.chat_message_group(conversation_id) VALUES ($1) RETURNING id::text,uuid::text`, conversation.ID).Scan(&messageID, &messageUUID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `INSERT INTO p_1.chat_message_items(message_group_id,item_type) VALUES ($1,'canvas_message') RETURNING id::text,uuid::text`, messageID).Scan(&canvasID, &canvasUUID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO p_1.chat_messages_canvas(id) VALUES ($1)`, canvasID); err != nil {
		t.Fatal(err)
	}
	router := chatAuthorityRouter(pool)
	for _, resource := range []struct{ kind, id string }{{"message", messageID}, {"message", messageUUID}, {"canvas", canvasID}, {"canvas", canvasUUID}} {
		if err := repo.AuthorizeChatResource(tokenContext, "1", resource.kind, resource.id); err != nil {
			t.Fatalf("token owner cannot read %s: %v", resource.kind, err)
		}
		prefix := "/1/messages/"
		if resource.kind == "canvas" {
			prefix = "/1/canvas/"
		}
		callChatAuthority(t, router, "8", "GET", prefix+resource.id, "", 404)
	}
	unresolved := auth.ContextWithUser(context.Background(), auth.User{ID: "99", TokenID: "99", AuthType: "token"})
	if err := repo.AuthorizeChatResource(unresolved, "1", "conversation", conversation.ID); err == nil {
		t.Fatal("unresolved token received user access")
	}
}

func TestChatAuthoritySupportFoldersAndCreationRollback(t *testing.T) {
	pool := newChatAuthorityPool(t)
	repo := repos.NewConversationsRepo(pool)
	router := chatAuthorityRouter(pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO centry.platform_config VALUES ('support_assistant','support_project_id','1')`); err != nil {
		t.Fatal(err)
	}
	conversation, err := repo.Create(chatActor("7"), "1", conversations.Conversation{Name: "Private support", Source: "support"})
	if err != nil {
		t.Fatal(err)
	}
	callChatAuthority(t, router, "9", "GET", "/1/conversations/"+conversation.ID, "", 200)
	callChatAuthority(t, router, "8", "GET", "/1/conversations/"+conversation.ID, "", 404)
	w := callChatAuthority(t, router, "9", "GET", "/1/folders?grouped=true", "", 200)
	if !strings.Contains(w.Body.String(), "Private support") {
		t.Fatal("support admin cannot see support conversation")
	}
	w = callChatAuthority(t, router, "8", "GET", "/1/folders?grouped=true", "", 200)
	if strings.Contains(w.Body.String(), "Private support") {
		t.Fatal("support conversation leaked to another participant")
	}
	// Fail after the new conversation and its first participant exist.
	if _, err := pool.Exec(ctx, `CREATE FUNCTION p_1.refuse_dummy() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF (SELECT entity_name FROM p_1.chat_participants WHERE id=NEW.participant_id)='dummy' THEN RAISE EXCEPTION 'fixture failure'; END IF; RETURN NEW; END $$;
CREATE TRIGGER refuse_dummy BEFORE INSERT ON p_1.chat_participant_mapping FOR EACH ROW EXECUTE FUNCTION p_1.refuse_dummy()`); err != nil {
		t.Fatal(err)
	}
	callChatAuthority(t, router, "8", "POST", "/1/conversations", `{"name":"Must rollback"}`, 500)
	var conversationsCount, actorsCount int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM p_1.chat_conversations),(SELECT count(*) FROM p_1.chat_participants WHERE entity_name='user' AND entity_meta->>'id'='8')`).Scan(&conversationsCount, &actorsCount); err != nil {
		t.Fatal(err)
	}
	if conversationsCount != 1 || actorsCount != 0 {
		t.Fatalf("partial create persisted: conversations=%d actors=%d", conversationsCount, actorsCount)
	}
}

type chatContextGate bool

func (g chatContextGate) ContextManagementEnabled(context.Context, string) (bool, error) {
	return bool(g), nil
}

type chatReasoningModel bool

func (g chatReasoningModel) SupportsReasoning(context.Context, string, string) (bool, error) {
	return bool(g), nil
}

func TestChatAuthorityPersonalizationContextAndParticipantMutations(t *testing.T) {
	pool := newChatAuthorityPool(t)
	_, err := pool.Exec(context.Background(), `CREATE TABLE centry.social_users(user_id integer,personalization jsonb,default_context_management jsonb,default_summarization jsonb);
 INSERT INTO centry.social_users VALUES (7,'{"persona":"developer","default_instructions":"Wrong fallback","personality_instructions":{"developer":"Selected instructions","analyst":"Other persona"}}','{"max_context_tokens":64000}',NULL);
 CREATE TABLE p_1.application_versions(id integer,llm_settings jsonb);
 INSERT INTO p_1.application_versions VALUES (3,'{"model_name":"model","reasoning_effort":"low"}');`)
	if err != nil {
		t.Fatal(err)
	}
	repo := repos.NewConversationsRepo(pool)
	h := conversations.NewHandler(repo).WithPool(pool).WithUserContextDefaults(repos.NewUserContextDefaultsRepo(pool)).WithContextManagementGate(chatContextGate(true)).WithReasoningModels(chatReasoningModel(true))
	r := chi.NewRouter()
	r.Post("/{projectID}/conversations", h.Create)
	r.Put("/{projectID}/conversations/{conversationID}", h.Update)
	r.Get("/{projectID}/conversations/{conversationID}/participants/{participantID}", h.GetParticipant)
	r.Delete("/{projectID}/conversations/{conversationID}/participants/{participantID}", h.RemoveParticipant)
	r.Put("/{projectID}/conversations/{conversationID}/participants/{participantID}", h.UpdateEntitySettings)
	w := callChatAuthority(t, r, "7", "POST", "/1/conversations", `{"name":"With defaults","participants":[{"entity_name":"application","entity_meta":{"id":41},"entity_settings":{"version_id":3}}]}`, 201)
	var created conversations.Conversation
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Instructions != "Selected instructions" || created.Meta["persona"] != "developer" {
		t.Fatalf("defaults lost: %+v", created)
	}
	strategy, ok := created.Meta["context_strategy"].(map[string]any)
	if !ok || strategy["max_context_tokens"] != float64(64000) {
		t.Fatalf("context not snapshotted: %+v", created.Meta)
	}
	var userID, appID int
	for _, p := range created.Participants {
		if p.EntityName == "user" {
			userID = p.ID
		}
		if p.EntityName == "application" {
			appID = p.ID
		}
	}
	path := fmt.Sprintf("/1/conversations/%s/participants/%d", created.ID, userID)
	callChatAuthority(t, r, "8", "GET", path, "", 404)
	callChatAuthority(t, r, "7", "GET", path, "", 200)
	callChatAuthority(t, r, "7", "DELETE", path, "", 400)
	callChatAuthority(t, r, "7", "PUT", "/1/conversations/"+created.ID, `{"instructions":""}`, 200)
	persisted, err := repo.Get(chatActor("7"), "1", created.ID)
	if err != nil || persisted.Instructions != "" {
		t.Fatalf("clear instructions failed: %+v %v", persisted, err)
	}
	callChatAuthority(t, r, "7", "PUT", "/1/conversations/"+created.ID, `{"is_private":false}`, 200)
	callChatAuthority(t, r, "7", "PUT", "/1/conversations/"+created.ID, `{"is_private":true}`, 400)
	appPath := fmt.Sprintf("/1/conversations/%s/participants/%d", created.ID, appID)
	callChatAuthority(t, r, "7", "PUT", appPath, `{"llm_settings":{}}`, 200)
	callChatAuthority(t, r, "7", "PUT", appPath, `{"llm_settings":{"model_name":"model","reasoning_effort":"high"}}`, 400)
	callChatAuthority(t, r, "7", "PUT", appPath, `{"version_id":"3","llm_settings":{"model_name":"model","reasoning_effort":"high"}}`, 400)
	callChatAuthority(t, r, "7", "PUT", appPath, `{"version_id":"3","llm_settings":{"model_name":"model","reasoning_effort":"low"}}`, 200)
	callChatAuthority(t, r, "7", "PUT", path, `{"llm_settings":{"model_name":"model","temperature":0.3,"reasoning_effort":"high"}}`, 400)
	callChatAuthority(t, r, "7", "PUT", path, `{"llm_settings":{"model_name":"model","temperature":0.3}}`, 400)
	callChatAuthority(t, r, "7", "PUT", path, `{"llm_settings":{"model_name":"model","reasoning_effort":"high"}}`, 200)
	if _, err := pool.Exec(context.Background(), `UPDATE p_1.chat_conversations SET attachment_participant_id=$1 WHERE id=$2`, appID, created.ID); err != nil {
		t.Fatal(err)
	}
	callChatAuthority(t, r, "7", "DELETE", appPath, "", 204)
	callChatAuthority(t, r, "7", "GET", appPath, "", 404)
	callChatAuthority(t, r, "7", "DELETE", appPath, "", 404)
	var attachment *int
	if err := pool.QueryRow(context.Background(), `SELECT attachment_participant_id FROM p_1.chat_conversations WHERE id=$1`, created.ID).Scan(&attachment); err != nil || attachment != nil {
		t.Fatalf("attachment participant not cleared: %v %v", attachment, err)
	}
	// An explicitly present persona map with no selected entry cannot use flat
	// legacy text or another persona's instructions.
	if _, err := pool.Exec(context.Background(), `UPDATE centry.social_users SET personalization='{"persona":"missing","default_instructions":"Wrong fallback","personality_instructions":{"developer":"Other persona"}}'`); err != nil {
		t.Fatal(err)
	}
	h.WithContextManagementGate(chatContextGate(false))
	w = callChatAuthority(t, r, "7", "POST", "/1/conversations", `{"name":"No default bleed"}`, 201)
	var next conversations.Conversation
	if err := json.Unmarshal(w.Body.Bytes(), &next); err != nil {
		t.Fatal(err)
	}
	if next.Instructions != "" || next.Meta["default_instructions"] != nil || next.Meta["context_strategy"] != nil {
		t.Fatalf("disabled or foreign defaults applied: %+v", next)
	}
}

func TestChatAuthorityParticipantBatchRollsBack(t *testing.T) {
	pool := newChatAuthorityPool(t)
	repo := repos.NewConversationsRepo(pool)
	conv, err := repo.Create(chatActor("7"), "1", conversations.Conversation{Name: "Existing chat"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(context.Background(), `CREATE FUNCTION p_1.fail_second_participant() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.entity_meta->>'id'='8' THEN RAISE EXCEPTION 'fixture failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_second BEFORE INSERT ON p_1.chat_participants FOR EACH ROW EXECUTE FUNCTION p_1.fail_second_participant();`)
	if err != nil {
		t.Fatal(err)
	}
	router := chatAuthorityRouter(pool)
	path := "/1/conversations/" + conv.ID + "/participants"
	callChatAuthority(t, router, "7", "POST", path, `[{"entity_name":"user","entity_meta":{"id":9}},{"entity_name":"user","entity_meta":{"id":8}}]`, 500)
	var leaked int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM p_1.chat_participants WHERE entity_meta->>'id'='9'`).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("batch leaked first participant: %d %v", leaked, err)
	}
	callChatAuthority(t, router, "7", "POST", path, `{"entity_name":"user","entity_meta":{"id":9}}`, 200)
}
