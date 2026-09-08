package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/applications"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

func versionSkillRouter(t *testing.T, pool *pgxpool.Pool, allow bool) chi.Router {
	t.Helper()
	tools, err := (postgresToolSource{}).tools(context.Background(), `"p_1"`, scope{kind: scopeCategory, category: internalApplicationsCategory})
	if err != nil {
		t.Fatal(err)
	}
	permissions := &recordingInternalPermissionResolver{resolution: auth.PermissionResolution{UserID: 41}}
	if allow {
		permissions.resolution.Permissions = []string{"models.applications.versions.create"}
	}
	return internalApplicationRouter(t, tools[3], newPostgresInternalApplicationExecutor(pool), permissions)
}

func callCreateVersion(t *testing.T, router chi.Router, args map[string]any) map[string]any {
	t.Helper()
	wire, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "post_elitea_core_versions", "arguments": args},
	})
	if err != nil {
		t.Fatal(err)
	}
	return resultOf(t, post(t, router, "/app/1/mcp/elitea_core/applications", string(wire)))
}

func createdVersionID(t *testing.T, result map[string]any) string {
	t.Helper()
	if result["isError"] == true {
		t.Fatalf("create failed: %s", textOf(t, result))
	}
	var version map[string]any
	if err := json.Unmarshal([]byte(textOf(t, result)), &version); err != nil {
		t.Fatal(err)
	}
	if _, exists := version["copy_skills_from_version_id"]; exists {
		t.Fatal("copy option is not a version field")
	}
	return scalarArgument(version["id"])
}

type copiedSkillBinding struct {
	EntityType     string
	SkillID        int64
	SkillVersionID *int64
}

func versionSkillBindings(t *testing.T, pool *pgxpool.Pool, versionID string) []copiedSkillBinding {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT entity_type, skill_id, skill_version_id
		FROM p_1.entity_skill_mapping WHERE entity_version_id = $1 ORDER BY id`, versionID)
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := pgx.CollectRows(rows, pgx.RowToStructByPos[copiedSkillBinding])
	if err != nil {
		t.Fatal(err)
	}
	return bindings
}

func TestInternalCreateVersionCopiesExactSkills(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	appID, sourceID := seedInternalApplicationVersion(t, pool)
	ctx := context.Background()
	// Five bindings, including an unpinned version and a pipeline binding. The
	// already valid source is copied, not run through new-attachment validation.
	for i := 0; i < 4; i++ {
		var skillID int64
		if err := pool.QueryRow(ctx, `INSERT INTO p_1.skills (name, description, owner_id, author_id)
			VALUES ($1, 'fixture', 1, 7) RETURNING id`, fmt.Sprintf("copy-skill-%d", i)).Scan(&skillID); err != nil {
			t.Fatal(err)
		}
		var versionID int64
		if err := pool.QueryRow(ctx, `INSERT INTO p_1.skill_versions (skill_id, name, instructions, author_id)
			VALUES ($1, 'pinned', 'Exact revision.', 7) RETURNING id`, skillID).Scan(&versionID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO p_1.entity_skill_mapping
			(entity_version_id, entity_type, skill_id, skill_version_id)
			VALUES ($1, $2, $3, NULLIF($4::integer, 0))`, sourceID, []string{"agent", "pipeline"}[i%2], skillID, versionID*int64(i%2)); err != nil {
			t.Fatal(err)
		}
	}
	source := strconv.FormatInt(sourceID, 10)
	want := versionSkillBindings(t, pool, source)
	if len(want) != 5 {
		t.Fatalf("fixture has %d bindings", len(want))
	}
	router := versionSkillRouter(t, pool, true)
	for _, agentType := range []string{"openai", "pipeline"} {
		t.Run(agentType, func(t *testing.T) {
			id := createdVersionID(t, callCreateVersion(t, router, map[string]any{
				"application_id": appID, "name": "copy-" + agentType, "agent_type": agentType,
				"copy_skills_from_version_id": sourceID,
			}))
			if got := versionSkillBindings(t, pool, id); !reflect.DeepEqual(got, want) {
				t.Fatalf("bindings = %+v, want %+v", got, want)
			}
			// Wrong-application cleanup must not remove these bindings.
			repo := repos.NewApplicationsRepo(pool)
			if err := repo.DeleteVersion(ctx, "1", "99999", id); err == nil {
				t.Fatal("foreign application deleted a version")
			}
			if got := versionSkillBindings(t, pool, id); !reflect.DeepEqual(got, want) {
				t.Fatal("foreign cleanup removed bindings")
			}
			if err := repo.DeleteVersion(ctx, "1", strconv.FormatInt(appID, 10), id); err != nil {
				t.Fatal(err)
			}
			if got := versionSkillBindings(t, pool, id); len(got) != 0 {
				t.Fatal("deleted version left orphaned bindings")
			}
		})
	}
	if got := versionSkillBindings(t, pool, source); !reflect.DeepEqual(got, want) {
		t.Fatal("copy or cleanup changed the source")
	}
}

