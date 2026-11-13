package evaluation

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/predict"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// EnhanceHandler serves POST eval_run_enhance: the Enhance with AI proposal.
type EnhanceHandler struct {
	runs      RunRepository
	completer Completer
	logger    *slog.Logger
}

// NewEnhanceHandler builds the handler. completer may be nil (no
// LLM_GATEWAY_URL); the route then answers 503 and stays registered, so an
// unconfigured deployment is distinguishable from a missing route.
func NewEnhanceHandler(runs RunRepository, completer Completer) *EnhanceHandler {
	return &EnhanceHandler{runs: runs, completer: completer, logger: slog.Default()}
}

// Enhance reads the run, its pinned agent definition and its missed results,
// asks the model once, and answers the validated proposal. It writes nothing.
func (h *EnhanceHandler) Enhance(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	runID := chi.URLParam(r, "runID")

	run, err := h.runs.GetRun(r.Context(), projectID, runID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if !IsRunTerminal(run.Status) {
		apierr.WriteStatus(w, http.StatusConflict, "the run has not finished: enhance reads a finished run's results")
		return
	}
	if run.ApplicationVersionID == nil {
		apierr.WriteStatus(w, http.StatusUnprocessableEntity, "the run is not pinned to an agent version")
		return
	}
	if h.completer == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, ErrJudgeNotConfigured.Error())
		return
	}

	// The PINNED definition: the frozen agent a gate run holds, else the
	// version row the run names. Never the agent's current default.
	version := AgentVersion{ID: *run.ApplicationVersionID}
	if frozen := run.Snapshot.Agent; frozen != nil {
		version.Instructions, version.ModelName = frozen.Instructions, frozen.ModelName
	} else if version, err = h.runs.AgentVersion(r.Context(), projectID, *run.ApplicationVersionID); err != nil {
		apierr.Write(w, err)
		return
	}

	results, err := h.allResults(r, projectID, runID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	cases, err := h.runs.DatasetCases(r.Context(), projectID, run.DatasetID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	missed := missedRows(run, results, cases)
	if len(missed) == 0 {
		apierr.WriteStatus(w, http.StatusUnprocessableEntity, "no scored case missed its target, so there is nothing to enhance")
		return
	}
	total := len(missed)
	if len(missed) > MaxEnhanceMissedRows {
		missed = missed[:MaxEnhanceMissedRows]
	}

	temperature := 0.2
	answer, err := h.completer.Complete(r.Context(), predict.CompletionRequest{
		ProjectID:   projectID,
		UserID:      callerUserIDString(r),
		Model:       version.ModelName,
		Temperature: &temperature,
		Messages: []predict.Message{
			{Role: "system", Content: enhanceSystemPrompt},
			{Role: "user", Content: buildEnhancePrompt(version.Instructions, run, missed)},
		},
	})
	if err != nil {
		h.logger.Warn("evaluation: enhance completion failed", "project_id", projectID, "run_id", runID, "error", err)
		apierr.WriteStatus(w, http.StatusBadGateway, "the LLM gateway could not complete this request")
		return
	}
	raw, ok := parseEnhanceAnswer(answer)
	if !ok {
		apierr.WriteStatus(w, http.StatusUnprocessableEntity, "the model did not answer with a usable proposal; try again")
		return
	}
	proposal := validateProposal(raw, version.Instructions, run, missed)
	proposal.AnalysedRows = len(missed)
	proposal.MissedRows = total
	writeJSON(w, http.StatusOK, proposal)
}

// allResults reads every page of the run's results.
func (h *EnhanceHandler) allResults(r *http.Request, projectID, runID string) ([]RunResult, error) {
	var results []RunResult
	for offset := 0; ; {
		page, total, err := h.runs.ListResults(r.Context(), projectID, runID,
			ResultPage{Limit: MaxResultPageLimit, Offset: offset})
		if err != nil {
			return nil, err
		}
		results = append(results, page...)
		offset += len(page)
		if len(page) == 0 || offset >= total {
			return results, nil
		}
	}
}

func callerUserIDString(r *http.Request) string {
	if id := callerUserID(r); id != nil {
		return strconv.Itoa(*id)
	}
	return ""
}
