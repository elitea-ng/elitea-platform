package repos

import (
	"strings"
	"testing"

	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

func TestAgentSessionMigrationCreatesScopedBoundedADKLineage(t *testing.T) {
	raw, err := platformmigrations.Files.ReadFile("agentstate/0002_agent_sessions.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sql := string(raw)
	for _, fragment := range []string{
		"CREATE TABLE elitea_runtime.agent_session_writers",
		"CREATE TABLE elitea_runtime.agent_session_app_states",
		"CREATE TABLE elitea_runtime.agent_session_user_states",
		"CREATE TABLE elitea_runtime.agent_sessions",
		"CREATE TABLE elitea_runtime.agent_session_events",
		"session_family = 'adk-session.2.0.0.v1'",
		"octet_length(definition_digest) = 32",
		"event_ordinal BIGINT NOT NULL",
		"next_event_ordinal BIGINT NOT NULL DEFAULT 1",
		"event_payload IS JSON OBJECT WITH UNIQUE KEYS",
		"payload_bytes BIGINT GENERATED ALWAYS AS",
		"event_ordinal DESC",
		"writer_claim_attempt BIGINT NOT NULL",
		"writer_lease_epoch BIGINT NOT NULL",
		"CONSTRAINT agent_session_app_state_scope CHECK",
		"CONSTRAINT agent_session_user_state_scope CHECK",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("agent session migration is missing %q", fragment)
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
			t.Fatalf("agent session migration contains forbidden legacy or secret field %q", forbidden)
		}
	}
}
