package repos

import (
	"context"
	"testing"
	"time"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCurrentModelsRepositoryPostgresParity(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	prepareCurrentConfigurationsProjectTwo(t, pool)
	seedCurrentModelConfigurations(t, pool)

	repository, err := NewCurrentModelsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	projectItems, err := repository.List(ctx, 2, configurationapp.CurrentModelSectionLLM, false)
	if err != nil {
		t.Fatalf("list project LLM candidates: %v", err)
	}
	if len(projectItems) != 3 || projectItems[0].Name != "alpha" || projectItems[0].MaxOutputTokens == nil ||
		*projectItems[0].MaxOutputTokens != 16000 || projectItems[1].Name != "beta" ||
		projectItems[2].Name != "alpha" || projectItems[2].MaxOutputTokens == nil || *projectItems[2].MaxOutputTokens != 32000 {
		t.Fatalf("project candidates=%#v", projectItems)
	}
	if projectItems[2].HighTier == nil || !*projectItems[2].HighTier {
		t.Fatalf("mid-tier fallback was not mapped: %#v", projectItems[2])
	}

	publicItems, err := repository.List(ctx, 1, configurationapp.CurrentModelSectionLLM, true)
	if err != nil {
		t.Fatalf("list public shared LLM candidates: %v", err)
	}
	if len(publicItems) != 1 || publicItems[0].Name != "public-shared" || publicItems[0].ProjectID != 1 || !publicItems[0].Shared {
		t.Fatalf("public shared candidates=%#v", publicItems)
	}

	response := configurationapp.BuildCurrentModelCatalog(configurationapp.CurrentModelCatalogRequest{
		Section:           configurationapp.CurrentModelSectionLLM,
		ProjectID:         2,
		PublicProjectID:   1,
		IncludeShared:     true,
		ProjectItems:      projectItems,
		PublicSharedItems: publicItems,
	})
	if response.Total != 3 || response.DefaultModelName == nil || *response.DefaultModelName != "alpha" ||
		response.DefaultModelProjectID == nil || *response.DefaultModelProjectID != 2 {
		t.Fatalf("combined catalog=%#v", response)
	}
	for _, item := range response.Items {
		if item.Name == "public-private" || item.Name == "disabled" || item.Name == "not-an-llm" {
			t.Fatalf("status, section, or public-shared filter leaked row: %#v", response.Items)
		}
	}

	if _, err := pool.Exec(ctx, `
INSERT INTO p_2.configuration (
    id, uuid, project_id, label, elitea_title, type, section, data, meta,
    shared, status_ok, source
, created_at) VALUES (
    6, '00000000-0000-0000-0000-000000000206', 2, 'Malformed',
    'project_two_llm_malformed', 'llm_model', 'llm',
    '{"name":"malformed","max_output_tokens":"private-secret"}'::jsonb,
    '{}'::jsonb, false, true, 'test'
, now() AT TIME ZONE 'UTC')`); err != nil {
		t.Fatalf("seed malformed LLM configuration: %v", err)
	}
	// The malformed row is SKIPPED, and the catalogue keeps every good row.
	//
	// THE DEFECT this pins: List aborted on the first row it could not map.
	// One wrongly typed `data` field therefore made
	// GET /api/v2/configurations/models/{projectID} answer 500 for the whole
	// project. The compatibility write path stores `data` with no
	// registry-schema check. The model picker was empty for every member until someone
	// repaired the row.
	afterMalformed, err := repository.List(ctx, 2, configurationapp.CurrentModelSectionLLM, false)
	if err != nil {
		t.Fatalf("one malformed row removed the whole catalogue: %v", err)
	}
	if len(afterMalformed) != len(projectItems) {
		t.Fatalf("candidates after the malformed row=%#v, want the same %d as before",
			afterMalformed, len(projectItems))
	}
	for _, item := range afterMalformed {
		if item.Name == "malformed" {
			t.Fatalf("the malformed row reached the catalogue: %#v", afterMalformed)
		}
	}
}

func seedCurrentModelConfigurations(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
INSERT INTO p_2.configuration (
    id, uuid, project_id, label, elitea_title, type, section, data, meta,
    shared, status_ok, source
, created_at) VALUES
    (1, '00000000-0000-0000-0000-000000000201', 2, 'Alpha Max',
     'project_two_llm_alpha_max', 'llm_model', 'llm',
     '{"name":"alpha","context_window":200000,"max_output_tokens":32000,"supports_reasoning":true,"mid_tier":true}'::jsonb,
     '{}'::jsonb, false, true, 'test', now() AT TIME ZONE 'UTC'),
    (2, '00000000-0000-0000-0000-000000000202', 2, 'Beta',
     'project_two_llm_beta', 'llm_model', 'llm',
     '{"name":"beta","max_output_tokens":8000}'::jsonb,
     '{}'::jsonb, false, true, 'test', now() AT TIME ZONE 'UTC'),
    (3, '00000000-0000-0000-0000-000000000203', 2, 'Alpha Newer But Smaller',
     'project_two_llm_alpha_smaller', 'llm_model', 'llm',
     '{"name":"alpha","max_output_tokens":16000,"high_tier":false,"mid_tier":true}'::jsonb,
     '{}'::jsonb, false, true, 'test', now() AT TIME ZONE 'UTC'),
    (4, '00000000-0000-0000-0000-000000000204', 2, 'Not an LLM',
     'project_two_embedding', 'embedding_model', 'embedding',
     '{"name":"not-an-llm"}'::jsonb,
     '{}'::jsonb, false, true, 'test', now() AT TIME ZONE 'UTC'),
    (5, '00000000-0000-0000-0000-000000000205', 2, 'Disabled',
     'project_two_llm_disabled', 'llm_model', 'llm',
     '{"name":"disabled","max_output_tokens":64000}'::jsonb,
     '{}'::jsonb, false, false, 'test', now() AT TIME ZONE 'UTC');

INSERT INTO p_1.configuration (
    id, uuid, project_id, label, elitea_title, type, section, data, meta,
    shared, status_ok, source
, created_at) VALUES
    (3, '00000000-0000-0000-0000-000000000103', 1, 'Public Shared',
     'public_llm_shared', 'llm_model', 'llm',
     '{"name":"public-shared","max_output_tokens":16000}'::jsonb,
     '{}'::jsonb, true, true, 'test', now() AT TIME ZONE 'UTC'),
    (4, '00000000-0000-0000-0000-000000000104', 1, 'Public Private',
     'public_llm_private', 'llm_model', 'llm',
     '{"name":"public-private","max_output_tokens":16000}'::jsonb,
     '{}'::jsonb, false, true, 'test', now() AT TIME ZONE 'UTC');`); err != nil {
		t.Fatalf("seed current model configurations: %v", err)
	}
}
