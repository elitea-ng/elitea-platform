package mcp

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	skillsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

func TestInternalSkillVersionSelectionAndAtomicUpdatesPostgres(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	ctx := auth.ContextWithUser(context.Background(), auth.User{UserID: "73"})
	repo := repos.NewSkillsRepo(pool)
	executor := newPostgresInternalSkillExecutor(pool)
	created, err := executor.Execute(ctx, 1, 73, internalCreateSkill, map[string]any{"name": "version-contract", "description": "Original purpose", "versions": []any{map[string]any{"name": "base", "instructions": "Base rules"}}})
	if err != nil || created.status != http.StatusCreated {
		t.Fatalf("create=%s err=%v", created.body, err)
	}
	var body map[string]any
	decodeInternalSkillResult(t, created, &body)
	skillID := scalarArgument(body["id"])
	baseID := scalarArgument(body["version_id"])
	var skillAuthor, versionAuthor int64
	if err := pool.QueryRow(ctx, `SELECT sk.author_id,sv.author_id FROM p_1.skills sk JOIN p_1.skill_versions sv ON sv.skill_id=sk.id WHERE sk.id=$1 AND sv.id=$2`, skillID, baseID).Scan(&skillAuthor, &versionAuthor); err != nil {
		t.Fatal(err)
	}
	if skillAuthor != 73 || versionAuthor != 73 {
		t.Fatalf("authors=%d/%d", skillAuthor, versionAuthor)
	}
	named, err := repo.CreateVersion(ctx, "1", skillID, skillsapi.VersionCreateInput{AuthorID: 73, Name: "reviewed", Instructions: "Reviewed rules", Tags: []string{"reviewed"}})
	if err != nil {
		t.Fatal(err)
	}
	namedID := named.VersionDetails.ID
	for _, meta := range []string{`{}`, `{"default_version_id":999999}`} {
		if _, err := pool.Exec(ctx, `UPDATE p_1.skills SET meta=$1::jsonb WHERE id=$2`, meta, skillID); err != nil {
			t.Fatal(err)
		}
		selected, err := repo.GetVersion(ctx, "1", skillID, namedID)
		if err != nil || selected.DefaultVersionID != baseID || selected.VersionDetails.ID != namedID {
			t.Fatalf("fallback selection=%+v err=%v", selected, err)
		}
	}
	// Legacy stores the configured default as a JSON number.
	if _, err := pool.Exec(ctx, `UPDATE p_1.skills SET meta=jsonb_build_object('default_version_id',$1::bigint) WHERE id=$2`, namedID, skillID); err != nil {
		t.Fatal(err)
	}
	got, err := executor.Execute(ctx, 1, 73, internalGetSkill, map[string]any{"skill_id": skillID})
	if err != nil {
		t.Fatal(err)
	}
	decodeInternalSkillResult(t, got, &body)
	if body["version_id"] != namedID || body["default_version_id"] != namedID {
		t.Fatalf("configured default=%v", body)
	}
	got, err = executor.Execute(ctx, 1, 73, internalGetSkill, map[string]any{"skill_id": skillID, "version_id": baseID})
	if err != nil {
		t.Fatal(err)
	}
	decodeInternalSkillResult(t, got, &body)
	if body["version_id"] != baseID || body["default_version_id"] != namedID {
		t.Fatalf("explicit selection=%v", body)
	}
	updated, err := executor.Execute(ctx, 1, 73, internalUpdateSkill, map[string]any{"skill_id": skillID, "description": "Updated purpose", "version": map[string]any{"instructions": "New default rules", "meta": map[string]any{"reviewed": true}}})
	if err != nil || updated.status != http.StatusOK {
		t.Fatalf("nested update=%s err=%v", updated.body, err)
	}
	named, err = repo.GetVersion(ctx, "1", skillID, namedID)
	if err != nil {
		t.Fatal(err)
	}
	base, err := repo.GetVersion(ctx, "1", skillID, baseID)
	if err != nil {
		t.Fatal(err)
	}
	if named.Instructions != "New default rules" || base.Instructions != "Base rules" || named.Description != "Updated purpose" || named.VersionDetails.Meta["reviewed"] != true {
		t.Fatalf("selected=%+v base=%+v", named, base)
	}
	updated, err = executor.Execute(ctx, 1, 73, internalUpdateSkill, map[string]any{"skill_id": skillID, "version_id": namedID, "name": "Reviewed Version", "instructions": "Flat rules", "tags": []any{}})
	if err != nil || updated.status != http.StatusOK {
		t.Fatalf("flat update=%s err=%v", updated.body, err)
	}
	decodeInternalSkillResult(t, updated, &body)
	if body["id"] != namedID || body["name"] != "Reviewed Version" {
		t.Fatalf("flat response=%v", body)
	}
	named, err = repo.GetVersion(ctx, "1", skillID, namedID)
	if err != nil {
		t.Fatal(err)
	}
	if named.Name != "version-contract" || named.Description != "Updated purpose" || named.Instructions != "Flat rules" || len(named.Tags) != 0 {
		t.Fatalf("flat persisted=%+v", named)
	}
	for _, status := range []string{"published", "embedded"} {
		if _, err := pool.Exec(ctx, `UPDATE p_1.skill_versions SET status=$1 WHERE id=$2`, status, namedID); err != nil {
			t.Fatal(err)
		}
		refused, err := executor.Execute(ctx, 1, 73, internalUpdateSkill, map[string]any{"skill_id": skillID, "description": "Must roll back", "version": map[string]any{"id": namedID, "instructions": "Forbidden"}})
		if err != nil || refused.status != http.StatusConflict {
			t.Fatalf("%s status=%d err=%v", status, refused.status, err)
		}
		fresh, err := repo.Get(ctx, "1", skillID)
		if err != nil {
			t.Fatal(err)
		}
		if fresh.Description != "Updated purpose" || fresh.Instructions != "Flat rules" {
			t.Fatalf("%s changed data: %+v", status, fresh)
		}
	}
	metadata, err := executor.Execute(ctx, 1, 73, internalUpdateSkill, map[string]any{"skill_id": skillID, "description": "Metadata remains editable"})
	if err != nil || metadata.status != http.StatusOK {
		t.Fatalf("metadata update=%s err=%v", metadata.body, err)
	}
	foreign, err := repo.Create(ctx, "1", skillsapi.Skill{AuthorID: 73, Name: "foreign-skill", Description: "Other", Instructions: "Other rules"})
	if err != nil {
		t.Fatal(err)
	}
	refused, err := executor.Execute(ctx, 1, 73, internalUpdateSkill, map[string]any{"skill_id": skillID, "version_id": foreign.VersionDetails.ID, "instructions": "Wrong owner"})
	if err != nil || refused.status != http.StatusNotFound {
		t.Fatalf("foreign status=%d err=%v", refused.status, err)
	}
}

