package evaluation

import (
	"context"

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

	created, err := s.runs.CreateRun(ctx, projectID, Run{
		DatasetID:            input.DatasetID,
		ApplicationID:        input.ApplicationID,
		ApplicationVersionID: input.ApplicationVersionID,
		TriggerType:          input.TriggerType,
		CreatedBy:            createdBy,
		Status:               RunStatusCreated,
		ExecutionMode:        ExecutionModePredictBlocking,
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
