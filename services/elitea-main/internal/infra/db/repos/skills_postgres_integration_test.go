package repos

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
)

// newSkillsTestPool returns an isolated, fully migrated PostgreSQL pool with
// tenant schema p_1 ready to use (the baseline migration creates it via
// `SELECT create_tenant_schema('p_1')`, see migrations/001_initial.sql).
// Skipped unless ELITEA_TEST_DATABASE_URL is set, matching every other
// *_postgres_integration_test.go in this package.
func newSkillsTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newPostgresIntegrationPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("run baseline migrations: %v", err)
	}
	return pool
}

func TestSkillsRepoPostgres_CreateReadRoundTrip(t *testing.T) {
	pool := newSkillsTestPool(t)
	repo := NewSkillsRepo(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	created, err := repo.Create(ctx, "1", skills.Skill{
		Name:         "Code Reviewer",
		Description:  "Reviews code for bugs",
		Instructions: "Always check for security issues.",
		Tags:         []string{"quality", "review"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected a generated skill id")
	}
	if created.Instructions != "Always check for security issues." {
		t.Errorf("create response instructions=%q", created.Instructions)
	}
	wantTags := []string{"quality", "review"}
	gotTags := append([]string(nil), created.Tags...)
	sort.Strings(gotTags)
	if !reflect.DeepEqual(gotTags, wantTags) {
		t.Errorf("create response tags=%v, want %v", gotTags, wantTags)
	}
	if created.VersionDetails == nil || created.VersionDetails.Name != "base" {
		t.Fatalf("expected a base version_details, got %+v", created.VersionDetails)
	}

	// Read back — instructions/tags must round-trip through skill_versions
	// and skill_version_tag_association, not just live on the create response.
	fetched, err := repo.Get(ctx, "1", created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if fetched.Name != "Code Reviewer" || fetched.Description != "Reviews code for bugs" {
		t.Errorf("fetched skill=%+v", fetched)
	}
	if fetched.Instructions != "Always check for security issues." {
		t.Errorf("fetched instructions=%q", fetched.Instructions)
	}
	gotTags = append([]string(nil), fetched.Tags...)
	sort.Strings(gotTags)
	if !reflect.DeepEqual(gotTags, wantTags) {
		t.Errorf("fetched tags=%v, want %v", gotTags, wantTags)
	}
	if len(fetched.Versions) != 1 || fetched.Versions[0].Name != "base" {
		t.Errorf("fetched versions=%+v", fetched.Versions)
	}
	if fetched.VersionDetails == nil || fetched.VersionDetails.Instructions != fetched.Instructions {
		t.Errorf("fetched version_details=%+v", fetched.VersionDetails)
	}
}

func TestSkillsRepoPostgres_UpdateReplacesTags(t *testing.T) {
	pool := newSkillsTestPool(t)
	repo := NewSkillsRepo(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	created, err := repo.Create(ctx, "1", skills.Skill{
		Name: "Doc Writer", Description: "Writes docs",
		Instructions: "Be concise.", Tags: []string{"docs", "writing"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	updated, err := repo.Update(ctx, "1", created.ID, skills.Skill{
		Name: "Doc Writer", Description: "Writes docs",
		Instructions: "Be thorough.", Tags: []string{"writing", "clarity"},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Instructions != "Be thorough." {
		t.Errorf("updated instructions=%q", updated.Instructions)
	}

	fetched, err := repo.Get(ctx, "1", created.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	gotTags := append([]string(nil), fetched.Tags...)
	sort.Strings(gotTags)
	wantTags := []string{"clarity", "writing"}
	if !reflect.DeepEqual(gotTags, wantTags) {
		t.Errorf("after update tags=%v, want %v (the old 'docs' tag association must be replaced, not merged)", gotTags, wantTags)
	}

	// The shared "writing" tag row must be reused, not duplicated — tags.name is UNIQUE.
	var tagCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM p_1.tags WHERE name = 'writing'`).Scan(&tagCount); err != nil {
		t.Fatalf("count writing tag: %v", err)
	}
	if tagCount != 1 {
		t.Errorf("expected exactly one 'writing' tag row, got %d", tagCount)
	}
}

func TestSkillsRepoPostgres_SearchAndSort(t *testing.T) {
	pool := newSkillsTestPool(t)
	repo := NewSkillsRepo(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for _, sk := range []skills.Skill{
		{Name: "Alpha Reviewer", Description: "reviews things"},
		{Name: "Beta Helper", Description: "helps with alpha tasks"},
		{Name: "Gamma Writer", Description: "writes content"},
	} {
		if _, err := repo.Create(ctx, "1", sk); err != nil {
			t.Fatalf("create %q: %v", sk.Name, err)
		}
	}

	// Search matches name OR description (ILIKE), case-insensitive.
	resp, err := repo.List(ctx, "1", skills.ListParams{Page: 1, PageSize: 20, Query: "alpha"})
	if err != nil {
		t.Fatalf("list search: %v", err)
	}
	if resp.Total != 2 {
		t.Fatalf("expected 2 matches for 'alpha' (name+description), got %d: %+v", resp.Total, resp.Items)
	}

	// Sort by name ascending must actually order results.
	resp, err = repo.List(ctx, "1", skills.ListParams{Page: 1, PageSize: 20, SortBy: "name", SortOrder: "asc"})
	if err != nil {
		t.Fatalf("list sort asc: %v", err)
	}
	if len(resp.Items) != 3 || resp.Items[0].Name != "Alpha Reviewer" || resp.Items[2].Name != "Gamma Writer" {
		t.Fatalf("unexpected asc order: %+v", namesOf(resp.Items))
	}

	resp, err = repo.List(ctx, "1", skills.ListParams{Page: 1, PageSize: 20, SortBy: "name", SortOrder: "desc"})
	if err != nil {
		t.Fatalf("list sort desc: %v", err)
	}
	if len(resp.Items) != 3 || resp.Items[0].Name != "Gamma Writer" || resp.Items[2].Name != "Alpha Reviewer" {
		t.Fatalf("unexpected desc order: %+v", namesOf(resp.Items))
	}
}

func namesOf(items []skills.Skill) []string {
	names := make([]string, len(items))
	for i, it := range items {
		names[i] = it.Name
	}
	return names
}

func TestSkillsRepoPostgres_GetByName(t *testing.T) {
	pool := newSkillsTestPool(t)
	repo := NewSkillsRepo(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := repo.Create(ctx, "1", skills.Skill{Name: "Unique Skill", Description: "d"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	found, ok, err := repo.GetByName(ctx, "1", "Unique Skill")
	if err != nil || !ok {
		t.Fatalf("expected to find skill by name, ok=%v err=%v", ok, err)
	}
	if found.Name != "Unique Skill" {
		t.Errorf("found=%+v", found)
	}

	_, ok, err = repo.GetByName(ctx, "1", "No Such Skill")
	if err != nil {
		t.Fatalf("get by name (missing): %v", err)
	}
	if ok {
		t.Error("expected ok=false for a name that doesn't exist")
	}
}

func TestSkillsRepoPostgres_DeleteCascadesVersionsAndTags(t *testing.T) {
	pool := newSkillsTestPool(t)
	repo := NewSkillsRepo(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	created, err := repo.Create(ctx, "1", skills.Skill{Name: "Temp Skill", Description: "d", Instructions: "i", Tags: []string{"x"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := repo.Delete(ctx, "1", created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := repo.Get(ctx, "1", created.ID); err == nil {
		t.Error("expected the skill to be gone after delete")
	}

	var versionCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM p_1.skill_versions WHERE skill_id = $1`, created.ID).Scan(&versionCount); err != nil {
		t.Fatalf("count skill_versions: %v", err)
	}
	if versionCount != 0 {
		t.Errorf("expected skill_versions to cascade-delete, got %d rows", versionCount)
	}
}

// TestSkillsRepoPostgres_DeleteRefusesWhilePublished — the cascade above is
// exactly why this guard exists (#249). A published skill's catalog copy lives
// in the public project and is retracted through the SOURCE skill, so deleting
// the source would strand the catalog entry with no way left to unpublish it.
func TestSkillsRepoPostgres_DeleteRefusesWhilePublished(t *testing.T) {
	pool := newSkillsTestPool(t)
	repo := NewSkillsRepo(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	created, err := repo.Create(ctx, "1", skills.Skill{Name: "Published Skill", Description: "d", Instructions: "i", Tags: []string{"x"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The publish surface writes an ADDITIONAL version with status
	// 'published'; the 'base' version stays a draft.
	if _, err := pool.Exec(ctx, `
		INSERT INTO p_1.skill_versions (skill_id, name, instructions, author_id, status)
		VALUES ($1, 'v1.0-release', 'i', 1, 'published')`, created.ID); err != nil {
		t.Fatalf("seed published version: %v", err)
	}

	err = repo.Delete(ctx, "1", created.ID)
	if err == nil {
		t.Fatal("delete succeeded while a version is published")
	}
	if !strings.Contains(err.Error(), "Unpublish first") {
		t.Errorf("delete error = %v, want the Unpublish-first refusal", err)
	}
	var skillCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM p_1.skills WHERE id = $1`, created.ID).Scan(&skillCount); err != nil {
		t.Fatalf("count skills: %v", err)
	}
	if skillCount != 1 {
		t.Errorf("skill rows after the refused delete = %d, want 1", skillCount)
	}

	// Once nothing is published, the ordinary delete works again.
	if _, err := pool.Exec(ctx, `DELETE FROM p_1.skill_versions WHERE skill_id = $1 AND status = 'published'`, created.ID); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if err := repo.Delete(ctx, "1", created.ID); err != nil {
		t.Fatalf("delete after unpublish: %v", err)
	}
}

// NOTE(#395): TestSkillsRepoPostgres_ListForApplicationVersionScopesToTheVersion
// stood here, together with its skillNames helper. It was the acceptance test
// for #367 against SkillsRepo.ListForApplicationVersion, and #395 deleted that
// method with the prototype route it served.
//
// The guarantee it measured is still measured, on the read that answers now:
// TestCurrentApplicationSkillsRoutePostgresContractAndTenantIsolation
// (internal/api/v2/applicationskills/postgres_integration_test.go) seeds one
// agent version, a second agent version and a `pipeline` row on the SAME
// entity_version_id, then asserts the answer holds only the first version's
// agent skills.
