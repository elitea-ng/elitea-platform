package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func TestInternalSkillLifecyclePersistsAndChangesAgentAttachment(t *testing.T) {
	pool := newInternalApplicationsPool(t)
	_, applicationVersionID := seedInternalApplicationVersion(t, pool)
	executor := newPostgresInternalSkillExecutor(pool)
	ctx := context.Background()

	created, err := executor.Execute(ctx, 1, 73, internalCreateSkill, map[string]any{
		"name":        "durable-worker",
		"description": "Durable worker guidance.",
		"versions": []any{map[string]any{
			"name":         "base",
			"instructions": "Keep the claim fenced.",
			"tags":         []any{map[string]any{"name": "rust"}},
		}},
	})
	if err != nil || created.status != http.StatusCreated {
		t.Fatalf("create skill: status=%d error=%v body=%s", created.status, err, created.body)
	}
	var createdBody map[string]any
	decodeInternalSkillResult(t, created, &createdBody)
	skillID := scalarArgument(createdBody["id"])
	versionID := scalarArgument(createdBody["version_id"])
	if skillID == "" || versionID == "" || createdBody["default_version_id"] != createdBody["version_id"] {
		t.Fatalf("created skill identity = %v", createdBody)
	}

	updated, err := executor.Execute(ctx, 1, 73, internalUpdateSkill, map[string]any{
		"skill_id": skillID,
		"version": map[string]any{
			"id":           versionID,
			"instructions": "Keep the claim and settlement fenced.",
			"tags":         []any{map[string]any{"name": "durability"}},
		},
	})
	if err != nil || updated.status != http.StatusOK {
		t.Fatalf("update skill: status=%d error=%v body=%s", updated.status, err, updated.body)
	}
	var updatedBody map[string]any
	decodeInternalSkillResult(t, updated, &updatedBody)
	if updatedBody["instructions"] != "Keep the claim and settlement fenced." {
		t.Fatalf("updated skill = %v", updatedBody)
	}

	attached, err := executor.Execute(ctx, 1, 73, internalUpdateSkillRelation, map[string]any{
		"skill_id":          skillID,
		"skill_version_id":  versionID,
		"entity_version_id": fmt.Sprintf("%d", applicationVersionID),
		"has_relation":      true,
	})
	if err != nil || attached.status != http.StatusCreated {
		t.Fatalf("attach skill: status=%d error=%v body=%s", attached.status, err, attached.body)
	}

	listed, err := executor.Execute(ctx, 1, 73, internalListAttachedSkills, map[string]any{
		"app_version_id": fmt.Sprintf("%d", applicationVersionID),
	})
	if err != nil || listed.status != http.StatusOK {
		t.Fatalf("list attached skills: status=%d error=%v body=%s", listed.status, err, listed.body)
	}
	var listedBody struct {
		Skills []struct {
			SkillID      string `json:"skill_id"`
			VersionID    string `json:"version_id"`
			Instructions string `json:"instructions"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(listed.body, &listedBody); err != nil {
		t.Fatalf("decode attached list: %v", err)
	}
	found := false
	for _, skill := range listedBody.Skills {
		if skill.SkillID == skillID && skill.VersionID == versionID &&
			skill.Instructions == "Keep the claim and settlement fenced." {
			found = true
		}
	}
	if !found {
		t.Fatalf("created skill missing from attached list: %s", listed.body)
	}

	detached, err := executor.Execute(ctx, 1, 73, internalUpdateSkillRelation, map[string]any{
		"skill_id":          skillID,
		"entity_version_id": fmt.Sprintf("%d", applicationVersionID),
		"has_relation":      false,
	})
	if err != nil || detached.status != http.StatusOK {
		t.Fatalf("detach skill: status=%d error=%v body=%s", detached.status, err, detached.body)
	}
	listed, err = executor.Execute(ctx, 1, 73, internalListAttachedSkills, map[string]any{
		"app_version_id": fmt.Sprintf("%d", applicationVersionID),
	})
	if err != nil || listed.status != http.StatusOK {
		t.Fatalf("list after detach: status=%d error=%v body=%s", listed.status, err, listed.body)
	}
	if string(listed.body) == "" {
		t.Fatal("list after detach returned no durable result")
	}
	if err := json.Unmarshal(listed.body, &listedBody); err != nil {
		t.Fatalf("decode list after detach: %v", err)
	}
	for _, skill := range listedBody.Skills {
		if skill.SkillID == skillID {
			t.Fatalf("detached skill remains attached: %s", listed.body)
		}
	}
}
