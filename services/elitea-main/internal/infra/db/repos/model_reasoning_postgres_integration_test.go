package repos

import (
	"context"
	"testing"
	"time"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

// The catalogue serves the reasoning control (ADR-0038 decision 3): a row's
// own keys win, a reasoning row takes the family default of shared 0225, and
// a row that does not reason takes none.
func TestCurrentModelsRepositoryResolvesTheReasoningControl(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	prepareCurrentConfigurationsProjectTwo(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
INSERT INTO p_2.configuration (
    id, uuid, project_id, label, elitea_title, type, section, data, meta,
    shared, status_ok, source, created_at) VALUES
    (11, '00000000-0000-0000-0000-000000000211', 2, 'Qwen', 'p2_qwen', 'llm_model', 'llm',
     '{"name":"Qwen/Qwen3-8B","supports_reasoning":true}'::jsonb, '{}'::jsonb, false, true, 'test', now() AT TIME ZONE 'UTC'),
    (12, '00000000-0000-0000-0000-000000000212', 2, 'Own', 'p2_own', 'llm_model', 'llm',
     '{"name":"local-reasoner","supports_reasoning":true,"reasoning_control":"fixed_on","reasoning_tool_steps":true}'::jsonb,
     '{}'::jsonb, false, true, 'test', now() AT TIME ZONE 'UTC'),
    (13, '00000000-0000-0000-0000-000000000213', 2, 'Plain', 'p2_plain', 'llm_model', 'llm',
     '{"name":"Qwen/Qwen2.5-7B"}'::jsonb, '{}'::jsonb, false, true, 'test', now() AT TIME ZONE 'UTC'),
    (14, '00000000-0000-0000-0000-000000000214', 2, 'Bad', 'p2_bad', 'llm_model', 'llm',
     '{"name":"claude-x","supports_reasoning":true,"reasoning_control":"adaptive"}'::jsonb,
     '{}'::jsonb, false, true, 'test', now() AT TIME ZONE 'UTC')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	repository, err := NewCurrentModelsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	items, err := repository.List(ctx, 2, configurationapp.CurrentModelSectionLLM, false)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]configurationapp.CurrentModelCatalogItem{}
	for _, item := range items {
		byName[item.Name] = item
	}
	control := func(name string) (string, bool) {
		item, found := byName[name]
		if !found {
			t.Fatalf("%s is not in the catalogue: %#v", name, items)
		}
		if item.ReasoningControl == nil {
			return "", false
		}
		return *item.ReasoningControl, item.ReasoningToolSteps != nil && *item.ReasoningToolSteps
	}
	if got, steps := control("Qwen/Qwen3-8B"); got != "toggle" || steps {
		t.Errorf("the Qwen family default: got %q steps=%v", got, steps)
	}
	if got, steps := control("local-reasoner"); got != "fixed_on" || !steps {
		t.Errorf("the row's own keys: got %q steps=%v", got, steps)
	}
	if got, _ := control("Qwen/Qwen2.5-7B"); got != "" {
		t.Errorf("a row that does not reason took %q", got)
	}
	// A malformed stored key keeps the row and takes the family default.
	if got, _ := control("claude-x"); got != "budget" {
		t.Errorf("a malformed own key: got %q, want the family default", got)
	}
}
