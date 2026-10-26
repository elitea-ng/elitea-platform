package workspacehandoff

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/desktopwire"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacechanges"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspacestore"
)

// The Files changed reads of a cloud thread (ADR-0034 Phase 2): the files its
// turns changed, and one file's kept diff. Who may read them is who may read
// the conversation, as for the banner state.
const (
	ChangesPath = "/api/v2/elitea_core/workspace_handoff_changes/prompt_lib/{projectID}/{conversationUUID}"
	DiffPath    = "/api/v2/elitea_core/workspace_handoff_diff/prompt_lib/{projectID}/{conversationUUID}"
)

// Changes is *workspacechanges.Service.
type Changes interface {
	View(ctx context.Context, thread workspacestore.Thread) (workspacechanges.View, error)
	Diff(ctx context.Context, thread workspacestore.Thread, commit, scope, path string) (workspacechanges.DiffView, error)
}

var _ Changes = (*workspacechanges.Service)(nil)

// RouteOption composes an optional part of the route.
type RouteOption func(*handler)

// WithChanges serves the Files changed reads; without it they answer 404.
func WithChanges(changes Changes) RouteOption {
	return func(h *handler) { h.changes = changes }
}

// readableThread answers the thread when the caller may read the conversation.
func (h *handler) readableThread(writer http.ResponseWriter, request *http.Request) (workspacestore.Thread, bool) {
	target, ok := thread(writer, request)
	if !ok {
		return workspacestore.Thread{}, false
	}
	if _, _, ok := caller(writer, request, false); !ok {
		return workspacestore.Thread{}, false
	}
	if h.changes == nil || h.conversations.AuthorizeChatResource(request.Context(),
		strconv.FormatInt(target.ProjectID, 10), "conversation", target.ConversationUUID) != nil {
		desktopwire.WriteError(writer, http.StatusNotFound, "handoff_not_found", "The conversation was not found.")
		return workspacestore.Thread{}, false
	}
	return target, true
}

func (h *handler) changesView(writer http.ResponseWriter, request *http.Request) {
	target, ok := h.readableThread(writer, request)
	if !ok {
		return
	}
	view, err := h.changes.View(request.Context(), target)
	if err != nil {
		writeChangesError(writer, err)
		return
	}
	desktopwire.WriteJSON(writer, http.StatusOK, view)
}

func (h *handler) diff(writer http.ResponseWriter, request *http.Request) {
	target, ok := h.readableThread(writer, request)
	if !ok {
		return
	}
	query := request.URL.Query()
	view, err := h.changes.Diff(request.Context(), target, query.Get("commit"), query.Get("scope"), query.Get("path"))
	if err != nil {
		writeChangesError(writer, err)
		return
	}
	desktopwire.WriteJSON(writer, http.StatusOK, view)
}

func writeChangesError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, workspacechanges.ErrInvalid):
		desktopwire.WriteError(writer, http.StatusBadRequest, "invalid_handoff", "Invalid changes request")
	case errors.Is(err, workspacechanges.ErrNotFound):
		desktopwire.WriteError(writer, http.StatusNotFound, "diff_not_found", "No diff of that file is kept.")
	default:
		writer.Header().Set("Retry-After", "1")
		desktopwire.WriteError(writer, http.StatusServiceUnavailable, "handoff_unavailable",
			"The cloud copy's changes are temporarily unavailable. Retry shortly.")
	}
}
