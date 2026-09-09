package mcp

import (
	"context"
	"fmt"
	skillsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
	"net/http"
	"reflect"
	"testing"
)

func TestInternalSkillDetailPreservesEachVersionTags(t *testing.T) {
	versions := []skillsapi.SkillVersion{
		{ID: "11", Name: "base", Tags: []string{"base-tag"}},
		{ID: "12", Name: "release", Tags: []string{"release-tag", "reviewed"}},
		{ID: "13", Name: "empty", Tags: []string{}},
		{ID: "14", Name: "untagged"},
	}
	skill := skillsapi.Skill{ID: "7", Tags: versions[0].Tags, Versions: versions, VersionDetails: &versions[1]}
	result, err := internalSkillDetail(http.StatusOK, skill)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	decodeInternalSkillResult(t, result, &body)
	got := body["versions"].([]any)
	for index, version := range versions {
		tags := got[index].(map[string]any)["tags"].([]any)
		if len(tags) != len(version.Tags) {
			t.Fatalf("version %s tags = %v", version.ID, tags)
		}
		for tagIndex, name := range version.Tags {
			if tags[tagIndex].(map[string]any)["name"] != name {
				t.Fatalf("version %s tags = %v", version.ID, tags)
			}
		}
	}
	if !reflect.DeepEqual(body["version_details"].(map[string]any)["tags"], got[1].(map[string]any)["tags"]) {
		t.Fatal("selected version tags differ")
	}
	if !reflect.DeepEqual(skill.Versions[1].Tags, []string{"release-tag", "reviewed"}) {
		t.Fatal("source tags changed")
	}
}

// These operations are outside the base-only executor fixtures.
func (*fakeInternalSkillsRepo) GetVersion(context.Context, string, string, string) (skillsapi.Skill, error) {
	return skillsapi.Skill{}, fmt.Errorf("unexpected GetVersion")
}
func (*fakeInternalSkillsRepo) CreateVersion(context.Context, string, string, skillsapi.VersionCreateInput) (skillsapi.Skill, error) {
	return skillsapi.Skill{}, fmt.Errorf("unexpected CreateVersion")
}
func (*fakeInternalSkillsRepo) UpdateVersion(context.Context, string, string, string, skillsapi.Skill) (skillsapi.Skill, error) {
	return skillsapi.Skill{}, fmt.Errorf("unexpected UpdateVersion")
}
func (*fakeInternalSkillsRepo) DeleteVersion(context.Context, string, string, string) error {
	return fmt.Errorf("unexpected DeleteVersion")
}
func (*fakeInternalSkillsRepo) RestoreVersion(context.Context, string, string, string) (skillsapi.Skill, error) {
	return skillsapi.Skill{}, fmt.Errorf("unexpected RestoreVersion")
}
func (*fakeInternalSkillsRepo) SetDefaultVersion(context.Context, string, string, string) (skillsapi.Skill, error) {
	return skillsapi.Skill{}, fmt.Errorf("unexpected SetDefaultVersion")
}
