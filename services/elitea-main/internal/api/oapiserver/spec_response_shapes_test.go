package oapiserver_test

// Response-SHAPE conformance for api/openapi/v2.yaml.
//
// oapiserver.Conformance (conformance_test.go) compares method+path
// registrations only. It never opens a response schema, so a spec that
// describes the wrong body passes it. Four such divergences shipped, and each
// one is pinned below.
//
// The cost of a wrong response shape is not cosmetic. orval generates the web
// client from this file, so a wrong schema becomes a wrong TypeScript type on
// every caller, and a zod parse against it throws on a correct server answer.

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"gopkg.in/yaml.v3"

	generated "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/generated"
	v2projects "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/projects"
	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
)

// shapeSpecPath is api/openapi/v2.yaml relative to this package directory.
const shapeSpecPath = "../../../api/openapi/v2.yaml"

type specDocument struct {
	Paths      map[string]map[string]any `yaml:"paths"`
	Components struct {
		Schemas map[string]any `yaml:"schemas"`
	} `yaml:"components"`
}

func loadShapeSpec(t *testing.T) specDocument {
	t.Helper()
	raw, err := os.ReadFile(shapeSpecPath)
	if err != nil {
		t.Fatalf("read %s: %v", shapeSpecPath, err)
	}
	var document specDocument
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatalf("parse %s: %v", shapeSpecPath, err)
	}
	if len(document.Paths) == 0 {
		t.Fatalf("%s declares no paths", shapeSpecPath)
	}
	return document
}

// httpMethods are the operation keys a path item may hold. Everything else in
// a path item (parameters, summary) is not an operation.
var httpMethods = []string{"get", "put", "post", "delete", "patch", "head", "options", "trace"}

// operationsOf returns the operations of one path item, keyed by method.
func operationsOf(item map[string]any) map[string]map[string]any {
	found := map[string]map[string]any{}
	for _, method := range httpMethods {
		operation, ok := item[method].(map[string]any)
		if ok {
			found[method] = operation
		}
	}
	return found
}

// responsesOf returns the operation's responses block.
func responsesOf(operation map[string]any) map[string]any {
	responses, _ := operation["responses"].(map[string]any)
	return responses
}

// findOperation returns the operation with the given operationId.
func findOperation(t *testing.T, document specDocument, operationID string) map[string]any {
	t.Helper()
	for _, item := range document.Paths {
		for _, operation := range operationsOf(item) {
			if operation["operationId"] == operationID {
				return operation
			}
		}
	}
	t.Fatalf("v2.yaml declares no operation %q", operationID)
	return nil
}

// jsonSchemaRef returns the $ref of a response's application/json schema, or
// "" when the response has no inline JSON schema (a $ref'd response component,
// or a schema that is not a bare $ref).
func jsonSchemaRef(response any) string {
	body, ok := response.(map[string]any)
	if !ok {
		return ""
	}
	content, ok := body["content"].(map[string]any)
	if !ok {
		return ""
	}
	media, ok := content["application/json"].(map[string]any)
	if !ok {
		return ""
	}
	schema, ok := media["schema"].(map[string]any)
	if !ok {
		return ""
	}
	ref, _ := schema["$ref"].(string)
	return ref
}

