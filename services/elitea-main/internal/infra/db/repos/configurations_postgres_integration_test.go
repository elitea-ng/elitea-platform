package repos

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCurrentEmbeddingConfigurationPostgresResolutionIsTenantRoutedAndAmbiguitySafe(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	repository, err := NewCurrentConfigurationsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.configuration (
    id, uuid, project_id, label, elitea_title, type, section, data, meta,
    shared, status_ok, source
) VALUES (
    2, '00000000-0000-0000-0000-000000000111', 1, 'Embedding', 'embedding_one',
    'embedding_model', 'embedding',
    '{"name":"text-embedding-3-small","ai_credentials":{"elitea_title":"credential-current","private":true}}'::jsonb,
    '{}'::jsonb, true, true, 'user'
)`); err != nil {
		t.Fatal(err)
	}

	configuration, found, err := repository.FindCurrentEmbeddingConfiguration(
		ctx, 1, "text-embedding-3-small", true,
	)
	if err != nil || !found {
		t.Fatalf("configuration=%#v found=%t err=%v", configuration, found, err)
	}
	if configuration.UUID != "00000000-0000-0000-0000-000000000111" ||
		!configuration.Shared ||
		string(configuration.Data) == "" {
		t.Fatalf("configuration=%#v", configuration)
	}

	if _, err := pool.Exec(ctx, `
INSERT INTO p_1.configuration (
    id, uuid, project_id, label, elitea_title, type, section, data, meta,
    shared, status_ok, source
) VALUES (
    3, '00000000-0000-0000-0000-000000000112', 1, 'Embedding duplicate', 'embedding_two',
    'embedding_model', 'embedding',
    '{"name":"text-embedding-3-small","ai_credentials":{"elitea_title":"credential-current","private":true}}'::jsonb,
    '{}'::jsonb, true, true, 'user'
)`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.FindCurrentEmbeddingConfiguration(
		ctx, 1, "text-embedding-3-small", true,
	); !errors.Is(err, indexingapp.ErrCurrentEmbeddingBindingAmbiguous) {
		t.Fatalf("duplicate error=%v", err)
	}
}

func TestCurrentConfigurationsRepositoryPostgresParity(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	prepareCurrentConfigurationsProjectTwo(t, pool)

	repository, err := NewCurrentConfigurationsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	label := "project-two-github"
	statusLogs := "validated"
	authorID := int32(42)
	created, err := repository.Create(ctx, configurationapp.CurrentConfigurationCreate{
		UUID:        "00000000-0000-0000-0000-000000000201",
		ProjectID:   2,
		Label:       &label,
		EliteaTitle: "project_two_github",
		Type:        "github",
		Section:     "credentials",
		Data: map[string]any{
			"token":  "{{secret.project_two_github_token}}",
			"nested": map[string]any{"owner": "team-two"},
		},
		Meta:       map[string]any{"scope": "project", "tags": []any{"rag", "source"}},
		Shared:     true,
		StatusOK:   true,
		StatusLogs: &statusLogs,
		Source:     "user",
		AuthorID:   &authorID,
	})
	if err != nil {
		t.Fatalf("create project configuration: %v", err)
	}
	if created.ID != 1 || created.UUID != "00000000-0000-0000-0000-000000000201" || created.ProjectID != 2 ||
		created.Label == nil || *created.Label != label || created.EliteaTitle != "project_two_github" ||
		created.Type != "github" || created.Section != "credentials" || !created.Shared || !created.StatusOK ||
		created.StatusLogs == nil || *created.StatusLogs != statusLogs || created.Source != "user" ||
		created.AuthorID == nil || *created.AuthorID != authorID || created.CreatedAt.IsZero() || created.UpdatedAt != nil {
		t.Fatalf("created row did not preserve all scalar columns: %#v", created)
	}
	if !reflect.DeepEqual(created.Data, map[string]any{
		"token":  "{{secret.project_two_github_token}}",
		"nested": map[string]any{"owner": "team-two"},
	}) {
		t.Fatalf("created data=%#v", created.Data)
	}
	if !reflect.DeepEqual(created.Meta, map[string]any{"scope": "project", "tags": []any{"rag", "source"}}) {
		t.Fatalf("created metadata=%#v", created.Meta)
	}

	projectOne, err := repository.Get(ctx, 1, created.ID)
	if err != nil {
		t.Fatalf("read same row ID from project one: %v", err)
	}
	projectTwo, err := repository.Get(ctx, 2, created.ID)
	if err != nil {
		t.Fatalf("read same row ID from project two: %v", err)
	}
	if projectOne.ProjectID != 1 || projectOne.EliteaTitle != "integration_fixture" ||
		projectTwo.ProjectID != 2 || projectTwo.EliteaTitle != "project_two_github" {
		t.Fatalf("tenant isolation failed: project one=%#v project two=%#v", projectOne, projectTwo)
	}

	companionLabel := "project-two-confluence"
	companion, err := repository.Create(ctx, configurationapp.CurrentConfigurationCreate{
		UUID:        "00000000-0000-0000-0000-000000000202",
		ProjectID:   2,
		Label:       &companionLabel,
		EliteaTitle: "project_two_confluence",
		Type:        "confluence",
		Section:     "credentials",
		Data:        map[string]any{"url": "https://confluence.invalid"},
		Meta:        map[string]any{},
		Shared:      false,
		StatusOK:    false,
		Source:      "user",
	})
	if err != nil {
		t.Fatalf("create companion configuration: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO centry.social_pins (
    entity, user_id, project_id, entity_id, updated_at
) VALUES ('configuration', 77, 2, $1, clock_timestamp())`, companion.ID); err != nil {
		t.Fatalf("pin companion configuration: %v", err)
	}
	pinnedCompanion, err := repository.Get(ctx, 2, companion.ID)
	if err != nil || !pinnedCompanion.IsPinned {
		t.Fatalf("pinned detail=%#v err=%v", pinnedCompanion, err)
	}
	sameIDInPublicProject, err := repository.Get(ctx, 1, companion.ID)
	if err != nil || sameIDInPublicProject.IsPinned {
		t.Fatalf("project-two pin crossed tenant boundary: detail=%#v err=%v", sameIDInPublicProject, err)
	}

	nilFilter := configurationapp.CurrentConfigurationListFilter{
		ProjectID: 2,
		Offset:    0,
		Limit:     20,
		SortBy:    "id",
		SortOrder: "asc",
	}
	total, err := repository.Count(ctx, nilFilter)
	if err != nil {
		t.Fatalf("count with nil filters: %v", err)
	}
	items, err := repository.List(ctx, nilFilter)
	if err != nil {
		t.Fatalf("list with nil filters: %v", err)
	}
	if total != 2 || len(items) != 2 || items[0].EliteaTitle != "project_two_confluence" ||
		items[1].EliteaTitle != "project_two_github" || !items[0].IsPinned || items[1].IsPinned ||
		items[0].Shared || !items[1].Shared {
		t.Fatalf("current project page total=%d items=%#v", total, items)
	}

	// ID filters apply before count and pagination, inside the selected tenant.
	for _, sharedOnly := range []bool{false, true} {
		filter := nilFilter
		filter.IDs = []int32{projectTwo.ID}
		filter.SharedOnly = sharedOnly
		filter.Limit = 1
		total, err := repository.Count(ctx, filter)
		if err != nil || total != 1 {
			t.Fatalf("filtered count: %d %v", total, err)
		}
		selected, err := repository.List(ctx, filter)
		if err != nil || len(selected) != 1 || selected[0].ID != projectTwo.ID || selected[0].ProjectID != 2 {
			t.Fatalf("filtered rows: %#v %v", selected, err)
		}
		filter.Offset = 1
		selected, err = repository.List(ctx, filter)
		if err != nil || len(selected) != 0 {
			t.Fatalf("filtered offset: %#v %v", selected, err)
		}
		filter.IDs = []int32{2147483647}
		total, err = repository.Count(ctx, filter)
		if err != nil || total != 0 {
			t.Fatalf("unknown IDs: %d %v", total, err)
		}
	}
	pinnedPage, err := repository.List(ctx, configurationapp.CurrentConfigurationListFilter{
		ProjectID: 2,
		Offset:    0,
		Limit:     1,
		SortBy:    "id",
		SortOrder: "asc",
	})
	if err != nil || len(pinnedPage) != 1 || pinnedPage[0].ID != companion.ID || !pinnedPage[0].IsPinned {
		t.Fatalf("pinned-first page=%#v err=%v", pinnedPage, err)
	}

	publicSharedFilter := configurationapp.CurrentConfigurationListFilter{
		ProjectID:  1,
		Offset:     0,
		Limit:      20,
		SortBy:     "id",
		SortOrder:  "asc",
		SharedOnly: true,
	}
	sharedTotal, err := repository.Count(ctx, publicSharedFilter)
	if err != nil {
		t.Fatalf("count public shared configurations: %v", err)
	}
	sharedItems, err := repository.List(ctx, publicSharedFilter)
	if err != nil {
		t.Fatalf("list public shared configurations: %v", err)
	}
	if sharedTotal != 1 || len(sharedItems) != 1 || sharedItems[0].ProjectID != 1 ||
		sharedItems[0].EliteaTitle != "public_shared_github" || !sharedItems[0].Shared {
		t.Fatalf("public shared page total=%d items=%#v", sharedTotal, sharedItems)
	}

	duplicateTitle := "duplicate_uuid"
	_, err = repository.Create(ctx, configurationapp.CurrentConfigurationCreate{
		UUID:        created.UUID,
		ProjectID:   2,
		EliteaTitle: duplicateTitle,
		Type:        "github",
		Section:     "credentials",
		Data:        map[string]any{},
		Meta:        map[string]any{},
		Source:      "user",
	})
	if !errors.Is(err, configurationapp.ErrCurrentConfigurationConflict) || err.Error() != configurationapp.ErrCurrentConfigurationConflict.Error() {
		t.Fatalf("unique conflict=%v", err)
	}

	replacementLabel := "project-two-github-updated"
	replacementLogs := "recheck required"
	replaced, err := repository.Replace(ctx, configurationapp.CurrentConfigurationReplace{
		ProjectID:       2,
		ConfigurationID: created.ID,
		Label:           &replacementLabel,
		EliteaTitle:     "project_two_github_updated",
		Data:            map[string]any{"token": "{{secret.rotated_token}}"},
		Meta:            map[string]any{"scope": "project", "owner": "new-team"},
		Shared:          false,
		StatusOK:        false,
		StatusLogs:      &replacementLogs,
	})
	if err != nil {
		t.Fatalf("replace configuration: %v", err)
	}
	if replaced.ID != created.ID || replaced.UUID != created.UUID || replaced.ProjectID != 2 ||
		replaced.Label == nil || *replaced.Label != replacementLabel || replaced.EliteaTitle != "project_two_github_updated" ||
		replaced.Type != created.Type || replaced.Section != created.Section || replaced.Shared || replaced.StatusOK ||
		replaced.StatusLogs == nil || *replaced.StatusLogs != replacementLogs || replaced.Source != created.Source ||
		replaced.AuthorID == nil || *replaced.AuthorID != authorID || !replaced.CreatedAt.Equal(created.CreatedAt) ||
		replaced.UpdatedAt == nil || replaced.UpdatedAt.Before(replaced.CreatedAt) {
		t.Fatalf("replaced row did not preserve immutable columns: %#v", replaced)
	}
	if !reflect.DeepEqual(replaced.Data, map[string]any{"token": "{{secret.rotated_token}}"}) ||
		!reflect.DeepEqual(replaced.Meta, map[string]any{"scope": "project", "owner": "new-team"}) {
		t.Fatalf("replacement JSON data=%#v metadata=%#v", replaced.Data, replaced.Meta)
	}

	if err := repository.Delete(ctx, 2, created.ID); err != nil {
		t.Fatalf("delete project-two configuration: %v", err)
	}
	if _, err := repository.Get(ctx, 2, created.ID); !errors.Is(err, configurationapp.ErrCurrentConfigurationNotFound) {
		t.Fatalf("deleted project-two row error=%v", err)
	}
	projectOneAfterDelete, err := repository.Get(ctx, 1, created.ID)
	if err != nil || projectOneAfterDelete.EliteaTitle != "integration_fixture" {
		t.Fatalf("project-one row changed by project-two delete: row=%#v err=%v", projectOneAfterDelete, err)
	}
	remaining, err := repository.Count(ctx, nilFilter)
	if err != nil || remaining != 1 {
		t.Fatalf("remaining project-two rows=%d err=%v", remaining, err)
	}

	if _, err := repository.Create(ctx, configurationapp.CurrentConfigurationCreate{
		UUID:        "00000000-0000-0000-0000-000000000203",
		ProjectID:   2,
		EliteaTitle: "project_two_openai_model",
		Type:        "openai",
		Section:     "llm",
		Data:        map[string]any{},
		Meta:        map[string]any{},
		Source:      "user",
	}); err != nil {
		t.Fatalf("create cross-section configuration: %v", err)
	}
	typesFilter := configurationapp.CurrentConfigurationTypesFilter{
		ProjectID: 2,
		Section:   "credentials",
		MaxRows:   configurationapp.MaxCurrentConfigurationTypes + 1,
	}
	credentialTypes, err := repository.ListDistinctTypes(ctx, typesFilter)
	if err != nil || !reflect.DeepEqual(credentialTypes, []string{"confluence"}) {
		t.Fatalf("project-two credential types=%v err=%v", credentialTypes, err)
	}
	typesFilter.Section = ""
	allTypes, err := repository.ListDistinctTypes(ctx, typesFilter)
	if err != nil || !reflect.DeepEqual(allTypes, []string{"confluence", "openai"}) {
		t.Fatalf("project-two all types=%v err=%v", allTypes, err)
	}
	typesFilter.ProjectID = 1
	typesFilter.Section = "credentials"
	// Two rows seed project 1's credentials section: postgresIntegrationSeedSQL's
	// own base fixture ("integration_fixture", type=openapi) and this test's own
	// prepareCurrentConfigurationsProjectTwo call above ("public_shared_github",
	// type=github, project_id=1 despite the helper's name — it seeds a shared
	// row visible to project 1, not project 2). This assertion previously only
	// expected {"github"}, which never actually ran against real Postgres before
	// (the whole test errored earlier, at the now-fixed centry.project
	// create_success NOT NULL violation) — {"github"} alone was stale.
	projectOneTypes, err := repository.ListDistinctTypes(ctx, typesFilter)
	if err != nil || !reflect.DeepEqual(projectOneTypes, []string{"github", "openapi"}) {
		t.Fatalf("project-one credential types=%v err=%v", projectOneTypes, err)
	}
}

