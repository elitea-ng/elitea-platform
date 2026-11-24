package storage

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/dbaction"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"google.golang.org/protobuf/proto"
)

func TestCurrentConfigurationsMaterializerUnsecretsDBActionSnapshot(t *testing.T) {
	t.Parallel()

	const sentinelPassword = "claimed-worker-only-sql-password"
	unsecreter := &currentMaterializationUnsecreterStub{values: map[int32]map[string]string{
		1: {"WH_PASS": sentinelPassword},
	}}
	materializer := newCurrentConfigurationsMaterializerForTest(t, unsecreter)

	rule := dbaction.Rule{
		ID: 5, ProjectID: 1, Revision: 1, Name: "warehouse.read",
		ConfigurationID: 12, Dialect: dbaction.DialectPostgres, DatabaseName: "analytics",
		Mode: dbaction.ModeRead, Enabled: true,
	}
	rule.Normalize()

	snapshot := dbaction.FrozenSnapshot{
		SchemaVersion: dbaction.SnapshotSchema,
		ProjectID:     1,
		ApplicationID: "31",
		VersionID:     "41",
		Rules: []dbaction.FrozenRule{{
			Name:            rule.Name,
			RuleID:          rule.ID,
			Revision:        rule.Revision,
			Dialect:         rule.Dialect,
			DatabaseName:    rule.DatabaseName,
			Mode:            rule.Mode,
			ConfigurationID: rule.ConfigurationID,
			SQLConfiguration: map[string]any{
				configurationapp.CurrentFrozenConfigurationMarker: true,
				"configuration_uuid":                              "60000000-0000-4000-8000-000000000721",
				"configuration_project_id":                        int32(1),
				"configuration_type":                              "sql",
				"host":                                            "warehouse.example.com",
				"port":                                            5432,
				"username":                                        "etl_ro",
				"password":                                        "{{secret.WH_PASS}}",
			},
			PolicyDigest: rule.PolicyDigest(),
		}},
	}
	applicationJSON, err := json.Marshal(map[string]any{
		"id":         31,
		"version_id": 41,
		"version_details": map[string]any{
			"id":                 41,
			"agent_type":         "pipeline",
			"tools":              []any{},
			"db_action_snapshot": snapshot,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	requestBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(&runtimev1.AgentExecutionInputV1{
		SchemaRevision:         "elitea.runtime.agent-execution-input.v1",
		Llm:                    []byte(`{"kwargs":{}}`),
		ChatHistory:            []byte(`[]`),
		UserInput:              []byte(`"run"`),
		Tools:                  []byte(`[]`),
		Application:            applicationJSON,
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
	})
	if err != nil {
		t.Fatal(err)
	}

	materialized, err := materializer.MaterializeContent(context.Background(), ContentAuthorization{
		CapabilityID:      executiondomain.AgentApplicationCapability,
		ResourceProjectID: "1",
		ActorID:           "7",
		SemanticRole:      executiondomain.AgentExecutionRequestRole,
	}, requestBytes, 1<<20)
	if err != nil {
		t.Fatalf("MaterializeContent: %v", err)
	}
	var decoded runtimev1.AgentExecutionInputV1
	if err := proto.Unmarshal(materialized, &decoded); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(decoded.Application), configurationapp.CurrentFrozenConfigurationMarker) {
		t.Fatal("materialized payload still contains CurrentFrozenConfigurationMarker")
	}
	var app map[string]any
	if err := json.Unmarshal(decoded.Application, &app); err != nil {
		t.Fatal(err)
	}
	details := app["version_details"].(map[string]any)
	snapMap := details["db_action_snapshot"].(map[string]any)
	rules := snapMap["rules"].([]any)
	sqlCfg := rules[0].(map[string]any)["sql_configuration"].(map[string]any)
	cred, err := dbaction.ParseMaterializedSQLCredential(dbaction.DialectPostgres, sqlCfg)
	if err != nil {
		t.Fatalf("ParseMaterializedSQLCredential: %v", err)
	}
	if cred.Password() != sentinelPassword || cred.Host != "warehouse.example.com" || cred.Port != 5432 {
		t.Fatalf("unexpected materialized credential: host=%s port=%d", cred.Host, cred.Port)
	}

	// Negative cases: foreign project, missing marker, wrong configuration type, unresolved secret.
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"foreign project id", func(cfg map[string]any) { cfg["configuration_project_id"] = int32(2) }},
		{"missing frozen marker", func(cfg map[string]any) { delete(cfg, configurationapp.CurrentFrozenConfigurationMarker) }},
		{"wrong configuration type", func(cfg map[string]any) { cfg["configuration_type"] = "openapi" }},
		{"unresolved secret", func(cfg map[string]any) { cfg["password"] = "{{secret.MISSING}}" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := map[string]any{
				configurationapp.CurrentFrozenConfigurationMarker: true,
				"configuration_uuid":                              "60000000-0000-4000-8000-000000000721",
				"configuration_project_id":                        int32(1),
				"configuration_type":                              "sql",
				"host":                                            "warehouse.example.com",
				"port":                                            5432,
				"username":                                        "etl_ro",
				"password":                                        "{{secret.WH_PASS}}",
			}
			tc.mutate(cfg)
			badSnap := snapshot
			badRule := snapshot.Rules[0]
			badRule.SQLConfiguration = cfg
			badSnap.Rules = []dbaction.FrozenRule{badRule}
			appBytes, _ := json.Marshal(map[string]any{
				"id": 31, "version_id": 41,
				"version_details": map[string]any{"id": 41, "agent_type": "pipeline", "tools": []any{}, "db_action_snapshot": badSnap},
			})
			reqBytes, _ := proto.MarshalOptions{Deterministic: true}.Marshal(&runtimev1.AgentExecutionInputV1{
				SchemaRevision: "elitea.runtime.agent-execution-input.v1",
				Llm:            []byte(`{"kwargs":{}}`), ChatHistory: []byte(`[]`), UserInput: []byte(`"run"`),
				Tools: []byte(`[]`), Application: appBytes, InternalTools: []byte(`[]`),
				McpTokens: []byte(`{}`), IgnoredMcpServers: []byte(`[]`), UserDeclinedMcpServers: []byte(`[]`),
				HitlDecisions: []byte(`[]`), Meta: []byte(`{}`), ContextSettings: []byte(`{}`),
				InvokedSkills: []byte(`[]`), AppliedSkills: []byte(`[]`), AttachedSkills: []byte(`[]`),
				InputAttachments: []byte(`[]`), ParallelReconcile: []byte(`null`), ParallelTerminalErrors: []byte(`[]`),
			})
			if _, err := materializer.MaterializeContent(context.Background(), ContentAuthorization{
				CapabilityID: executiondomain.AgentApplicationCapability, ResourceProjectID: "1", ActorID: "7", SemanticRole: executiondomain.AgentExecutionRequestRole,
			}, reqBytes, 1<<20); err == nil {
				t.Fatalf("expected MaterializeContent refusal for %s", tc.name)
			}
		})
	}
}
