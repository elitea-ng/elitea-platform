package repos

import (
	"strings"
	"testing"

	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

func TestHTTPChildShapeMigrationRequiresEveryPopulatedScopeField(t *testing.T) {
	raw, err := platformmigrations.Files.ReadFile("shared/0141_execution_saved_child_scopes.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	start := strings.Index(sql, "ADD CONSTRAINT execution_http_effects_saved_child_shape CHECK(")
	end := strings.Index(sql, "ADD CONSTRAINT execution_http_effects_saved_child_owner FOREIGN KEY")
	if start < 0 || end <= start {
		t.Fatal("owning shape and FK missing")
	}
	check := sql[start:end]
	parts := strings.Split(check, "OR(")
	if len(parts) != 2 {
		t.Fatal("root and child shape changed")
	}
	for _, field := range []string{"saved_child_scope_id", "saved_child_scope_revision", "saved_child_scope_digest", "saved_child_graph_thread"} {
		if !strings.Contains(parts[0], field+" IS NULL") || !strings.Contains(parts[1], field+" IS NOT NULL") {
			t.Fatalf("nullable %s can evade CHECK/FK", field)
		}
	}
	if !strings.Contains(parts[1], "saved_child_scope_revision=1") || !strings.Contains(parts[1], "octet_length(saved_child_graph_thread) BETWEEN 1 AND 1024") {
		t.Fatal("original populated constraints weakened")
	}
}
