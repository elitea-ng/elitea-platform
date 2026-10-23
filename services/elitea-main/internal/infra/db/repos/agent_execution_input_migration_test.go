package repos

import (
	"strings"
	"testing"

	platformmigrations "github.com/elitea-ng/elitea-platform/services/elitea-main/migrations"
)

func TestAgentExecutionInputMigrationPreservesConfigurationBound(t *testing.T) {
	raw, err := platformmigrations.Files.ReadFile("shared/0054_agent_execution_input_bound.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	sql := string(raw)
	for _, fragment := range []string{
		"media_type = 'application/json'",
		"content_size BETWEEN 1 AND 262144",
		"media_type = 'application/vnd.elitea.agent-execution-input.v1+protobuf'",
		"content_size BETWEEN 1 AND 1048576",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("migration is missing %q", fragment)
		}
	}
}
