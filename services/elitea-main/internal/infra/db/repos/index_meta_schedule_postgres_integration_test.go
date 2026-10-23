package repos

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	indexmetaapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexmeta"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCurrentIndexMetaScheduleRepositoryPostgresTenantParity(
	t *testing.T,
) {
	pool := newMigratedPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
    id, type, name, settings, author_id, meta
, created_at) VALUES (
    19,
    'github',
    'Project one source',
    '{}'::jsonb,
    11,
    '{
       "indexes_meta":{
         "Docs":{"schedules":{"11":{"enabled":true},"12":{"enabled":false}}},
         "Other":{"schedules":{"11":{"enabled":true}}}
       },
       "unrelated":{"preserve":true}
     }'::jsonb
, now() AT TIME ZONE 'UTC');

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
INSERT INTO p_2.elitea_tools (
    id, type, name, settings, author_id, meta
, created_at) VALUES (
    19,
    'github',
    'Project two source',
    '{}'::jsonb,
    12,
    '{"indexes_meta":{"Docs":{"schedules":{"12":{"enabled":true}}}}}'::jsonb
, now() AT TIME ZONE 'UTC')`); err != nil {
		t.Fatal(err)
	}

	repository, err := NewCurrentIndexMetaScheduleRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.DeleteCurrentIndexSchedule(
		ctx,
		1,
		19,
		"Docs",
	); err != nil {
		t.Fatal(err)
	}

	projectOne := loadCurrentIndexScheduleMeta(t, ctx, pool, "p_1", 19)
	projectTwo := loadCurrentIndexScheduleMeta(t, ctx, pool, "p_2", 19)
	projectOneIndexes := projectOne["indexes_meta"].(map[string]any)
	projectTwoIndexes := projectTwo["indexes_meta"].(map[string]any)
	if _, present := projectOneIndexes["Docs"]; present ||
		projectOneIndexes["Other"] == nil ||
		projectOne["unrelated"] == nil {
		t.Fatalf("project-one metadata=%+v", projectOne)
	}
	if projectTwoIndexes["Docs"] == nil {
		t.Fatalf("cross-project metadata changed=%+v", projectTwo)
	}

	// The current cleanup is idempotent when the schedule entry is absent.
	if err := repository.DeleteCurrentIndexSchedule(
		ctx,
		1,
		19,
		"Docs",
	); err != nil {
		t.Fatalf("absent schedule cleanup error=%v", err)
	}
	if err := repository.DeleteCurrentIndexSchedule(
		ctx,
		1,
		999,
		"Docs",
	); !errors.Is(err, indexmetaapp.ErrCurrentIndexScheduleToolkitMissing) {
		t.Fatalf("missing toolkit error=%v", err)
	}
}

func loadCurrentIndexScheduleMeta(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	schema string,
	toolkitID int32,
) map[string]any {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(
		ctx,
		`SELECT meta FROM `+schema+`.elitea_tools WHERE id = $1`,
		toolkitID,
	).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	return metadata
}
