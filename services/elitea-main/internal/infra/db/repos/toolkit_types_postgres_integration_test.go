package repos

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCurrentToolkitTypesRepositoryPostgresParity(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	prepareCurrentToolkitTypesProjects(t, pool)

	repository, err := NewCurrentToolkitTypesRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	tests := []struct {
		name              string
		projectID         int32
		filterMCP         bool
		filterApplication bool
		want              []string
	}{
		{
			name:      "default excludes MCP and application",
			projectID: 1,
			want:      []string{"github"},
		},
		{
			name:      "MCP filter remains independent",
			projectID: 1,
			filterMCP: true,
			want:      []string{"mcp", "mcp_flag"},
		},
		{
			name:              "application filter remains independent",
			projectID:         1,
			filterApplication: true,
			want:              []string{"app_flag", "application"},
		},
		{
			name:              "both filters require both classifications",
			projectID:         1,
			filterMCP:         true,
			filterApplication: true,
			want:              []string{"application", "both_flag", "mcp"},
		},
		{
			name:      "tenant project remains isolated",
			projectID: 2,
			want:      []string{"private_type"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rows, err := repository.ListCurrentToolkitTypes(
				ctx,
				test.projectID,
				test.filterMCP,
				test.filterApplication,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(rows, test.want) {
				t.Fatalf("rows=%v want=%v", rows, test.want)
			}
		})
	}
}

func prepareCurrentToolkitTypesProjects(t *testing.T, pool *pgxpool.Pool) {
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
INSERT INTO p_1.elitea_tools (type, settings, author_id, meta, created_at) VALUES
    ('github', '{}'::jsonb, 1, '{}'::jsonb, now() AT TIME ZONE 'UTC'),
    ('mcp', '{}'::jsonb, 1, '{}'::jsonb, now() AT TIME ZONE 'UTC'),
    ('mcp_flag', '{}'::jsonb, 1, '{"mcp":true}'::jsonb, now() AT TIME ZONE 'UTC'),
    ('application', '{}'::jsonb, 1, '{}'::jsonb, now() AT TIME ZONE 'UTC'),
    ('app_flag', '{}'::jsonb, 1, '{"application":true}'::jsonb, now() AT TIME ZONE 'UTC'),
    ('both_flag', '{}'::jsonb, 1, '{"mcp":true,"application":true}'::jsonb, now() AT TIME ZONE 'UTC'),
    ('mcp', '{}'::jsonb, 1, '{"application":true}'::jsonb, now() AT TIME ZONE 'UTC'),
    ('application', '{}'::jsonb, 1, '{"mcp":true}'::jsonb, now() AT TIME ZONE 'UTC');

INSERT INTO centry.project (id, create_success, suspended)
VALUES (2, TRUE, FALSE);
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
INSERT INTO p_2.elitea_tools (type, settings, author_id, meta, created_at)
VALUES ('private_type', '{}'::jsonb, 2, '{}'::jsonb, now() AT TIME ZONE 'UTC');`); err != nil {
		t.Fatalf("prepare current toolkit types projects: %v", err)
	}
}
