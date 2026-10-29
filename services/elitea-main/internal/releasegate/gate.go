package releasegate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/evaluation"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// Release is the slice of an application release a gate needs: the
// version it was published from and the agent turn input of its
// definition.
type Release struct {
	ID                   int64
	ApplicationVersionID int64
	Instructions         string
	ModelName            string
}

// GateKey names one gate.
type GateKey struct {
	TenantProjectID int64
	GateID          int64
}

// ScheduledDue is an agent whose scheduled re-evaluation is due.
type ScheduledDue struct {
	TenantProjectID int64
	ApplicationID   int64
	ReleaseID       int64
	// ActorID is the user who last scheduled one of the agent's suites. The
	// runs' model calls are made for this user.
	ActorID *int64
}

// Store is the gate's storage (*repos.ReleaseGates).
type Store interface {
	ListSuites(ctx context.Context, tenantProjectID, applicationID int64) ([]SuiteConfig, error)
	PutSuite(ctx context.Context, tenantProjectID int64, suite SuiteConfig) (SuiteConfig, error)
	DeleteSuite(ctx context.Context, tenantProjectID, applicationID, datasetID int64) (bool, error)
	// DatasetApplication answers the agent a dataset belongs to (nil for a
	// project-wide dataset), or ErrDatasetNotFound.
	DatasetApplication(ctx context.Context, tenantProjectID, datasetID int64) (*int64, error)
	// VersionApplication answers the agent of a version, or
	// ErrVersionNotFound.
	VersionApplication(ctx context.Context, tenantProjectID, versionID int64) (int64, error)
	// Release answers a release of the agent, or ErrReleaseNotFound.
	Release(ctx context.Context, tenantProjectID, applicationID, releaseID int64) (Release, error)
	// CurrentRelease answers the agent's published release, or nil.
	CurrentRelease(ctx context.Context, tenantProjectID, applicationID int64) (*Release, error)

	// CreateGate stores a gate and its runs in one transaction. A gate
	// whose State is not pending is stored settled. A Starting gate is
	// stored before its runs start and is not evaluated until FinishStart.
	CreateGate(ctx context.Context, gate GateResult) (int64, error)
	// AddGateRuns records runs a starting gate started.
	AddGateRuns(ctx context.Context, tenantProjectID, gateID int64, runs []GateRun) error
	// FinishStart marks every run of a starting gate started.
	FinishStart(ctx context.Context, tenantProjectID, gateID int64) error
	GetGate(ctx context.Context, tenantProjectID, gateID int64) (GateResult, error)
	ListGates(ctx context.Context, tenantProjectID, applicationID int64, limit int) ([]GateResult, error)
	// SettleGate stores the outcome of a pending gate. It changes nothing
	// when the gate is already settled.
	SettleGate(ctx context.Context, tenantProjectID, gateID int64, state string, outcome []SuiteOutcome, failures []GateFailure) error
	// OverrideGate records the override of a failed gate that has none. It
	// answers ErrGateNotFailed or ErrAlreadyOverridden otherwise.
	OverrideGate(ctx context.Context, tenantProjectID, gateID, actorID int64, reason string) error
	// PendingGates answers pending gates of every project, oldest first.
	PendingGates(ctx context.Context, limit int) ([]GateKey, error)
	// DueScheduled answers the agents whose scheduled re-evaluation is due
	// at now: a published release, a suite with reevaluate_every_days, no
	// pending scheduled gate and no scheduled gate of the release within
	// the shortest cadence.
	DueScheduled(ctx context.Context, now time.Time, limit int) ([]ScheduledDue, error)
	// ReferenceRuns answers the candidate runs of the earliest passed
	// scheduled gate of the release: what a scheduled gate compares with.
	ReferenceRuns(ctx context.Context, tenantProjectID, applicationID, releaseID int64) ([]GateRun, error)
	// ScheduledGatesSince answers the scheduled gates of the release
	// created at or after since, newest first.
	ScheduledGatesSince(ctx context.Context, tenantProjectID, applicationID, releaseID int64, since time.Time, limit int) ([]int64, error)
}

