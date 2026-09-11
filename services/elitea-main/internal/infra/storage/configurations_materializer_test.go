package storage

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
	"google.golang.org/protobuf/proto"
)

type currentMaterializationUnsecreterStub struct {
	values   map[int32]map[string]string
	projects []int32
	inputs   []map[string]any
	err      error
	fn       func(int32, map[string]any) (map[string]any, error)
}

type currentAgentPrebuiltMCPResolverStub struct {
	result   map[string]any
	found    bool
	err      error
	toolType string
	settings map[string]any
	keep     []string
	calls    int
}

func (stub *currentAgentPrebuiltMCPResolverStub) ResolveCurrentAgentPrebuiltMCP(
	_ context.Context,
	_ int32,
	_ int32,
	toolType string,
	settings map[string]any,
	_ func(map[string]any) (map[string]any, error),
) (map[string]any, bool, error) {
	stub.calls++
	stub.toolType = toolType
	stub.settings = settings
	if stub.keep != nil {
		stub.settings = make(map[string]any, len(stub.keep))
		for _, name := range stub.keep {
			if value, ok := settings[name]; ok {
				stub.settings[name] = value
			}
		}
	}
	return stub.result, stub.found, stub.err
}

func (s *currentMaterializationUnsecreterStub) Unsecret(
	_ context.Context,
	projectID int32,
	value map[string]any,
) (map[string]any, error) {
	s.projects = append(s.projects, projectID)
	s.inputs = append(s.inputs, replaceCurrentMaterializationSecrets(value, nil).(map[string]any))
	if s.err != nil {
		return nil, s.err
	}
	if s.fn != nil {
		return s.fn(projectID, value)
	}
	return replaceCurrentMaterializationSecrets(value, s.values[projectID]).(map[string]any), nil
}

func TestCurrentConfigurationsMaterializerRedeemsFrozenConfigurationOwnersWithoutLookup(t *testing.T) {
	unsecreter := &currentMaterializationUnsecreterStub{values: map[int32]map[string]string{
		7: {
			"PROJECT_NOTE": "project-note",
		},
		1: {
			"CONFLUENCE_TOKEN": "confluence-secret",
		},
		700: {
			"PGVECTOR_CONN": "postgresql://personal-vectorstore",
		},
	}}
	materializer, err := NewCurrentConfigurationsMaterializer(unsecreter)
	if err != nil {
		t.Fatal(err)
	}

	source := []byte(`{"id":19,"type":"confluence","toolkit_name":"wiki","settings":{"confluence_configuration":{"elitea_title":"wiki-credential","private":false,"url":"https://wiki.example","token":"{{secret.CONFLUENCE_TOKEN}}","configuration_uuid":"configuration-confluence","configuration_project_id":1,"configuration_type":"confluence","__elitea_frozen_configuration_v1":true,"vector":{"elitea_title":"personal-vectorstore","private":true,"connection_string":"{{secret.PGVECTOR_CONN}}","configuration_uuid":"configuration-pgvector","configuration_project_id":700,"configuration_type":"pgvector","__elitea_frozen_configuration_v1":true}},"note":"{{secret.PROJECT_NOTE}}","integer_id":9007199254740993}}`)
	result, err := materializer.MaterializeContent(context.Background(), ContentAuthorization{
		ResourceProjectID: "7",
		ActorID:           "42",
		CapabilityID:      executiondomain.IndexIngestCapability,
		SemanticRole:      executiondomain.IndexToolkitConfigurationRole,
	}, source, 256*1024)
	if err != nil {
		t.Fatal(err)
	}

	actual, err := decodeCurrentMaterializationObject(result)
	if err != nil {
		t.Fatal(err)
	}
	settings := actual["settings"].(map[string]any)
	wiki := settings["confluence_configuration"].(map[string]any)
	vector := wiki["vector"].(map[string]any)
	if wiki["configuration_type"] != "confluence" || wiki["configuration_project_id"] != json.Number("1") ||
		wiki["token"] != "confluence-secret" ||
		vector["configuration_type"] != "pgvector" || vector["configuration_project_id"] != json.Number("700") ||
		vector["connection_string"] != "postgresql://personal-vectorstore" ||
		settings["note"] != "project-note" || settings["integer_id"] != json.Number("9007199254740993") {
		t.Fatalf("unexpected materialized settings: %#v", settings)
	}
	if !reflect.DeepEqual(unsecreter.projects, []int32{700, 1, 7}) {
		t.Fatalf("unsecret project ownership=%v, want [700 1 7]", unsecreter.projects)
	}
	if encoded, marshalErr := json.Marshal(unsecreter.inputs[1]); marshalErr != nil || strings.Contains(string(encoded), "PGVECTOR_CONN") {
		t.Fatalf("parent configuration saw nested-owner references: %s error=%v", encoded, marshalErr)
	}
	if encoded, marshalErr := json.Marshal(unsecreter.inputs[2]); marshalErr != nil ||
		strings.Contains(string(encoded), "CONFLUENCE_TOKEN") || strings.Contains(string(encoded), "PGVECTOR_CONN") {
		t.Fatalf("invoking project saw configuration-owner references: %s error=%v", encoded, marshalErr)
	}
	if strings.Contains(string(source), "confluence-secret") || strings.Contains(string(source), "postgresql://personal-vectorstore") {
		t.Fatal("immutable admitted source was mutated with plaintext")
	}
	if strings.Contains(string(result), configurationapp.CurrentFrozenConfigurationMarker) {
		t.Fatalf("internal frozen-owner marker reached worker output: %s", result)
	}
}

