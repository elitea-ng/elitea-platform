package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	applicationskillsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/applicationskills"
	skillsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
)

type fakeInternalSkillsRepo struct {
	listResult         skillsapi.ListResponse
	listErr            error
	versionID          string
	updateVersionCalls int
	attachedResult     skillsapi.ListResponse
	getResult          skillsapi.Skill
	createResult       skillsapi.Skill
	updateResult       skillsapi.Skill
	attachmentResult   skillsapi.SkillAttachment
	listParams         skillsapi.ListParams
	created            skillsapi.Skill
	updated            skillsapi.Skill
	relation           skillsapi.SkillRelation
	projectID          string
	skillID            string
	applicationVersion string
	createCalls        int
	updateCalls        int
	attachCalls        int
	detachCalls        int
}

func (repo *fakeInternalSkillsRepo) List(
	_ context.Context,
	projectID string,
	params skillsapi.ListParams,
) (skillsapi.ListResponse, error) {
	repo.projectID = projectID
	repo.listParams = params
	return repo.listResult, repo.listErr
}

func (repo *fakeInternalSkillsRepo) ListCurrentApplicationSkills(
	_ context.Context,
	projectID int32,
	applicationVersion int32,
) ([]applicationskillsapi.CurrentApplicationSkill, error) {
	repo.projectID = strconv.FormatInt(int64(projectID), 10)
	repo.applicationVersion = strconv.FormatInt(int64(applicationVersion), 10)
	var result []applicationskillsapi.CurrentApplicationSkill
	for _, skill := range repo.attachedResult.Items {
		id, _ := strconv.ParseInt(skill.ID, 10, 32)
		item := applicationskillsapi.CurrentApplicationSkill{
			SkillID: int32(id), Name: skill.Name, Description: skill.Description,
		}
		if skill.VersionDetails != nil {
			versionID, _ := strconv.ParseInt(skill.VersionDetails.ID, 10, 32)
			version := int32(versionID)
			item.VersionID = &version
			item.VersionName = skill.VersionDetails.Name
			item.Instructions = skill.VersionDetails.Instructions
		}
		result = append(result, item)
	}
	return result, nil
}

func (repo *fakeInternalSkillsRepo) AttachSkill(
	_ context.Context,
	projectID string,
	skillID string,
	relation skillsapi.SkillRelation,
) (skillsapi.SkillAttachment, error) {
	repo.attachCalls++
	repo.projectID = projectID
	repo.skillID = skillID
	repo.relation = relation
	return repo.attachmentResult, nil
}

func (repo *fakeInternalSkillsRepo) DetachSkill(
	_ context.Context,
	projectID string,
	skillID string,
	relation skillsapi.SkillRelation,
) error {
	repo.detachCalls++
	repo.projectID = projectID
	repo.skillID = skillID
	repo.relation = relation
	return nil
}

func (repo *fakeInternalSkillsRepo) Get(
	_ context.Context,
	projectID string,
	skillID string,
) (skillsapi.Skill, error) {
	repo.projectID = projectID
	repo.skillID = skillID
	return repo.getResult, nil
}

func (repo *fakeInternalSkillsRepo) GetByName(
	context.Context,
	string,
	string,
) (skillsapi.Skill, bool, error) {
	return skillsapi.Skill{}, false, nil
}

func (repo *fakeInternalSkillsRepo) Create(
	_ context.Context,
	projectID string,
	skill skillsapi.Skill,
) (skillsapi.Skill, error) {
	repo.createCalls++
	repo.projectID = projectID
	repo.created = skill
	return repo.createResult, nil
}

func (repo *fakeInternalSkillsRepo) Update(
	_ context.Context,
	projectID string,
	skillID string,
	skill skillsapi.Skill,
) (skillsapi.Skill, error) {
	repo.updateCalls++
	repo.projectID = projectID
	repo.skillID = skillID
	repo.updated = skill
	return repo.updateResult, nil
}

func (repo *fakeInternalSkillsRepo) Delete(context.Context, string, string) error {
	return nil
}

func TestInternalSkillListUsesBoundedMainFiltersAndCurrentEnvelope(t *testing.T) {
	repo := &fakeInternalSkillsRepo{listResult: skillsapi.ListResponse{
		Items: []skillsapi.Skill{{ID: "7", Name: "search-skill"}},
		Total: 1,
	}}
	executor := &repositoryInternalSkillExecutor{repo: repo}
	result, err := executor.Execute(context.Background(), 9, 41, internalListSkills, map[string]any{
		"query":      "  durable  ",
		"page":       json.Number("3"),
		"page_size":  json.Number("25"),
		"sort_by":    "name",
		"sort_order": "asc",
	})
	if err != nil {
		t.Fatalf("list skills: %v", err)
	}
	if result.status != http.StatusOK || repo.projectID != "9" {
		t.Fatalf("result status=%d project=%q body=%s", result.status, repo.projectID, result.body)
	}
	wantParams := skillsapi.ListParams{
		Page: 3, PageSize: 25, Query: "durable", SortBy: "name", SortOrder: "asc",
	}
	if !reflect.DeepEqual(repo.listParams, wantParams) {
		t.Fatalf("list params = %#v, want %#v", repo.listParams, wantParams)
	}
	var body map[string]any
	decodeInternalSkillResult(t, result, &body)
	if body["total"] != float64(1) || len(body["rows"].([]any)) != 1 {
		t.Fatalf("list body = %v", body)
	}
}

