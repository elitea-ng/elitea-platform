package eliteacore_test

// SEC-11: import and fork apply the YAML expansion budget the Worker parses a
// stored pipeline under, with the shared boundary cases
// (testdata/pipeline-yaml-budget/cases.json).
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
)

func budgetCaseYAML(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "..", "testdata", "pipeline-yaml-budget", "cases.json"))
	if err != nil {
		t.Fatalf("read shared cases: %v", err)
	}
	var fixture struct {
		Cases []struct{ Name, YAML string } `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("parse shared cases: %v", err)
	}
	for _, c := range fixture.Cases {
		if c.Name == name {
			return c.YAML
		}
	}
	t.Fatalf("no shared case %q", name)
	return ""
}

func TestImportAndForkRefuseOverBudgetPipelineExpansion(t *testing.T) {
	calls := map[string]func(t *testing.T, h *eliteacore.Handler, instructions string) (int, string){
		"import": func(t *testing.T, h *eliteacore.Handler, instructions string) (int, string) {
			rec := importLinkDo(t, importLinkRouter(h), importPipelineBody("pipeline", instructions))
			return rec.Code, rec.Body.String()
		},
		"fork": func(t *testing.T, h *eliteacore.Handler, instructions string) (int, string) {
			rec := forkDo(t, forkRouter(h, true), forkPipelineBody("pipeline", instructions))
			return rec.Code, rec.Body.String()
		},
	}
	for route, call := range calls {
		for _, tc := range []struct {
			name    string
			refused bool
		}{
			{"nodes at the limit", false},
			{"nodes one past the limit", true},
			{"alias bomb", true},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				pool := newImportLinkPool(t)
				code, body := call(t, eliteacore.NewHandler(pool), budgetCaseYAML(t, tc.name))
				versions := importLinkCount(t, pool, `SELECT count(*) FROM p_1.application_versions`)
				if !tc.refused {
					if code != http.StatusCreated || versions != 1 {
						t.Fatalf("status=%d versions=%d body=%.400s", code, versions, body)
					}
					return
				}
				msgs := decodeLimitErrors(t, body).Errors.Agents
				if code != http.StatusBadRequest || len(msgs) != 1 ||
					!strings.Contains(msgs[0].Msg, "once anchors and aliases are expanded") || !strings.Contains(msgs[0].Msg, "before saving") {
					t.Fatalf("status=%d body=%.400s", code, body)
				}
				if versions != 0 || importLinkCount(t, pool, `SELECT count(*) FROM p_1.applications`) != 0 {
					t.Error("a refused entry stored rows")
				}
			})
		}
	}
}