func TestCurrentConfigurationsMaterializerRedeemsConfiguredAgentToolsAtClaimTime(t *testing.T) {
	unsecreter := &currentMaterializationUnsecreterStub{values: map[int32]map[string]string{
		7: {"PROJECT_NOTE": "project-note"},
		1: {"SHAREPOINT_TOKEN": "sharepoint-secret"},
	}}
	materializer := newCurrentConfigurationsMaterializerForTest(t, unsecreter)
	application := []byte(`{"id":3,"version_id":4,"version_details":{"id":4,"tools":[{"id":2,"type":"sharepoint","settings":{"sharepoint_configuration":{"configuration_uuid":"configuration-sharepoint","configuration_project_id":1,"configuration_type":"sharepoint","__elitea_frozen_configuration_v1":true,"token":"{{secret.SHAREPOINT_TOKEN}}"},"note":"{{secret.PROJECT_NOTE}}"}}]}}`)
	request := &runtimev1.AgentExecutionInputV1{
		SchemaRevision:         "elitea.runtime.agent-execution-input.v1",
		Llm:                    []byte(`{"kwargs":{}}`),
		ChatHistory:            []byte(`[]`),
		UserInput:              []byte(`"hello"`),
		Tools:                  []byte(`[]`),
		Application:            application,
		InternalTools:          []byte(`[]`),
		McpTokens:              []byte(`{}`),
		IgnoredMcpServers:      []byte(`[]`),
		UserDeclinedMcpServers: []byte(`[]`),
		HitlDecisions:          []byte(`[]`),
		Meta:                   []byte(`{}`),
		ContextSettings:        []byte(`{}`),
		InvokedSkills:          []byte(`[]`),
		AppliedSkills:          []byte(`[]`),
		AttachedSkills:         []byte(`[]`),
		InputAttachments:       []byte(`[]`),
		ParallelReconcile:      []byte(`null`),
		ParallelTerminalErrors: []byte(`[]`),
	}
	source, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	result, err := materializer.MaterializeContent(context.Background(), ContentAuthorization{
		ResourceProjectID: "7",
		ActorID:           "42",
		CapabilityID:      executiondomain.AgentApplicationCapability,
		SemanticRole:      executiondomain.AgentExecutionRequestRole,
	}, source, 256*1024)
	if err != nil {
		t.Fatal(err)
	}
	var materialized runtimev1.AgentExecutionInputV1
	if err := proto.Unmarshal(result, &materialized); err != nil {
		t.Fatal(err)
	}
	resolved, err := decodeCurrentMaterializationObject(materialized.GetApplication())
	if err != nil {
		t.Fatal(err)
	}
	tool := resolved["version_details"].(map[string]any)["tools"].([]any)[0].(map[string]any)
	settings := tool["settings"].(map[string]any)
	credential := settings["sharepoint_configuration"].(map[string]any)
	if credential["token"] != "sharepoint-secret" || settings["note"] != "project-note" ||
		strings.Contains(string(result), configurationapp.CurrentFrozenConfigurationMarker) {
		t.Fatalf("materialized settings=%#v", settings)
	}
	if !reflect.DeepEqual(unsecreter.projects, []int32{1, 7}) {
		t.Fatalf("unsecret project ownership=%v, want [1 7]", unsecreter.projects)
	}
	if strings.Contains(string(source), "sharepoint-secret") {
		t.Fatal("immutable agent input was mutated with plaintext")
	}
}

