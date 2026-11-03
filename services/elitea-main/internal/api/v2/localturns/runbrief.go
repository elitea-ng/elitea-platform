package localturns

// The run brief and the goal check of a desktop chain (ADR-0045 decision 11,
// #1564). The desktop host asks Main for the brief when it starts a chain,
// and for the check when a turn would end the chain. Main makes both model
// calls, so the cloud and the desktop use the same prompts. Only the chain's
// owner is served; any other chain, a cloud chain, or a chain of another
// project answers 404 chain_not_found.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/desktopwire"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runbrief"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

const (
	RunBriefPath = "/api/v2/elitea_core/run_brief/prompt_lib/{projectID}"
	RunCheckPath = "/api/v2/elitea_core/run_check/prompt_lib/{projectID}"

	// maxRunBriefBody leaves room for the 32 KiB text and JSON escapes.
	maxRunBriefBody = int64(256 * 1024)
)

// RunBriefs is *runbrief.Service.
type RunBriefs interface {
	Chain(ctx context.Context, root string) (runbrief.ChainBrief, bool, error)
	EnsureBrief(ctx context.Context, root, request string) (runbrief.ChainBrief, bool, error)
	Check(
		ctx context.Context, root, executionID string, transcript func(context.Context) (string, error),
	) (runbrief.Result, bool, error)
}

var _ RunBriefs = (*runbrief.Service)(nil)

type runBriefBody struct {
	ChainRootExecutionID string `json:"chain_root_execution_id"`
	// Request is the person's first message; Main keeps its first 32 KiB.
	Request string `json:"request"`
}

type runCheckBody struct {
	ChainRootExecutionID string `json:"chain_root_execution_id"`
	ExecutionID          string `json:"execution_id"`
	// Transcript is the local transcript excerpt; Main keeps its newest
	// 32 KiB.
	Transcript string `json:"transcript"`
}

// runBriefAnswer is the brief state the host reads.
type runBriefAnswer struct {
	State string          `json:"state"`
	Brief *runbrief.Brief `json:"brief,omitempty"`
}

// ownedChain reads the caller's desktop chain, or writes the error.
func (h *handler) ownedChain(
	writer http.ResponseWriter, request *http.Request, root string,
) (runbrief.ChainBrief, bool) {
	if h.goals == nil {
		writer.Header().Set("Retry-After", "30")
		desktopwire.WriteError(writer, http.StatusServiceUnavailable, "run_brief_unavailable",
			"The run brief is not available on this deployment.")
		return runbrief.ChainBrief{}, false
	}
	projectID, ok := desktopwire.PositiveID(chi.URLParam(request, "projectID"))
	if !ok || !unattended.ValidExecutionID(root) {
		desktopwire.WriteError(writer, http.StatusBadRequest, "invalid_run_brief", "Invalid run brief request")
		return runbrief.ChainBrief{}, false
	}
	_, actor, ok := caller(writer, request)
	if !ok {
		return runbrief.ChainBrief{}, false
	}
	chain, found, err := h.goals.Chain(request.Context(), root)
	if err != nil {
		writeRunBriefError(request.Context(), writer, err)
		return runbrief.ChainBrief{}, false
	}
	if !found || chain.OwnerUserID != actor || chain.ProjectID != projectID ||
		chain.Origin != unattended.OriginDesktop {
		desktopwire.WriteError(writer, http.StatusNotFound, "chain_not_found",
			"The unattended chain was not found for this caller.")
		return runbrief.ChainBrief{}, false
	}
	return chain, true
}

// runBrief is POST run_brief: the chain's brief, written on the first call.
func (h *handler) runBrief(writer http.ResponseWriter, request *http.Request) {
	var body runBriefBody
	if !decodeBriefBody(writer, request, &body) {
		return
	}
	chain, ok := h.ownedChain(writer, request, body.ChainRootExecutionID)
	if !ok {
		return
	}
	if chain.State == unattended.BriefPending {
		if body.Request == "" {
			desktopwire.WriteError(writer, http.StatusBadRequest, "invalid_run_brief", "request is required")
			return
		}
		written, _, err := h.goals.EnsureBrief(request.Context(), chain.RootExecutionID, body.Request)
		if err != nil {
			writeRunBriefError(request.Context(), writer, err)
			return
		}
		chain = written
	}
	desktopwire.WriteJSON(writer, http.StatusOK, runBriefAnswer{State: chain.State, Brief: chain.Brief})
}

// runCheck is POST run_check: the goal check of a turn that would end the
// chain. A second call for the same turn answers the stored check.
func (h *handler) runCheck(writer http.ResponseWriter, request *http.Request) {
	var body runCheckBody
	if !decodeBriefBody(writer, request, &body) {
		return
	}
	if !unattended.ValidExecutionID(body.ExecutionID) {
		desktopwire.WriteError(writer, http.StatusBadRequest, "invalid_run_brief", "Invalid run check request")
		return
	}
	chain, ok := h.ownedChain(writer, request, body.ChainRootExecutionID)
	if !ok {
		return
	}
	transcript := body.Transcript
	result, checked, err := h.goals.Check(request.Context(), chain.RootExecutionID, body.ExecutionID,
		func(context.Context) (string, error) { return transcript, nil })
	if err != nil {
		writeRunBriefError(request.Context(), writer, err)
		return
	}
	if !checked {
		result = runbrief.NotChecked(chain.Brief)
	}
	if result.Conditions == nil {
		result.Conditions = []runbrief.ConditionResult{}
	}
	desktopwire.WriteJSON(writer, http.StatusOK, result)
}

func decodeBriefBody(writer http.ResponseWriter, request *http.Request, into any) bool {
	return decodeBody(writer, request, maxRunBriefBody, into)
}

func writeRunBriefError(ctx context.Context, writer http.ResponseWriter, err error) {
	if errors.Is(err, runbrief.ErrNotLink) {
		desktopwire.WriteError(writer, http.StatusNotFound, "chain_not_found",
			"The turn is not a link of this unattended chain.")
		return
	}
	slog.ErrorContext(ctx, "run brief failed", "err", err)
	desktopwire.WriteError(writer, http.StatusInternalServerError, "run_brief_failed",
		"The run brief or the goal check could not be recorded.")
}