func TestInternalSkillCreateMapsCurrentNestedVersionShape(t *testing.T) {
	version := skillsapi.SkillVersion{ID: "51", Name: "base", Instructions: "Use the source.", Tags: []string{"rust"}}
	repo := &fakeInternalSkillsRepo{createResult: skillsapi.Skill{
		ID: "17", Name: "durable-worker", Description: "Worker guidance.", VersionDetails: &version,
	}}
	executor := &repositoryInternalSkillExecutor{repo: repo}
	result, err := executor.Execute(context.Background(), 9, 41, internalCreateSkill, map[string]any{
		"name":        "durable-worker",
		"description": "Worker guidance.",
		"versions": []any{map[string]any{
			"name":         "base",
			"instructions": "Use the source.",
			"tags": []any{map[string]any{
				"name": "rust",
				"data": map[string]any{"color": "orange"},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("create skill: %v", err)
	}
	if result.status != http.StatusCreated || repo.createCalls != 1 || repo.projectID != "9" {
		t.Fatalf("result status=%d calls=%d project=%q body=%s",
			result.status, repo.createCalls, repo.projectID, result.body)
	}
	if repo.created.AuthorID != 41 || repo.created.Name != "durable-worker" || repo.created.Instructions != "Use the source." ||
		!reflect.DeepEqual(repo.created.Tags, []string{"rust"}) {
		t.Fatalf("created skill = %#v", repo.created)
	}
	var body map[string]any
	decodeInternalSkillResult(t, result, &body)
	if body["default_version_id"] != "51" || body["version_id"] != "51" {
		t.Fatalf("create body = %v", body)
	}
}

func TestInternalSkillCreateRejectsReservedOrMalformedNamesBeforeMutation(t *testing.T) {
	for _, name := range []string{"Claude-helper", "anthropic-helper", "ends-", "two words", " padded "} {
		t.Run(name, func(t *testing.T) {
			repo := &fakeInternalSkillsRepo{}
			executor := &repositoryInternalSkillExecutor{repo: repo}
			result, err := executor.Execute(context.Background(), 9, 41, internalCreateSkill, map[string]any{
				"name":        name,
				"description": "description",
				"versions": []any{map[string]any{
					"name": "base", "instructions": "instructions",
				}},
			})
			if err != nil {
				t.Fatalf("validate name: %v", err)
			}
			if result.status != http.StatusBadRequest || repo.createCalls != 0 {
				t.Fatalf("name %q status=%d calls=%d body=%s", name, result.status, repo.createCalls, result.body)
			}
		})
	}
}

func FuzzInternalSkillNameMatchesCurrentGrammar(f *testing.F) {
	for _, seed := range []string{
		"a", "durable-worker", "9-lives", "-starts", "ends-", "two words",
		"claude-helper", "anthropic-helper", "UPPER", "", "a--b",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		_, err := requiredInternalSkillName(name)
		valid := independentCurrentSkillName(name)
		if (err == nil) != valid {
			t.Fatalf("name %q validity = %t, want %t", name, err == nil, valid)
		}
	})
}

func independentCurrentSkillName(name string) bool {
	characters := []rune(name)
	length := 0
	for index, character := range characters {
		length++
		isLetter := character >= 'a' && character <= 'z'
		isDigit := character >= '0' && character <= '9'
		if !isLetter && !isDigit && character != '-' {
			return false
		}
		if character == '-' && (index == 0 || index == len(characters)-1) {
			return false
		}
	}
	return length >= 1 && length <= 64 &&
		!strings.Contains(name, "claude") && !strings.Contains(name, "anthropic")
}

func TestInternalSkillUpdateMergesPartialBodyAndChecksVersionOwnership(t *testing.T) {
	version := skillsapi.SkillVersion{ID: "51", Name: "base", Instructions: "Old instructions.", Tags: []string{"old"}}
	current := skillsapi.Skill{
		ID: "17", Name: "old-name", Description: "Keep this.", Instructions: version.Instructions,
		Tags: version.Tags, VersionDetails: &version,
	}
	updatedVersion := version
	updatedVersion.Instructions = "New instructions."
	updatedVersion.Tags = []string{"new"}
	repo := &fakeInternalSkillsRepo{
		getResult: current,
		updateResult: skillsapi.Skill{
			ID: "17", Name: "new-name", Description: "Keep this.", VersionDetails: &updatedVersion,
		},
	}
	executor := &repositoryInternalSkillExecutor{repo: repo}
	result, err := executor.Execute(context.Background(), 9, 41, internalUpdateSkill, map[string]any{
		"skill_id": json.Number("17"),
		"name":     "new-name",
		"version": map[string]any{
			"id":           json.Number("51"),
			"instructions": "New instructions.",
			"tags":         []any{map[string]any{"name": "new"}},
		},
	})
	if err != nil {
		t.Fatalf("update skill: %v", err)
	}
	if result.status != http.StatusOK || repo.updateCalls != 1 {
		t.Fatalf("update status=%d calls=%d body=%s", result.status, repo.updateCalls, result.body)
	}
	if repo.updated.Name != "new-name" || repo.updated.Description != "Keep this." ||
		repo.updated.Instructions != "New instructions." || !reflect.DeepEqual(repo.updated.Tags, []string{"new"}) {
		t.Fatalf("merged update = %#v", repo.updated)
	}

	repo.updateCalls = 0
	foreign, err := executor.Execute(context.Background(), 9, 41, internalUpdateSkill, map[string]any{
		"skill_id": json.Number("17"),
		"version": map[string]any{
			"id": json.Number("99"), "instructions": "Wrong version.",
		},
	})
	if err != nil {
		t.Fatalf("validate foreign version: %v", err)
	}
	if foreign.status != http.StatusNotFound || repo.updateCalls != 0 {
		t.Fatalf("foreign version status=%d calls=%d body=%s", foreign.status, repo.updateCalls, foreign.body)
	}
}

func TestInternalSkillRelationAndAttachedListUseAgentVersionKeys(t *testing.T) {
	version := skillsapi.SkillVersion{ID: "51", Name: "base", Instructions: "Use this skill."}
	repo := &fakeInternalSkillsRepo{
		attachmentResult: skillsapi.SkillAttachment{
			SkillID: 17, SkillVersionID: 51, SkillName: "durable-worker", VersionName: "base",
		},
		attachedResult: skillsapi.ListResponse{Items: []skillsapi.Skill{{
			ID: "17", Name: "durable-worker", Description: "Worker guidance.", VersionDetails: &version,
		}}},
	}
	executor := &repositoryInternalSkillExecutor{repo: repo, attached: repo}
	attached, err := executor.Execute(context.Background(), 9, 41, internalUpdateSkillRelation, map[string]any{
		"skill_id": json.Number("17"), "entity_version_id": json.Number("33"),
		"has_relation": true, "skill_version_id": json.Number("51"),
	})
	if err != nil {
		t.Fatalf("attach skill: %v", err)
	}
	if attached.status != http.StatusCreated || repo.attachCalls != 1 ||
		repo.relation.EntityVersionID != "33" || repo.relation.SkillVersionID != "51" ||
		repo.relation.EntityType != skillsapi.SkillEntityTypeAgent {
		t.Fatalf("attach result=%s relation=%#v calls=%d", attached.body, repo.relation, repo.attachCalls)
	}

	listed, err := executor.Execute(context.Background(), 9, 41, internalListAttachedSkills, map[string]any{
		"app_version_id": json.Number("33"),
	})
	if err != nil {
		t.Fatalf("list attached skills: %v", err)
	}
	var body map[string]any
	decodeInternalSkillResult(t, listed, &body)
	if repo.applicationVersion != "33" || body["max_skills"] != float64(skillsapi.MaxSkillsPerEntityVersion) ||
		len(body["skills"].([]any)) != 1 {
		t.Fatalf("attached list project=%q version=%q body=%v", repo.projectID, repo.applicationVersion, body)
	}

	detached, err := executor.Execute(context.Background(), 9, 41, internalUpdateSkillRelation, map[string]any{
		"skill_id": json.Number("17"), "entity_version_id": json.Number("33"), "has_relation": false,
	})
	if err != nil {
		t.Fatalf("detach skill: %v", err)
	}
	if detached.status != http.StatusOK || repo.detachCalls != 1 {
		t.Fatalf("detach status=%d calls=%d body=%s", detached.status, repo.detachCalls, detached.body)
	}
}

func decodeInternalSkillResult(t *testing.T, result internalApplicationExecution, target any) {
	t.Helper()
	if err := json.Unmarshal(result.body, target); err != nil {
		t.Fatalf("decode skill result %s: %v", result.body, err)
	}
}
