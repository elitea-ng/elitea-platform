package evaluation

import (
	"context"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/pkg/apierr"
)

// Enqueuer is the orchestrator, as the route needs it.
//
// A narrow interface rather than *Orchestrator, so the handler tests can prove
// that starting a run ENQUEUES it — which is the difference between a route
// that stores a `created` row nobody will ever execute and a route that starts
// a run. That exact shape (a write that answers 200 and nothing happens) is
// what this repository's evidence bar is written about.
type Enqueuer interface {
	Enqueue(ref RunRef)
	Cancel(ref RunRef)
}

// RunHandler serves the five run routes.
type RunHandler struct {
	runs         RunRepository
	dimensions   Repository
	orchestrator Enqueuer
}

func NewRunHandler(runs RunRepository, dimensions Repository, orchestrator Enqueuer) *RunHandler {
	return &RunHandler{runs: runs, dimensions: dimensions, orchestrator: orchestrator}
}

type runListResponse struct {
	Rows  []Run `json:"rows"`
	Total int   `json:"total"`
}

func (h *RunHandler) List(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	filter := RunListFilter{Limit: DefaultRunListLimit}
	if raw := r.URL.Query().Get("application_id"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			apierr.Write(w, apierr.BadRequest("application_id must be an integer"))
			return
		}
		filter.ApplicationID = &parsed
	}
	if raw := r.URL.Query().Get("dataset_id"); raw != "" {
		filter.DatasetID = &raw
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > MaxRunListLimit {
			apierr.Write(w, apierr.BadRequest("limit must be a whole number between 1 and 200"))
			return
		}
		filter.Limit = parsed
	}

	runs, err := h.runs.ListRuns(r.Context(), projectID, filter)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if runs == nil {
		runs = []Run{}
	}
	writeJSON(w, http.StatusOK, runListResponse{Rows: runs, Total: len(runs)})
}

