package repos

import (
	"strings"
	"testing"

	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

func TestAgentGraphCheckpointMigrationCreatesFreshBoundedRustLineage(t *testing.T) {
	raw, err := platformmigrations.Files.ReadFile(
		"agentstate/0001_agent_graph_checkpoints.sql",
	)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sql := string(raw)
	for _, fragment := range []string{
		"CREATE TABLE elitea_runtime.agent_graph_checkpoint_writers",
		"CREATE TABLE elitea_runtime.agent_graph_checkpoints",
		"checkpoint_family = 'adk-graph.2.0.0.v1'",
		"octet_length(definition_digest) = 32",
		"created_at_rfc3339 TEXT NOT NULL",
		"payload_bytes BIGINT GENERATED ALWAYS AS",
		"child_ledger IS JSON OBJECT WITH UNIQUE KEYS",
		"octet_length(state)",
		"BETWEEN 5 AND 8388608",
		"save_ordinal BIGINT NOT NULL",
		"next_save_ordinal BIGINT NOT NULL DEFAULT 1",
		"thread_id, save_ordinal DESC",
		"writer_claim_attempt BIGINT NOT NULL",
		"writer_lease_epoch BIGINT NOT NULL",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("agent graph checkpoint migration is missing %q", fragment)
		}
	}
	for _, forbidden := range []string{
		"references centry.project",
		"langgraph_checkpoint",
		"checkpoint_blobs",
		"checkpoint_writes",
		"fence_token",
		"credential",
	} {
		if strings.Contains(strings.ToLower(sql), forbidden) {
			t.Fatalf("agent graph checkpoint migration contains forbidden legacy or secret field %q", forbidden)
		}
	}
}