// Runs is the evaluation run storage the gate reads
// (evaluation.RunRepository).
type Runs interface {
	GetRun(ctx context.Context, projectID, runID string) (evaluation.Run, error)
	ListResults(ctx context.Context, projectID, runID string, page evaluation.ResultPage) ([]evaluation.RunResult, int, error)
	DatasetCases(ctx context.Context, projectID, datasetID string) ([]evaluation.DatasetCase, error)
	AgentVersion(ctx context.Context, projectID string, versionID int) (evaluation.AgentVersion, error)
}

// Starter starts an evaluation run (*evaluation.RunStarter).
type Starter interface {
	Start(ctx context.Context, projectID string, input evaluation.RunStartInput, createdBy *int, agent *evaluation.SnapshotAgent) (evaluation.Run, error)
}

// RunCanceller is a Starter that can also cancel a run it started
// (*evaluation.RunStarter). A gate whose start failed cancels the runs it
// had started; with a Starter that cannot, they run to the end, recorded
// on the gate.
type RunCanceller interface {
	CancelRun(ctx context.Context, projectID, runID string) error
}

// startTimeout is how long a gate may stay starting before it is settled
// failed (gate_start_failed): the process that started it stopped.
const startTimeout = 15 * time.Minute

// Gate is the release gate.
type Gate struct {
	store   Store
	runs    Runs
	starter Starter
	logger  *slog.Logger
	now     func() time.Time
	// actorCheck, when set, resolves again that a scheduled gate's user may
	// start evaluation runs in the project (WithActorCheck).
	actorCheck ActorCheck
}

// ActorCheck answers whether a user may start evaluation runs in a
// project now.
type ActorCheck func(ctx context.Context, tenantProjectID, userID int64) (bool, error)

// WithActorCheck sets the check a scheduled gate makes on its user.
func (g *Gate) WithActorCheck(check ActorCheck) *Gate {
	g.actorCheck = check
	return g
}

