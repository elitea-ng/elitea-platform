package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	skillsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/skills"
)

// Issue #6744 part 1. The MCP skill tools capped instructions at 5 000
// characters while the editor saves up to 50 000, so a skill saved in the
// editor could not be created or edited through MCP. Both the advertised
// schema and the executor now use skillsapi.SkillInstructionsMaxLength.

func createSkillArguments(instructions string) map[string]any {
	return map[string]any{
		"name":        "long-skill",
		"description": "Long instructions.",
		"versions": []any{map[string]any{
			"name":         "base",
			"instructions": instructions,
		}},
	}
}

func TestInternalSkillCreateAcceptsInstructionsUpToTheSharedLimit(t *testing.T) {
	version := skillsapi.SkillVersion{ID: "51", Name: "base"}
	repo := &fakeInternalSkillsRepo{createResult: skillsapi.Skill{ID: "17", Name: "long-skill", VersionDetails: &version}}
	executor := &repositoryInternalSkillExecutor{repo: repo}

	// Multi-byte runes: the limit counts characters, not bytes.
	atLimit := strings.Repeat("é", skillsapi.SkillInstructionsMaxLength)
	result, err := executor.Execute(context.Background(), 9, 41, internalCreateSkill, createSkillArguments(atLimit))
	if err != nil || result.status != http.StatusCreated {
		t.Fatalf("create with %d characters: status=%d err=%v body=%s",
			skillsapi.SkillInstructionsMaxLength, result.status, err, result.body)
	}
	if repo.createCalls != 1 || repo.created.Instructions != atLimit {
		t.Fatalf("create calls = %d, stored %d characters", repo.createCalls, len([]rune(repo.created.Instructions)))
	}

	overLimit := atLimit + "x"
	result, err = executor.Execute(context.Background(), 9, 41, internalCreateSkill, createSkillArguments(overLimit))
	if err != nil || result.status != http.StatusBadRequest {
		t.Fatalf("create one character over the limit: status=%d err=%v, want 400", result.status, err)
	}
	if repo.createCalls != 1 {
		t.Fatalf("a refused create reached the repository (calls = %d)", repo.createCalls)
	}
}

func TestInternalSkillToolSchemasAdvertiseTheSharedLimit(t *testing.T) {
	var advertised int
	for _, tool := range internalSkillTools() {
		encoded, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("encode %s schema: %v", tool.Name, err)
		}
		if strings.Contains(string(encoded), `"maxLength":5000,`) {
			t.Errorf("%s still advertises the old 5000 limit: %s", tool.Name, encoded)
		}
		advertised += strings.Count(string(encoded), `"maxLength":50000`)
	}
	if advertised != 3 {
		t.Fatalf("instructions properties advertising %d = %d, want 3", skillsapi.SkillInstructionsMaxLength, advertised)
	}
}
