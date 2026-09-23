package agentexecution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
)

// CurrentProjectContextResolver reads project-owned content after the caller
// authorizes the turn. Implementations must not read content from client input.
type CurrentProjectContextResolver interface {
	ResolveCurrentAgentProjectContext(context.Context, int32, int32) (CurrentAgentProjectContext, error)
}

type CurrentAgentProjectContext struct {
	ID                    int32
	Enabled               bool
	Content               string
	ActivationDescription string
}

func currentInstructionRevision(content string) string {
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
}

func (service *CurrentApplicationToolSnapshotService) freezeCurrentInstructionSnapshots(ctx context.Context, request CurrentApplicationVersionFreezeRequest, version map[string]any) error {
	// Never accept a stored/client snapshot as an authority for this new turn.
	delete(version, "project_context")
	scope := fmt.Sprintf("project:%d", request.ProjectID)
	skills, ok := currentAttachedSkillObjects(version["skills"])
	if !ok {
		return unsupportedStart("skill snapshots are not objects")
	}
	for _, skill := range skills {
		skillID, ok := positiveCurrentAgentJSONInteger(skill["skill_id"])
		content, contentOK := skill["instructions"].(string)
		if !ok || !contentOK {
			return unsupportedStart("skill snapshot identity or instructions are invalid")
		}
		versionID := "base"
		if value, ok := positiveCurrentAgentJSONInteger(skill["skill_version_id"]); ok {
			versionID = fmt.Sprint(value)
		}
		skill["id"] = fmt.Sprintf("skill:%d:version:%s", skillID, versionID)
		skill["revision"] = currentInstructionRevision(content)
		skill["scope"] = scope
	}
	if version["agent_type"] == "pipeline" {
		return nil
	}
	if meta, ok := version["meta"].(map[string]any); ok && meta["ignore_project_context"] == true {
		return nil
	}
	source, err := service.projectContext.ResolveCurrentAgentProjectContext(ctx, request.ProjectID, request.ActorUserID)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return unsupportedStartBecause("project context resolution", err)
	}
	if !source.Enabled || source.Content == "" {
		return nil
	}
	if source.ID <= 0 {
		return unsupportedStart("project context identity is invalid")
	}
	snapshot := &runtimev1.ProjectContextSnapshotV1{
		Id:       fmt.Sprintf("project-context:%d:%d", request.ProjectID, source.ID),
		Revision: currentInstructionRevision(source.Content), Scope: scope,
		Content: source.Content, ActivationDescription: strings.Join(strings.Fields(source.ActivationDescription), " "),
	}
	if !validCurrentProjectContextSnapshot(snapshot) {
		return unsupportedStart("project context snapshot is invalid")
	}
	version["project_context"] = map[string]any{
		"id": snapshot.Id, "revision": snapshot.Revision, "scope": snapshot.Scope,
		"content": snapshot.Content, "activation_description": snapshot.ActivationDescription,
	}
	return nil
}

func currentFrozenProjectContext(version map[string]any) (*runtimev1.ProjectContextSnapshotV1, error) {
	value := version["project_context"]
	if value == nil {
		return nil, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	var snapshot runtimev1.ProjectContextSnapshotV1
	if json.Unmarshal(data, &snapshot) != nil || !validCurrentProjectContextSnapshot(&snapshot) {
		return nil, ErrUnsupportedCurrentAgentStart
	}
	return &snapshot, nil
}

func validCurrentProjectContextSnapshot(snapshot *runtimev1.ProjectContextSnapshotV1) bool {
	if snapshot == nil {
		return true
	}
	return len(snapshot.ProtoReflect().GetUnknown()) == 0 &&
		snapshot.Id != "" && len(snapshot.Id) <= 256 &&
		snapshot.Scope != "" && len(snapshot.Scope) <= 256 &&
		snapshot.Content != "" && snapshot.Revision == currentInstructionRevision(snapshot.Content)
}
