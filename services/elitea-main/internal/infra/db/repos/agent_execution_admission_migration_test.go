package repos

import (
	"strings"
	"testing"

	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

func TestAgentExecutionAdmissionMigrationExtendsRuntimeWithoutDuplicatingChatState(
	t *testing.T,
) {
	raw, err := platformmigrations.Files.ReadFile(
		"shared/0055_agent_execution_admission.sql",
	)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sql := string(raw)
	for _, fragment := range []string{
		"CREATE TABLE elitea_runtime.agent_execution_jobs",
		"'agent.execute.application.v1'",
		"'agent.execute.adhoc.v1'",
		"REFERENCES elitea_runtime.execution_jobs",
		"REFERENCES elitea_runtime.input_bundle_entries",
		"request_entry_id TEXT NOT NULL",
		"client_stream_id TEXT NOT NULL",
		"client_message_id TEXT NOT NULL",
		"client_execution_generation TEXT NOT NULL",
		"sio_event IN ('chat_predict', 'chat_continue_predict')",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("agent admission migration is missing %q", fragment)
		}
	}
	for _, forbidden := range []string{
		"chat_message_trace_step",
		"chat_message_group",
		"thinking_steps",
		"tool_calls",
		"checkpoint",
		"credential",
	} {
		if strings.Contains(strings.ToLower(sql), forbidden) {
			t.Fatalf("agent admission migration duplicates current state %q", forbidden)
		}
	}
}
