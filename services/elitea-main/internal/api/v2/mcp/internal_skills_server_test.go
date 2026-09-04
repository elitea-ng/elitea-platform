package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type recordingInternalSkillExecutor struct {
	projectID int64
	actorID   int64
	operation internalSkillOperation
	arguments map[string]any
	result    internalApplicationExecution
	err       error
	calls     int
}

func (executor *recordingInternalSkillExecutor) Execute(
	_ context.Context,
	projectID int64,
	actorID int64,
	operation internalSkillOperation,
	arguments map[string]any,
) (internalApplicationExecution, error) {
	executor.calls++
	executor.projectID = projectID
	executor.actorID = actorID
	executor.operation = operation
	executor.arguments = arguments
	return executor.result, executor.err
}

func internalSkillRouter(
	t *testing.T,
	tool Tool,
	executor internalSkillExecutor,
	permissions auth.PermissionResolver,
) chi.Router {
	t.Helper()
	handler := NewHandler(nil, nil, nil, permissions)
	handler.source = staticSource(tool)
	handler.internalSkills = executor
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			principal := auth.User{ID: "41", UserID: "41"}
			ctx := auth.ContextWithUser(request.Context(), principal)
			next.ServeHTTP(writer, request.WithContext(ctx))
		})
	})
	router.Post("/app/{projectID}/mcp/*", handler.Endpoint)
	return router
}

func internalSkillTool(operation internalSkillOperation, permission string) Tool {
	return Tool{
		Name:                   "internal_skill_test",
		Description:            "test",
		InputSchema:            objectSchema(map[string]any{}),
		internalSkillOperation: operation,
		permission:             permission,
	}
}

func TestInternalSkillsCategoryPublishesOnlyMainOwnedCurrentOperations(t *testing.T) {
	tools, err := (postgresToolSource{}).tools(
		context.Background(), `"p_7"`, scope{kind: scopeCategory, category: internalSkillsCategory},
	)
	if err != nil {
		t.Fatalf("list internal skill tools: %v", err)
	}
	wantNames := []string{
		"get_elitea_core_skills",
		"post_elitea_core_skills",
		"get_elitea_core_skill",
		"put_elitea_core_skill",
		"patch_elitea_core_skill",
		"get_elitea_core_application_skills",
	}
	wantPermissions := []string{
		"models.applications.skills.list",
		"models.applications.skills.create",
		"models.applications.skills.details",
		"models.applications.skills.update",
		"models.applications.skills.update",
		"models.applications.applications.details",
	}
	if len(tools) != len(wantNames) {
		t.Fatalf("tool count = %d, want %d", len(tools), len(wantNames))
	}
	for index, want := range wantNames {
		if tools[index].Name != want || tools[index].permission != wantPermissions[index] {
			t.Fatalf("tool %d = %q/%q, want %q/%q",
				index, tools[index].Name, tools[index].permission, want, wantPermissions[index])
		}
	}

	listProperties := tools[0].InputSchema["properties"].(map[string]any)
	for _, unsupported := range []string{"tags", "author_id", "statuses", "ids", "limit", "offset"} {
		if _, advertised := listProperties[unsupported]; advertised {
			t.Fatalf("list schema advertises unsupported filter %q", unsupported)
		}
	}
	wire, err := json.Marshal(tools)
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	for _, forbidden := range []string{
		"internalSkillOperation", "permission", "generate_skill_draft", "delete", "create_skill_version",
	} {
		if strings.Contains(string(wire), forbidden) {
			t.Fatalf("wire contains private or unsupported operation %q: %s", forbidden, wire)
		}
	}
}

func TestInternalSkillCallUsesExactPermissionAndEndpointProject(t *testing.T) {
	const permission = "models.applications.skills.details"
	executor := &recordingInternalSkillExecutor{
		result: internalApplicationExecution{status: http.StatusOK, body: []byte(`{"ok":true}`)},
	}
	permissions := &recordingInternalPermissionResolver{
		resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}},
	}
	router := internalSkillRouter(t, internalSkillTool(internalGetSkill, permission), executor, permissions)
	result := resultOf(t, post(t, router, "/app/7/mcp/elitea_core/skills",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_skill_test","arguments":{}}}`))

	if executor.calls != 1 || executor.projectID != 7 || executor.actorID != 41 {
		t.Fatalf("executor = calls:%d project:%d actor:%d", executor.calls, executor.projectID, executor.actorID)
	}
	if got := scalarArgument(executor.arguments["project_id"]); got != "7" {
		t.Fatalf("injected project_id = %q, want 7", got)
	}
	if permissions.mode != auth.PermissionModeDefault || permissions.projectID != "7" {
		t.Fatalf("permission request = mode:%q project:%q", permissions.mode, permissions.projectID)
	}
	if text := textOf(t, result); text != `{"ok":true}` {
		t.Fatalf("result text = %q", text)
	}
}

func TestInternalSkillCallRefusesForeignProjectBeforeAuthorization(t *testing.T) {
	const permission = "models.applications.skills.list"
	executor := &recordingInternalSkillExecutor{}
	permissions := &recordingInternalPermissionResolver{
		resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}},
	}
	router := internalSkillRouter(t, internalSkillTool(internalListSkills, permission), executor, permissions)
	result := resultOf(t, post(t, router, "/app/7/mcp/elitea_core/skills",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_skill_test","arguments":{"project_id":8}}}`))

	if result["isError"] != true || executor.calls != 0 || permissions.calls != 0 {
		t.Fatalf("result=%v executor calls=%d permission calls=%d", result, executor.calls, permissions.calls)
	}
}

func TestInternalSkillInfrastructureFailureIsRedacted(t *testing.T) {
	const permission = "models.applications.skills.details"
	executor := &recordingInternalSkillExecutor{err: errors.New("postgres password=do-not-leak")}
	permissions := &recordingInternalPermissionResolver{
		resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}},
	}
	router := internalSkillRouter(t, internalSkillTool(internalGetSkill, permission), executor, permissions)
	result := resultOf(t, post(t, router, "/app/7/mcp/elitea_core/skills",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_skill_test","arguments":{}}}`))

	text := textOf(t, result)
	if result["isError"] != true || strings.Contains(text, "password") || strings.Contains(text, "do-not-leak") {
		t.Fatalf("unredacted failure result: %v", result)
	}
}
