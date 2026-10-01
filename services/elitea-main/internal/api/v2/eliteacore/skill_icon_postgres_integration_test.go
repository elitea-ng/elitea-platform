package eliteacore_test

// Real-PostgreSQL coverage for the half of the skill icon family that touches
// the database: binding an icon to a skill version, and unlinking it again.
//
// WHY A DATABASE IS REQUIRED HERE. The handler-level file beside this one can
// prove the BYTES round-trip, because the object store is fakeable. It cannot
// prove the icon reaches the SKILL — that is a jsonb write to
// skill_versions.meta and a read back out through repos.SkillsRepo, the query
// the skills page actually serves. Those two are written by different people at
// different times and the write can be perfectly correct while the read never
// projects the column: the row holds the icon, the PUT answers
// `{"updated": true}`, and the form still shows a placeholder. That is the
// invisible-write defect, and only a real read-back of the real query can see
// it. Each case therefore ends at SkillsRepo.Get, not at a status code.
//
// The pool helper is this package's usual shape (see
// import_tool_link_postgres_integration_test.go's header) and SKIPS without
// ELITEA_TEST_DATABASE_URL.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/migrate"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
	platformmigrations "github.com/EliteaAI/elitea-platform/services/elitea-main/migrations"
)

func newSkillIconPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	const environment = "ELITEA_TEST_DATABASE_URL"
	databaseURL := os.Getenv(environment)
	if databaseURL == "" {
		t.Skipf("set %s to run the PostgreSQL skill icon integration test", environment)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", environment, err)
	}
	adminConfig.MaxConns = 4
	adminPool, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatalf("open PostgreSQL admin pool: %v", err)
	}
	if err := adminPool.Ping(ctx); err != nil {
		adminPool.Close()
		t.Fatalf("ping PostgreSQL: %v", err)
	}

	databaseName := fmt.Sprintf("elitea_skillicon_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{databaseName}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		adminPool.Close()
		t.Fatalf("create isolated PostgreSQL integration database: %v", err)
	}

	testConfig := adminConfig.Copy()
	testConfig.ConnConfig.Database = databaseName
	testConfig.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, testConfig)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE "+quotedDatabase+" WITH (FORCE)")
		adminPool.Close()
		t.Fatalf("open isolated PostgreSQL integration database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer dropCancel()
		if _, err := adminPool.Exec(dropCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
			t.Errorf("drop isolated PostgreSQL integration database: %v", err)
		}
		adminPool.Close()
	})

	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("apply bootstrap migrations: %v", err)
	}
	runner := migrate.New(pool, platformmigrations.Files)
	if err := runner.ApplyShared(ctx); err != nil {
		t.Fatalf("apply embedded shared migrations: %v", err)
	}
	if err := runner.ApplyTenant(ctx, 1); err != nil {
		t.Fatalf("apply embedded tenant migrations: %v", err)
	}
	return pool
}

