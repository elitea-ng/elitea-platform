package mcp

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	draftsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/drafts"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

type internalDraftOperation string

const (
	internalDraftSkill          internalDraftOperation = "generate_skill_draft"
	internalDraftProjectContext internalDraftOperation = "generate_project_context_draft"
)

// WithInternalDraftHandler shares the REST draft implementation and LLM gateway.
func WithInternalDraftHandler(handler *draftsapi.Handler) Option {
	return func(h *Handler) { h.internalDrafts = handler }
}

func internalDraftTool(operation internalDraftOperation) Tool {
	properties := map[string]any{
		"project_id":       intProperty("Current project ID. The server verifies this value."),
		"user_description": boundedStringProperty("Describe the requested draft or changes.", 1, 1<<20),
		"llm_settings": objectSchema(map[string]any{
			"model_name":       stringProperty("Optional model name within the authorized project."),
			"max_tokens":       map[string]any{"type": "integer"},
			"temperature":      map[string]any{"type": "number"},
			"reasoning_effort": stringProperty("Optional model reasoning effort."),
		}),
	}
	tool := Tool{Name: "post_prompt_lib_" + string(operation),
		Description:            "Generate a validated draft for review. This operation does not save changes.",
		internalDraftOperation: operation,
	}
	switch operation {
	case internalDraftSkill:
		tool.permission = "models.applications.skills.create"
		properties["skill_id"] = intProperty("Stored skill ID. Supply it together with version_id for an edit draft.")
		properties["version_id"] = intProperty("Stored version ID within the selected skill.")
	case internalDraftProjectContext:
		tool.permission = "models.project_context.edit"
		properties["current_project_background"] = nullableBoundedProjectContextString("Existing project background to revise.", 2500)
	}
	tool.InputSchema = objectSchema(properties, "project_id", "user_description")
	return tool
}

func (h *Handler) callInternalDraftTool(r *http.Request, projectID int64, target Tool, arguments map[string]any) map[string]any {
	if h.internalDrafts == nil {
		return errorResult("this deployment cannot execute internal draft tools; nothing was executed")
	}
	return h.callInternalTool(r, projectID, target, arguments, "draft", func(actorID int64) (internalApplicationExecution, error) {
		return h.executeInternalDraft(r.Context(), projectID, actorID, target.internalDraftOperation, arguments)
	})
}

func (h *Handler) executeInternalDraft(ctx context.Context, projectID, actorID int64, operation internalDraftOperation, arguments map[string]any) (internalApplicationExecution, error) {
	if err := ctx.Err(); err != nil {
		return internalApplicationExecution{}, err
	}
	actor := strconv.FormatInt(actorID, 10)
	principal, ok := auth.UserFromContext(ctx)
	if !ok {
		principal = auth.User{ID: actor}
	}
	// Keep token identity for the additional skill-detail permission check.
	principal.UserID = actor
	ctx = auth.ContextWithUser(ctx, principal)
	params := map[string]string{"projectID": strconv.FormatInt(projectID, 10)}
	body := make(map[string]any)
	fields := []string{"user_description", "llm_settings"}
	var handler http.HandlerFunc
	switch operation {
	case internalDraftSkill:
		fields = append(fields, "skill_id", "version_id")
		handler = h.internalDrafts.GenerateSkillDraft
	case internalDraftProjectContext:
		fields = append(fields, "current_project_background")
		handler = h.internalDrafts.GenerateProjectContextDraft
	default:
		return internalApplicationExecution{}, errors.New("unknown internal draft operation")
	}
	for _, field := range fields {
		if value, ok := arguments[field]; ok {
			body[field] = value
		}
	}
	return invokeInternalHandler(ctx, http.MethodPost, nil, body, params, handler)
}