// TestListProjectsDocumentsTheStructTheHandlerMarshals pins the project
// listing's 200 body against the Go struct that produces it.
//
// THE DEFECT. components/schemas/Project declared
// `required: [id, name, status, suspended]` with `status` as an
// `active|suspended` enum. internal/api/v2/projects/handler.go's Project
// struct has no status field, and it does carry owner_id, plugins,
// keycloak_groups, create_success and groups, none of which the schema
// declared. A live GET /api/v2/projects/project/default/1 therefore failed
// validation against its own spec on the missing required `status`, and the
// generated TypeScript type promised a field the server never sends.
//
// EVIDENCE. The keys below come from marshaling the real struct, so the test
// cannot drift away from the handler.
func TestListProjectsDocumentsTheStructTheHandlerMarshals(t *testing.T) {
	document := loadShapeSpec(t)
	operation := findOperation(t, document, "listProjects")

	success, ok := responsesOf(operation)["200"].(map[string]any)
	if !ok {
		t.Fatal("listProjects declares no 200 response")
	}
	content, _ := success["content"].(map[string]any)
	media, _ := content["application/json"].(map[string]any)
	schema, _ := media["schema"].(map[string]any)
	if schema["type"] != "array" {
		t.Fatalf("listProjects 200 is not an array: %v", schema["type"])
	}
	items, _ := schema["items"].(map[string]any)
	ref, _ := items["$ref"].(string)
	const prefix = "#/components/schemas/"
	if !strings.HasPrefix(ref, prefix) {
		t.Fatalf("listProjects 200 items is not a schema reference: %q", ref)
	}
	name := strings.TrimPrefix(ref, prefix)

	target, ok := document.Components.Schemas[name].(map[string]any)
	if !ok {
		t.Fatalf("v2.yaml has no schema %q", name)
	}

	// The Go struct is the producer, so its JSON keys are the contract.
	encoded, err := json.Marshal(v2projects.Project{})
	if err != nil {
		t.Fatalf("marshal projects.Project: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatalf("decode the marshaled struct: %v", err)
	}
	wanted := make([]string, 0, len(body))
	for key := range body {
		wanted = append(wanted, key)
	}
	sort.Strings(wanted)

	declared := make([]string, 0, len(wanted))
	properties, _ := target["properties"].(map[string]any)
	for key := range properties {
		declared = append(declared, key)
	}
	sort.Strings(declared)
	if strings.Join(declared, ",") != strings.Join(wanted, ",") {
		t.Fatalf("schema %q declares properties %v, but the handler marshals %v",
			name, declared, wanted)
	}

	required := make([]string, 0, len(wanted))
	for _, value := range target["required"].([]any) {
		required = append(required, value.(string))
	}
	sort.Strings(required)
	if strings.Join(required, ",") != strings.Join(wanted, ",") {
		t.Fatalf("schema %q requires %v, but the handler always marshals %v",
			name, required, wanted)
	}
}

func TestProjectContextDocumentsTheCurrentFiveFieldBuilderContract(t *testing.T) {
	document := loadShapeSpec(t)
	target, ok := document.Components.Schemas["ProjectContext"].(map[string]any)
	if !ok {
		t.Fatal("v2.yaml has no ProjectContext schema")
	}

	wire, err := json.Marshal(generated.ProjectContext{})
	if err != nil {
		t.Fatalf("marshal generated ProjectContext: %v", err)
	}
	var generatedFields map[string]any
	if err := json.Unmarshal(wire, &generatedFields); err != nil {
		t.Fatalf("decode generated ProjectContext: %v", err)
	}
	wantFields := []string{"activation_description", "content", "enabled", "id", "updated_at"}
	gotFields := make([]string, 0, len(generatedFields))
	for name := range generatedFields {
		gotFields = append(gotFields, name)
	}
	sort.Strings(gotFields)
	if strings.Join(gotFields, ",") != strings.Join(wantFields, ",") {
		t.Fatalf("generated ProjectContext fields = %v, want %v", gotFields, wantFields)
	}

	requiredValues, ok := target["required"].([]any)
	if !ok {
		t.Fatalf("ProjectContext required fields = %#v", target["required"])
	}
	required := make([]string, 0, len(requiredValues))
	for _, value := range requiredValues {
		required = append(required, value.(string))
	}
	sort.Strings(required)
	if strings.Join(required, ",") != strings.Join(wantFields, ",") {
		t.Fatalf("ProjectContext required fields = %v, want %v", required, wantFields)
	}

	properties := target["properties"].(map[string]any)
	if properties["content"].(map[string]any)["maxLength"] != 2500 ||
		properties["activation_description"].(map[string]any)["maxLength"] != 300 {
		t.Fatalf("ProjectContext bounds = %#v", properties)
	}
	deleted := findOperation(t, document, "deleteProjectContext")
	if _, ok := responsesOf(deleted)["204"]; !ok {
		t.Fatalf("deleteProjectContext responses = %#v", responsesOf(deleted))
	}
}

// TestArtifact404sUseTheNestedErrorEnvelope pins the artifact 404 body.
//
// THE DEFECT. Fourteen artifact operations referenced the shared
// #/components/responses/404, which resolves to ErrorResponse — the FLAT
// {"error": "<string>"}. Every artifact 404 is produced by
// internal/api/v2/artifacts/handler.go's writeError, which writes the NESTED
// {"error":{"code","message"}} of components/schemas/Error. The same
// operations already documented 400/403/409 with the nested schema, so only
// 404 — the one status that is always handler-produced — was wrong.
func TestArtifact404sUseTheNestedErrorEnvelope(t *testing.T) {
	document := loadShapeSpec(t)

	checked := 0
	for path, item := range document.Paths {
		if !strings.HasPrefix(path, "/artifacts/") {
			continue
		}
		for method, operation := range operationsOf(item) {
			notFound, ok := responsesOf(operation)["404"]
			if !ok {
				continue
			}
			checked++
			if ref, isRef := notFound.(map[string]any)["$ref"]; isRef {
				t.Errorf("%s %s: 404 points at the shared response %v, which is the "+
					"flat {\"error\": string}; the handler writes the nested envelope",
					strings.ToUpper(method), path, ref)
				continue
			}
			if got := jsonSchemaRef(notFound); got != "#/components/schemas/Error" {
				t.Errorf("%s %s: 404 schema is %q, want #/components/schemas/Error",
					strings.ToUpper(method), path, got)
			}
		}
	}
	// A refactor that renames the paths must not turn this test into a no-op.
	if checked < 14 {
		t.Fatalf("only %d artifact 404 responses were checked, want at least 14", checked)
	}
}

// TestListRegisteredMcpServersDeclaresItsSuccess pins the tools_list contract.
//
// THE DEFECT. The operation declared only 401/403/501 and its description said
// it "always answers 501". Issue 335 replaced that refusal with a durable read:
// internal/api/v2/mcp/registry.go answers 200 with a JSON array. Because the
// spec declared no 2xx, orval collapsed the response type to an error-only
// union, so no TypeScript caller could read the list without a cast, and orval
// emitted no MSW handler for the operation at all.
func TestListRegisteredMcpServersDeclaresItsSuccess(t *testing.T) {
	document := loadShapeSpec(t)
	responses := responsesOf(findOperation(t, document, "listRegisteredMcpServers"))

	success, ok := responses["200"].(map[string]any)
	if !ok {
		t.Fatal("listRegisteredMcpServers declares no 200: the handler answers 200 " +
			"with a JSON array (internal/api/v2/mcp/registry.go)")
	}
	if _, unreachable := responses["501"]; unreachable {
		t.Error("listRegisteredMcpServers still declares 501, which no code path produces")
	}
	content, _ := success["content"].(map[string]any)
	media, _ := content["application/json"].(map[string]any)
	schema, _ := media["schema"].(map[string]any)
	if schema["type"] != "array" {
		t.Fatalf("the 200 body is not a bare array: %v — the SDK parses the body "+
			"directly as a list, so an envelope would break every worker", schema["type"])
	}

	// tools_call is the control. It still answers 501, so a change that deleted
	// every 501 in the file would be caught here rather than read as a pass.
	callResponses := responsesOf(findOperation(t, document, "callRegisteredMcpServerTool"))
	if _, ok := callResponses["501"]; !ok {
		t.Error("callRegisteredMcpServerTool lost its 501: that endpoint really is " +
			"unimplemented (ToolRegistryUnavailableReason)")
	}
}

// TestSecretsAdministrationModeIsNotDocumentedAsUnimplemented pins the secrets
// mode contract.
//
// THE DEFECT. Five operations declared a 501 for `administration` mode, and
// the block header said elitea-main answers that mode with 501. Both are
// unreachable: internal/api/v2/secrets/handler.go's withModes dispatches
// `administration` to real handlers in admin.go for list, show, update, delete
// and hide. Only POST /secrets/{mode}/{projectID} — the bulk global-vault
// replacement — is still unimplemented, so createSecret is the control here.
func TestSecretsAdministrationModeIsNotDocumentedAsUnimplemented(t *testing.T) {
	document := loadShapeSpec(t)

	for _, operationID := range []string{
		"listSecrets", "showSecret", "updateSecret", "deleteSecret", "hideSecret",
	} {
		if _, unreachable := responsesOf(findOperation(t, document, operationID))["501"]; unreachable {
			t.Errorf("%s declares 501 for administration mode, but admin.go implements it",
				operationID)
		}
	}

	// The control: this ONE administration branch really is unimplemented.
	if _, ok := responsesOf(findOperation(t, document, "createSecret"))["501"]; !ok {
		t.Error("createSecret lost its 501: the administration branch of " +
			"POST /secrets/{mode}/{projectID} is notImplementedBulkReplace")
	}

	// The administration-mode create route is live (admin.go's AdminCreate) and
	// used to be absent from the spec, which is why the admin page hand-rolled
	// its client.
	findOperation(t, document, "createSecretInMode")
}

// listSecretsSuccessSchema returns the resolved 200 schema of listSecrets.
func listSecretsSuccessSchema(t *testing.T) *openapi3.Schema {
	t.Helper()
	loader := openapi3.NewLoader()
	document, err := loader.LoadFromFile(shapeSpecPath)
	if err != nil {
		t.Fatalf("load %s: %v", shapeSpecPath, err)
	}
	item := document.Paths.Find("/secrets/secrets/{mode}/{projectID}")
	if item == nil || item.Get == nil {
		t.Fatal("v2.yaml declares no GET /secrets/secrets/{mode}/{projectID}")
	}
	response := item.Get.Responses.Status(http.StatusOK)
	if response == nil || response.Value == nil {
		t.Fatal("listSecrets declares no 200 response")
	}
	media := response.Value.Content.Get("application/json")
	if media == nil || media.Schema == nil || media.Schema.Value == nil {
		t.Fatal("the listSecrets 200 response declares no JSON schema")
	}
	return media.Schema.Value
}

// assertOnlyBranch checks that `body` matches branch `want` and no other.
func assertOnlyBranch(t *testing.T, schema *openapi3.Schema, want int, body any, label string) {
	t.Helper()
	for index, branch := range schema.AnyOf {
		err := branch.Value.VisitJSON(body)
		if index == want && err != nil {
			t.Errorf("branch %d rejects the %s row: %v", index, label, err)
		}
		if index != want && err == nil {
			t.Errorf("branch %d accepts the %s row, so the branches overlap", index, label)
		}
	}
}

// TestListSecretsAcceptsTheEmptyListItsHandlersReturn pins the listSecrets 200
// combinator.
//
// THE DEFECT. The 200 body used `oneOf` over two ARRAY branches. An empty
// array matches both branches, so `oneOf` refuses it. A vault that does not
// exist yet answers `[]` with 200 in both modes
// (internal/api/v2/secrets/handler.go's List, admin.go's AdminList). The most
// common real body failed its own contract. `anyOf` accepts it.
//
// EVIDENCE. The empty body below comes from the handler's own slice type, so
// the test cannot drift away from the producer.
func TestListSecretsAcceptsTheEmptyListItsHandlersReturn(t *testing.T) {
	schema := listSecretsSuccessSchema(t)

	encoded, err := json.Marshal([]v2secrets.SecretListItem{})
	if err != nil {
		t.Fatalf("marshal the empty secret list: %v", err)
	}
	var empty any
	if err := json.Unmarshal(encoded, &empty); err != nil {
		t.Fatalf("decode the marshaled list: %v", err)
	}
	if err := schema.VisitJSON(empty); err != nil {
		t.Errorf("the 200 schema rejects the empty list both handlers return: %v", err)
	}
	if len(schema.OneOf) > 0 {
		t.Error("the listSecrets 200 body uses oneOf; an empty array matches " +
			"both array branches, so oneOf refuses it")
	}
	if len(schema.AnyOf) != 2 {
		t.Fatalf("the listSecrets 200 body declares %d anyOf branches, want 2",
			len(schema.AnyOf))
	}

	// Controls. Each mode's row must still match one branch only. They keep the
	// test honest if a later edit replaces both branches with one loose array.
	projectRow := []any{map[string]any{
		"name": "token", "secret_name": "{{secret.token}}", "is_default": false,
	}}
	adminRow := []any{map[string]any{"name": "token", "secret": "******"}}
	assertOnlyBranch(t, schema, 0, projectRow, "default-mode")
	assertOnlyBranch(t, schema, 1, adminRow, "administration-mode")
}
