package repos

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	indexscheduleapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexschedule"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCurrentIndexSchedulePatchRepositoryPostgresTenantAndConcurrentParity(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	prepareCurrentIndexSchedulePatchProjects(t, pool)
	prepareCurrentIndexSchedulePatchFixtures(t, pool)

	repository, err := NewCurrentIndexSchedulePatchRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	privateCredential := true
	mutations := []indexscheduleapp.Mutation{
		{
			ProjectID: 1, ActorUserID: 11, ToolkitID: 1, IndexMetaID: "docs",
			RequestedUserID: -1,
			Schedule: indexscheduleapp.Schedule{
				Cron: "0 3 * * *", Enabled: true, CreatedBy: 11, Timezone: "UTC",
				LastRun: "2026-07-27T09:34:56+00:00",
				Credentials: &indexscheduleapp.Credentials{
					Private: &privateCredential, EliteaTitle: "personal-github",
				},
			},
		},
		{
			ProjectID: 1, ActorUserID: 11, ToolkitID: 1, IndexMetaID: "wiki",
			RequestedUserID: -1,
			Schedule: indexscheduleapp.Schedule{
				Cron: "0 4 * * *", Enabled: false, CreatedBy: 11, Timezone: "Europe/Kyiv",
				LastRun: "2026-07-27T09:34:57+00:00",
			},
		},
	}

	results := make([]indexscheduleapp.MutationResult, len(mutations))
	failures := make([]error, len(mutations))
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := range mutations {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			results[index], failures[index] = repository.Patch(ctx, mutations[index])
		}()
	}
	close(start)
	wait.Wait()
	for index, err := range failures {
		if err != nil {
			t.Fatalf("concurrent mutation %d: %v", index, err)
		}
		if results[index].EffectiveUserID != -1 {
			t.Fatalf("public effective user %d=%d", index, results[index].EffectiveUserID)
		}
	}

	projectOne := readCurrentScheduleMeta(t, pool, "p_1")
	if projectOne["root"] != "project-one" {
		t.Fatalf("project-one unrelated metadata=%#v", projectOne)
	}
	projectOneIndexes := projectOne["indexes_meta"].(map[string]any)
	assertCurrentSchedulePersisted(t, projectOneIndexes, "docs", "-1", "0 3 * * *")
	assertCurrentSchedulePersisted(t, projectOneIndexes, "wiki", "-1", "0 4 * * *")
	projectSchedule := projectOneIndexes["docs"].(map[string]any)["schedules"].(map[string]any)["-1"].(map[string]any)
	credentials, ok := projectSchedule["credentials"].(map[string]any)
	if !ok ||
		credentials["private"] != true ||
		credentials["elitea_title"] != "personal-github" ||
		projectSchedule["created_by"] != json.Number("11") {
		t.Fatalf("public project private creator credential drifted: %#v", projectSchedule)
	}

	projectTwoResult, err := repository.Patch(ctx, indexscheduleapp.Mutation{
		ProjectID: 2, ActorUserID: 12, ToolkitID: 1, IndexMetaID: "docs",
		RequestedUserID: -1,
		Schedule: indexscheduleapp.Schedule{
			Cron: "0 5 * * *", Enabled: true, CreatedBy: 12, Timezone: "UTC",
			LastRun: "2026-07-27T09:34:58+00:00",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if projectTwoResult.EffectiveUserID != 12 {
		t.Fatalf("private effective user=%d", projectTwoResult.EffectiveUserID)
	}
	projectTwo := readCurrentScheduleMeta(t, pool, "p_2")
	if projectTwo["root"] != "project-two" {
		t.Fatalf("project-two unrelated metadata=%#v", projectTwo)
	}
	projectTwoIndexes := projectTwo["indexes_meta"].(map[string]any)
	assertCurrentSchedulePersisted(t, projectTwoIndexes, "docs", "12", "0 5 * * *")
	if _, leaked := projectTwoIndexes["wiki"]; leaked {
		t.Fatalf("project-one metadata leaked into project two: %#v", projectTwoIndexes)
	}

	projectOneAfter := readCurrentScheduleMeta(t, pool, "p_1")
	projectOneDocs := projectOneAfter["indexes_meta"].(map[string]any)["docs"].(map[string]any)
	if _, leaked := projectOneDocs["schedules"].(map[string]any)["12"]; leaked {
		t.Fatalf("project-two schedule leaked into project one: %#v", projectOneDocs)
	}
}

func prepareCurrentIndexSchedulePatchProjects(t *testing.T, pool *pgxpool.Pool) {
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
    id, type, name, settings, author_id, meta
, created_at) VALUES (
    1, 'github', 'project-one', '{}'::jsonb, 11, '{}'::jsonb
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
    1, 'github', 'project-two', '{}'::jsonb, 12, '{}'::jsonb
, now() AT TIME ZONE 'UTC');`); err != nil {
		t.Fatalf("prepare current schedule projects: %v", err)
	}
}

func prepareCurrentIndexSchedulePatchFixtures(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
UPDATE p_1.elitea_tools
SET settings = '{"pgvector_configuration":{"private":false},"keep":1}'::jsonb,
    meta = '{
      "root":"project-one",
      "indexes_meta":{
        "docs":{"title":"Docs","schedules":{"5":{"enabled":false}}},
        "wiki":{"title":"Wiki","schedules":{}}
      }
    }'::jsonb
WHERE id = 1;

UPDATE p_2.elitea_tools
SET settings = '{"pgvector_configuration":{"private":true},"keep":2}'::jsonb,
    meta = '{
      "root":"project-two",
      "indexes_meta":{"docs":{"title":"Docs","schedules":{}}}
    }'::jsonb
WHERE id = 1;`); err != nil {
		t.Fatalf("prepare current schedule fixtures: %v", err)
	}
}

func readCurrentScheduleMeta(
	t *testing.T,
	pool *pgxpool.Pool,
	schema string,
) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var raw []byte
	query := "SELECT meta FROM " + schema + ".elitea_tools WHERE id = 1"
	if err := pool.QueryRow(ctx, query).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	value, err := decodeCurrentScheduleObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func assertCurrentSchedulePersisted(
	t *testing.T,
	indexes map[string]any,
	indexMetaID, userID, cron string,
) {
	t.Helper()
	index, ok := indexes[indexMetaID].(map[string]any)
	if !ok {
		t.Fatalf("missing index %q: %#v", indexMetaID, indexes)
	}
	schedules, ok := index["schedules"].(map[string]any)
	if !ok {
		t.Fatalf("missing schedules %q: %#v", indexMetaID, index)
	}
	schedule, ok := schedules[userID].(map[string]any)
	if !ok || schedule["cron"] != cron {
		encoded, _ := json.Marshal(schedules)
		t.Fatalf("schedule %s/%s=%s", indexMetaID, userID, encoded)
	}
}
