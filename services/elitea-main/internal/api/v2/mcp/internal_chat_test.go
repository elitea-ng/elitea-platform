package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

type chatHandlerProbe struct {
	calls              int
	method, path, body string
	user               auth.User
}

func (p *chatHandlerProbe) serve(w http.ResponseWriter, r *http.Request) {
	p.calls++
	p.method = r.Method
	p.path = chi.URLParam(r, "projectID") + "/" + chi.URLParam(r, "conversationID") + "/" + chi.URLParam(r, "participantID") + "/" + chi.URLParam(r, "folderID")
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		p.body = string(raw)
	}
	p.user, _ = auth.UserFromContext(r.Context())
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}
func (p *chatHandlerProbe) List(w http.ResponseWriter, r *http.Request)              { p.serve(w, r) }
func (p *chatHandlerProbe) Create(w http.ResponseWriter, r *http.Request)            { p.serve(w, r) }
func (p *chatHandlerProbe) Get(w http.ResponseWriter, r *http.Request)               { p.serve(w, r) }
func (p *chatHandlerProbe) Update(w http.ResponseWriter, r *http.Request)            { p.serve(w, r) }
func (p *chatHandlerProbe) GetParticipant(w http.ResponseWriter, r *http.Request)    { p.serve(w, r) }
func (p *chatHandlerProbe) RemoveParticipant(w http.ResponseWriter, r *http.Request) { p.serve(w, r) }
func (p *chatHandlerProbe) AddParticipant(w http.ResponseWriter, r *http.Request)    { p.serve(w, r) }
func (p *chatHandlerProbe) UpdateEntitySettings(w http.ResponseWriter, r *http.Request) {
	p.serve(w, r)
}

func TestInternalChatCatalogAndEveryHandlerOperation(t *testing.T) {
	tools, err := (postgresToolSource{}).tools(context.Background(), `"p_7"`, scope{kind: scopeCategory, category: internalChatCategory})
	if err != nil || len(tools) != 11 {
		t.Fatalf("catalog: %d %v", len(tools), err)
	}
	fixtures := map[internalChatOperation]struct {
		method string
		args   map[string]any
	}{
		internalChatList:                 {"GET", map[string]any{"limit": 5}},
		internalChatCreate:               {"POST", map[string]any{"name": "New chat", "participants": []any{}}},
		internalChatGet:                  {"GET", map[string]any{"conversation_id": 12}},
		internalChatUpdate:               {"PUT", map[string]any{"conversation_id": 12, "instructions": "New instructions", "folder_id": nil}},
		internalChatParticipantGet:       {"GET", map[string]any{"conversation_id": 12, "participant_id": 13}},
		internalChatParticipantDelete:    {"DELETE", map[string]any{"conversation_id": 12, "participant_id": 13}},
		internalChatParticipantsAdd:      {"POST", map[string]any{"conversation_id": 12, "participants": []any{map[string]any{"entity_name": "user", "entity_meta": map[string]any{"id": 41}}}}},
		internalChatParticipantConfigure: {"PUT", map[string]any{"conversation_id": 12, "participant_id": 13, "version_id": 51}},
		internalChatFoldersList:          {"GET", map[string]any{"grouped": true}},
		internalChatFolderCreate:         {"POST", map[string]any{"name": "Folder"}},
		internalChatFolderUpdate:         {"PUT", map[string]any{"folder_id": 15, "name": "Rename", "position": 10}},
	}
	for _, tool := range tools {
		t.Run(tool.Name, func(t *testing.T) {
			f, ok := fixtures[tool.internalChatOperation]
			if !ok {
				t.Fatal("unreviewed operation")
			}
			probe := &chatHandlerProbe{}
			executor := &handlerInternalChatExecutor{probe, probe}
			ctx := auth.ContextWithUser(context.Background(), auth.User{ID: "99", TokenID: "99", UserID: "41", AuthType: "token"})
			result, err := executor.Execute(ctx, 7, 41, tool.internalChatOperation, f.args)
			if err != nil || result.status != 200 || probe.calls != 1 {
				t.Fatalf("execute: status %d, error %v, calls %d, body %s", result.status, err, probe.calls, result.body)
			}
			actor, _ := probe.user.OwningUserID()
			if actor != 41 || probe.user.TokenID != "99" || probe.method != f.method || !strings.HasPrefix(probe.path, "7/") {
				t.Fatalf("dispatch: %+v", probe)
			}
			if tool.internalChatOperation == internalChatParticipantsAdd && !strings.HasPrefix(probe.body, "[") {
				t.Fatalf("array shape lost: %s", probe.body)
			}
			if tool.internalChatOperation == internalChatParticipantConfigure && !strings.Contains(probe.body, `"version_id":51`) {
				t.Fatalf("settings shape lost: %s", probe.body)
			}
			if tool.permission == "" {
				t.Fatal("missing permission")
			}
		})
	}
	wire, _ := json.Marshal(tools)
	for _, forbidden := range []string{"internalChatOperation", "permission", "send_message", "continue_predict", "author_id", "owner_id"} {
		if strings.Contains(string(wire), forbidden) {
			t.Fatalf("unexpected publication %s", forbidden)
		}
	}
}

