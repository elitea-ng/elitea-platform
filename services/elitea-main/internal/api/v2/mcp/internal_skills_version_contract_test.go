package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	skillsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
)

func TestInternalSkillGetSelectsVersionWithoutChangingDefault(t *testing.T) {
	version := skillsapi.SkillVersion{ID: "12", Name: "reviewed", Instructions: "Reviewed rules", Tags: []string{"reviewed"}}
	repo := &fakeInternalSkillsRepo{getResult: skillsapi.Skill{ID: "7", DefaultVersionID: "11", VersionDetails: &version}}
	executor := &repositoryInternalSkillExecutor{repo: repo}
	result, err := executor.Execute(context.Background(), 9, 41, internalGetSkill, map[string]any{"skill_id": json.Number("7"), "version_id": json.Number("12")})
	if err != nil || result.status != http.StatusOK {
		t.Fatalf("get status=%d err=%v", result.status, err)
	}
	var body map[string]any
	decodeInternalSkillResult(t, result, &body)
	if repo.versionID != "12" || body["default_version_id"] != "11" || body["version_id"] != "12" {
		t.Fatalf("version identity=%v", body)
	}
	for _, raw := range []any{json.Number("99"), json.Number("0"), "malformed"} {
		result, err = executor.Execute(context.Background(), 9, 41, internalGetSkill, map[string]any{"skill_id": json.Number("7"), "version_id": raw})
		if err != nil || (result.status != http.StatusBadRequest && result.status != http.StatusNotFound) {
			t.Fatalf("invalid selector %v status=%d err=%v", raw, result.status, err)
		}
	}
}

func TestInternalSkillFlatVersionUpdateKeepsSkillMetadata(t *testing.T) {
	version := skillsapi.SkillVersion{ID: "12", Name: "reviewed", Instructions: "Original rules", Tags: []string{"original"}}
	repo := &fakeInternalSkillsRepo{getResult: skillsapi.Skill{ID: "7", Name: "skill-name", Description: "Skill purpose", Instructions: version.Instructions, Tags: version.Tags, VersionDetails: &version}}
	executor := &repositoryInternalSkillExecutor{repo: repo}
	result, err := executor.Execute(context.Background(), 9, 41, internalUpdateSkill, map[string]any{"skill_id": json.Number("7"), "version_id": json.Number("12"), "name": "Reviewed Version", "instructions": "New rules", "meta": map[string]any{"reviewed": true}})
	if err != nil || result.status != http.StatusOK {
		t.Fatalf("flat update status=%d err=%v body=%s", result.status, err, result.body)
	}
	if repo.updateVersionCalls != 1 || repo.updateCalls != 0 || repo.updated.Name != "skill-name" || repo.updated.Description != "Skill purpose" || repo.updated.VersionDetails.Name != "Reviewed Version" || repo.updated.Instructions != "New rules" {
		t.Fatalf("flat update=%+v", repo.updated)
	}
	var body map[string]any
	decodeInternalSkillResult(t, result, &body)
	if body["id"] != "12" || body["name"] != "Reviewed Version" {
		t.Fatalf("flat response=%v", body)
	}
	if _, ok := body["version_details"]; ok {
		t.Fatalf("flat response contains a skill envelope: %v", body)
	}
}

func TestInternalSkillMetadataUpdateDoesNotWriteVersion(t *testing.T) {
	version := skillsapi.SkillVersion{ID: "12", Name: "published", Instructions: "Frozen", Status: "published"}
	repo := &fakeInternalSkillsRepo{getResult: skillsapi.Skill{ID: "7", Name: "skill-name", Description: "Old", VersionDetails: &version}}
	executor := &repositoryInternalSkillExecutor{repo: repo}
	result, err := executor.Execute(context.Background(), 9, 41, internalUpdateSkill, map[string]any{"skill_id": json.Number("7"), "description": "New"})
	if err != nil || result.status != http.StatusOK || !repo.updated.MetadataOnly || repo.updateCalls != 1 {
		t.Fatalf("metadata status=%d update=%+v err=%v", result.status, repo.updated, err)
	}
}

func TestInternalSkillUpdateRejectsMixedShapesBeforeMutation(t *testing.T) {
	for _, arguments := range []map[string]any{
		{"version_id": json.Number("12"), "version": map[string]any{"instructions": "Wrong shape"}},
		{"version_id": json.Number("12"), "description": "Not version metadata"},
		{"instructions": "Missing selector"},
		{"version": nil},
	} {
		arguments["skill_id"] = json.Number("7")
		repo := &fakeInternalSkillsRepo{}
		result, err := (&repositoryInternalSkillExecutor{repo: repo}).Execute(context.Background(), 9, 41, internalUpdateSkill, arguments)
		if err != nil || result.status != http.StatusBadRequest || repo.updateCalls != 0 || repo.updateVersionCalls != 0 {
			t.Fatalf("arguments=%v result=%+v err=%v", arguments, result, err)
		}
	}
}

func TestInternalSkillListPropagatesRepositoryFailure(t *testing.T) {
	repo := &fakeInternalSkillsRepo{listErr: errors.New("database unavailable")}
	result, err := (&repositoryInternalSkillExecutor{repo: repo}).Execute(context.Background(), 9, 41, internalListSkills, map[string]any{})
	if err == nil || result.status == http.StatusOK {
		t.Fatalf("failed list status=%d err=%v", result.status, err)
	}
}

func TestInternalSkillCatalogIncludesVersionSelectorsWithoutCreateVersion(t *testing.T) {
	tools := internalSkillTools()
	if len(tools) != 6 {
		t.Fatalf("skill tool count=%d", len(tools))
	}
	for _, tool := range tools {
		if tool.Name == "post_elitea_core_skill" {
			t.Fatal("version creation is not opted into MCP")
		}
		if tool.Name == "get_elitea_core_skill" || tool.Name == "put_elitea_core_skill" {
			if _, ok := tool.InputSchema["properties"].(map[string]any)["version_id"]; !ok {
				t.Fatalf("%s lacks version_id", tool.Name)
			}
		}
	}
}

func TestInternalSkillListAdmitsLegacyFiltersAndBounds(t *testing.T) {
	repo := &fakeInternalSkillsRepo{}
	executor := &repositoryInternalSkillExecutor{repo: repo}
	result, err := executor.Execute(context.Background(), 9, 41, internalListSkills, map[string]any{"ids": "7,8,7", "tags": "11,12", "author_id": json.Number("73"), "statuses": "published,unknown", "limit": json.Number("25"), "offset": json.Number("3")})
	if err != nil || result.status != http.StatusOK {
		t.Fatalf("list=%s err=%v", result.body, err)
	}
	params := repo.listParams
	if len(params.IDs) != 2 || len(params.TagIDs) != 2 || params.AuthorID != 73 || len(params.Statuses) != 1 || params.Statuses[0] != "published" || params.Limit != 25 || params.Offset != 3 {
		t.Fatalf("filters=%+v", params)
	}
	for _, arguments := range []map[string]any{{"ids": "1,not-an-id"}, {"tags": "0"}, {"ids": []any{1}}, {"author_id": json.Number("0")}, {"limit": 1001}, {"offset": 100001}} {
		result, err := executor.Execute(context.Background(), 9, 41, internalListSkills, arguments)
		if err != nil || result.status != http.StatusBadRequest {
			t.Fatalf("invalid filters=%v result=%s err=%v", arguments, result.body, err)
		}
	}
}
