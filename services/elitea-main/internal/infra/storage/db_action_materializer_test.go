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

func buildValidDBActionSnapshot() dbaction.FrozenSnapshot {
	rule := dbaction.Rule{
		ID: 5, ProjectID: 1, Revision: 1, Name: "warehouse.read",
		ConfigurationID: 12, Dialect: dbaction.DialectPostgres, DatabaseName: "analytics",
		Mode: dbaction.ModeRead, Enabled: true,
	}
	rule.Normalize()

	frozen := dbaction.FrozenRule{
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
	}
	snapshot := dbaction.FrozenSnapshot{
		SchemaVersion: dbaction.SnapshotSchema,
		ProjectID:     1,
		ApplicationID: "31",
		VersionID:     "41",
		Rules:         []dbaction.FrozenRule{frozen},
	}
	snapshot.Rules[0].BindingDigest = dbaction.FrozenRuleBindingDigest(snapshot, snapshot.Rules[0])
	return snapshot
}

func marshalAgentExecutionWithDBSnapshot(t *testing.T, appID, versionID int64, snap any) []byte {
	t.Helper()
	applicationJSON, err := json.Marshal(map[string]any{
		"id":         appID,
		"version_id": versionID,
		"version_details": map[string]any{
			"id":                 versionID,
			"agent_type":         "pipeline",
			"tools":              []any{},
			"db_action_snapshot": snap,
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
	return requestBytes
}

func TestCurrentConfigurationsMaterializerUnsecretsDBActionSnapshot(t *testing.T) {
	t.Parallel()

	const sentinelPassword = "claimed-worker-only-sql-password"
	unsecreter := &currentMaterializationUnsecreterStub{values: map[int32]map[string]string{
		1: {"WH_PASS": sentinelPassword},
		2: {"FOREIGN_SECRET": "do-not-leak"},
	}}
	materializer := newCurrentConfigurationsMaterializerForTest(t, unsecreter)

	snapshot := buildValidDBActionSnapshot()
	requestBytes := marshalAgentExecutionWithDBSnapshot(t, 31, 41, snapshot)

	materialized, err := materializer.MaterializeContent(context.Background(), ContentAuthorization{
		CapabilityID:      executiondomain.AgentApplicationCapability,
		ResourceProjectID: "1",
		ActorID:           "7",
		SemanticRole:      executiondomain.AgentExecutionRequestRole,
	}, requestBytes, 1<<20)
	if err != nil {
		t.Fatalf("MaterializeContent: %v", err)
	}
	// Verify only project 1 was unsecreted and ONLY the password key was passed.
	if len(unsecreter.inputs) != 1 || len(unsecreter.inputs[0]) != 1 {
		t.Fatalf("unsecreter inputs = %#v, want single map with password key only", unsecreter.inputs)
	}
	if _, hasPass := unsecreter.inputs[0]["password"]; !hasPass {
		t.Fatalf("unsecreter input missing password key: %#v", unsecreter.inputs[0])
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
	defer cred.Zero()
	if cred.Password() != sentinelPassword || cred.Host != "warehouse.example.com" || cred.Port != 5432 {
		t.Fatalf("unexpected materialized credential: host=%s port=%d", cred.Host, cred.Port)
	}

	// Negative hardening cases (SEC-5E2-01, SEC-5E2-02, SEC-5E2-04):
	for _, tc := range []struct {
		name   string
		mutate func(*dbaction.FrozenSnapshot)
	}{
		{"missing binding_digest", func(s *dbaction.FrozenSnapshot) { s.Rules[0].BindingDigest = "" }},
		{"tampered policy_digest", func(s *dbaction.FrozenSnapshot) { s.Rules[0].PolicyDigest = strings.Repeat("0", 64) }},
		{"tampered mode read->write", func(s *dbaction.FrozenSnapshot) {
			s.Rules[0].Mode = dbaction.ModeWrite
			s.Rules[0].MarkerTable = dbaction.DefaultMarkerTable
		}},
		{"wrong snapshot project_id", func(s *dbaction.FrozenSnapshot) { s.ProjectID = 2 }},
		{"wrong snapshot application_id", func(s *dbaction.FrozenSnapshot) { s.ApplicationID = "99" }},
		{"wrong snapshot version_id", func(s *dbaction.FrozenSnapshot) { s.VersionID = "99" }},
		{"foreign sql_configuration project_id", func(s *dbaction.FrozenSnapshot) {
			s.Rules[0].SQLConfiguration["configuration_project_id"] = int32(2)
		}},
		{"missing frozen marker", func(s *dbaction.FrozenSnapshot) {
			delete(s.Rules[0].SQLConfiguration, configurationapp.CurrentFrozenConfigurationMarker)
		}},
		{"wrong configuration_type", func(s *dbaction.FrozenSnapshot) {
			s.Rules[0].SQLConfiguration["configuration_type"] = "openapi"
		}},
		{"nested foreign config map in extra key", func(s *dbaction.FrozenSnapshot) {
			s.Rules[0].SQLConfiguration["nested"] = map[string]any{
				configurationapp.CurrentFrozenConfigurationMarker: true,
				"configuration_uuid":                              "foreign-uuid",
				"configuration_project_id":                        int32(2),
				"configuration_type":                              "sql",
				"leak":                                            "{{secret.FOREIGN_SECRET}}",
			}
		}},
		{"unsealed plaintext password in snapshot", func(s *dbaction.FrozenSnapshot) {
			s.Rules[0].SQLConfiguration["password"] = "raw-plaintext-password"
		}},
		{"secret placeholder in username", func(s *dbaction.FrozenSnapshot) {
			s.Rules[0].SQLConfiguration["username"] = "{{secret.WH_PASS}}"
		}},
		{"unresolved secret in password", func(s *dbaction.FrozenSnapshot) {
			s.Rules[0].SQLConfiguration["password"] = "{{secret.MISSING}}"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			badSnap := buildValidDBActionSnapshot()
			tc.mutate(&badSnap)
			reqBytes := marshalAgentExecutionWithDBSnapshot(t, 31, 41, badSnap)
			if _, err := materializer.MaterializeContent(context.Background(), ContentAuthorization{
				CapabilityID: executiondomain.AgentApplicationCapability, ResourceProjectID: "1", ActorID: "7", SemanticRole: executiondomain.AgentExecutionRequestRole,
			}, reqBytes, 1<<20); err == nil {
				t.Fatalf("expected MaterializeContent refusal for %s", tc.name)
			}
		})
	}
}

func TestFreezeHTTPChildVersionStripsPreExistingDBActionSnapshot(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(map[string]any{
		"agent_type":         "pipeline",
		"instructions":       "entry_point: a\nnodes:\n  - id: a\n    type: llm\n    transition: END\n",
		"db_action_snapshot": buildValidDBActionSnapshot(),
	})
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := FreezeHTTPChildVersion(31, 41, raw)
	if err != nil {
		t.Fatalf("FreezeHTTPChildVersion: %v", err)
	}
	if strings.Contains(string(frozen), "db_action_snapshot") {
		t.Fatalf("FreezeHTTPChildVersion kept pre-existing db_action_snapshot: %s", frozen)
	}
}
