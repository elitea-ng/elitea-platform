package repos

import (
	"context"
	"errors"
	"testing"
	"time"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
)

// TestCurrentEmbeddingBindingResolvesFromTenantPostgres proves the binding is
// resolved entirely from the tenant configuration rows — the same rows the
// Bifrost gateway reads per project — with no LLM proxy registry involved.
func TestCurrentEmbeddingBindingResolvesFromTenantPostgres(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	configurations, err := NewCurrentConfigurationsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
INSERT INTO centry.project (id, create_success, suspended)
VALUES (2, TRUE, FALSE);
CREATE SCHEMA p_2;
CREATE TABLE p_2.configuration (LIKE p_1.configuration INCLUDING ALL);
INSERT INTO p_1.configuration (
    id, uuid, project_id, label, elitea_title, type, section, data, meta,
    shared, status_ok, source
, created_at) VALUES (
    3, '00000000-0000-0000-0000-000000000107', 1, 'Embedding', 'embedding_current',
    'embedding_model', 'embedding',
    '{"name":"text-embedding-current","ai_credentials":{"elitea_title":"credential-current","private":true}}'::jsonb,
    '{}'::jsonb, true, true, 'user'
, now() AT TIME ZONE 'UTC'), (
    4, '00000000-0000-0000-0000-000000000108', 1, 'Private embedding', 'embedding_private',
    'embedding_model', 'embedding',
    '{"name":"private-embedding","ai_credentials":{"elitea_title":"credential-private","private":true}}'::jsonb,
    '{}'::jsonb, false, true, 'user'
, now() AT TIME ZONE 'UTC')`); err != nil {
		t.Fatal(err)
	}

	resolver, err := indexingapp.NewCurrentEmbeddingBindingResolver(configurations, 1)
	if err != nil {
		t.Fatal(err)
	}

	// The caller's project p_2 holds no such row, so the shared public-project
	// row backs the binding — and the wire name stays unprefixed, because the
	// gateway resolves the project from the edge identity.
	binding, err := resolver.Resolve(ctx, 2, "text-embedding-current", nil)
	if err != nil {
		t.Fatal(err)
	}
	if binding.ModelName != "text-embedding-current" ||
		binding.ResolvedModelGroup != "text-embedding-current" ||
		binding.Route != "raw" ||
		binding.ConfigurationProjectID != 1 ||
		binding.ConfigurationUUID != "00000000-0000-0000-0000-000000000107" ||
		binding.ConfigurationDigest.IsZero() {
		t.Fatalf("binding=%#v", binding)
	}
	preferredPublicProject := int32(1)
	if _, err := resolver.Resolve(ctx, 2, "private-embedding", &preferredPublicProject); !errors.Is(
		err,
		indexingapp.ErrCurrentEmbeddingBindingUnavailable,
	) {
		t.Fatalf("private public-project binding escaped tenant scope: %v", err)
	}

	// A project-local row of the same name must win over the shared one.
	if _, err := pool.Exec(ctx, `
INSERT INTO p_2.configuration (
    id, uuid, project_id, label, elitea_title, type, section, data, meta,
    shared, status_ok, source
, created_at) VALUES (
    5, '00000000-0000-0000-0000-000000000207', 2, 'Embedding', 'embedding_current',
    'embedding_model', 'embedding',
    '{"name":"text-embedding-current","ai_credentials":{"elitea_title":"credential-local","private":true}}'::jsonb,
    '{}'::jsonb, false, true, 'user'
, now() AT TIME ZONE 'UTC')`); err != nil {
		t.Fatal(err)
	}
	local, err := resolver.Resolve(ctx, 2, "text-embedding-current", nil)
	if err != nil {
		t.Fatal(err)
	}
	if local.ConfigurationProjectID != 2 ||
		local.ConfigurationUUID != "00000000-0000-0000-0000-000000000207" ||
		local.ConfigurationDigest == binding.ConfigurationDigest {
		t.Fatalf("project-local row lost to the shared duplicate: %#v", local)
	}

	// Two mutable rows sharing one model name stay an ambiguous catalog, not a
	// silent pick of the first: the LIMIT 2 sentinel must reach the caller.
	if _, err := pool.Exec(ctx, `
INSERT INTO p_2.configuration (
    id, uuid, project_id, label, elitea_title, type, section, data, meta,
    shared, status_ok, source
, created_at) VALUES (
    6, '00000000-0000-0000-0000-000000000208', 2, 'Embedding copy', 'embedding_current_copy',
    'embedding_model', 'embedding',
    '{"name":"text-embedding-current","ai_credentials":{"elitea_title":"credential-local","private":true}}'::jsonb,
    '{}'::jsonb, false, true, 'user'
, now() AT TIME ZONE 'UTC')`); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(ctx, 2, "text-embedding-current", nil); !errors.Is(
		err,
		indexingapp.ErrCurrentEmbeddingBindingAmbiguous,
	) {
		t.Fatalf("duplicate mutable definitions error=%v", err)
	}
}

func TestPostgresServiceBackedCurrentModelCatalogBoundsBeforeJSONProjection(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
INSERT INTO centry.project (id, create_success, suspended)
VALUES (2, TRUE, FALSE);
CREATE SCHEMA p_2;
CREATE TABLE p_2.configuration (LIKE p_1.configuration INCLUDING ALL);
INSERT INTO p_2.configuration (
    uuid, project_id, label, elitea_title, type, section, data, meta,
    shared, status_ok, source
, created_at)
SELECT gen_random_uuid(),
       2,
       'Embedding ' || value,
       'embedding_' || value,
       'embedding_model',
       'embedding',
       jsonb_build_object(
           'name', 'embedding-' || value,
           'padding', repeat('x', 220 * 1024)
       ),
       '{}'::jsonb,
       false,
       true,
       'user', now() AT TIME ZONE 'UTC'
FROM generate_series(1, 40) AS value`); err != nil {
		t.Fatal(err)
	}
	repository, err := NewCurrentModelsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.List(
		ctx,
		2,
		configurationapp.CurrentModelSectionEmbedding,
		false,
	); !errors.Is(err, errCurrentModelCatalogTooLarge) {
		t.Fatalf("oversized projected catalog error=%v", err)
	}

	if _, err := pool.Exec(ctx, `
TRUNCATE p_2.configuration RESTART IDENTITY;
INSERT INTO p_2.configuration (
    uuid, project_id, label, elitea_title, type, section, data, meta,
    shared, status_ok, source
, created_at)
SELECT gen_random_uuid(),
       2,
       'Embedding ' || value,
       'embedding_' || value,
       'embedding_model',
       'embedding',
       jsonb_build_object('name', 'embedding-' || value),
       '{}'::jsonb,
       false,
       true,
       'user', now() AT TIME ZONE 'UTC'
FROM generate_series(1, 10001) AS value`); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.List(
		ctx,
		2,
		configurationapp.CurrentModelSectionEmbedding,
		false,
	); !errors.Is(err, errCurrentModelCatalogTooLarge) {
		t.Fatalf("oversized row-count catalog error=%v", err)
	}

	if _, err := pool.Exec(ctx, `
TRUNCATE p_2.configuration RESTART IDENTITY;
INSERT INTO p_2.configuration (
    uuid, project_id, label, elitea_title, type, section, data, meta,
    shared, status_ok, source
, created_at) VALUES (
    gen_random_uuid(),
    2,
    'Large but admitted embedding',
    'large_but_admitted_embedding',
    'embedding_model',
    'embedding',
    jsonb_build_object(
        'name', 'large-but-admitted',
        'padding', repeat('x', 300 * 1024)
    ),
    '{}'::jsonb,
    false,
    true,
    'user'
, now() AT TIME ZONE 'UTC')`); err != nil {
		t.Fatal(err)
	}
	items, err := repository.List(
		ctx,
		2,
		configurationapp.CurrentModelSectionEmbedding,
		false,
	)
	if err != nil || len(items) != 1 || items[0].Name != "large-but-admitted" {
		t.Fatalf("parity-preserved large model item=%#v err=%v", items, err)
	}
}