func TestCurrentConfigurationsMaterializerRedeemsAdhocAgentToolsAtClaimTime(t *testing.T) {
	unsecreter := &currentMaterializationUnsecreterStub{values: map[int32]map[string]string{
		7: {"PROJECT_NOTE": "project-note"},
		1: {"AHA_TOKEN": "aha-secret"},
	}}
	materializer := newCurrentConfigurationsMaterializerForTest(t, unsecreter)
	request := &runtimev1.AgentExecutionInputV1{
		SchemaRevision: "elitea.runtime.agent-execution-input.v1",
		Llm:            []byte(`{"kwargs":{}}`),
		ChatHistory:    []byte(`[]`),
		UserInput:      []byte(`"hello"`),
		Tools: []byte(`[{
			"id":3,
			"type":"aha",
			"toolkit_name":"aha",
			"settings":{
				"aha_configuration":{
					"configuration_uuid":"configuration-aha",
					"configuration_project_id":1,
					"configuration_type":"aha",
					"__elitea_frozen_configuration_v1":true,
					"api_key":"{{secret.AHA_TOKEN}}"
				},
				"note":"{{secret.PROJECT_NOTE}}"
			}
		}]`),
		Application:            []byte(`{"instructions":"Use the attached tools."}`),
		InternalTools:          []byte(`[]`),
		McpTokens:              []byte(`{}`),
		IgnoredMcpServers:      []byte(`[]`),
		UserDeclinedMcpServers: []byte(`[]`),
		HitlDecisions:          []byte(`[]`),
		Meta:                   []byte(`{}`),
		ContextSettings:        []byte(`{}`),
		InvokedSkills:          []byte(`[]`),
		AppliedSkills:          []byte(`[]`),
		AttachedSkills:         []byte(`[]`),
		InputAttachments:       []byte(`[]`),
		ParallelReconcile:      []byte(`null`),
		ParallelTerminalErrors: []byte(`[]`),
	}
	source, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	result, err := materializer.MaterializeContent(context.Background(), ContentAuthorization{
		ResourceProjectID: "7",
		ActorID:           "42",
		CapabilityID:      executiondomain.AgentAdhocCapability,
		SemanticRole:      executiondomain.AgentExecutionRequestRole,
	}, source, 256*1024)
	if err != nil {
		t.Fatal(err)
	}
	var materialized runtimev1.AgentExecutionInputV1
	if err := proto.Unmarshal(result, &materialized); err != nil {
		t.Fatal(err)
	}
	tools, err := decodeCurrentMaterializationArray(materialized.GetTools())
	if err != nil {
		t.Fatal(err)
	}
	settings := tools[0].(map[string]any)["settings"].(map[string]any)
	credential := settings["aha_configuration"].(map[string]any)
	if credential["api_key"] != "aha-secret" || settings["note"] != "project-note" ||
		strings.Contains(string(result), configurationapp.CurrentFrozenConfigurationMarker) {
		t.Fatalf("materialized settings=%#v", settings)
	}
	if !reflect.DeepEqual(unsecreter.projects, []int32{1, 7}) {
		t.Fatalf("unsecret project ownership=%v, want [1 7]", unsecreter.projects)
	}
	if strings.Contains(string(source), "aha-secret") {
		t.Fatal("immutable agent input was mutated with plaintext")
	}
}

