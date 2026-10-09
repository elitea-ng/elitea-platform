package eliteacore_test

// The save-time pipeline bound (internal/domain/pipelinelimits) on the two
// eliteacore routes that write new version instructions from a request body:
// import and fork. Publish, embed and mirror copy instructions that are already
// stored, so they are not bounded here.
//
// Requires a PostgreSQL service (ELITEA_TEST_DATABASE_URL).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
)

const pipelineLimitMarker = "LIMIT-MARKER-DO-NOT-ECHO"

func limitPipelineYAML(nodes, size int) string {
	var b strings.Builder
	b.WriteString("entry_point: 1\nnodes:\n")
	for i := 1; i <= nodes; i++ {
		fmt.Fprintf(&b, "  - id: %d\n    type: state_modifier\n    transition: %d\n", i, i+1)
	}
	if size > 0 {
		b.WriteString("# " + pipelineLimitMarker + " ")
		if pad := size - b.Len(); pad > 0 {
			b.WriteString(strings.Repeat("x", pad))
		}
	}
	return b.String()
}

func importPipelineBody(agentType, instructions string) []map[string]any {
	return []map[string]any{{
		"entity": "agents", "import_uuid": "ag-limit", "name": "limit agent", "description": "d",
		"versions": []map[string]any{{
			"name": "latest", "agent_type": agentType, "instructions": instructions,
			"import_version_uuid": "ver-limit",
		}},
	}}
}

func forkPipelineBody(agentType, instructions string) map[string]any {
	return map[string]any{"applications": []any{map[string]any{
		"id": "77", "owner_id": "9", "name": "limit fork", "description": "d",
		"versions": []any{map[string]any{"name": "latest", "agent_type": agentType, "instructions": instructions}},
	}}}
}

type limitErrors struct {
	Errors struct {
		Agents []struct {
			Msg string `json:"msg"`
		} `json:"agents"`
	} `json:"errors"`
}

func decodeLimitErrors(t *testing.T, body string) limitErrors {
	t.Helper()
	var decoded limitErrors
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return decoded
}

func TestImportAndForkRefuseOverBoundPipelineInstructions(t *testing.T) {
	over := limitPipelineYAML(1, pipelinelimits.MaxInstructionsBytes+1)
	atBytes := limitPipelineYAML(1, pipelinelimits.MaxInstructionsBytes)
	tooMany := limitPipelineYAML(pipelinelimits.MaxNodes+1, 0)
	atNodes := limitPipelineYAML(pipelinelimits.MaxNodes, 0)

	type route struct {
		name string
		call func(t *testing.T, h *eliteacore.Handler, agentType, instructions string) (int, string)
	}
	routes := []route{
		{"import", func(t *testing.T, h *eliteacore.Handler, agentType, instructions string) (int, string) {
			rec := importLinkDo(t, importLinkRouter(h), importPipelineBody(agentType, instructions))
			return rec.Code, rec.Body.String()
		}},
		{"fork", func(t *testing.T, h *eliteacore.Handler, agentType, instructions string) (int, string) {
			rec := forkDo(t, forkRouter(h, true), forkPipelineBody(agentType, instructions))
			return rec.Code, rec.Body.String()
		}},
	}
	for _, r := range routes {
		for name, tc := range map[string]struct {
			agentType, instructions, text string
			refused                       bool
		}{
			"bytes+1":            {"pipeline", over, "512 KiB", true},
			"nodes+1":            {"pipeline", tooMany, "128 nodes", true},
			"bytes at limit":     {"pipeline", atBytes, "", false},
			"nodes at limit":     {"pipeline", atNodes, "", false},
			"agent not bounded":  {"openai", over, "", false},
			"unparseable accept": {"pipeline", "nodes: [unterminated\n - : :", "", false},
		} {
			t.Run(r.name+"/"+name, func(t *testing.T) {
				pool := newImportLinkPool(t)
				code, body := r.call(t, eliteacore.NewHandler(pool), tc.agentType, tc.instructions)
				versions := importLinkCount(t, pool, `SELECT count(*) FROM p_1.application_versions`)
				if tc.refused {
					msgs := decodeLimitErrors(t, body).Errors.Agents
					if code != http.StatusBadRequest || len(msgs) != 1 ||
						!strings.Contains(msgs[0].Msg, tc.text) || !strings.Contains(msgs[0].Msg, "before saving") {
						t.Fatalf("status=%d body=%.400s", code, body)
					}
					if strings.Contains(body, pipelineLimitMarker) {
						t.Error("the refusal echoes pipeline content")
					}
					if applications := importLinkCount(t, pool, `SELECT count(*) FROM p_1.applications`); versions != 0 || applications != 0 {
						t.Errorf("a refused entry left %d versions and %d applications", versions, applications)
					}
					return
				}
				if code != http.StatusCreated || versions != 1 {
					t.Fatalf("status=%d versions=%d body=%.400s", code, versions, body)
				}
			})
		}
	}
}
