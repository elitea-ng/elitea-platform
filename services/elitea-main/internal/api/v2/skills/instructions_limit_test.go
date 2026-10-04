package skills_test

// #6744 part 1, REST side. The MCP skill tools refuse instructions longer than
// SkillInstructionsMaxLength. The REST writes must refuse them too, or a skill
// saved here cannot be read back or edited through MCP.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	handler "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
)

func TestSkillRESTWritesEnforceTheInstructionsLimit(t *testing.T) {
	// A multi-byte rune proves the limit counts characters, not bytes.
	atLimit := strings.Repeat("é", handler.SkillInstructionsMaxLength)
	overLimit := atLimit + "x"
	importMD := func(instructions string) string {
		return "---\nname: Long Skill\ndescription: Long\n---\n" + instructions
	}

	for _, test := range []struct {
		name, method, path string
		body               func(instructions string) []byte
	}{
		{"create flat", http.MethodPost, "/api/v2/projects/proj-1/skills/", func(i string) []byte {
			b, _ := json.Marshal(map[string]any{"name": "Long Skill", "instructions": i})
			return b
		}},
		{"create versions shape", http.MethodPost, "/api/v2/projects/proj-1/skills/", func(i string) []byte {
			b, _ := json.Marshal(map[string]any{"name": "Long Skill", "versions": []map[string]any{{"name": "base", "instructions": i}}})
			return b
		}},
		{"create version", http.MethodPost, "/api/v2/projects/proj-1/skills/skill-1/versions", func(i string) []byte {
			b, _ := json.Marshal(map[string]any{"name": "v2", "instructions": i})
			return b
		}},
		{"update", http.MethodPut, "/api/v2/projects/proj-1/skills/skill-1", func(i string) []byte {
			b, _ := json.Marshal(map[string]any{"name": "Long Skill", "instructions": i})
			return b
		}},
		{"import", http.MethodPost, "/api/v2/projects/proj-1/skills/import", func(i string) []byte {
			b, _ := json.Marshal(map[string]any{"content": importMD(i), "filename": "long.md"})
			return b
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			do := func(instructions string) *httptest.ResponseRecorder {
				router := setupSkillsRouter(&mockSkillRepo{})
				req := httptest.NewRequest(test.method, test.path, bytes.NewReader(test.body(instructions)))
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				return rec
			}
			over := do(overLimit)
			if over.Code != http.StatusBadRequest || !strings.Contains(over.Body.String(), "at most 50000 characters") {
				t.Fatalf("over the limit = %d %s, want 400", over.Code, over.Body.String())
			}
			if at := do(atLimit); at.Code == http.StatusBadRequest {
				t.Fatalf("at the limit = 400 %s, want it accepted", at.Body.String())
			}
		})
	}
}
