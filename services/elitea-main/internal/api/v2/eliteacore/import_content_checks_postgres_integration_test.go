package eliteacore_test

// Real-PostgreSQL coverage for the content checks of the import and the fork
// (ADR-0043, #1500). Both paths stored meta.citations and the meta output
// contract as is. The worker refused such a version, so every chat turn of
// the agent failed. Now both paths refuse the agent and write nothing.
//
// The pool and the helpers come from
// import_tool_link_postgres_integration_test.go and
// fork_postgres_integration_test.go.

import (
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
)

func contentCheckVersion(meta map[string]any) map[string]any {
	return map[string]any{
		"name": "base", "agent_type": "openai",
		"instructions": "do the thing",
		"llm_settings": map[string]any{"model_name": "gpt-4o"},
		"meta":         meta,
	}
}

func TestImportRefusesAnInvalidCitationsSetting(t *testing.T) {
	pool := newImportLinkPool(t)
	router := importLinkRouter(eliteacore.NewHandler(pool))
	for name, meta := range map[string]map[string]any{
		"citations":       {"citations": "always"},
		"output contract": {"output_contract": map[string]any{"type": "format", "format": "xml"}},
	} {
		answer := decodeImportLink(t, importLinkDo(t, router, []any{map[string]any{
			"entity": "agents", "name": "bad " + name, "import_uuid": "bad-" + name,
			"versions": []any{contentCheckVersion(meta)},
		}}))
		if len(answer.Errors.Agents) != 1 || !strings.Contains(answer.Errors.Agents[0].Msg, "Import function has been failed") {
			t.Errorf("%s: errors.agents = %+v, want one refusal", name, answer.Errors.Agents)
		}
	}
	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.applications`); count != 0 {
		t.Errorf("applications rows = %d, want 0: a refused agent must write nothing", count)
	}
}

func TestForkRefusesAnInvalidCitationsSetting(t *testing.T) {
	pool := newImportLinkPool(t)
	router := forkRouter(eliteacore.NewHandler(pool), true)
	body := map[string]any{"applications": []any{map[string]any{
		"id": "77", "owner_id": "9", "name": "forked agent",
		"versions": []any{contentCheckVersion(map[string]any{"citations": true})},
	}}}
	answer := decodeImportLink(t, forkDo(t, router, body))
	if len(answer.Errors.Agents) != 1 || !strings.Contains(answer.Errors.Agents[0].Msg, "meta.citations") {
		t.Fatalf("errors.agents = %+v, want the citations refusal", answer.Errors.Agents)
	}
	if count := importLinkCount(t, pool, `SELECT count(*) FROM p_1.applications`); count != 0 {
		t.Errorf("applications rows = %d, want 0", count)
	}
}
