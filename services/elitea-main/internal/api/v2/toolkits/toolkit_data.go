package toolkits

// Deleting what an application engine holds for a toolkit (issue #1244, and
// #1243's per-toolkit half).
//
// An Inventory toolkit's graph lives in the Inventory engine, not in the
// tenant's elitea_tools row, so `DELETE FROM elitea_tools` leaves it behind for
// ever. When a toolkit of such an application is deleted, the handler asks the
// engine to delete the toolkit's data FIRST, over the platform gRPC service
// (internal/providerhost/platformops), and removes the row only once the
// engine has answered.
//
// WHY THE ENGINE GOES FIRST HERE, when the project delete goes row-first with a
// journal (#1245). A project delete has a journal row keyed by the project id
// to remember work still owed after the row is gone; a toolkit has no such
// record, and inventing one for a single external call is a table and a
// reconciler for a case the engine-first order handles without either. The
// engine's deletion is idempotent, and a toolkit whose graph is gone but whose
// row survived (the row delete failed after the engine answered) is a usable
// toolkit that re-ingests on its next run: nothing is lost that a retry of the
// delete does not finish. The opposite order (row first, engine after) would be
// the leak the issue describes whenever the engine call failed.
//
// WHAT A REFUSAL MEANS. ErrToolkitDataBusy: the engine is still stopping an
// ingest of this toolkit (or one started meanwhile and holds the graph);
// nothing was deleted, on either side, and the client is told to retry (409).
// Any other error: the engine could not be reached or failed; nothing was
// deleted on either side, and the client is told so (503). A toolkit whose type
// no engine covers, or an application that keeps nothing per toolkit
// (DeepWiki's index is per wiki), is a no-op and the row is deleted as before.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
)

// ErrToolkitDataBusy is the remover's answer when the engine refused the
// deletion because a run still holds the toolkit's data; nothing was deleted
// and a retry later is the right thing.
var ErrToolkitDataBusy = errors.New("toolkits: the toolkit's data is held by a running ingest; retry")

// ToolkitDataRemover deletes what an application engine holds for one toolkit.
// It is given the toolkit's type so it can decide which engine (if any) the
// toolkit belongs to; a type no engine covers is a nil return.
type ToolkitDataRemover interface {
	DeleteToolkitData(ctx context.Context, projectID, toolkitID int64, toolkitType string) error
}

// WithToolkitDataRemover wires the engine-side deletion into Delete. Without it
// Delete removes the row alone, which is what every deployment did before the
// engines served a platform service.
func WithToolkitDataRemover(remover ToolkitDataRemover) Option {
	return func(h *Handler) { h.toolkitData = remover }
}

// removeToolkitData runs the engine-side half of a delete, when wired. It
// answers the request itself on a refusal and reports false; true means the
// row delete may proceed. A toolkit the repository cannot describe (gone
// already, or a non-numeric id) proceeds too: the row delete is the authority
// on those, and answers as it always has.
func (h *Handler) removeToolkitData(w http.ResponseWriter, r *http.Request, projectID, toolkitID string) bool {
	if h.toolkitData == nil {
		return true
	}
	project, projectErr := strconv.ParseInt(projectID, 10, 64)
	toolkit, toolkitErr := strconv.ParseInt(toolkitID, 10, 64)
	if projectErr != nil || toolkitErr != nil || project <= 0 || toolkit <= 0 {
		return true
	}
	record, err := h.repo.GetToolkit(r.Context(), projectID, toolkitID)
	if err != nil || record == nil {
		return true
	}
	toolkitType, _ := record["type"].(string)
	if toolkitType == "" {
		return true
	}
	err = h.toolkitData.DeleteToolkitData(r.Context(), project, toolkit, toolkitType)
	switch {
	case err == nil:
		return true
	case errors.Is(err, ErrToolkitDataBusy):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "the toolkit's data is held by a running ingest; the toolkit was not deleted. Stop or wait for the run, then retry",
		})
	default:
		// Logged with the cause, answered without it: the error can carry an
		// address. 503, not 500: nothing on this side failed, and the engine
		// being unreachable is a condition a retry resolves.
		slog.ErrorContext(r.Context(), "delete_toolkit_data: the engine could not delete the toolkit's data; the toolkit was not deleted",
			"project_id", project, "toolkit_id", toolkit, "toolkit_type", toolkitType, "error",
			fmt.Errorf("delete %s toolkit %d of project %d in its engine: %w", toolkitType, toolkit, project, err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "the toolkit's engine data could not be deleted; the toolkit was not deleted. Retry later",
		})
	}
	return false
}