func TestInternalSkillListDatabaseFailureIsNotEmptySuccessPostgres(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	executor := newPostgresInternalSkillExecutor(pool)
	// The project schema does not exist in this isolated test database.
	result, err := executor.Execute(context.Background(), 2147483647, 73, internalListSkills, map[string]any{})
	if err == nil || result.status == http.StatusOK {
		t.Fatal(fmt.Sprintf("failed list status=%d err=%v", result.status, err))
	}
}

func TestInternalSkillListFiltersBeforePaginationAndHidesFolderContentPostgres(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	ctx := auth.ContextWithUser(context.Background(), auth.User{UserID: "41"})
	repo := repos.NewSkillsRepo(pool)
	executor := newPostgresInternalSkillExecutor(pool)
	type seededSkill struct{ id, versionID string }
	seeded := make([]seededSkill, 0, 3)
	for _, name := range []string{"filter-a", "filter-b", "filter-hidden"} {
		skill, err := repo.Create(ctx, "1", skillsapi.Skill{AuthorID: 41, Name: name, Description: "Filter contract", Instructions: "Base", Tags: []string{"alpha"}})
		if err != nil {
			t.Fatal(err)
		}
		tags := []string{"beta"}
		if name == "filter-b" {
			tags = []string{"gamma"}
		}
		version, err := repo.CreateVersion(ctx, "1", skill.ID, skillsapi.VersionCreateInput{AuthorID: 73, Name: "reviewed", Instructions: "Private selected rules", Tags: tags})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE p_1.skill_versions SET status='published' WHERE id=$1`, version.VersionDetails.ID); err != nil {
			t.Fatal(err)
		}
		seeded = append(seeded, seededSkill{skill.ID, version.VersionDetails.ID})
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE p_1.entity_folders(id integer PRIMARY KEY);
 CREATE TABLE p_1.social_folder_items(folder_id integer,entity text,entity_id integer);
 CREATE TABLE p_1.folder_access_overrides(folder_id integer,user_id integer,access_level text);
 INSERT INTO p_1.entity_folders VALUES(1);
 INSERT INTO p_1.folder_access_overrides VALUES(1,41,'no_access');`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO p_1.social_folder_items VALUES(1,'skill',$1)`, seeded[2].id); err != nil {
		t.Fatal(err)
	}
	var alpha, beta int64
	if err := pool.QueryRow(ctx, `SELECT id FROM p_1.tags WHERE name='alpha'`).Scan(&alpha); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM p_1.tags WHERE name='beta'`).Scan(&beta); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		filters map[string]any
		total   int
		ids     []string
	}{
		{"tags across versions", map[string]any{"tags": fmt.Sprintf("%d,%d,%d", alpha, beta, alpha)}, 1, []string{seeded[0].id}},
		{"version author and status", map[string]any{"author_id": "73", "statuses": "published,unknown"}, 2, []string{seeded[0].id, seeded[1].id}},
		{"unknown status ignored", map[string]any{"statuses": "unknown"}, 2, []string{seeded[0].id, seeded[1].id}},
		{"ids expand limit", map[string]any{"ids": seeded[0].id + "," + seeded[1].id, "limit": 1}, 2, []string{seeded[0].id, seeded[1].id}},
		{"offset after visibility", map[string]any{"limit": 1, "offset": 1}, 2, []string{seeded[1].id}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.filters["query"] = "filter-"
			tc.filters["sort_by"] = "name"
			tc.filters["sort_order"] = "asc"
			result, err := executor.Execute(ctx, 1, 41, internalListSkills, tc.filters)
			if err != nil || result.status != http.StatusOK {
				t.Fatalf("list=%s err=%v", result.body, err)
			}
			var body struct {
				Total int `json:"total"`
				Rows  []struct {
					ID string `json:"id"`
				} `json:"rows"`
			}
			decodeInternalSkillResult(t, result, &body)
			if body.Total != tc.total || len(body.Rows) != len(tc.ids) {
				t.Fatalf("body=%s", result.body)
			}
			for index, id := range tc.ids {
				if body.Rows[index].ID != id {
					t.Fatalf("body=%s", result.body)
				}
			}
		})
	}
	for _, arguments := range []map[string]any{{"skill_id": seeded[2].id}, {"skill_id": seeded[2].id, "version_id": seeded[2].versionID}} {
		result, err := executor.Execute(ctx, 1, 41, internalGetSkill, arguments)
		if err != nil || result.status != http.StatusNotFound {
			t.Fatalf("hidden details=%s err=%v", result.body, err)
		}
	}
}