func TestInternalCreateVersionIgnoresInvalidSkillSources(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	appID, sourceID := seedInternalApplicationVersion(t, pool)
	var foreignApp, foreignVersion int64
	if err := pool.QueryRow(context.Background(), `INSERT INTO p_1.applications (name, owner_id)
		VALUES ('foreign', 1) RETURNING id`).Scan(&foreignApp); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `INSERT INTO p_1.application_versions (application_id, name, author_id)
		VALUES ($1, 'base', 7) RETURNING id`, foreignApp).Scan(&foreignVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO p_1.entity_skill_mapping
		(entity_version_id, entity_type, skill_id, skill_version_id)
		SELECT $1, entity_type, skill_id, skill_version_id FROM p_1.entity_skill_mapping
		WHERE entity_version_id = $2`, foreignVersion, sourceID); err != nil {
		t.Fatal(err)
	}
	router := versionSkillRouter(t, pool, true)
	for i, source := range []any{nil, "bad", 0, -1, 2147483648, 1.5, true, map[string]any{}, 999999, foreignVersion} {
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			id := createdVersionID(t, callCreateVersion(t, router, map[string]any{
				"application_id": appID, "name": fmt.Sprintf("case-%d", i), "copy_skills_from_version_id": source,
			}))
			if len(versionSkillBindings(t, pool, id)) != 0 {
				t.Fatal("invalid or foreign source copied bindings")
			}
		})
	}
}

func TestInternalCreateVersionSkillCopyRollback(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	appID, sourceID := seedInternalApplicationVersion(t, pool)
	ctx := context.Background()
	router := versionSkillRouter(t, pool, true)
	for _, table := range []string{"entity_skill_mapping", "application_variables", "application_version_tag_association"} {
		t.Run(table, func(t *testing.T) {
			// Only an isolated fixture contains this failure trigger.
			_, err := pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION p_1.reject_copy() RETURNS trigger
				LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'copy fixture failure'; END; $$;
				CREATE TRIGGER reject_copy BEFORE INSERT ON p_1.%s
				FOR EACH ROW EXECUTE FUNCTION p_1.reject_copy()`, table))
			if err != nil {
				t.Fatal(err)
			}
			result := callCreateVersion(t, router, map[string]any{
				"application_id": appID, "name": "must-rollback", "copy_skills_from_version_id": sourceID,
				"variables": []any{map[string]any{"name": "copy", "value": "value"}},
				"tags":      []any{map[string]any{"name": "copy"}},
			})
			if result["isError"] != true {
				t.Fatalf("injected failure was accepted: %v", result)
			}
			var versions, bindings int
			if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM p_1.application_versions),
				(SELECT count(*) FROM p_1.entity_skill_mapping)`).Scan(&versions, &bindings); err != nil {
				t.Fatal(err)
			}
			if versions != 1 || bindings != 1 {
				t.Fatalf("partial copy survived: versions=%d bindings=%d", versions, bindings)
			}
			if _, err := pool.Exec(ctx, fmt.Sprintf(`DROP TRIGGER reject_copy ON p_1.%s; DROP FUNCTION p_1.reject_copy()`, table)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInternalCreateVersionSkillCopyRequiresPermission(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	appID, sourceID := seedInternalApplicationVersion(t, pool)
	result := callCreateVersion(t, versionSkillRouter(t, pool, false), map[string]any{
		"application_id": appID, "name": "denied", "copy_skills_from_version_id": sourceID,
	})
	if result["isError"] != true {
		t.Fatal("missing create-version permission was accepted")
	}
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM p_1.application_versions`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("denied copy changed versions: count=%d err=%v", count, err)
	}
}