func TestCurrentConfigurationsMaterializerRedeemsDirectToolkitAtClaimTime(t *testing.T) {
	unsecreter := &currentMaterializationUnsecreterStub{values: map[int32]map[string]string{
		7: {"GITHUB_TOKEN": "plain-token"},
	}}
	materializer := newCurrentConfigurationsMaterializerForTest(t, unsecreter)
	request := &runtimev1.ToolkitExecuteReadInputV1{
		SchemaRevision:    "elitea.runtime.toolkit-execute-read-input.v1",
		ToolkitType:       "github",
		ToolkitName:       "Source Control",
		ToolName:          "get_issue",
		Toolkit:           []byte(`{"id":19,"type":"github","toolkit_name":"Source Control","settings":{"token":"{{secret.GITHUB_TOKEN}}","selected_tools":["get_issue"]}}`),
		Arguments:         []byte(`{"issue":7}`),
		ToolkitGuardrails: []byte(`{"blocked_tools":{},"sensitive_tools":{}}`),
	}
	source, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}

	result, err := materializer.MaterializeContent(context.Background(), ContentAuthorization{
		ResourceProjectID: "7",
		ActorID:           "11",
		CapabilityID:      executiondomain.ToolkitExecuteReadCapability,
		SemanticRole:      executiondomain.ToolkitExecuteReadRequestRole,
	}, source, executiondomain.MaxToolkitExecuteReadInputBytes)
	if err != nil {
		t.Fatal(err)
	}
	var materialized runtimev1.ToolkitExecuteReadInputV1
	if err := proto.Unmarshal(result, &materialized); err != nil {
		t.Fatal(err)
	}
	toolkit, err := decodeCurrentMaterializationObject(materialized.GetToolkit())
	if err != nil {
		t.Fatal(err)
	}
	settings, ok := toolkit["settings"].(map[string]any)
	if !ok {
		t.Fatalf("settings = %#v", toolkit["settings"])
	}
	if settings["token"] != "plain-token" {
		t.Fatalf("materialized token = %#v", settings["token"])
	}
	if string(materialized.GetArguments()) != string(request.GetArguments()) ||
		string(materialized.GetToolkitGuardrails()) != string(request.GetToolkitGuardrails()) {
		t.Fatal("claim materialization changed invocation arguments or guardrails")
	}
}

