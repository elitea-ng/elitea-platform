package eliteacore_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Issue 1968: an old export may carry a description over 2000 characters or a
// name over 128. The import cuts it (the tenant CHECK constraints would
// otherwise answer SQLSTATE 23514) and still imports the entity.
func TestImportCutsTextOverTheEntityLimits(t *testing.T) {
	pool := newImportCorpusPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	provisionTenantProject(t, pool, importProjectID)

	body := importToolkits(t, pool, []any{
		map[string]any{
			"entity": "toolkits", "name": strings.Repeat("я", 200), "type": "github",
			"description": strings.Repeat("字", 3000), "import_uuid": "tk-long",
			"settings": map[string]any{"url": "https://example.invalid/repo"},
		},
		map[string]any{
			"entity": "skills", "import_uuid": "sk-long", "name": strings.Repeat("s", 200),
			"description": strings.Repeat("d", 2500),
			"versions":    []any{map[string]any{"name": "base", "instructions": "read this"}},
		},
	})

	schema := tenantSchema(importProjectID)
	for table, wantDescription := range map[string]int{"elitea_tools": 2000, "skills": 2000} {
		var nameLen, descLen int
		if err := pool.QueryRow(ctx, fmt.Sprintf(
			`SELECT char_length(name), char_length(description) FROM %s.%s`, schema, table),
		).Scan(&nameLen, &descLen); err != nil {
			t.Fatalf("%s: the long entity was not imported: %v — response %v", table, err, body)
		}
		if nameLen != 128 || descLen != wantDescription {
			t.Errorf("%s: name %d / description %d characters, want 128 / %d", table, nameLen, descLen, wantDescription)
		}
	}
}
