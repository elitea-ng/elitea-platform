package repos

import (
	"context"
	"reflect"
	"testing"
	"time"

	configurationapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCurrentConfigurationOptionCandidatesPostgresParity(t *testing.T) {
	pool := newPostgresIntegrationPool(t)
	prepareCurrentConfigurationOptionCandidates(t, pool)

	repository, err := NewCurrentConfigurationsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	items, err := repository.ListCurrentConfigurationOptionCandidates(
		ctx,
		configurationapp.CurrentConfigurationOptionCandidatesQuery{
			ProjectID:       2,
			PublicProjectID: 1,
			IncludeShared:   true,
			Types:           []string{"github"},
			Sections:        []string{"llm"},
			MaxRows:         10,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	projectGitHubLabel := "Project GitHub"
	publicGitHubLabel := "Public shared GitHub"
	publicOpenAILabel := "Public shared OpenAI"
	want := []configurationapp.CurrentConfigurationOption{
		{
			EliteaTitle: "project_openai",
			Type:        "openai",
			Section:     "llm",
			Shared:      true,
			ProjectID:   2,
		},
		{
			EliteaTitle: "project_github",
			Label:       &projectGitHubLabel,
			Type:        "github",
			Section:     "credentials",
			Shared:      false,
			ProjectID:   2,
		},
		{
			EliteaTitle: "public_shared_github",
			Label:       &publicGitHubLabel,
			Type:        "github",
			Section:     "credentials",
			Shared:      true,
			ProjectID:   1,
		},
		{
			EliteaTitle: "public_shared_openai",
			Label:       &publicOpenAILabel,
			Type:        "openai",
			Section:     "llm",
			Shared:      true,
			ProjectID:   1,
		},
	}
	if !reflect.DeepEqual(items, want) {
		t.Fatalf("items=%#v want=%#v", items, want)
	}

	publicItems, err := repository.ListCurrentConfigurationOptionCandidates(
		ctx,
		configurationapp.CurrentConfigurationOptionCandidatesQuery{
			ProjectID:       1,
			PublicProjectID: 1,
			IncludeShared:   true,
			Types:           []string{"github", "openapi"},
			Sections:        []string{"llm"},
			MaxRows:         10,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	publicTitles := make([]string, len(publicItems))
	for index, item := range publicItems {
		publicTitles[index] = item.EliteaTitle
	}
	if !reflect.DeepEqual(publicTitles, []string{
		"integration_fixture",
		"public_shared_github",
		"public_private_openai",
		"public_shared_openai",
	}) {
		t.Fatalf("public project options=%v", publicTitles)
	}
	if publicItems[0].Shared || publicItems[2].Shared {
		t.Fatalf("public project lost its own unshared rows: %#v", publicItems)
	}

	bounded, err := repository.ListCurrentConfigurationOptionCandidates(
		ctx,
		configurationapp.CurrentConfigurationOptionCandidatesQuery{
			ProjectID:       2,
			PublicProjectID: 1,
			IncludeShared:   true,
			Types:           []string{"github"},
			Sections:        []string{"llm"},
			MaxRows:         3,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	boundedTitles := make([]string, len(bounded))
	for index, item := range bounded {
		boundedTitles[index] = item.EliteaTitle
	}
	if !reflect.DeepEqual(boundedTitles, []string{
		"project_openai",
		"project_github",
		"public_shared_github",
	}) {
		t.Fatalf("globally bounded options=%v", boundedTitles)
	}
}

func prepareCurrentConfigurationOptionCandidates(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
CREATE SCHEMA centry;
CREATE TABLE centry.project (id INTEGER PRIMARY KEY);
INSERT INTO centry.project (id) VALUES (1), (2);

CREATE SCHEMA p_1;
CREATE TABLE p_1.configuration (
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

INSERT INTO p_2.configuration (
    id, uuid, project_id, label, elitea_title, type, section, data, meta,
    shared, status_ok, source
, created_at) VALUES
    (
        9, '00000000-0000-0000-0000-000000000209', 2, 'Project GitHub',
        'project_github', 'github', 'credentials', '{}'::jsonb, '{}'::jsonb,
        false, true, 'user'
    , now() AT TIME ZONE 'UTC'),
    (
        5, '00000000-0000-0000-0000-000000000205', 2, 'Project Confluence',
        'project_confluence', 'confluence', 'credentials', '{}'::jsonb, '{}'::jsonb,
        false, true, 'user'
    , now() AT TIME ZONE 'UTC'),
    (
        3, '00000000-0000-0000-0000-000000000203', 2, NULL,
        'project_openai', 'openai', 'llm', '{}'::jsonb, '{}'::jsonb,
        true, true, 'user'
    , now() AT TIME ZONE 'UTC');

INSERT INTO p_1.configuration (
    id, uuid, project_id, label, elitea_title, type, section, data, meta,
    shared, status_ok, source
, created_at) VALUES
    (
        1, '00000000-0000-0000-0000-000000000001', 1, 'Integration fixture',
        'integration_fixture', 'openapi', 'credentials', '{}'::jsonb, '{}'::jsonb,
        false, false, 'user'
    , now() AT TIME ZONE 'UTC'),
    (
        2, '00000000-0000-0000-0000-000000000102', 1, 'Public shared GitHub',
        'public_shared_github', 'github', 'credentials', '{}'::jsonb, '{}'::jsonb,
        true, true, 'system'
    , now() AT TIME ZONE 'UTC'),
    (
        8, '00000000-0000-0000-0000-000000000108', 1, 'Public shared OpenAI',
        'public_shared_openai', 'openai', 'llm', '{}'::jsonb, '{}'::jsonb,
        true, true, 'system'
    , now() AT TIME ZONE 'UTC'),
    (
        6, '00000000-0000-0000-0000-000000000106', 1, 'Public Confluence',
        'public_confluence', 'confluence', 'credentials', '{}'::jsonb, '{}'::jsonb,
        true, true, 'system'
    , now() AT TIME ZONE 'UTC'),
    (
        4, '00000000-0000-0000-0000-000000000104', 1, 'Public private OpenAI',
        'public_private_openai', 'openai', 'llm', '{}'::jsonb, '{}'::jsonb,
        false, true, 'system'
    , now() AT TIME ZONE 'UTC');`); err != nil {
		t.Fatalf("prepare option candidates: %v", err)
	}
}