func TestInternalChatSchemaRejectsForgedAuthorityAndMalformedArguments(t *testing.T) {
	for _, args := range []map[string]any{
		{"name": "New chat", "author_id": 8}, {"name": "New chat", "owner_id": 8}, {"name": "New chat", "is_private": "false"}, {"name": "New chat", "participants": map[string]any{}}, {"name": "No", "participants": []any{}},
	} {
		probe := &chatHandlerProbe{}
		executor := &handlerInternalChatExecutor{probe, probe}
		result, err := executor.Execute(context.Background(), 7, 41, internalChatCreate, args)
		if err != nil || result.status != 400 || probe.calls != 0 {
			t.Fatalf("unsafe arguments reached handler: %+v %v", result, err)
		}
	}
}

func TestInternalChatProtocolUsesExactPermissionAndEndpointAuthority(t *testing.T) {
	for _, tool := range internalChatTools() {
		t.Run(tool.Name, func(t *testing.T) {
			probe := &chatHandlerProbe{}
			permissions := &recordingInternalPermissionResolver{resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{tool.permission}}}
			h := NewHandler(nil, nil, nil, permissions)
			h.source = staticSource(tool)
			h.internalChat = &handlerInternalChatExecutor{probe, probe}
			router := chi.NewRouter()
			router.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), auth.User{ID: "41"})))
				})
			})
			router.Post("/app/{projectID}/mcp/*", h.Endpoint)
			// Endpoint mismatch is refused before calling either the permission resolver or handler.
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{"project_id":8}}}`, tool.Name)
			result := resultOf(t, post(t, router, "/app/7/mcp/elitea_core/chat", body))
			if result["isError"] != true || probe.calls != 0 {
				t.Fatalf("project override accepted: %+v", result)
			}
			// A same-project call still requires this operation's exact permission.
			permissions.resolution.Permissions = []string{"unrelated"}
			body = strings.Replace(body, `"project_id":8`, `"project_id":7`, 1)
			result = resultOf(t, post(t, router, "/app/7/mcp/elitea_core/chat", body))
			if result["isError"] != true || probe.calls != 0 {
				t.Fatalf("permission bypass: %+v", result)
			}
		})
	}
}

func TestInternalChatProtocolSuccessfulMutationUsesSharedHandler(t *testing.T) {
	tool := internalChatTools()[1]
	probe := &chatHandlerProbe{}
	permissions := &recordingInternalPermissionResolver{resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{tool.permission}}}
	h := NewHandler(nil, nil, nil, permissions)
	h.source = staticSource(tool)
	h.internalChat = &handlerInternalChatExecutor{probe, probe}
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), auth.User{ID: "41"})))
		})
	})
	router.Post("/app/{projectID}/mcp/*", h.Endpoint)
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{"name":"Created through MCP"}}}`, tool.Name)
	result := resultOf(t, post(t, router, "/app/7/mcp/elitea_core/chat", body))
	if result["isError"] == true || probe.calls != 1 || probe.method != "POST" || probe.path != "7///" {
		t.Fatalf("shared mutation not invoked: %+v %+v", result, probe)
	}
}
