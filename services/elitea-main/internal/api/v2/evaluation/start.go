package evaluation

import (
	"context"
	"net/http"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// RunStarter starts a run: it reads the cases, freezes the snapshot, writes
// the `created` row and enqueues it. The run start route and the release
// gate (internal/releasegate) both start runs through it, so there is one
// way to start a run and one orchestrator that executes it.
type RunStarter struct {
	runs         RunRepository
	dimensions   Repository
	orchestrator Enqueuer
	// suites resolves a start's suite_id. Nil refuses a start that names one.
	suites SuiteReader
}

// WithSuites lets the starter resolve the suite a start names, and freeze its
// limits into the run's snapshot.
func (s *RunStarter) WithSuites(suites SuiteReader) *RunStarter {
	s.suites = suites
	return s
}

// NewRunStarter builds the starter. A nil orchestrator stores the row and
// leaves it to the recovery sweep of whichever process has one.
func NewRunStarter(runs RunRepository, dimensions Repository, orchestrator Enqueuer) *RunStarter {
	return &RunStarter{runs: runs, dimensions: dimensions, orchestrator: orchestrator}
}

// Start starts one run of a validated input. createdBy is the person the
// run's model calls are made for. agent, when not nil, freezes the agent
// turn's instructions and model into the snapshot (SnapshotAgent).
//
// The refusals are typed API errors: an empty dataset is a 400, a dataset
// whose every case is excluded is a 422, and a dimension that is not in the
// library or cannot be scored by the ai engine is a 400 or a 501.
func (s *RunStarter) Start(
	ctx context.Context,
	projectID string,
	input RunStartInput,
	createdBy *int,
	agent *SnapshotAgent,
) (Run, error) {
	// The cases are read through the SAME project id the route is scoped to.
	// A dataset id belonging to another project resolves against p_<this
	// project> and simply is not found, which is the cross-project refusal:
	// there is no path by which a caller reaches another tenant's schema,
	// because the schema name comes from the URL segment the membership
	// middleware already bound to the caller.
	cases, err := s.runs.DatasetCases(ctx, projectID, input.DatasetID)
	if err != nil {
		return Run{}, err
	}
	if len(cases) == 0 {
		return Run{}, apierr.BadRequest("this dataset has no cases: add at least one before starting a run")
	}
	// Excluded cases stay in the dataset and out of the run. The filter is
	// applied HERE, before the snapshot, so the snapshot is the list the
	// orchestrator executes and a toggle after the start cannot change it.
	cases = activeCases(cases)
	if len(cases) == 0 {
		return Run{}, errAllCasesExcluded
	}

	snapshot, err := buildSnapshot(ctx, s.dimensions, projectID, input.DimensionIDs, cases)
	if err != nil {
		return Run{}, err
	}
	snapshot.Agent = agent
	if err := s.freezeSuite(ctx, projectID, input, &snapshot); err != nil {
		return Run{}, err
	}
	mode, note, err := resolveExecutionMode(input, snapshot, agent)
	if err != nil {
		return Run{}, err
	}
	snapshot.ExecutionNote = note
	if mode == ExecutionModeRuntimePlane && input.ApplicationID == nil {
		return Run{}, apierr.BadRequest("application_id is required: a runtime_plane run executes the agent")
	}
	freezeExecutionMode(&snapshot, mode)

	created, err := s.runs.CreateRun(ctx, projectID, Run{
		DatasetID:            input.DatasetID,
		ApplicationID:        input.ApplicationID,
		ApplicationVersionID: input.ApplicationVersionID,
		TriggerType:          input.TriggerType,
		CreatedBy:            createdBy,
		Status:               RunStatusCreated,
		ExecutionMode:        mode,
		Snapshot:             snapshot,
		Progress:             RunProgress{Done: 0, Total: len(cases)},
	})
	if err != nil {
		return Run{}, err
	}

	// The row is written FIRST and enqueued SECOND. If the process dies
	// between the two, the recovery sweep finds a `created` row and runs it;
	// enqueueing first would leave a job with no row for the worker to claim.
	if s.orchestrator != nil {
		s.orchestrator.Enqueue(RunRef{ProjectID: projectID, RunID: created.ID})
	}
	return created, nil
}

// CancelRun cancels a run this starter started and nobody will read (a
// release gate whose start failed, releasegate.RunCanceller): the row
// first, which makes the cancel durable, then the in-process work, as the
// cancel route does.
func (s *RunStarter) CancelRun(ctx context.Context, projectID, runID string) error {
	if _, err := s.runs.CancelRun(ctx, projectID, runID); err != nil {
		return err
	}
	if s.orchestrator != nil {
		s.orchestrator.Cancel(RunRef{ProjectID: projectID, RunID: runID})
	}
	return nil
}

// freezeSuite copies the named suite's limits into the snapshot. A run started
// without a suite has no limit.
func (s *RunStarter) freezeSuite(ctx context.Context, projectID string, input RunStartInput, snapshot *RunSnapshot) error {
	if input.SuiteID == nil || *input.SuiteID == "" {
		return nil
	}
	if s.suites == nil {
		return apierr.NotImplemented("evaluation suites are not available on this deployment")
	}
	suite, err := s.suites.GetSuite(ctx, projectID, *input.SuiteID)
	if err != nil {
		return err
	}
	limits := suite.RunLimits
	snapshot.Limits = &limits
	snapshot.SuiteID = suite.ID
	return nil
}

// errRuntimePlaneFrozenAgent is the 422 for a runtime_plane run that carries a
// frozen agent definition. A release gate freezes the instructions and model it
// measures, and a real agent run executes the version as the chat start path
// freezes it, so the two cannot both be honoured.
var errRuntimePlaneFrozenAgent error = &apierr.APIError{
	Status:  http.StatusUnprocessableEntity,
	Code:    "unprocessable_entity",
	Message: "a release gate run measures a frozen agent definition and is executed as predict_blocking; the runtime_plane execution mode is not available for it",
}

// ExecutionNoteGateFallback is the note a release gate run carries when its
// suite asked for runtime_plane and the gate ran it as predict_blocking.
const ExecutionNoteGateFallback = "The saved limits ask for agent runs, but a release gate run measures a frozen agent definition, so its cases ran as one model call each."

// resolveExecutionMode picks how the run executes its cases: the start's own
// choice, else the suite's, else predict_blocking. A mode the SUITE asked for
// that a gate run cannot honour falls back to predict_blocking with a note on
// the run; an explicit override of the start is refused (422) instead.
func resolveExecutionMode(input RunStartInput, snapshot RunSnapshot, agent *SnapshotAgent) (string, string, error) {
	if input.ExecutionMode != "" {
		if input.ExecutionMode == ExecutionModeRuntimePlane && agent != nil {
			return "", "", errRuntimePlaneFrozenAgent
		}
		return input.ExecutionMode, "", nil
	}
	if snapshot.Limits != nil && snapshot.Limits.ExecutionMode == ExecutionModeRuntimePlane {
		if agent != nil {
			return ExecutionModePredictBlocking, ExecutionNoteGateFallback, nil
		}
		return ExecutionModeRuntimePlane, "", nil
	}
	return ExecutionModePredictBlocking, "", nil
}

// freezeExecutionMode makes the snapshot say what the run does: the mode, and
// the runtime_plane controls only for a runtime_plane run (a start that
// overrides a runtime_plane suite to predict_blocking must not carry limits
// that the run cannot enforce). A runtime_plane run with no suite gets a
// limits block with the defaults, so the orchestrator reads one shape.
func freezeExecutionMode(snapshot *RunSnapshot, mode string) {
	if snapshot.Limits == nil {
		if mode != ExecutionModeRuntimePlane {
			return
		}
		keep := true
		snapshot.Limits = &RunLimits{OnRunLimit: RunLimitActionStop, StoreToolOutputs: &keep}
	}
	snapshot.Limits.ExecutionMode = mode
	if mode != ExecutionModeRuntimePlane {
		snapshot.Limits.StepLimitPerCase = nil
		snapshot.Limits.TimeLimitPerCaseSeconds = nil
	}
}
