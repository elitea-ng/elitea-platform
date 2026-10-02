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

type recordingInternalSecretExecutor struct {
	projectID int64
	actorID   int64
	operation internalSecretOperation
	arguments map[string]any
	result    internalApplicationExecution
	err       error
	calls     int
}

func (executor *recordingInternalSecretExecutor) Execute(
	_ context.Context,
	projectID int64,
	actorID int64,
	operation internalSecretOperation,
	arguments map[string]any,
) (internalApplicationExecution, error) {
	executor.calls++
	executor.projectID = projectID
	executor.actorID = actorID
	executor.operation = operation
	executor.arguments = arguments
	return executor.result, executor.err
}

func internalSecretRouter(
	t *testing.T,
	tool Tool,
	executor internalSecretExecutor,
	permissions auth.PermissionResolver,
) chi.Router {
	t.Helper()
	handler := NewHandler(nil, nil, nil, permissions)
	handler.source = staticSource(tool)
	handler.internalSecrets = executor
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

func internalSecretTool(operation internalSecretOperation, permission string) Tool {
	return Tool{
		Name:                    "internal_secret_test",
		Description:             "test",
		InputSchema:             objectSchema(map[string]any{}),
		internalSecretOperation: operation,
		permission:              permission,
	}
}

func TestInternalSecretsCategoryPublishesExactlyCurrentSafeMCPOptIns(t *testing.T) {
	tools, err := (postgresToolSource{}).tools(
		context.Background(), `"p_7"`, scope{kind: scopeCategory, category: internalSecretsCategory},
	)
	if err != nil {
		t.Fatalf("list internal secret tools: %v", err)
	}
	wantNames := []string{"get_secrets_secrets", "post_secrets_secrets", "put_secrets_secret"}
	wantPermissions := []string{
		"configuration.secrets.secret.list",
		"configuration.secrets.secret.create",
		"configuration.secrets.secret.edit",
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
	updateProperties := tools[2].InputSchema["properties"].(map[string]any)
	if _, advertised := updateProperties["name"]; advertised {
		t.Fatal("update schema advertises a rename that current pylon ignores")
	}
	for _, required := range []string{"project_id", "secret"} {
		if _, advertised := updateProperties[required]; !advertised {
			t.Fatalf("update schema does not advertise %q", required)
		}
	}

	wire, err := json.Marshal(tools)
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	for _, forbidden := range []string{
		"internalSecretOperation", "permission", `"name":"get_secrets_secret"`,
		`"name":"delete_secrets_secret"`, "hide", "administration",
	} {
		if strings.Contains(string(wire), forbidden) {
			t.Fatalf("wire contains private or unsupported operation %q: %s", forbidden, wire)
		}
	}
}

func TestInternalSecretCallUsesExactPermissionActorAndEndpointProject(t *testing.T) {
	const permission = "configuration.secrets.secret.create"
	executor := &recordingInternalSecretExecutor{
		result: internalApplicationExecution{
			status: http.StatusCreated,
			body:   []byte(`{"name":"TOKEN","secret_name":"{{secret.TOKEN}}","is_default":false}`),
		},
	}
	permissions := &recordingInternalPermissionResolver{
		resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}},
	}
	router := internalSecretRouter(
		t, internalSecretTool(internalCreateSecret, permission), executor, permissions,
	)
	result := resultOf(t, post(t, router, "/app/7/mcp/secrets",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_secret_test","arguments":{"name":"TOKEN","value":"private-marker"}}}`))

	if executor.calls != 1 || executor.projectID != 7 || executor.actorID != 41 ||
		executor.operation != internalCreateSecret {
		t.Fatalf("executor = calls:%d project:%d actor:%d operation:%q",
			executor.calls, executor.projectID, executor.actorID, executor.operation)
	}
	if got := scalarArgument(executor.arguments["project_id"]); got != "7" {
		t.Fatalf("injected project_id = %q, want 7", got)
	}
	if permissions.mode != auth.PermissionModeDefault || permissions.projectID != "7" {
		t.Fatalf("permission request = mode:%q project:%q", permissions.mode, permissions.projectID)
	}
	text := textOf(t, result)
	if strings.Contains(text, "private-marker") || !strings.Contains(text, "{{secret.TOKEN}}") {
		t.Fatalf("unsafe or incomplete result text = %q", text)
	}
}

func TestInternalSecretCallRefusesForeignProjectBeforeAuthorization(t *testing.T) {
	const permission = "configuration.secrets.secret.edit"
	executor := &recordingInternalSecretExecutor{}
	permissions := &recordingInternalPermissionResolver{
		resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}},
	}
	router := internalSecretRouter(
		t, internalSecretTool(internalUpdateSecret, permission), executor, permissions,
	)
	result := resultOf(t, post(t, router, "/app/7/mcp/secrets",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_secret_test","arguments":{"project_id":8,"secret":"TOKEN","value":"private-marker"}}}`))

	if result["isError"] != true || executor.calls != 0 || permissions.calls != 0 {
		t.Fatalf("result=%v executor calls=%d permission calls=%d", result, executor.calls, permissions.calls)
	}
}

func TestInternalSecretUnavailableAndInfrastructureFailuresAreSafe(t *testing.T) {
	const permission = "configuration.secrets.secret.list"
	permissions := &recordingInternalPermissionResolver{
		resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission}},
	}
	for _, test := range []struct {
		name     string
		executor internalSecretExecutor
	}{
		{name: "unavailable"},
		{name: "redacted", executor: &recordingInternalSecretExecutor{
			err: errors.New("vault token=private-marker"),
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			router := internalSecretRouter(
				t, internalSecretTool(internalListSecrets, permission), test.executor, permissions,
			)
			result := resultOf(t, post(t, router, "/app/7/mcp/secrets",
				`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"internal_secret_test","arguments":{}}}`))
			text := textOf(t, result)
			if result["isError"] != true || strings.Contains(text, "private-marker") || strings.Contains(text, "token=") {
				t.Fatalf("unsafe failure result: %v", result)
			}
		})
	}
}
