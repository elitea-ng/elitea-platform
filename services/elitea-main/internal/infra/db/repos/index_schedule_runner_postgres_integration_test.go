package repos

import (
	"context"
	"testing"
	"time"

	indexscheduleapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexschedule"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCurrentIndexScheduleCatalogPostgresKeysetAndDeleteWins(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	prepareCurrentIndexSchedulePatchProjects(t, pool)
	prepareCurrentIndexScheduleRunnerFixtures(t, pool)

	catalog, err := NewCurrentIndexScheduleCatalog(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	firstProjects, err := catalog.ListProjectPage(ctx, 0, 1)
	if err != nil || len(firstProjects) != 1 || firstProjects[0] != 1 {
		t.Fatalf("first project page=%v error=%v", firstProjects, err)
	}
	secondProjects, err := catalog.ListProjectPage(ctx, firstProjects[0], 1)
	if err != nil || len(secondProjects) != 1 || secondProjects[0] != 2 {
		t.Fatalf("second project page=%v error=%v", secondProjects, err)
	}
	lastProjects, err := catalog.ListProjectPage(ctx, secondProjects[0], 1)
	if err != nil || len(lastProjects) != 0 {
		t.Fatalf("last project page=%v error=%v", lastProjects, err)
	}

	firstToolkits, err := catalog.ListToolkitSchedulePage(ctx, 1, 0, 1)
	if err != nil || len(firstToolkits) != 1 || firstToolkits[0].ToolkitID != 1 ||
		len(firstToolkits[0].Candidates) != 2 {
		t.Fatalf("first toolkit page=%+v error=%v", firstToolkits, err)
	}
	secondToolkits, err := catalog.ListToolkitSchedulePage(ctx, 1, 1, 1)
	if err != nil || len(secondToolkits) != 1 || secondToolkits[0].ToolkitID != 3 ||
		len(secondToolkits[0].Candidates) != 1 {
		t.Fatalf("second toolkit page=%+v error=%v", secondToolkits, err)
	}
	projectTwo, err := catalog.ListToolkitSchedulePage(ctx, 2, 0, 2)
	if err != nil || len(projectTwo) != 1 ||
		projectTwo[0].Candidates[0].ProjectID != 2 {
		t.Fatalf("project-two page=%+v error=%v", projectTwo, err)
	}

	candidate := firstToolkits[0].Candidates[0]
	markedAt := time.Date(2026, 7, 28, 3, 0, 0, 123456000, time.UTC)
	updated, err := catalog.MarkLastRun(ctx, candidate, markedAt)
	if err != nil || !updated {
		t.Fatalf("MarkLastRun() updated=%v error=%v", updated, err)
	}
	stored := readRunnerSchedule(t, pool, "p_1", 1, candidate.IndexMetaID, "-1")
	if stored.LastRun != "2026-07-28T03:00:00.123456+00:00" {
		t.Fatalf("last_run=%q", stored.LastRun)
	}
	rawStored := readRunnerScheduleObject(
		t,
		pool,
		"p_1",
		1,
		candidate.IndexMetaID,
		"-1",
	)
	if rawStored["future_schedule_field"] != "preserve-me" {
		t.Fatalf("unknown schedule field was dropped: %#v", rawStored)
	}

	// Reusing the stale candidate must not overwrite the first committed mark.
	updated, err = catalog.MarkLastRun(ctx, candidate, markedAt.Add(time.Minute))
	if err != nil || updated {
		t.Fatalf("stale MarkLastRun() updated=%v error=%v", updated, err)
	}

	edited := secondToolkits[0].Candidates[0]
	if _, err := pool.Exec(ctx, `
UPDATE p_1.elitea_tools
SET meta = jsonb_set(
    meta,
    '{indexes_meta,wiki,schedules,-1,cron}',
    '"15 5 * * *"'::jsonb
)
WHERE id = 3`); err != nil {
		t.Fatalf("edit live schedule: %v", err)
	}
	updated, err = catalog.MarkLastRun(ctx, edited, markedAt)
	if err != nil || updated {
		t.Fatalf("edited MarkLastRun() updated=%v error=%v", updated, err)
	}
	liveEdited := readRunnerSchedule(t, pool, "p_1", 3, "wiki", "-1")
	if liveEdited.Cron != "15 5 * * *" {
		t.Fatalf("last_run CAS overwrote a live edit: %+v", liveEdited)
	}

	deleteRunnerSchedule(t, pool, "p_1", 1, candidate.IndexMetaID, "12")
	refreshed := firstToolkits[0].Candidates[1]
	updated, err = catalog.MarkLastRun(ctx, refreshed, markedAt)
	if err != nil || updated {
		t.Fatalf("deleted MarkLastRun() updated=%v error=%v", updated, err)
	}
}

func prepareCurrentIndexScheduleRunnerFixtures(
	t *testing.T,
	pool *pgxpool.Pool,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
UPDATE p_1.elitea_tools
SET settings = '{"pgvector_configuration":{"private":false}}'::jsonb,
    meta = '{
      "indexes_meta": {
        "docs": {
          "schedules": {
            "-1": {
              "cron":"0 3 * * *","enabled":true,"credentials":null,
              "created_by":11,"timezone":"UTC",
              "last_run":"2026-07-27T03:00:00+00:00",
              "future_schedule_field":"preserve-me"
            },
            "12": {
              "cron":"0 4 * * *","enabled":false,"credentials":null,
              "created_by":12,"timezone":"UTC",
              "last_run":"2026-07-27T04:00:00+00:00"
            }
          }
        }
      }
    }'::jsonb
WHERE id = 1;

INSERT INTO p_1.elitea_tools (
    id, type, name, settings, author_id, meta
, created_at) VALUES
    (2, 'github', 'without-indexes', '{}'::jsonb, 11, '{}'::jsonb, now() AT TIME ZONE 'UTC'),
    (3, 'confluence', 'scheduled', '{}'::jsonb, 11, '{
      "indexes_meta": {
        "wiki": {
          "schedules": {
            "-1": {
              "cron":"0 5 * * *","enabled":true,"credentials":null,
              "created_by":11,"timezone":"UTC",
              "last_run":"2026-07-27T05:00:00+00:00"
            }
          }
        }
      }
    }'::jsonb, now() AT TIME ZONE 'UTC');

UPDATE p_2.elitea_tools
SET settings = '{"pgvector_configuration":{"private":true}}'::jsonb,
    meta = '{
      "indexes_meta": {
        "private-docs": {
          "schedules": {
            "12": {
              "cron":"0 6 * * *","enabled":true,
              "credentials":{"private":true,"elitea_title":"personal"},
              "created_by":12,"timezone":"UTC",
              "last_run":"2026-07-27T06:00:00+00:00"
            }
          }
        }
      }
    }'::jsonb