func TestCurrentAgentMaterializerUsesOnlyResolvedPrebuiltMCPAuthority(t *testing.T) {
	unsecreter := &currentMaterializationUnsecreterStub{values: map[int32]map[string]string{
		7: {"CALLER_SECRET": "must-not-be-redeemed"},
	}}
	resolver := &currentAgentPrebuiltMCPResolverStub{
		found: true,
		keep:  []string{"server_name", "selected_tools"},
		result: map[string]any{
			"server_name":    "release_intelligence",
			"url":            "https://mcp.example.test/v1/mcp",
			"headers":        map[string]any{"X-Platform": "fixed-secret"},
			"selected_tools": []any{"lookup_release"},
		},
	}
	materializer, err := NewCurrentAgentConfigurationsMaterializer(
		unsecreter,
		resolver,
	)
	if err != nil {
		t.Fatal(err)
	}
	request := currentAgentMaterializationRequest(`[{
		"id":3,
		"type":"mcp_config",
		"toolkit_name":"release intelligence",
		"settings":{
			"server_name":"release_intelligence",
			"url":"https://caller.example.test/mcp",
			"headers":{"Authorization":"caller-secret"},
			"unknown":"{{secret.CALLER_SECRET}}",
			"selected_tools":["lookup_release"]
		}
	}]`)
	source, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	result, err := materializer.MaterializeContent(context.Background(), ContentAuthorization{
		ResourceProjectID: "7",
		ActorID:           "42",
		CapabilityID:      executiondomain.AgentAdhocCapability,
		SemanticRole:      executiondomain.AgentExecutionRequestRole,
	}, source, 256*1024)
	if err != nil {
		t.Fatal(err)
	}
	if resolver.calls != 1 || resolver.toolType != "mcp_config" {
		t.Fatalf("resolver calls=%d type=%q", resolver.calls, resolver.toolType)
	}
	if _, present := resolver.settings["unknown"]; present || len(unsecreter.projects) != 0 {
		t.Fatal("caller connection fields reached claim-time secret resolution")
	}

	var materialized runtimev1.AgentExecutionInputV1
	if err := proto.Unmarshal(result, &materialized); err != nil {
		t.Fatal(err)
	}
	tools, err := decodeCurrentMaterializationArray(materialized.GetTools())
	if err != nil {
		t.Fatal(err)
	}
	settings := tools[0].(map[string]any)["settings"].(map[string]any)
	if settings["url"] != "https://mcp.example.test/v1/mcp" {
		t.Fatalf("materialized settings=%#v", settings)
	}
	if strings.Contains(string(result), "caller-secret") ||
		strings.Contains(string(result), "caller.example.test") {
		t.Fatal("caller connection authority reached the claimed input")
	}
}