func TestInternalCreateVersionSkillsStayInTenantAndReleaseConnection(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	appID, sourceID := seedInternalApplicationVersion(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO centry.project (id, name, owner_id) VALUES (2, 'Skill copy fixture', 41);
		SELECT create_tenant_schema('p_2')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.New(pool, platformmigrations.Files).ApplyTenant(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO p_2.applications (id, name, owner_id) VALUES ($1, 'Other project', 2)`, appID); err != nil {
		t.Fatal(err)
	}
	// A single connection proves that the next request cannot inherit p_2.
	config := pool.Config().Copy()
	config.MaxConns = 1
	single, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer single.Close()
	var before, after string
	if err := single.QueryRow(ctx, `SHOW search_path`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	repo := repos.NewApplicationsRepo(single)
	created, err := repo.CreateVersion(ctx, "2", strconv.FormatInt(appID, 10), applications.Version{
		Name: "isolated", AuthorID: 41, CopySkillsFromVersionID: int32(sourceID),
	})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := single.QueryRow(ctx, `SELECT count(*) FROM p_2.entity_skill_mapping WHERE entity_version_id=$1`, created.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("copied foreign-project skills: count=%d err=%v", count, err)
	}
	if err := repo.DeleteVersion(ctx, "2", strconv.FormatInt(appID, 10), created.ID); err != nil {
		t.Fatal(err)
	}
	if err := single.QueryRow(ctx, `SHOW search_path`).Scan(&after); err != nil || after != before {
		t.Fatalf("tenant state leaked: before=%q after=%q err=%v", before, after, err)
	}
	if len(versionSkillBindings(t, pool, strconv.FormatInt(sourceID, 10))) != 1 {
		t.Fatal("other tenant cleanup changed source skills")
	}
}

func TestInternalCreateVersionSkillCopyCancellationRollsBack(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	appID, sourceID := seedInternalApplicationVersion(t, pool)
	ctx := context.Background()
	// The sequence proves that cancellation occurs inside copying, after the
	// target version INSERT. Sequence increments survive transaction rollback.
	if _, err := pool.Exec(ctx, `CREATE SEQUENCE p_1.copy_started;
		CREATE FUNCTION p_1.slow_copy() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM nextval('p_1.copy_started'); PERFORM pg_sleep(5); RETURN NEW; END; $$;
		CREATE TRIGGER slow_copy BEFORE INSERT ON p_1.entity_skill_mapping
		FOR EACH ROW EXECUTE FUNCTION p_1.slow_copy()`); err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	created, err := repos.NewApplicationsRepo(pool).CreateVersion(deadline, "1", strconv.FormatInt(appID, 10), applications.Version{
		Name: "cancelled", AuthorID: 41, CopySkillsFromVersionID: int32(sourceID),
	})
	if !errors.Is(err, context.DeadlineExceeded) || created.ID != "" {
		t.Fatalf("cancelled copy returned a version: id=%q err=%v", created.ID, err)
	}
	var versions, bindings int
	var started bool
	if err := pool.QueryRow(ctx, `SELECT (SELECT is_called FROM p_1.copy_started),
		(SELECT count(*) FROM p_1.application_versions), (SELECT count(*) FROM p_1.entity_skill_mapping)`).Scan(&started, &versions, &bindings); err != nil {
		t.Fatal(err)
	}
	if !started || versions != 1 || bindings != 1 {
		t.Fatalf("copy cancellation: started=%v versions=%d bindings=%d", started, versions, bindings)
	}
}