// seedSkillIconSkill inserts one skill and its `base` version, and returns both
// ids. The version name matters: every Go skills read joins skill_versions on
// name = 'base', so a version under any other name is invisible to the read
// this test asserts through.
func seedSkillIconSkill(t *testing.T, pool *pgxpool.Pool, name string) (skillID, versionID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var skill, version int
	if err := pool.QueryRow(ctx,
		`INSERT INTO p_1.skills (name, description, owner_id, author_id) VALUES ($1, 'seed', 1, 1) RETURNING id`,
		name).Scan(&skill); err != nil {
		t.Fatalf("seed skill: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO p_1.skill_versions (skill_id, name, instructions, author_id, status)
		 VALUES ($1, 'base', 'do the thing', 1, 'draft') RETURNING id`,
		skill).Scan(&version); err != nil {
		t.Fatalf("seed skill version: %v", err)
	}
	return strconv.Itoa(skill), strconv.Itoa(version)
}

// readSkillIconMeta reads the icon back through repos.SkillsRepo.Get — the
// query the skills page serves — and returns version_details.meta.icon_meta.
func readSkillIconMeta(t *testing.T, pool *pgxpool.Pool, skillID string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(auth.ContextWithUser(context.Background(), auth.User{UserID: "1"}), 15*time.Second)
	defer cancel()

	skill, err := repos.NewSkillsRepo(pool).Get(ctx, "1", skillID)
	if err != nil {
		t.Fatalf("read skill back: %v", err)
	}
	if skill.VersionDetails == nil {
		t.Fatalf("skill %s came back with no version_details", skillID)
	}
	iconMeta, _ := skill.VersionDetails.Meta["icon_meta"].(map[string]any)
	return iconMeta
}

func TestSkillIconUploadWithAVersionIsReadableOnTheSkill(t *testing.T) {
	pool := newSkillIconPool(t)
	store := newListableIconStore()
	h := eliteacore.NewHandler(pool, eliteacore.WithObjectStore(store))
	skillID, versionID := seedSkillIconSkill(t, pool, "iconned")

	rec := httptest.NewRecorder()
	req := skillIconUploadRequest(t, "1", "logo.png", []byte("bytes"), nil)
	setURLParam(req, "versionId", versionID)
	h.UploadSkillIcon(rec, req)
	assertStatus(t, rec, http.StatusOK)
	uploaded := decodeObj(t, rec)

	// The read-back that matters: through the skills query, not the response.
	iconMeta := readSkillIconMeta(t, pool, skillID)
	if iconMeta["name"] != uploaded["name"] || iconMeta["url"] != uploaded["url"] {
		t.Fatalf("skill's icon_meta = %v, want the uploaded %v", iconMeta, uploaded)
	}

	// And the url the skill now carries must serve the bytes.
	name, _ := iconMeta["name"].(string)
	downloadRec := httptest.NewRecorder()
	eliteacore.DownloadIcon(store).ServeHTTP(downloadRec,
		newRequest(http.MethodGet, "/", map[string]string{"projectID": "1", "filename": name}, nil))
	if downloadRec.Code != http.StatusOK || downloadRec.Body.String() != "bytes" {
		t.Fatalf("download of the skill's own icon = %d %q", downloadRec.Code, downloadRec.Body.String())
	}
}

func TestSkillIconUpdateBindsAnAlreadyUploadedIcon(t *testing.T) {
	pool := newSkillIconPool(t)
	store := newListableIconStore()
	h := eliteacore.NewHandler(pool, eliteacore.WithObjectStore(store))
	skillID, versionID := seedSkillIconSkill(t, pool, "picked")

	// The picker's two-step flow: upload with no version, then PUT the meta.
	uploaded := uploadOneSkillIcon(t, h, "1", "picked.png", []byte("bytes"))
	if got := readSkillIconMeta(t, pool, skillID); got != nil {
		t.Fatalf("upload without a version bound an icon anyway: %v", got)
	}

	body := fmt.Sprintf(`{"name":%q,"url":%q}`, uploaded["name"], uploaded["url"])
	rec := httptest.NewRecorder()
	h.UpdateSkillIcon(rec, newRequest(http.MethodPut, "/",
		map[string]string{"projectID": "1", "versionId": versionID}, strings.NewReader(body)))
	assertStatus(t, rec, http.StatusOK)
	if updated := decodeObj(t, rec)["updated"]; updated != true {
		t.Fatalf("response = %v, want {\"updated\": true}", updated)
	}

	iconMeta := readSkillIconMeta(t, pool, skillID)
	if iconMeta["name"] != uploaded["name"] || iconMeta["url"] != uploaded["url"] {
		t.Fatalf("skill's icon_meta = %v, want the PUT payload", iconMeta)
	}
}

// TestSkillIconUpdateRefusesAVersionThatDoesNotExist is the case a blind UPDATE
// passes: it matches no row, changes nothing, and — without RowsAffected — still
// answers "saved".
func TestSkillIconUpdateRefusesAVersionThatDoesNotExist(t *testing.T) {
	pool := newSkillIconPool(t)
	h := eliteacore.NewHandler(pool, eliteacore.WithObjectStore(newListableIconStore()))

	rec := httptest.NewRecorder()
	h.UpdateSkillIcon(rec, newRequest(http.MethodPut, "/",
		map[string]string{"projectID": "1", "versionId": "999999"},
		strings.NewReader(`{"name":"skill_x.png","url":"/icons/1/skill_x.png"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an absent version", rec.Code)
	}
}

func TestSkillIconDeleteUnlinksEverySkillWearingIt(t *testing.T) {
	pool := newSkillIconPool(t)
	store := newListableIconStore()
	h := eliteacore.NewHandler(pool, eliteacore.WithObjectStore(store))

	firstSkill, firstVersion := seedSkillIconSkill(t, pool, "first")
	secondSkill, secondVersion := seedSkillIconSkill(t, pool, "second")
	keptSkill, keptVersion := seedSkillIconSkill(t, pool, "kept")

	shared := uploadOneSkillIcon(t, h, "1", "shared.png", []byte("bytes"))
	other := uploadOneSkillIcon(t, h, "1", "other.png", []byte("bytes"))
	for _, binding := range []struct {
		versionID string
		meta      map[string]any
	}{
		{firstVersion, shared}, {secondVersion, shared}, {keptVersion, other},
	} {
		rec := httptest.NewRecorder()
		h.UpdateSkillIcon(rec, newRequest(http.MethodPut, "/",
			map[string]string{"projectID": "1", "versionId": binding.versionID},
			strings.NewReader(fmt.Sprintf(`{"name":%q,"url":%q}`, binding.meta["name"], binding.meta["url"]))))
		assertStatus(t, rec, http.StatusOK)
	}

	rec := httptest.NewRecorder()
	h.DeleteSkillIcon(rec, newRequest(http.MethodDelete, "/",
		map[string]string{"projectID": "1", "name": shared["name"].(string)}, nil))
	assertStatus(t, rec, http.StatusOK)

	// Both wearers revert to the default icon...
	for _, skillID := range []string{firstSkill, secondSkill} {
		if got := readSkillIconMeta(t, pool, skillID); len(got) != 0 {
			t.Errorf("skill %s still wears the deleted icon: %v", skillID, got)
		}
	}
	// ...and the skill wearing a DIFFERENT icon is untouched. Without this the
	// unlink could be a blanket wipe and every assertion above would still pass.
	if got := readSkillIconMeta(t, pool, keptSkill); got["name"] != other["name"] {
		t.Errorf("unrelated skill lost its icon: %v", got)
	}
}
