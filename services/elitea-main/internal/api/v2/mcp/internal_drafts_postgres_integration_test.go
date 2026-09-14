package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	draftsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/drafts"
	skillsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

// This test uses PostgreSQL and an in-process MCP server with a fake LLM.
func TestInternalSkillDraftReadsSelectedVersionWithoutSaving(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	repo := repos.NewSkillsRepo(pool)
	ctx := auth.ContextWithUser(context.Background(), auth.User{ID: "41", UserID: "41"})
	skill, err := repo.Create(ctx, "1", skillsapi.Skill{AuthorID: 1, Name: "draft-source", Description: "Stored purpose", Instructions: "Base rules"})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := repo.CreateVersion(ctx, "1", skill.ID, skillsapi.VersionCreateInput{AuthorID: 1, Name: "reviewed", Instructions: "Selected version rules", Tags: []string{"reviewed"}})
	if err != nil {
		t.Fatal(err)
	}
	other, err := repo.Create(ctx, "1", skillsapi.Skill{AuthorID: 1, Name: "other-source", Description: "Other purpose", Instructions: "Other rules"})
	if err != nil {
		t.Fatal(err)
	}
	completer := &draftCompleter{content: `{"name":"revised-draft","description":"Revised purpose","instructions":"Revised rules"}`}
	permission := internalDraftTool(internalDraftSkill).permission
	permissions := &recordingInternalPermissionResolver{resolution: auth.PermissionResolution{UserID: 41, Permissions: []string{permission, "models.applications.skills.details"}}}
	router := draftRouter(t, internalDraftSkill, draftsapi.NewHandler(completer, draftsapi.WithSkillVersions(repo), draftsapi.WithPermissions(permissions)), permissions)
	invoke := func(skillID, versionID string) map[string]any {
		arguments, _ := json.Marshal(map[string]any{"user_description": "Revise", "skill_id": skillID, "version_id": versionID})
		return resultOf(t, post(t, router, "/app/1/mcp/elitea_core/skills", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"post_prompt_lib_generate_skill_draft","arguments":`+string(arguments)+`}}`))
	}
	result := invoke(skill.ID, selected.VersionDetails.ID)
	if result["isError"] == true || completer.calls != 1 {
		t.Fatalf("result=%v calls=%d", result, completer.calls)
	}
	prompt := completer.request.Messages[0].Content
	if !strings.Contains(prompt, "Selected version rules") || strings.Contains(prompt, "Base rules") {
		t.Fatalf("wrong source version")
	}
	persisted, err := repo.GetVersion(ctx, "1", skill.ID, selected.VersionDetails.ID)
	if err != nil || persisted.Instructions != "Selected version rules" || persisted.Name != "draft-source" || len(persisted.Versions) != 2 {
		t.Fatalf("draft changed stored skill: %+v err=%v", persisted, err)
	}
	result = invoke(skill.ID, other.VersionDetails.ID)
	if result["isError"] != true || completer.calls != 1 || !strings.Contains(textOf(t, result), "not found") {
		t.Fatalf("foreign version accepted: %v", result)
	}
	result = invoke("2147483647", selected.VersionDetails.ID)
	if result["isError"] != true || completer.calls != 1 {
		t.Fatalf("missing skill accepted: %v", result)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE p_1.entity_folders(id integer PRIMARY KEY);
 CREATE TABLE p_1.social_folder_items(folder_id integer,entity text,entity_id integer);
 CREATE TABLE p_1.folder_access_overrides(folder_id integer,user_id integer,access_level text);
 INSERT INTO p_1.entity_folders VALUES(1);
 INSERT INTO p_1.folder_access_overrides VALUES(1,41,'no_access');`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO p_1.social_folder_items VALUES(1,'skill',$1)`, skill.ID); err != nil {
		t.Fatal(err)
	}
	result = invoke(skill.ID, selected.VersionDetails.ID)
	if result["isError"] != true || completer.calls != 1 {
		t.Fatalf("hidden skill reached model: %v calls=%d", result, completer.calls)
	}

}