func prepareCurrentConfigurationsProjectTwo(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
-- centry.social_pins is created by newMigratedPostgresIntegrationPool:
-- shared/0064 took ownership of it, so declaring it again here is a duplicate
-- relation. The transcribed shape was identical, which is why this only broke
-- once the shared history reached 0064.
INSERT INTO centry.project (id) VALUES (2);
CREATE SCHEMA p_2;
CREATE TABLE p_2.configuration (
    id SERIAL PRIMARY KEY,
    uuid UUID NOT NULL UNIQUE,
    project_id INTEGER NOT NULL,
    label VARCHAR,
    elitea_title VARCHAR NOT NULL UNIQUE,
    type VARCHAR NOT NULL,
    section VARCHAR NOT NULL,
    data JSONB NOT NULL,
    meta JSONB NOT NULL,
    shared BOOLEAN NOT NULL,
    status_ok BOOLEAN NOT NULL,
    status_logs TEXT,
    source VARCHAR NOT NULL,
    author_id INTEGER,
    created_at TIMESTAMP NOT NULL DEFAULT now(),
    updated_at TIMESTAMP
);
INSERT INTO p_1.configuration (
    id, uuid, project_id, label, elitea_title, type, section, data, meta,
    shared, status_ok, status_logs, source, author_id
) VALUES (
    2, '00000000-0000-0000-0000-000000000102', 1, 'Public shared GitHub',
    'public_shared_github', 'github', 'credentials', '{}'::jsonb,
    '{"scope":"public"}'::jsonb, true, true, 'validated', 'system', 1
);
SELECT setval(pg_get_serial_sequence('p_1.configuration', 'id'), (SELECT MAX(id) FROM p_1.configuration));`); err != nil {
		t.Fatalf("prepare current configuration project two: %v", err)
	}

	expectedColumns := []string{
		"id", "uuid", "project_id", "label", "elitea_title", "type", "section", "data",
		"meta", "shared", "status_ok", "status_logs", "source", "author_id", "created_at", "updated_at",
	}
	var columns []string
	if err := pool.QueryRow(ctx, `
SELECT array_agg(column_name::text ORDER BY ordinal_position)
FROM information_schema.columns
WHERE table_schema = 'p_2' AND table_name = 'configuration'`).Scan(&columns); err != nil {
		t.Fatalf("read project-two configuration columns: %v", err)
	}
	if !reflect.DeepEqual(columns, expectedColumns) {
		t.Fatalf("project-two configuration columns=%v", columns)
	}
}
