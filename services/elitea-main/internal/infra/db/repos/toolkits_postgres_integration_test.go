package repos

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCurrentToolkitsRepositoryPostgresParity(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	prepareCurrentToolkitProjects(t, pool)

	repository, err := NewCurrentToolkitsRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	projectOne, err := repository.Get(ctx, 1, 1)
	if err != nil {
		t.Fatalf("get project-one toolkit: %v", err)
	}
	projectTwo, err := repository.Get(ctx, 2, 1)
	if err != nil {
		t.Fatalf("get project-two toolkit: %v", err)
	}
	if projectOne.Name == nil || *projectOne.Name != "project-one" || projectOne.Type != "confluence" ||
		projectOne.Description == nil || *projectOne.Description != "Project one source" ||
		projectOne.AuthorID != 11 || projectOne.SharedOwnerID == nil || *projectOne.SharedOwnerID != 71 ||
		projectOne.SharedID == nil || *projectOne.SharedID != 81 || projectOne.CreatedAt.IsZero() ||
		projectOne.UpdatedAt == nil {
		t.Fatalf("project-one toolkit=%#v", projectOne)
	}
	if projectTwo.Name == nil || *projectTwo.Name != "project-two" || projectTwo.Type != "jira" ||
		projectTwo.Description != nil || projectTwo.AuthorID != 12 || projectTwo.SharedOwnerID != nil ||
		projectTwo.SharedID != nil || projectTwo.CreatedAt.IsZero() || projectTwo.UpdatedAt != nil {
		t.Fatalf("project-two toolkit=%#v", projectTwo)
	}

	wantSettings := map[string]any{
		"large_id": json.Number("9007199254740993"),
		"nested":   map[string]any{"enabled": true},
	}
	if !reflect.DeepEqual(projectTwo.Settings, wantSettings) {
		t.Fatalf("project-two settings=%#v", projectTwo.Settings)
	}
	if !reflect.DeepEqual(projectOne.Meta, map[string]any{"source": "current"}) ||
		!reflect.DeepEqual(projectTwo.Meta, map[string]any{"source": "current"}) {
		t.Fatalf("metadata project_one=%#v project_two=%#v", projectOne.Meta, projectTwo.Meta)
	}

	if _, err := repository.Get(ctx, 2, 999); !errors.Is(err, ErrCurrentToolkitNotFound) {
		t.Fatalf("missing toolkit error=%v", err)
	}

	// The social folder feature is optional. Its absence keeps project RBAC as
	// the authority. Once the complete projection exists, a no_access override
	// hides the toolkit only from the named actor.
	if toolkit, err := repository.GetMCPVisible(ctx, 1, 11, 1); err != nil || toolkit.ID != 1 {
		t.Fatalf("MCP read without social projection toolkit=%#v error=%v", toolkit, err)
	}
	if _, err := pool.Exec(ctx, `
CREATE TABLE p_1.entity_folders (
    id INTEGER PRIMARY KEY,
    entity_type VARCHAR(32) NOT NULL
);
CREATE TABLE p_1.social_folder_items (
    id INTEGER PRIMARY KEY,
    folder_id INTEGER NOT NULL,
    entity VARCHAR(32) NOT NULL,
    entity_id INTEGER NOT NULL
);
CREATE TABLE p_1.folder_access_overrides (
    id INTEGER PRIMARY KEY,
    folder_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    access_level VARCHAR(16) NOT NULL
);
INSERT INTO p_1.entity_folders (id, entity_type) VALUES (3, 'toolkit');
INSERT INTO p_1.social_folder_items (id, folder_id, entity, entity_id)
VALUES (5, 3, 'toolkit', 1);
INSERT INTO p_1.folder_access_overrides (id, folder_id, user_id, access_level)
VALUES (7, 3, 11, 'no_access');`); err != nil {
		t.Fatalf("prepare folder access projection: %v", err)
	}
	if _, err := repository.GetMCPVisible(ctx, 1, 11, 1); !errors.Is(err, ErrCurrentToolkitNotFound) {
		t.Fatalf("restricted MCP toolkit error=%v", err)
	}
	if toolkit, err := repository.GetMCPVisible(ctx, 1, 12, 1); err != nil || toolkit.ID != 1 {
		t.Fatalf("unrestricted MCP toolkit=%#v error=%v", toolkit, err)
	}
}

func prepareCurrentToolkitProjects(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
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
INSERT INTO p_1.elitea_tools (
    id, created_at, updated_at, type, name, description, settings, author_id,
    shared_owner_id, shared_id, meta
) VALUES (
    1, '2026-07-22 08:00:00', '2026-07-22 09:00:00', 'confluence', 'project-one',
    'Project one source', '{"large_id":9007199254740992}'::jsonb, 11, 71, 81,
    '{"source":"current"}'::jsonb
);

INSERT INTO centry.project (id) VALUES (2);
CREATE SCHEMA p_2;
CREATE TABLE p_2.elitea_tools (
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
INSERT INTO p_2.elitea_tools (
    id, type, name, description, settings, author_id, shared_owner_id, shared_id, meta
) VALUES (
    1, 'jira', 'project-two', NULL,
    '{"large_id":9007199254740993,"nested":{"enabled":true}}'::jsonb,
    12, NULL, NULL, '{"source":"current"}'::jsonb
);`); err != nil {
		t.Fatalf("prepare current toolkit projects: %v", err)
	}

	expectedColumns := []string{
		"id", "created_at", "updated_at", "type", "name", "description", "settings",
		"author_id", "shared_owner_id", "shared_id", "meta",
	}
	for _, schema := range []string{"p_1", "p_2"} {
		var columns []string
		if err := pool.QueryRow(ctx, `
SELECT array_agg(column_name::text ORDER BY ordinal_position)
FROM information_schema.columns
WHERE table_schema = $1 AND table_name = 'elitea_tools'`, schema).Scan(&columns); err != nil {
			t.Fatalf("read %s elitea_tools columns: %v", schema, err)
		}
		if !reflect.DeepEqual(columns, expectedColumns) {
			t.Fatalf("%s elitea_tools columns=%v", schema, columns)
		}
	}
}