func TestCurrentAgentMaterializerRejectsUnresolvedPrebuiltMCP(t *testing.T) {
	request := currentAgentMaterializationRequest(`[{"id":3,"type":"mcp_config","settings":{"server_name":"missing"}}]`)
	source, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	authorization := ContentAuthorization{
		ResourceProjectID: "7",
		ActorID:           "42",
		CapabilityID:      executiondomain.AgentAdhocCapability,
		SemanticRole:      executiondomain.AgentExecutionRequestRole,
	}
	materializer := newCurrentConfigurationsMaterializerForTest(
		t,
		&currentMaterializationUnsecreterStub{},
	)
	if _, err := materializer.MaterializeContent(
		context.Background(), authorization, source, 256*1024,
	); !errors.Is(err, ErrContentRejected) {
		t.Fatalf("unwired prebuilt error=%v", err)
	}

	resolver := &currentAgentPrebuiltMCPResolverStub{}
	materializer, err = NewCurrentAgentConfigurationsMaterializer(
		&currentMaterializationUnsecreterStub{},
		resolver,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := materializer.MaterializeContent(
		context.Background(), authorization, source, 256*1024,
	); !errors.Is(err, ErrContentRejected) {
		t.Fatalf("missing catalogue entry error=%v", err)
	}

	resolver.err = errors.New("catalogue unavailable details")
	if _, err := materializer.MaterializeContent(
		context.Background(), authorization, source, 256*1024,
	); !errors.Is(err, ErrContentUnavailable) || strings.Contains(err.Error(), "details") {
		t.Fatalf("catalogue dependency error=%v", err)
	}
}

func TestCurrentConfigurationsMaterializerRejectsIncompleteFrozenConfigurationMetadata(t *testing.T) {
	unsecreter := &currentMaterializationUnsecreterStub{}
	materializer := newCurrentConfigurationsMaterializerForTest(t, unsecreter)
	authorization := ContentAuthorization{
		ResourceProjectID: "7",
		ActorID:           "42",
		CapabilityID:      executiondomain.IndexIngestCapability,
		SemanticRole:      executiondomain.IndexToolkitConfigurationRole,
	}

	for name, source := range map[string]string{
		"missing marker": `{"id":19,"type":"github","settings":{"credential":{"configuration_uuid":"uuid","configuration_project_id":7,"configuration_type":"github"}}}`,
		"missing owner":  `{"id":19,"type":"github","settings":{"credential":{"configuration_uuid":"uuid","configuration_type":"github","__elitea_frozen_configuration_v1":true}}}`,
		"invalid owner":  `{"id":19,"type":"github","settings":{"credential":{"configuration_uuid":"uuid","configuration_project_id":0,"configuration_type":"github","__elitea_frozen_configuration_v1":true}}}`,
		"invalid uuid":   `{"id":19,"type":"github","settings":{"credential":{"configuration_uuid":"","configuration_project_id":7,"configuration_type":"github","__elitea_frozen_configuration_v1":true}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := materializer.MaterializeContent(context.Background(), authorization, []byte(source), 4096)
			if !errors.Is(err, ErrContentRejected) {
				t.Fatalf("error=%v, want rejected", err)
			}
		})
	}
	if len(unsecreter.projects) != 0 {
		t.Fatalf("invalid snapshot reached a vault: %v", unsecreter.projects)
	}
}

func TestCurrentConfigurationsMaterializerUnsecretsProjectInvocationObjects(t *testing.T) {
	unsecreter := &currentMaterializationUnsecreterStub{values: map[int32]map[string]string{
		7: {"QUERY": "resolved-query"},
	}}
	materializer := newCurrentConfigurationsMaterializerForTest(t, unsecreter)

	result, err := materializer.MaterializeContent(context.Background(), ContentAuthorization{
		ResourceProjectID: "7",
		ActorID:           "42",
		CapabilityID:      executiondomain.IndexIngestCapability,
		SemanticRole:      executiondomain.IndexToolParametersRole,
	}, []byte(`{"index_name":"docs","query":"{{secret.QUERY}}"}`), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != `{"index_name":"docs","query":"resolved-query"}` {
		t.Fatalf("result=%s", result)
	}
}

func TestCurrentConfigurationsMaterializerPreservesNonIndexContentByteForByte(t *testing.T) {
	materializer := newCurrentConfigurationsMaterializerForTest(t, &currentMaterializationUnsecreterStub{})
	source := []byte(" {\n  \"token\": \"{{secret.VALUE}}\"\n}\n")
	result, err := materializer.MaterializeContent(context.Background(), ContentAuthorization{
		CapabilityID: "configuration.validate.v1",
	}, source, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if &result[0] != &source[0] || string(result) != string(source) {
		t.Fatal("non-index input did not retain its byte-for-byte content path")
	}
}

func TestCurrentConfigurationsMaterializerFailsClosedAndClassifiesDependencies(t *testing.T) {
	dependency := &currentMaterializationUnsecreterStub{err: errors.New("vault details must not escape")}
	materializer := newCurrentConfigurationsMaterializerForTest(t, dependency)
	base := ContentAuthorization{
		ResourceProjectID: "7",
		ActorID:           "42",
		CapabilityID:      executiondomain.IndexIngestCapability,
		SemanticRole:      executiondomain.IndexToolParametersRole,
	}

	if _, err := materializer.MaterializeContent(context.Background(), base, []byte(`{"value":"x"}`), 1024); !errors.Is(err, ErrContentUnavailable) || strings.Contains(err.Error(), "vault details") {
		t.Fatalf("dependency error=%v", err)
	}

	for name, mutate := range map[string]func(*ContentAuthorization){
		"non-canonical project": func(value *ContentAuthorization) { value.ResourceProjectID = "07" },
		"missing actor":         func(value *ContentAuthorization) { value.ActorID = "" },
		"unknown role":          func(value *ContentAuthorization) { value.SemanticRole = "index.unknown" },
	} {
		t.Run(name, func(t *testing.T) {
			authorization := base
			mutate(&authorization)
			if _, err := materializer.MaterializeContent(context.Background(), authorization, []byte(`{"value":"x"}`), 1024); !errors.Is(err, ErrContentRejected) {
				t.Fatalf("error=%v, want rejected", err)
			}
		})
	}

	if _, err := materializer.MaterializeContent(context.Background(), base, []byte(`[]`), 1024); !errors.Is(err, ErrContentRejected) {
		t.Fatalf("non-object error=%v, want rejected", err)
	}
	if _, err := materializer.MaterializeContent(context.Background(), base, []byte(`{"value":"x"}`), 2); !errors.Is(err, ErrContentRejected) {
		t.Fatalf("oversize error=%v, want rejected", err)
	}
}

func TestCurrentConfigurationsMaterializerValidatesConstructionAndModelShape(t *testing.T) {
	if _, err := NewCurrentConfigurationsMaterializer(nil); err == nil {
		t.Fatal("expected missing unsecreter to fail")
	}
	if _, err := NewCurrentAgentConfigurationsMaterializer(
		&currentMaterializationUnsecreterStub{}, nil,
	); err == nil {
		t.Fatal("expected missing prebuilt MCP resolver to fail")
	}
	materializer := newCurrentConfigurationsMaterializerForTest(t, &currentMaterializationUnsecreterStub{})
	authorization := ContentAuthorization{
		ResourceProjectID: "7",
		ActorID:           "42",
		CapabilityID:      executiondomain.IndexIngestCapability,
		SemanticRole:      executiondomain.IndexLLMModelRole,
	}
	if result, err := materializer.MaterializeContent(context.Background(), authorization, []byte(`"configured-model"`), 1024); err != nil || string(result) != `"configured-model"` {
		t.Fatalf("model result=%s error=%v", result, err)
	}
	if _, err := materializer.MaterializeContent(context.Background(), authorization, []byte(`null`), 1024); !errors.Is(err, ErrContentRejected) {
		t.Fatalf("invalid model error=%v", err)
	}
	authorization.SemanticRole = executiondomain.IndexEmbeddingBindingRole
	binding := []byte(`{"schema_version":"elitea.index.embedding-binding.v1"}`)
	if result, err := materializer.MaterializeContent(
		context.Background(),
		authorization,
		binding,
		1024,
	); err != nil || !reflect.DeepEqual(result, binding) {
		t.Fatalf("embedding binding result=%s error=%v", result, err)
	}
}

func currentAgentMaterializationRequest(tools string) *runtimev1.AgentExecutionInputV1 {
	return &runtimev1.AgentExecutionInputV1{
		SchemaRevision:         "elitea.runtime.agent-execution-input.v1",
		Llm:                    []byte(`{"kwargs":{}}`),
		ChatHistory:            []byte(`[]`),
		UserInput:              []byte(`"hello"`),
		Tools:                  []byte(tools),
		Application:            []byte(`{"instructions":"Use the attached tools."}`),
		InternalTools:          []byte(`[]`),
		McpTokens:              []byte(`{}`),
		IgnoredMcpServers:      []byte(`[]`),
		UserDeclinedMcpServers: []byte(`[]`),
		HitlDecisions:          []byte(`[]`),
		Meta:                   []byte(`{}`),
		ContextSettings:        []byte(`{}`),
		InvokedSkills:          []byte(`[]`),
		AppliedSkills:          []byte(`[]`),
		AttachedSkills:         []byte(`[]`),
		InputAttachments:       []byte(`[]`),
		ParallelReconcile:      []byte(`null`),
		ParallelTerminalErrors: []byte(`[]`),
	}
}

func newCurrentConfigurationsMaterializerForTest(
	t *testing.T,
	unsecreter configurationapp.CurrentExpansionUnsecreter,
) *CurrentConfigurationsMaterializer {
	t.Helper()
	materializer, err := NewCurrentConfigurationsMaterializer(unsecreter)
	if err != nil {
		t.Fatal(err)
	}
	return materializer
}

func replaceCurrentMaterializationSecrets(value any, secrets map[string]string) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[key] = replaceCurrentMaterializationSecrets(item, secrets)
		}
		return result
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = replaceCurrentMaterializationSecrets(item, secrets)
		}
		return result
	case string:
		for name, replacement := range secrets {
			typed = strings.ReplaceAll(typed, "{{secret."+name+"}}", replacement)
		}
		return typed
	default:
		return typed
	}
}
