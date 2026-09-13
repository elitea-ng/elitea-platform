package toolkits

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkitrun"
	toolkitcalltoolapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitcalltool"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/go-chi/chi/v5"
)

type toolResultReader interface {
	ReadToolRun(context.Context, toolkitcalltoolapp.ResultRequest) (toolkitcalltoolapp.RunOutcome, bool, error)
}

func (h *Handler) TestToolResult(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	reader, ok := h.toolRuns.(toolResultReader)
	if !ok {
		toolkitrun.WriteUnavailable(w)
		return
	}
	user, found := auth.UserFromContext(r.Context())
	if !found {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "authentication required"})
		return
	}
	actorID, ok := user.OwningUserID()
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "authentication required"})
		return
	}
	projectID, _ := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	toolkitID, _ := strconv.ParseInt(chi.URLParam(r, "toolID"), 10, 64)
	request := toolkitcalltoolapp.ResultRequest{ProjectID: projectID, ActorUserID: actorID, ToolkitID: toolkitID, ExecutionID: chi.URLParam(r, "executionID")}
	if request.Validate() != nil {
		toolkitrun.WriteInvalidRequest(w)
		return
	}
	result, pending, err := reader.ReadToolRun(r.Context(), request)
	if errors.Is(err, toolkitcalltoolapp.ErrToolRunNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "tool run not found"})
		return
	}
	if err != nil {
		toolkitrun.WriteError(w, err)
		return
	}
	if pending {
		writeJSON(w, http.StatusAccepted, map[string]any{"pending": true, "task_id": request.ExecutionID})
		return
	}
	toolkitrun.WriteOutcome(w, result)
}
