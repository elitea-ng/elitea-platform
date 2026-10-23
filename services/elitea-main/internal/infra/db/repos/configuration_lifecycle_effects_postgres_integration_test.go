package repos

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCurrentConfigurationLifecycleEffectsPostgresParity(t *testing.T) {
	pool := newPostgresIntegrationPool(t)
	prepareCurrentConfigurationLifecycleEffectsPostgres(t, pool)
	repository := mustCurrentConfigurationLifecycleEffectsPostgresRepository(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	updated, err := repository.SetCurrentConfigurationLifecycleStatus(ctx, configurationapp.CurrentConfigurationLifecycleStatusTarget{
		ProjectID: 1, ConfigurationID: 11,
		ConfigurationUUID: "11111111-1111-4111-8111-111111111111",
		StatusOK:          false,
	})
	if err != nil || !updated {
		t.Fatalf("set exact status updated=%t error=%v", updated, err)
	}
	updated, err = repository.SetCurrentConfigurationLifecycleStatus(ctx, configurationapp.CurrentConfigurationLifecycleStatusTarget{
		ProjectID: 1, ConfigurationID: 11,
		ConfigurationUUID: "22222222-2222-4222-8222-222222222222",
		StatusOK:          true,
	})
	if err != nil || updated {
		t.Fatalf("stale status updated=%t error=%v", updated, err)
	}
	var statusOK bool
	var label string
	if err := pool.QueryRow(ctx, `SELECT status_ok, label FROM p_1.configuration WHERE id = 11`).Scan(&statusOK, &label); err != nil {
		t.Fatal(err)
	}
	if statusOK || label != "preserved-label" {
		t.Fatalf("status_ok=%t label=%q", statusOK, label)
	}

	toolkits, err := repository.ListCurrentConfigurationRenameToolkits(
		ctx,
		1,
		configurationapp.CurrentConfigurationRenameScanLimits{MaxRows: 3, MaxSettingsBytes: 1024, MaxTotalBytes: 2048},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(toolkits) != 2 || toolkits[0].ToolkitID != 2 || toolkits[1].ToolkitID != 5 {
		t.Fatalf("toolkits=%#v", toolkits)
	}

	projectIDs, err := repository.ListActiveCurrentProjectIDs(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(projectIDs) != 2 || projectIDs[0] != 1 || projectIDs[1] != 4 {
		t.Fatalf("active projects=%v", projectIDs)
	}

	replaced, err := repository.ReplaceCurrentDeletedLLMApplicationReferences(
		ctx,
		configurationapp.CurrentDeletedLLMReferenceReplacement{
			ProjectID: 1, DeletedModelName: "old-model", DefaultModelName: "default-model",
			DefaultModelProjectID: 4, MaxRows: 10,
		},
	)
	if err != nil || replaced != 2 {
		t.Fatalf("replaced=%d error=%v", replaced, err)
	}
	rows, err := pool.Query(ctx, `SELECT id, llm_settings FROM p_1.application_versions ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	wantNames := []string{"default-model", "default-model", "old-model-extra"}
	index := 0
	for rows.Next() {
		var id int32
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			t.Fatal(err)
		}
		var settings map[string]any
		if err := json.Unmarshal(raw, &settings); err != nil {
			t.Fatal(err)
		}
		if index >= len(wantNames) || settings["model_name"] != wantNames[index] || settings["temperature"] != float64(index+1) {
			t.Fatalf("id=%d settings=%v", id, settings)
		}
		if index < 2 && settings["model_project_id"] != float64(4) {
			t.Fatalf("id=%d model_project_id=%v", id, settings["model_project_id"])
		}
		index++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if index != 3 {
		t.Fatalf("application version rows=%d", index)
	}
}

func TestCurrentConfigurationLifecycleEffectsPostgresOverflowMakesNoPartialUpdate(t *testing.T) {
	pool := newPostgresIntegrationPool(t)
	prepareCurrentConfigurationLifecycleEffectsPostgres(t, pool)
	repository := mustCurrentConfigurationLifecycleEffectsPostgresRepository(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := repository.ListCurrentConfigurationRenameToolkits(
		ctx,
		1,
		configurationapp.CurrentConfigurationRenameScanLimits{MaxRows: 3, MaxSettingsBytes: 8, MaxTotalBytes: 2048},
	)
	if !errors.Is(err, configurationapp.ErrCurrentConfigurationLifecycleInternalLimit) {
		t.Fatalf("oversized toolkit error=%v", err)
	}

	replaced, err := repository.ReplaceCurrentDeletedLLMApplicationReferences(
		ctx,
		configurationapp.CurrentDeletedLLMReferenceReplacement{
			ProjectID: 1, DeletedModelName: "old-model", DefaultModelName: "default-model",
			DefaultModelProjectID: 4, MaxRows: 1,
		},
	)
	if !errors.Is(err, configurationapp.ErrCurrentConfigurationLifecycleInternalLimit) || replaced != 0 {
		t.Fatalf("overflow replaced=%d error=%v", replaced, err)
	}
	var unchanged int
	if err := pool.QueryRow(ctx, `
SELECT count(*)
FROM p_1.application_versions
WHERE llm_settings ->> 'model_name' = 'old-model'
  AND llm_settings ->> 'model_project_id' = '1'`).Scan(&unchanged); err != nil {
		t.Fatal(err)
	}
	if unchanged != 2 {
		t.Fatalf("overflow changed rows; unchanged=%d", unchanged)
	}
}

func TestCurrentConfigurationLifecycleEffectsPostgresCASConcurrency(t *testing.T) {
	pool := newPostgresIntegrationPool(t)
	prepareCurrentConfigurationLifecycleEffectsPostgres(t, pool)
	repository := mustCurrentConfigurationLifecycleEffectsPostgresRepository(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	toolkit, found, err := repository.GetCurrentConfigurationRenameToolkit(ctx, 1, 2)
	if err != nil || !found {
		t.Fatalf("get toolkit found=%t error=%v", found, err)
	}

	start := make(chan struct{})
	results := make(chan bool, 2)
	errorsSeen := make(chan error, 2)
	settings := []json.RawMessage{
		json.RawMessage(`{"winner":"one"}`),
		json.RawMessage(`{"winner":"two"}`),
	}
	var workers sync.WaitGroup
	for index := range settings {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			updated, err := repository.CompareAndSwapCurrentConfigurationRenameToolkit(
				ctx,
				configurationapp.CurrentConfigurationRenameToolkitUpdate{
					ProjectID: 1, ToolkitID: 2, ExpectedVersion: toolkit.Version,
					Settings: settings[index],
				},
			)
			results <- updated
			errorsSeen <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	winners := 0
	for updated := range results {
		if updated {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("CAS winners=%d", winners)
	}

	current, found, err := repository.GetCurrentConfigurationRenameToolkit(ctx, 1, 2)
	if err != nil || !found {
		t.Fatalf("get winning toolkit found=%t error=%v", found, err)
	}
	var winningSettings map[string]string
	if err := json.Unmarshal(current.Settings, &winningSettings); err != nil {
		t.Fatal(err)
	}
	if winningSettings["winner"] != "one" && winningSettings["winner"] != "two" {
		t.Fatalf("winning settings=%v", winningSettings)
	}
}

func mustCurrentConfigurationLifecycleEffectsPostgresRepository(
	t *testing.T,
	pool *pgxpool.Pool,
) *CurrentConfigurationLifecycleEffectsRepository {
	t.Helper()
	repository, err := NewCurrentConfigurationLifecycleEffectsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	return repository
}

func prepareCurrentConfigurationLifecycleEffectsPostgres(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
CREATE SCHEMA centry;
CREATE TABLE centry.project (
    id SERIAL PRIMARY KEY,
    name VARCHAR(256) NOT NULL,
    owner_id INTEGER NOT NULL,
    secrets_json JSON,
    plugins TEXT[],
    keycloak_groups JSON NOT NULL,
    create_success BOOLEAN NOT NULL,
    suspended BOOLEAN NOT NULL DEFAULT false
);
INSERT INTO centry.project (id, name, owner_id, keycloak_groups, create_success, suspended) VALUES
    (1, 'active-one', 1, '[]', true, false),
    (2, 'incomplete', 1, '[]', false, false),
    (3, 'suspended', 1, '[]', true, true),
    (4, 'active-four', 1, '[]', true, false);

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
INSERT INTO p_1.configuration (
    id, uuid, project_id, label, elitea_title, type, section, data, meta,
    shared, status_ok, source
, created_at) VALUES (
    11, '11111111-1111-4111-8111-111111111111', 1, 'preserved-label',
    'configuration-one', 'openai', 'credentials', '{}', '{}', false, true, 'user'
, now() AT TIME ZONE 'UTC');

CREATE TABLE p_1.elitea_tools (
    id SERIAL PRIMARY KEY,
    created_at TIMESTAMP NOT NULL DEFAULT now(),
    updated_at TIMESTAMP,
    type VARCHAR NOT NULL,
    name VARCHAR(128),
    description VARCHAR(1024),
    settings JSONB NOT NULL,
    author_id INTEGER NOT NULL,
    shared_owner_id INTEGER,
    shared_id INTEGER,
    meta JSONB NOT NULL
);
INSERT INTO p_1.elitea_tools (id, type, name, settings, author_id, meta, created_at) VALUES
    (5, 'jira', 'later-id', '{"configuration":{"elitea_title":"before","private":true}}', 1, '{}', now() AT TIME ZONE 'UTC'),
    (2, 'github', 'earlier-id', '{"configuration":{"elitea_title":"before","private":true}}', 1, '{}', now() AT TIME ZONE 'UTC');

CREATE TABLE p_1.application_versions (
    id SERIAL PRIMARY KEY,
    application_id INTEGER NOT NULL,
    name VARCHAR(128) NOT NULL,
    status VARCHAR NOT NULL,
    author_id INTEGER NOT NULL,
    uuid UUID NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL DEFAULT now(),
    shared_owner_id INTEGER,
    shared_id INTEGER,
    llm_settings JSON NOT NULL,
    instructions VARCHAR,
    conversation_starters JSON NOT NULL,
    welcome_message VARCHAR NOT NULL,
    agent_type VARCHAR NOT NULL,
    meta JSONB NOT NULL,
    pipeline_settings JSONB NOT NULL,
    UNIQUE (application_id, name),
    UNIQUE (shared_owner_id, shared_id)
);
INSERT INTO p_1.application_versions (
    id, application_id, name, status, author_id, uuid, llm_settings,
    conversation_starters, welcome_message, agent_type, meta, pipeline_settings
, created_at) VALUES
    (1, 1, 'v1', 'active', 1, 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1', '{"model_name":"old-model","model_project_id":1,"temperature":1}', '[]', '', 'agent', '{}', '{}', now() AT TIME ZONE 'UTC'),
    (2, 1, 'v2', 'draft', 1, 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa2', '{"model_name":"old-model","model_project_id":1,"temperature":2}', '[]', '', 'agent', '{}', '{}', now() AT TIME ZONE 'UTC'),
    (3, 2, 'v1', 'active', 1, 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa3', '{"model_name":"old-model-extra","model_project_id":1,"temperature":3}', '[]', '', 'agent', '{}', '{}', now() AT TIME ZONE 'UTC');
`); err != nil {
		t.Fatalf("prepare configuration lifecycle effects PostgreSQL: %v", err)
	}
}
