package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	draftsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/drafts"
	predictapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/predict"
	skillsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

type draftCompleter struct {
	request predictapi.CompletionRequest
	calls   int
	content string
	err     error
}

func (c *draftCompleter) Complete(_ context.Context, request predictapi.CompletionRequest) (string, error) {
	c.calls++
	c.request = request
	return c.content, c.err
}

func draftRouter(t *testing.T, operation internalDraftOperation, draft *draftsapi.Handler, permissions auth.PermissionResolver) chi.Router {
	t.Helper()
	handler := NewHandler(nil, nil, nil, permissions, WithInternalDraftHandler(draft))
	handler.source = staticSource(internalDraftTool(operation))
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), auth.User{ID: "41", UserID: "41"})))
		})
	})
	router.Post("/app/{projectID}/mcp/*", handler.Endpoint)
	return router
}

func TestInternalDraftProtocolUsesSharedHandlerAndAuthorizedIdentity(t *testing.T) {
	for _, tc := range []struct {
		operation            internalDraftOperation
		answer, extra, prior string
	}{
		{internalDraftSkill, `{"name":"Review PR","description":"Review changes","instructions":"Check errors"}`, ``, ``},
		{internalDraftProjectContext, `{"project_background":"Refined context"}`, `,"current_project_background":"Existing team rules"`, `Existing team rules`},
	} {
		t.Run(string(tc.operation), func(t *testing.T) {
			completer := &draftCompleter{content: tc.answer}
			permission := internalDraftTool(tc.operation).permission
			permissions := &recordingInternalPermissionResolver{resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}}}
			router := draftRouter(t, tc.operation, draftsapi.NewHandler(completer), permissions)
			wire := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"post_prompt_lib_` + string(tc.operation) + `","arguments":{"user_description":"Refine this", "author_id":999,"llm_settings":{"model_name":"test-model","model_project_id":999,"max_tokens":4096,"temperature":0}` + tc.extra + `}}}`
			result := resultOf(t, post(t, router, "/app/7/mcp/elitea_core/skills", wire))
			if result["isError"] == true || !json.Valid([]byte(textOf(t, result))) || completer.calls != 1 {
				t.Fatalf("result=%v calls=%d", result, completer.calls)
			}
			req := completer.request
			if req.ProjectID != "7" || req.UserID != "41" || req.Model != "test-model" || req.MaxTokens == nil || *req.MaxTokens != 4096 || req.Temperature == nil || *req.Temperature != 0 {
				t.Fatalf("completion scope or settings=%+v", req)
			}
			if req.Messages[1].Content != "Refine this" || !strings.Contains(req.Messages[0].Content, "ONE JSON object") || !strings.Contains(req.Messages[0].Content, tc.prior) {
				t.Fatalf("source prompt or prior context missing")
			}
			if permissions.projectID != "7" || permissions.mode != auth.PermissionModeDefault {
				t.Fatalf("permission scope=%+v", permissions)
			}
		})
	}
}

func TestInternalDraftProtocolRefusesUnauthorizedAndUnsafeRequests(t *testing.T) {
	for _, op := range []internalDraftOperation{internalDraftSkill, internalDraftProjectContext} {
		for _, tc := range []struct {
			name, arguments, answer string
			allowed                 bool
			err                     error
			calls                   int
		}{
			{"denied", `{"user_description":"Generate"}`, ``, false, nil, 0},
			{"foreign project", `{"user_description":"Generate","project_id":8}`, ``, true, nil, 0},
			{"missing description", `{}`, ``, true, nil, 0},
			{"wrong description type", `{"user_description":42}`, ``, true, nil, 0},
			{"invalid output", `{"user_description":"Generate"}`, `{"private":"secret"}`, true, nil, 1},
			{"gateway failure", `{"user_description":"Generate"}`, ``, true, errors.New("password=private-secret"), 1},
		} {
			t.Run(string(op)+"/"+tc.name, func(t *testing.T) {
				completer := &draftCompleter{content: tc.answer, err: tc.err}
				permissions := &recordingInternalPermissionResolver{resolution: auth.PermissionResolution{UserID: 41}}
				if tc.allowed {
					permissions.resolution.Permissions = []string{internalDraftTool(op).permission}
				}
				router := draftRouter(t, op, draftsapi.NewHandler(completer), permissions)
				result := resultOf(t, post(t, router, "/app/7/mcp/elitea_core/skills", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"post_prompt_lib_`+string(op)+`","arguments":`+tc.arguments+`}}`))
				if result["isError"] != true || completer.calls != tc.calls {
					t.Fatalf("result=%v calls=%d", result, completer.calls)
				}
				if strings.Contains(textOf(t, result), "private") {
					t.Fatalf("unsafe failure: %v", result)
				}
			})
		}
	}
}

func TestInternalDraftMissingGatewayIsToolError(t *testing.T) {
	for _, draft := range []*draftsapi.Handler{nil, draftsapi.NewHandler(nil)} {
		permission := internalDraftTool(internalDraftSkill).permission
		permissions := &recordingInternalPermissionResolver{resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}}}
		router := draftRouter(t, internalDraftSkill, draft, permissions)
		result := resultOf(t, post(t, router, "/app/7/mcp/elitea_core/skills", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"post_prompt_lib_generate_skill_draft","arguments":{"user_description":"Generate"}}}`))
		if result["isError"] != true {
			t.Fatalf("missing gateway result=%v", result)
		}
	}
}

type draftTokenPermissions struct{}

func (draftTokenPermissions) ResolvePermissions(_ context.Context, user auth.User, _, _ string) (auth.PermissionResolution, error) {
	permissions := []string{"models.applications.skills.create"}
	if user.TokenID == "" {
		permissions = append(permissions, "models.applications.skills.details")
	}
	return auth.PermissionResolution{UserID: 41, Permissions: permissions}, nil
}

type draftDeniedReader struct{ calls int }

func (r *draftDeniedReader) GetVersion(context.Context, string, string, string) (skillsapi.Skill, error) {
	r.calls++
	return skillsapi.Skill{}, errors.New("protected instructions")
}
func TestInternalDraftEditPreservesTokenPermissionScope(t *testing.T) {
	permissions := draftTokenPermissions{}
	reader := &draftDeniedReader{}
	completer := &draftCompleter{}
	handler := NewHandler(nil, nil, nil, permissions, WithInternalDraftHandler(draftsapi.NewHandler(completer, draftsapi.WithSkillVersions(reader), draftsapi.WithPermissions(permissions))))
	handler.source = staticSource(internalDraftTool(internalDraftSkill))
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.ContextWithUser(r.Context(), auth.User{ID: "41", UserID: "41", TokenID: "99", AuthType: "token"})))
		})
	})
	router.Post("/app/{projectID}/mcp/*", handler.Endpoint)
	result := resultOf(t, post(t, router, "/app/7/mcp/elitea_core/skills", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"post_prompt_lib_generate_skill_draft","arguments":{"user_description":"Read protected rules","skill_id":3,"version_id":12}}}`))
	if result["isError"] != true || reader.calls != 0 || completer.calls != 0 {
		t.Fatalf("token scope bypass: result=%v reads=%d calls=%d", result, reader.calls, completer.calls)
	}
}