// New builds the gate. Every dependency is required.
func New(store Store, runs Runs, starter Starter, logger *slog.Logger) (*Gate, error) {
	if store == nil || runs == nil || starter == nil {
		return nil, errors.New("release gate: a store, the run storage and a run starter are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Gate{store: store, runs: runs, starter: starter, logger: logger, now: time.Now}, nil
}

// ListSuites answers the agent's gate suites.
func (g *Gate) ListSuites(ctx context.Context, tenantProjectID, applicationID int64) ([]SuiteConfig, error) {
	return g.store.ListSuites(ctx, tenantProjectID, applicationID)
}

// DeleteSuite unmarks a gate suite. Gates already started keep the suite
// they froze.
func (g *Gate) DeleteSuite(ctx context.Context, tenantProjectID, applicationID, datasetID int64) (bool, error) {
	return g.store.DeleteSuite(ctx, tenantProjectID, applicationID, datasetID)
}

// ListGates answers the agent's gates, newest first. A pending gate whose
// runs are terminal is evaluated first, as Result does.
func (g *Gate) ListGates(ctx context.Context, tenantProjectID, applicationID int64, limit int) ([]GateResult, error) {
	gates, err := g.store.ListGates(ctx, tenantProjectID, applicationID, limit)
	if err != nil {
		return nil, err
	}
	for index, gate := range gates {
		if gate.State != StatePending {
			continue
		}
		settled, err := g.Result(ctx, tenantProjectID, gate.ID)
		if err != nil {
			return nil, err
		}
		gates[index] = settled
	}
	return gates, nil
}

// startRequest is a gate to start.
type startRequest struct {
	tenantProjectID    int64
	applicationID      int64
	kind               string
	candidateVersionID int64
	candidateAgent     evaluation.SnapshotAgent
	release            *Release
	// references are the reused baseline runs of a scheduled gate, by
	// dataset.
	references map[int64]GateRun
	createdBy  *int64
	runActor   *int64
	// actorRefused is why a scheduled gate may not run for runActor.
	actorRefused string
}

// StartGate starts a change gate: every gate suite of the agent runs
// against the candidate version, and the regression suite (and every suite
// with a no_worse_than criterion) also runs against the current release.
// An agent with no gate suite gets a gate that passed with no suite.
//
// The runs' model calls are made for the user of ctx (the caller's
// principal). A suite that cannot start (its dataset was deleted, has no
// active case, or names a dimension the ai engine cannot score) fails the
// gate at once with gate_suite_not_runnable, and a run whose start fails
// (the evaluation store or queue) fails it with gate_start_failed; the
// answer is still the gate id. An error is answered only when nothing was
// recorded and no run started.
func (g *Gate) StartGate(ctx context.Context, tenantProjectID, applicationID, candidateVersionID int64, currentReleaseID *int64) (int64, error) {
	if tenantProjectID <= 0 || applicationID <= 0 || candidateVersionID <= 0 {
		return 0, ErrVersionNotFound
	}
	owner, err := g.store.VersionApplication(ctx, tenantProjectID, candidateVersionID)
	if err != nil {
		return 0, err
	}
	if owner != applicationID {
		return 0, ErrVersionNotFound
	}
	version, err := g.runs.AgentVersion(ctx, projectKey(tenantProjectID), int(candidateVersionID))
	if err != nil {
		if isStatus(err, http.StatusNotFound) {
			return 0, ErrVersionNotFound
		}
		return 0, fmt.Errorf("release gate: read the candidate version: %w", err)
	}
	var release *Release
	if currentReleaseID != nil {
		found, err := g.store.Release(ctx, tenantProjectID, applicationID, *currentReleaseID)
		if err != nil {
			return 0, err
		}
		release = &found
	}
	actor := callerID(ctx)
	return g.start(ctx, startRequest{
		tenantProjectID: tenantProjectID, applicationID: applicationID, kind: KindChange,
		candidateVersionID: candidateVersionID,
		candidateAgent:     evaluation.SnapshotAgent{Instructions: version.Instructions, ModelName: version.ModelName},
		release:            release, createdBy: actor, runActor: actor,
	})
}

// StartScheduled starts a scheduled gate of the agent's current release:
// the gate suites run against the release's definition and are compared
// with the release's reference runs (ReferenceRuns). The first scheduled
// gate of a release has no reference: only its threshold criteria decide
// it, and when it passes its runs become the reference. runActor is the
// user the runs' model calls are made for. An agent with no published
// release answers 0 and no error.
func (g *Gate) StartScheduled(ctx context.Context, tenantProjectID, applicationID int64, runActor *int64) (int64, error) {
	release, err := g.store.CurrentRelease(ctx, tenantProjectID, applicationID)
	if err != nil || release == nil {
		return 0, err
	}
	references, err := g.store.ReferenceRuns(ctx, tenantProjectID, applicationID, release.ID)
	if err != nil {
		return 0, err
	}
	byDataset := make(map[int64]GateRun, len(references))
	for _, run := range references {
		byDataset[run.DatasetID] = run
	}
	refused, err := g.refuseActor(ctx, tenantProjectID, runActor)
	if err != nil {
		return 0, err
	}
	return g.start(ctx, startRequest{
		tenantProjectID: tenantProjectID, applicationID: applicationID, kind: KindScheduled,
		candidateVersionID: release.ApplicationVersionID,
		candidateAgent:     evaluation.SnapshotAgent{Instructions: release.Instructions, ModelName: release.ModelName},
		release:            release, references: byDataset, runActor: runActor, actorRefused: refused,
	})
}

// refuseActor answers why the user may not have a scheduled gate's model
// calls made for them, or "". A scheduled gate runs long after the user
// set the cadence, so the permission is resolved again.
func (g *Gate) refuseActor(ctx context.Context, tenantProjectID int64, actor *int64) (string, error) {
	if g.actorCheck == nil {
		return "", nil
	}
	if actor == nil {
		return "no user is recorded for the scheduled re-evaluation: save the suite config again", nil
	}
	allowed, err := g.actorCheck(ctx, tenantProjectID, *actor)
	if err != nil {
		return "", err
	}
	if !allowed {
		return "user " + strconv.FormatInt(*actor, 10) +
			" who scheduled the re-evaluation may no longer start evaluation runs in this project: save the suite config again", nil
	}
	return "", nil
}

func (g *Gate) start(ctx context.Context, request startRequest) (int64, error) {
	suites, err := g.store.ListSuites(ctx, request.tenantProjectID, request.applicationID)
	if err != nil {
		return 0, err
	}
	gate := GateResult{
		TenantProjectID: request.tenantProjectID, ApplicationID: request.applicationID, Kind: request.kind,
		CandidateVersionID: request.candidateVersionID, State: StatePending,
		Suites: suites, Runs: []GateRun{}, Outcome: []SuiteOutcome{}, Failures: []GateFailure{},
		CreatedBy: request.createdBy,
	}
	if request.release != nil {
		releaseID, versionID := request.release.ID, request.release.ApplicationVersionID
		gate.CurrentReleaseID = &releaseID
		gate.BaselineVersionID = &versionID
	}
	if len(suites) == 0 {
		gate.State = StatePassed
		return g.store.CreateGate(ctx, gate)
	}
	if request.actorRefused != "" {
		// Recorded as a failed gate, not skipped: the cadence then waits a
		// full interval, and the failure is evidence that the drift check
		// stopped.
		evidence := make([]SuiteEvidence, 0, len(suites))
		for _, suite := range suites {
			evidence = append(evidence, SuiteEvidence{Suite: suite, Skipped: true})
		}
		_, gate.Outcome, _ = Evaluate(evidence)
		gate.State = StateFailed
		gate.Failures = []GateFailure{{Code: FailureScheduleActorRefused, Message: request.actorRefused}}
		return g.store.CreateGate(ctx, gate)
	}

	project := projectKey(request.tenantProjectID)
	notRunnable := map[int64]string{}
	// Every suite is checked before any run starts, so a suite that cannot
	// run does not leave the others' runs paid for and unused.
	for _, suite := range suites {
		reason, err := g.preflight(ctx, project, suite)
		if err != nil {
			return 0, err
		}
		if reason != "" {
			notRunnable[suite.DatasetID] = reason
		}
	}

	if len(notRunnable) > 0 {
		// Nothing started: the gate is recorded settled.
		gate.State, gate.Outcome, gate.Failures = Evaluate(notRunnableEvidence(suites, notRunnable))
		return g.store.CreateGate(ctx, gate)
	}

	// The gate is recorded before any run starts, so a run never exists
	// without the gate that reads it, and a start that fails half way
	// leaves a failed gate (the scheduled selection then waits for the
	// cadence) rather than orphan runs and a retry on every pass.
	gate.Starting = true
	gateID, err := g.store.CreateGate(ctx, gate)
	if err != nil {
		return 0, err
	}
	var (
		started  []GateRun
		startErr error
	)
	for _, suite := range suites {
		runs, reason, err := g.startSuite(ctx, project, request, suite)
		if len(runs) > 0 {
			started = append(started, runs...)
			if recordErr := g.store.AddGateRuns(ctx, request.tenantProjectID, gateID, runs); recordErr != nil && err == nil {
				err = recordErr
			}
		}
		if err != nil {
			startErr = err
			break
		}
		if reason != "" {
			notRunnable[suite.DatasetID] = reason
			break
		}
	}
	switch {
	case startErr != nil:
		g.logger.Error("release gate: a run did not start; the gate fails",
			"project_id", request.tenantProjectID, "gate_id", gateID, "error", startErr)
		g.cancelRuns(ctx, project, gateID, started)
		_, outcome, _ := Evaluate(notRunnableEvidence(suites, notRunnable))
		g.settleStartFailed(ctx, request.tenantProjectID, gateID, outcome,
			"an evaluation run of the gate could not be started; submit again or ask for an override")
	case len(notRunnable) > 0:
		g.cancelRuns(ctx, project, gateID, started)
		state, outcome, failures := Evaluate(notRunnableEvidence(suites, notRunnable))
		if err := g.store.SettleGate(ctx, request.tenantProjectID, gateID, state, outcome, failures); err != nil {
			g.logger.Error("release gate: could not settle a gate whose suite cannot run",
				"project_id", request.tenantProjectID, "gate_id", gateID, "error", err)
		}
	default:
		if err := g.store.FinishStart(ctx, request.tenantProjectID, gateID); err != nil {
			// The gate stays starting and settles failed after startTimeout.
			g.logger.Error("release gate: could not mark the gate's runs started",
				"project_id", request.tenantProjectID, "gate_id", gateID, "error", err)
		}
	}
	return gateID, nil
}

// notRunnableEvidence is the evidence of a gate settled before its runs
// finished: the suites that cannot run say why, the others are skipped.
func notRunnableEvidence(suites []SuiteConfig, notRunnable map[int64]string) []SuiteEvidence {
	evidence := make([]SuiteEvidence, 0, len(suites))
	for _, suite := range suites {
		evidence = append(evidence, SuiteEvidence{
			Suite: suite, NotRunnable: notRunnable[suite.DatasetID],
			Skipped: notRunnable[suite.DatasetID] == "",
		})
	}
	return evidence
}

// settleStartFailed settles a starting gate failed with gate_start_failed.
// A failure to store it is logged: the gate stays starting and settles the
// same way after startTimeout.
func (g *Gate) settleStartFailed(ctx context.Context, tenantProjectID, gateID int64, outcome []SuiteOutcome, message string) {
	failures := []GateFailure{{Code: FailureStartFailed, Message: message}}
	if err := g.store.SettleGate(ctx, tenantProjectID, gateID, StateFailed, outcome, failures); err != nil {
		g.logger.Error("release gate: could not record a failed gate start",
			"project_id", tenantProjectID, "gate_id", gateID, "error", err)
	}
}

// cancelRuns cancels runs a gate started and will not read, when the run
// starter can cancel. Reused reference runs are not the gate's to cancel.
func (g *Gate) cancelRuns(ctx context.Context, project string, gateID int64, runs []GateRun) {
	canceller, ok := g.starter.(RunCanceller)
	if !ok {
		return
	}
	for _, run := range runs {
		if run.Reused {
			continue
		}
		if err := canceller.CancelRun(ctx, project, strconv.FormatInt(run.EvalRunID, 10)); err != nil {
			g.logger.Warn("release gate: could not cancel a run of a failed gate start",
				"gate_id", gateID, "eval_run_id", run.EvalRunID, "error", err)
		}
	}
}

// preflight answers why a suite cannot run, or "".
func (g *Gate) preflight(ctx context.Context, project string, suite SuiteConfig) (string, error) {
	if suite.DatasetMissing {
		return "dataset " + strconv.FormatInt(suite.DatasetID, 10) + " was deleted", nil
	}
	cases, err := g.runs.DatasetCases(ctx, project, strconv.FormatInt(suite.DatasetID, 10))
	if err != nil {
		if refusal, refused := clientRefusal(err); refused {
			return refusal, nil
		}
		return "", fmt.Errorf("release gate: read the cases of dataset %d: %w", suite.DatasetID, err)
	}
	for _, testCase := range cases {
		if !testCase.Excluded {
			return "", nil
		}
	}
	return "dataset " + strconv.FormatInt(suite.DatasetID, 10) + " has no active case", nil
}

// startSuite starts the runs of one suite.
func (g *Gate) startSuite(ctx context.Context, project string, request startRequest, suite SuiteConfig) ([]GateRun, string, error) {
	trigger := evaluation.RunTriggerOnDemand
	if request.kind == KindScheduled {
		trigger = evaluation.RunTriggerOfflineBatch
	}
	startOne := func(versionID int64, agent evaluation.SnapshotAgent) (int64, string, error) {
		application, version := int(request.applicationID), int(versionID)
		input := evaluation.RunStartInput{
			DatasetID: strconv.FormatInt(suite.DatasetID, 10), ApplicationID: &application,
			ApplicationVersionID: &version, DimensionIDs: suite.DimensionIDs(), TriggerType: trigger,
		}
		input.Normalize()
		if err := input.Validate(); err != nil {
			reason, _ := clientRefusal(err)
			return 0, reason, nil
		}
		frozen := agent
		run, err := g.starter.Start(ctx, project, input, intActor(request.runActor), &frozen)
		if err != nil {
			if reason, refused := clientRefusal(err); refused {
				return 0, reason, nil
			}
			return 0, "", fmt.Errorf("release gate: start a run of dataset %d: %w", suite.DatasetID, err)
		}
		id, err := strconv.ParseInt(run.ID, 10, 64)
		if err != nil || id <= 0 {
			return 0, "", fmt.Errorf("release gate: the run of dataset %d has id %q", suite.DatasetID, run.ID)
		}
		return id, "", nil
	}

	var runs []GateRun
	candidateID, reason, err := startOne(request.candidateVersionID, request.candidateAgent)
	if err != nil || reason != "" {
		return runs, reason, err
	}
	candidateVersion := request.candidateVersionID
	runs = append(runs, GateRun{
		DatasetID: suite.DatasetID, Role: RoleCandidate, ApplicationVersionID: &candidateVersion, EvalRunID: candidateID,
	})

	if request.release == nil || !suite.NeedsBaseline() {
		return runs, "", nil
	}
	if request.kind == KindScheduled {
		// The baseline of a scheduled gate is the release's reference run,
		// not a new run: the release measured against itself.
		if reference, found := request.references[suite.DatasetID]; found {
			reference.Role, reference.Reused = RoleBaseline, true
			runs = append(runs, reference)
		}
		return runs, "", nil
	}
	baselineVersion := request.release.ApplicationVersionID
	baselineID, reason, err := startOne(baselineVersion, evaluation.SnapshotAgent{
		Instructions: request.release.Instructions, ModelName: request.release.ModelName,
	})
	if err != nil || reason != "" {
		return runs, reason, err
	}
	runs = append(runs, GateRun{
		DatasetID: suite.DatasetID, Role: RoleBaseline, ApplicationVersionID: &baselineVersion, EvalRunID: baselineID,
	})
	return runs, "", nil
}

// Result answers a gate. A pending gate whose runs are all terminal is
// evaluated and its outcome stored first; a pending gate with a run still
// queued or running is answered pending.
func (g *Gate) Result(ctx context.Context, tenantProjectID, gateID int64) (GateResult, error) {
	gate, err := g.store.GetGate(ctx, tenantProjectID, gateID)
	if err != nil {
		return GateResult{}, err
	}
	if gate.State != StatePending {
		return gate, nil
	}
	settled, err := g.settle(ctx, gate)
	if err != nil || !settled {
		return gate, err
	}
	return g.store.GetGate(ctx, tenantProjectID, gateID)
}

// settle evaluates a pending gate when its runs are terminal. It answers
// false when a run is not terminal yet.
func (g *Gate) settle(ctx context.Context, gate GateResult) (bool, error) {
	project := projectKey(gate.TenantProjectID)
	if gate.Starting {
		// Its runs are still being started: not every suite has its runs
		// yet. A start that never finished is a failed start.
		if g.now().Sub(gate.CreatedAt) < startTimeout {
			return false, nil
		}
		g.cancelRuns(ctx, project, gate.ID, gate.Runs)
		_, outcome, _ := Evaluate(notRunnableEvidence(gate.Suites, nil))
		failures := []GateFailure{{Code: FailureStartFailed,
			Message: "the gate's runs were not all started (the process starting them stopped); submit again or ask for an override"}}
		if err := g.store.SettleGate(ctx, gate.TenantProjectID, gate.ID, StateFailed, outcome, failures); err != nil {
			return false, err
		}
		return true, nil
	}
	evidence := make([]SuiteEvidence, 0, len(gate.Suites))
	for _, suite := range gate.Suites {
		item := SuiteEvidence{Suite: suite, CaseNames: map[string]string{}}
		for _, run := range gate.Runs {
			if run.DatasetID != suite.DatasetID {
				continue
			}
			read, terminal, err := g.readRun(ctx, project, run.EvalRunID)
			if err != nil {
				return false, err
			}
			if !terminal {
				return false, nil
			}
			if run.Role == RoleBaseline {
				item.Baseline = &read
			} else {
				item.Candidate = &read
			}
		}
		// A change gate with a current release runs a baseline for every
		// suite that needs one. A missing baseline row must fail the
		// suite, not make its no_worse_than criteria not applicable.
		if gate.Kind == KindChange && gate.CurrentReleaseID != nil && suite.NeedsBaseline() && item.Baseline == nil {
			item.Baseline = &RunEvidence{Missing: true}
		}
		if suite.Regression && item.Baseline != nil {
			cases, err := g.runs.DatasetCases(ctx, project, strconv.FormatInt(suite.DatasetID, 10))
			if err == nil {
				for _, testCase := range cases {
					item.CaseNames[testCase.ID] = CaseName(testCase.Input)
				}
			} else {
				// The listing keeps the case ids without names.
				g.logger.Warn("release gate: could not read case names", "gate_id", gate.ID, "error", err)
			}
		}
		evidence = append(evidence, item)
	}
	state, outcome, failures := Evaluate(evidence)
	if err := g.store.SettleGate(ctx, gate.TenantProjectID, gate.ID, state, outcome, failures); err != nil {
		return false, err
	}
	return true, nil
}

// readRun reads one run and, when it finished, every page of its results.
// It answers whether the run is terminal; a missing run is terminal.
func (g *Gate) readRun(ctx context.Context, project string, runID int64) (RunEvidence, bool, error) {
	evidence := RunEvidence{EvalRunID: runID}
	id := strconv.FormatInt(runID, 10)
	run, err := g.runs.GetRun(ctx, project, id)
	if err != nil {
		if isStatus(err, http.StatusNotFound) {
			evidence.Missing = true
			return evidence, true, nil
		}
		return evidence, false, fmt.Errorf("release gate: read run %d: %w", runID, err)
	}
	if !evaluation.IsRunTerminal(run.Status) {
		return evidence, false, nil
	}
	evidence.Status, evidence.Error = run.Status, run.Error
	if run.Status != evaluation.RunStatusFinished {
		return evidence, true, nil
	}
	for offset := 0; ; {
		page, total, err := g.runs.ListResults(ctx, project, id,
			evaluation.ResultPage{Limit: evaluation.MaxResultPageLimit, Offset: offset})
		if err != nil {
			return evidence, false, fmt.Errorf("release gate: read the results of run %d: %w", runID, err)
		}
		evidence.Results = append(evidence.Results, page...)
		offset += len(page)
		if len(page) == 0 || offset >= total {
			break
		}
	}
	return evidence, true, nil
}

// Override records that a governance-role holder accepted a failed gate.
// The caller checks the role (models.applications.governance). The reason
// is required, at most 200 characters. The gate stays failed; the override
// is stored beside it, once.
func (g *Gate) Override(ctx context.Context, tenantProjectID, gateID, actorID int64, reason string) error {
	reason = strings.TrimSpace(reason)
	if actorID <= 0 {
		return ErrInvalidActor
	}
	if reason == "" || utf8.RuneCountInString(reason) > MaxReasonLength {
		return ErrReasonRequired
	}
	gate, err := g.Result(ctx, tenantProjectID, gateID)
	if err != nil {
		return err
	}
	if gate.State != StateFailed {
		return ErrGateNotFailed
	}
	if gate.Override != nil {
		return ErrAlreadyOverridden
	}
	return g.store.OverrideGate(ctx, tenantProjectID, gateID, actorID, reason)
}

// RegressedSince answers the newest scheduled gate of the agent's current
// release, created at or after since, that failed and was not overridden:
// a scheduled re-evaluation regressed past the gate criteria. A gate that
// did not run because its user was refused is skipped. It answers
// nil when there is none, when the agent has no published release, or
// when such a gate is still pending. Pending gates are evaluated first when
// their runs are terminal.
func (g *Gate) RegressedSince(ctx context.Context, tenantProjectID, applicationID int64, since time.Time) (*GateResult, error) {
	release, err := g.store.CurrentRelease(ctx, tenantProjectID, applicationID)
	if err != nil || release == nil {
		return nil, err
	}
	ids, err := g.store.ScheduledGatesSince(ctx, tenantProjectID, applicationID, release.ID, since, 50)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		gate, err := g.Result(ctx, tenantProjectID, id)
		if err != nil {
			return nil, err
		}
		if gate.State == StateFailed && gate.Override == nil && !onlyActorRefused(gate) {
			return &gate, nil
		}
	}
	return nil, nil
}

// onlyActorRefused reports a scheduled gate that did not run because its
// user was refused: no evaluation happened, so it is not a regression.
func onlyActorRefused(gate GateResult) bool {
	for _, failure := range gate.Failures {
		if failure.Code != FailureScheduleActorRefused {
			return false
		}
	}
	return len(gate.Failures) > 0
}

// PassResult counts what one routine pass did.
type PassResult struct {
	Settled int
	Started int
}

// Bounds of one routine pass.
const (
	passPendingLimit = 100
	passDueLimit     = 20
)

// Pass is one run of the routine: it evaluates the pending gates whose runs
// are terminal (so a gate settles when nobody reads it) and starts the
// scheduled gates that are due. A failure of one gate is logged and the
// pass continues.
func (g *Gate) Pass(ctx context.Context) (PassResult, error) {
	var result PassResult
	pending, err := g.store.PendingGates(ctx, passPendingLimit)
	if err != nil {
		return result, err
	}
	for _, key := range pending {
		gate, err := g.Result(ctx, key.TenantProjectID, key.GateID)
		if err != nil {
			g.logger.Error("release gate: could not evaluate a gate",
				"project_id", key.TenantProjectID, "gate_id", key.GateID, "error", err)
			continue
		}
		if gate.State != StatePending {
			result.Settled++
		}
	}
	due, err := g.store.DueScheduled(ctx, g.now(), passDueLimit)
	if err != nil {
		return result, err
	}
	for _, item := range due {
		id, err := g.StartScheduled(ctx, item.TenantProjectID, item.ApplicationID, item.ActorID)
		if err != nil {
			g.logger.Error("release gate: could not start a scheduled gate",
				"project_id", item.TenantProjectID, "application_id", item.ApplicationID, "error", err)
			continue
		}
		if id > 0 {
			result.Started++
		}
	}
	return result, nil
}

// PutSuite validates and stores a gate suite. The dataset must exist in
// the project and belong to the agent or to no agent.
func (g *Gate) PutSuite(ctx context.Context, tenantProjectID int64, suite SuiteConfig) (SuiteConfig, error) {
	if err := suite.Validate(); err != nil {
		return SuiteConfig{}, err
	}
	owner, err := g.store.DatasetApplication(ctx, tenantProjectID, suite.DatasetID)
	if err != nil {
		return SuiteConfig{}, err
	}
	if owner != nil && *owner != suite.ApplicationID {
		return SuiteConfig{}, ErrDatasetNotFound
	}
	existing, err := g.store.ListSuites(ctx, tenantProjectID, suite.ApplicationID)
	if err != nil {
		return SuiteConfig{}, err
	}
	others := 0
	for _, current := range existing {
		if current.DatasetID == suite.DatasetID {
			continue
		}
		others++
		if suite.Regression && current.Regression {
			return SuiteConfig{}, ErrRegressionTaken
		}
	}
	if others >= MaxSuites {
		return SuiteConfig{}, ErrTooManySuites
	}
	return g.store.PutSuite(ctx, tenantProjectID, suite)
}

// projectKey is the project id as the evaluation repositories take it.
func projectKey(tenantProjectID int64) string { return strconv.FormatInt(tenantProjectID, 10) }

// callerID is the user id of ctx's principal, or nil.
func callerID(ctx context.Context) *int64 {
	user, ok := auth.UserFromContext(ctx)
	if !ok {
		return nil
	}
	raw := user.UserID
	if raw == "" {
		raw = user.ID
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return nil
	}
	return &id
}

func intActor(actor *int64) *int {
	if actor == nil {
		return nil
	}
	value := int(*actor)
	return &value
}

func isStatus(err error, status int) bool {
	var apiErr *apierr.APIError
	return errors.As(err, &apiErr) && apiErr.Status == status
}

// clientRefusal answers the message of a typed 4xx/501 refusal: the
// evaluation said the suite cannot run, not that the database failed.
func clientRefusal(err error) (string, bool) {
	var apiErr *apierr.APIError
	if errors.As(err, &apiErr) && (apiErr.Status < http.StatusInternalServerError || apiErr.Status == http.StatusNotImplemented) {
		return apiErr.Message, true
	}
	return "", false
}