func (h *RunHandler) Get(w http.ResponseWriter, r *http.Request) {
	run, err := h.runs.GetRun(r.Context(),
		chi.URLParam(r, "projectID"), chi.URLParam(r, "runID"))
	if err != nil {
		apierr.Write(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// Start builds the run's SNAPSHOT and queues the job.
//
// The snapshot is built HERE and not in the repository, because it encodes
// product rules — an `ai` binding takes the dimension's own default weight and
// target, and a `code` dimension is refused with a named reason — and those
// rules belong where a handler test can reach them.
func (h *RunHandler) Start(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")

	var input RunStartInput
	if !decodeStrict(w, r, &input) {
		return
	}
	input.Normalize()
	if err := input.Validate(); err != nil {
		apierr.Write(w, err)
		return
	}

	// The cases are read through the SAME project id the route is scoped to.
	// A dataset id belonging to another project resolves against p_<this
	// project> and simply is not found, which is the cross-project refusal:
	// there is no path by which a caller reaches another tenant's schema,
	// because the schema name comes from the URL segment the membership
	// middleware already bound to the caller.
	cases, err := h.runs.DatasetCases(r.Context(), projectID, input.DatasetID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if len(cases) == 0 {
		apierr.Write(w, apierr.BadRequest("this dataset has no cases: add at least one before starting a run"))
		return
	}

	snapshot, err := h.buildSnapshot(r.Context(), projectID, input.DimensionIDs, cases)
	if err != nil {
		apierr.Write(w, err)
		return
	}

	created, err := h.runs.CreateRun(r.Context(), projectID, Run{
		DatasetID:            input.DatasetID,
		ApplicationID:        input.ApplicationID,
		ApplicationVersionID: input.ApplicationVersionID,
		TriggerType:          input.TriggerType,
		CreatedBy:            callerUserID(r),
		Status:               RunStatusCreated,
		ExecutionMode:        ExecutionModePredictBlocking,
		Snapshot:             snapshot,
		Progress:             RunProgress{Done: 0, Total: len(cases)},
	})
	if err != nil {
		apierr.Write(w, err)
		return
	}

	// The row is written FIRST and enqueued SECOND. If the process dies
	// between the two, the recovery sweep finds a `created` row and runs it;
	// enqueueing first would leave a job with no row for the worker to claim.
	if h.orchestrator != nil {
		h.orchestrator.Enqueue(RunRef{ProjectID: projectID, RunID: created.ID})
	}
	writeJSON(w, http.StatusCreated, created)
}

// buildSnapshot freezes the dimensions a run is scored against.
func (h *RunHandler) buildSnapshot(
	ctx context.Context,
	projectID string,
	dimensionIDs []string,
	cases []DatasetCase,
) (RunSnapshot, error) {
	library, err := h.dimensions.List(ctx, projectID, ListFilter{IncludePlatform: true})
	if err != nil {
		return RunSnapshot{}, err
	}
	byID := make(map[string]Dimension, len(library))
	for _, dimension := range library {
		byID[dimension.ID] = dimension
	}

	snapshot := RunSnapshot{
		Cases:      make([]SnapshotCase, 0, len(cases)),
		Dimensions: make(map[string]SnapshotDimension, len(dimensionIDs)),
		Bindings:   make([]SnapshotBinding, 0, len(dimensionIDs)),
	}
	for index, testCase := range cases {
		snapshot.Cases = append(snapshot.Cases, SnapshotCase{ID: testCase.ID, OrderIndex: index})
	}

	for index, id := range dimensionIDs {
		dimension, known := byID[id]
		if !known {
			// NOT skipped. A run that quietly dropped a dimension the caller
			// asked for would report a headline over fewer criteria than the
			// request named, and nothing would say so.
			return RunSnapshot{}, apierr.BadRequest("dimension " + id + " is not in this project's library")
		}
		// ENGINES ARE RESTRICTED TO `ai` IN THIS SLICE, and the refusal names
		// the reason rather than scoring the dimension some other way. There is
		// no code sandbox in elitea-main; a `code` validation that silently
		// passed would be the most dangerous fallback in the feature.
		if !supportsAI(dimension) {
			return RunSnapshot{}, apierr.NotImplemented(
				"dimension " + dimension.Name + " cannot be scored by this release: it allows only the " +
					engineList(dimension) + " engine, and this release serves the ai engine alone — " +
					"the code engine needs a sandbox elitea-main does not have, and the human engine needs the " +
					"human-score surface, which is not built")
		}
		snapshot.Dimensions[id] = SnapshotDimension{
			Name:        dimension.Name,
			Description: dimension.Description,
			ScaleType:   dimension.ScaleType,
			ScaleMin:    dimension.ScaleMin,
			ScaleMax:    dimension.ScaleMax,
			Polarity:    dimension.Polarity,
		}
		snapshot.Bindings = append(snapshot.Bindings, SnapshotBinding{
			DimensionID:    id,
			Engine:         EngineAI,
			Weight:         dimension.DefaultWeight,
			Target:         dimension.DefaultTarget,
			TargetOperator: dimension.DefaultTargetOperator,
			OrderIndex:     index,
		})
	}
	return snapshot, nil
}

func supportsAI(dimension Dimension) bool {
	for _, engine := range dimension.AllowedEngines {
		if engine == EngineAI {
			return true
		}
	}
	return false
}

func engineList(dimension Dimension) string {
	out := ""
	for index, engine := range dimension.AllowedEngines {
		if index > 0 {
			out += "/"
		}
		out += engine
	}
	if out == "" {
		return "(none)"
	}
	return out
}

// Cancel stops a run.
//
// THE ROW IS WRITTEN FIRST and the goroutine is signalled second. The write is
// what makes the cancel durable: a signal to an in-process goroutine is lost
// if the run is being executed by a DIFFERENT replica, and a queued run has no
// goroutine at all. The orchestrator's own claim is `created → running`, so a
// row already moved to `cancelled` can never be claimed.
func (h *RunHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	runID := chi.URLParam(r, "runID")

	cancelled, err := h.runs.CancelRun(r.Context(), projectID, runID)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if h.orchestrator != nil {
		h.orchestrator.Cancel(RunRef{ProjectID: projectID, RunID: runID})
	}
	writeJSON(w, http.StatusOK, cancelled)
}

// scorecardResponse is the READ-ONLY scorecard.
//
// Its keys are the reference's ResultsScorecardDialog's: `run`, `results`,
// `headline_score`, `total`, `offset`. `human_scores` is NOT present, and its
// absence is deliberate and load-bearing: this release has no human-score
// surface, so answering `"human_scores": []` would tell a client "there are
// none" where the truth is "this deployment cannot have any". An OMITTED key
// with a flag saying why is the shape budgets/usage_dimensions.go established
// and #617's evidence bar asks for by name.
type scorecardResponse struct {
	Run           Run         `json:"run"`
	Results       []RunResult `json:"results"`
	HeadlineScore *float64    `json:"headline_score"`
	Total         int         `json:"total"`
	Offset        int         `json:"offset"`
	// Unavailable names the parts of the reference scorecard this release does
	// not serve, each with a reason. It is not an error list; it is what stops
	// an absent capability from reading as an empty one.
	Unavailable []capabilityGap `json:"unavailable"`
}

type capabilityGap struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// scorecardGaps is the same list on every response, because the reasons are
// properties of the RELEASE and not of the run. Building it per request from
// the run would make a deployment's capabilities look like they varied.
var scorecardGaps = []capabilityGap{
	{
		Key:    "human_scores",
		Reason: "this release has no human-score surface: eval_human_scores is not created and nothing writes it",
	},
	{
		Key:    "platform_validations",
		Reason: "platform validations are code-engine bindings, and there is no code sandbox in elitea-main",
	},
}

func (h *RunHandler) Results(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	runID := chi.URLParam(r, "runID")

	page := ResultPage{Limit: DefaultResultPageLimit}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > MaxResultPageLimit {
			apierr.Write(w, apierr.BadRequest("limit must be a whole number between 1 and 2000"))
			return
		}
		page.Limit = parsed
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			apierr.Write(w, apierr.BadRequest("offset must be a whole number of 0 or more"))
			return
		}
		page.Offset = parsed
	}

	// The RUN is read first. A scorecard for a run that does not exist must be
	// a 404 and not a 200 with an empty result list — the second is what a
	// reader interprets as "the run produced nothing".
	run, err := h.runs.GetRun(r.Context(), projectID, runID)
	if err != nil {
		apierr.Write(w, err)
		return
	}

	results, total, err := h.runs.ListResults(r.Context(), projectID, runID, page)
	if err != nil {
		apierr.Write(w, err)
		return
	}
	if results == nil {
		results = []RunResult{}
	}
	writeJSON(w, http.StatusOK, scorecardResponse{
		Run:           run,
		Results:       results,
		HeadlineScore: run.HeadlineScore,
		Total:         total,
		Offset:        page.Offset,
		Unavailable:   scorecardGaps,
	})
}

// callerUserID reads the authenticated principal's numeric user id, or nil.
//
// It is stored on the run because the run's model calls happen minutes after
// this request is over: the orchestrator has no request context, so an actor it
// did not persist is an actor it cannot sign. The gateway bills and authorizes
// on that identity, and a background job with no actor bills the project to
// nobody.
//
// nil is not an error. The route is mounted under the auth composition root in
// production, but the integration harness mounts it bare, and a run started
// there is still a legitimate run — it just carries no person.
func callerUserID(r *http.Request) *int {
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		return nil
	}
	// UserID is the resolved auth_core__user id; ID is the compatibility
	// alias. UserID is preferred at every security boundary in this codebase,
	// so it is preferred here too, with ID as the fallback for the validators
	// that set only the alias.
	raw := user.UserID
	if raw == "" {
		raw = user.ID
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		return nil
	}
	return &parsed
}