WHERE id = 1;`); err != nil {
		t.Fatalf("prepare schedule runner fixtures: %v", err)
	}
}

func readRunnerSchedule(
	t *testing.T,
	pool *pgxpool.Pool,
	schema string,
	toolkitID int64,
	indexMetaID, userID string,
) indexscheduleapp.Schedule {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var raw []byte
	query := "SELECT meta FROM " + schema + ".elitea_tools WHERE id = $1"
	if err := pool.QueryRow(ctx, query, toolkitID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	meta, err := decodeCurrentScheduleObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	indexes, _ := scheduleObjectValue(meta["indexes_meta"])
	index, _ := scheduleObjectValue(indexes[indexMetaID])
	schedules, _ := scheduleObjectValue(index["schedules"])
	schedule, err := decodeCurrentStoredSchedule(schedules[userID])
	if err != nil {
		t.Fatal(err)
	}
	return schedule
}

func readRunnerScheduleObject(
	t *testing.T,
	pool *pgxpool.Pool,
	schema string,
	toolkitID int64,
	indexMetaID, userID string,
) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var raw []byte
	query := "SELECT meta #> ARRAY['indexes_meta', $2, 'schedules', $3] FROM " +
		schema + ".elitea_tools WHERE id = $1"
	if err := pool.QueryRow(
		ctx,
		query,
		toolkitID,
		indexMetaID,
		userID,
	).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	value, err := decodeCurrentScheduleObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func deleteRunnerSchedule(
	t *testing.T,
	pool *pgxpool.Pool,
	schema string,
	toolkitID int64,
	indexMetaID, userID string,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	query := `
UPDATE ` + schema + `.elitea_tools
SET meta = jsonb_set(
    meta,
    ARRAY['indexes_meta', $2::text, 'schedules'],
    (meta #> ARRAY['indexes_meta', $2::text, 'schedules']) - $3::text
)
WHERE id = $1`
	if _, err := pool.Exec(ctx, query, toolkitID, indexMetaID, userID); err != nil {
		t.Fatal(err)
	}
}
