package evaluation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/predict"
)

// WHERE A RUN EXECUTES, AND WHY IT IS HERE.
//
// A run executes as a BOUNDED BACKGROUND JOB INSIDE elitea-main: a goroutine
// per run, taken from a fixed-size worker pool, driven by a context, with the
// state machine persisted on the row. It is deliberately NOT on the runtime
// plane and NOT on the scheduler, and both alternatives were considered:
//
//   - THE RUNTIME PLANE would be the faithful answer — the reference's own
//     orchestrator runs the agent — but its entry point is asynchronous
//     (StartCurrentApplication returns an execution id and the answer arrives
//     over SSE into a conversation), so a run would have to mint a conversation
//     per case, start a turn, and poll the messages table for an assistant row.
//     That needs `runtime.enabled`, a workload session and the whole agent
//     transport, none of which a deployment that only wants to score prompts
//     has to have; and a feature that is dark on every deployment without the
//     runtime plane is a feature that ships as a 404.
//   - THE SCHEDULER is a separate service with its own deployment and its own
//     database access. Putting the loop there would mean a second writer for
//     these tables and a cross-service contract for a job whose whole lifetime
//     is minutes.
//
// The cost of the choice is stated on every row rather than hidden: an agent
// turn here is ONE BLOCKING LLM CALL built from the version's instructions and
// the case input, so it has no tools, no toolkits and no conversation memory.
// `eval_runs.execution_mode` records that as `predict_blocking`, every read
// returns it, and the UI shows it. See run.go's ExecutionModePredictBlocking.
//
// WHAT MAKES IT SURVIVE A RESTART. A goroutine dies with its process. So the
// state machine lives on the ROW — created → running → finished|errored|
// cancelled — and the orchestrator stamps `heartbeat_at` as it works. At
// startup it re-queues every `running` row whose heartbeat is older than
// StaleRunTTL, then picks up everything `created`. Results are upserted on
// (run, case, dimension), so a re-queued run re-scores what it already scored
// without producing a second, contradictory row the average counts twice.

// StaleRunTTL is how long a `running` row may go without a heartbeat before it
// is treated as orphaned.
//
// It must be comfortably longer than one case's work — an agent call plus one
// judge call per dimension — or a slow run re-queues itself while it is still
// running and two workers score the same run. The heartbeat is stamped after
// EVERY case, so the bound is one case and not one run.
const StaleRunTTL = 10 * time.Minute

// defaultWorkers bounds concurrent runs in one process. Each run is a serial
// walk over its cases, so this is the number of PROJECTS that can be
// evaluating at once, not the number of model calls in flight.
const defaultWorkers = 2

// Orchestrator executes runs.
type Orchestrator struct {
	repo      RunRepository
	judge     Judge
	completer Completer
	logger    *slog.Logger

	queue chan RunRef
	// inFlight lets Cancel reach a running goroutine. A cancel writes the
	// terminal status through the repository too, so a run that is queued but
	// not yet started is cancelled by the ROW and not by this map — the map is
	// what stops work already in progress, and it is not the source of truth.
	mu       sync.Mutex
	inFlight map[string]context.CancelFunc
}

// NewOrchestrator builds the job runner. Either dependency may be nil:
//
//   - a nil repo makes the orchestrator inert, which is the shape of a
//     deployment with no database pool composed;
//   - a nil judge (or one built over a nil completer) makes every case fail
//     with ErrJudgeNotConfigured and the RUN finish as `errored` with that
//     reason on the row. It does NOT make the run routes disappear: the
//     operator can see the run, read why it failed, and fix the deployment.
func NewOrchestrator(repo RunRepository, judge Judge, completer Completer, logger *slog.Logger) *Orchestrator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Orchestrator{
		repo:      repo,
		judge:     judge,
		completer: completer,
		logger:    logger,
		queue:     make(chan RunRef, 256),
		inFlight:  map[string]context.CancelFunc{},
	}
}

// Start launches the worker pool and the recovery sweep, and returns.
//
// It is called once from the composition root. The workers stop when ctx is
// cancelled, which is process shutdown; a run in flight then finishes its
// current case and leaves the row `running` with a fresh heartbeat, so the next
// process re-queues it after the TTL rather than losing it.
func (o *Orchestrator) Start(ctx context.Context) {
	if o == nil || o.repo == nil {
		return
	}
	for i := 0; i < defaultWorkers; i++ {
		go o.worker(ctx)
	}
	go o.recover(ctx)
}

// Enqueue asks for a run to be executed. It never blocks: a full queue means
// the process is already saturated, and the row stays `created`, so the
// recovery sweep picks it up. Reporting "queued" for a run the process dropped
// on the floor is the failure this non-blocking send exists to avoid — the row
// is the queue, and this channel is only the fast path.
func (o *Orchestrator) Enqueue(ref RunRef) {
	if o == nil {
		return
	}
	select {
	case o.queue <- ref:
	default:
		o.logger.Warn("evaluation: run queue is full, leaving the run for the recovery sweep",
			"project_id", ref.ProjectID, "run_id", ref.RunID)
	}
}

// Cancel stops a run that this process is executing. The ROW is what makes a
// cancel durable — the route writes the terminal status through the
// repository — so this is best-effort work-stopping and not the decision.
func (o *Orchestrator) Cancel(ref RunRef) {
	if o == nil {
		return
	}
	o.mu.Lock()
	cancel, running := o.inFlight[inFlightKey(ref)]
	o.mu.Unlock()
	if running {
		cancel()
	}
}

func inFlightKey(ref RunRef) string { return ref.ProjectID + "/" + ref.RunID }

func (o *Orchestrator) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ref := <-o.queue:
			o.execute(ctx, ref)
		}
	}
}

// recover re-queues orphans at startup and then keeps sweeping.
//
// The sweep repeats rather than running once, because a process can also die
// mid-run while ANOTHER process is alive: a single startup pass would leave
// that orphan until the next deployment.
func (o *Orchestrator) recover(ctx context.Context) {
	ticker := time.NewTicker(StaleRunTTL / 2)
	defer ticker.Stop()
	o.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.sweep(ctx)
		}
	}
}

func (o *Orchestrator) sweep(ctx context.Context) {
	requeued, err := o.repo.RequeueStaleRuns(ctx, int(StaleRunTTL.Seconds()))
	if err != nil {
		o.logger.Error("evaluation: re-queue sweep failed", "error", err)
	}
	for _, ref := range requeued {
		o.logger.Warn("evaluation: re-queued a run whose worker did not survive",
			"project_id", ref.ProjectID, "run_id", ref.RunID)
	}
	pending, err := o.repo.PendingRuns(ctx, cap(o.queue))
	if err != nil {
		o.logger.Error("evaluation: pending-run scan failed", "error", err)
		return
	}
	for _, ref := range pending {
		o.Enqueue(ref)
	}
}

// Execute runs one job to completion. Exported so a test can drive it
// synchronously — the worker pool is the only other caller.
func (o *Orchestrator) Execute(ctx context.Context, ref RunRef) { o.execute(ctx, ref) }

func (o *Orchestrator) execute(parent context.Context, ref RunRef) {
	ctx, cancel := context.WithCancel(parent)
	o.mu.Lock()
	o.inFlight[inFlightKey(ref)] = cancel
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		delete(o.inFlight, inFlightKey(ref))
		o.mu.Unlock()
		cancel()
	}()

	// The claim is a conditional UPDATE (created → running) that answers
	// pgx.ErrNoRows when somebody else already has it. That is what makes the
	// enqueue path and the recovery sweep safe to race: both may deliver the
	// same ref, and exactly one of them wins.
	run, err := o.repo.ClaimRun(ctx, ref.ProjectID, ref.RunID)
	if err != nil {
		o.logger.Info("evaluation: run not claimed",
			"project_id", ref.ProjectID, "run_id", ref.RunID, "reason", err)
		return
	}

	failure := o.walk(ctx, ref, run)

	// A cancellation is NOT a failure. The route has already written
	// `cancelled` on the row, so the finish must not overwrite it with
	// `errored` — FinishRun refuses to move a row that is already terminal, and
	// this branch returns without asking it to.
	if ctx.Err() != nil {
		o.logger.Info("evaluation: run stopped",
			"project_id", ref.ProjectID, "run_id", ref.RunID, "reason", ctx.Err())
		return
	}

	headline, scored := o.headline(ctx, ref)
	status := RunStatusFinished
	reason := ""
	if failure != nil {
		status = RunStatusErrored
		reason = failure.Error()
	}
	if !scored {
		headline = nil
	}
	if err := o.repo.FinishRun(ctx, ref.ProjectID, ref.RunID, status, headline, reason); err != nil {
		o.logger.Error("evaluation: could not finish the run",
			"project_id", ref.ProjectID, "run_id", ref.RunID, "error", err)
	}
}

// walk scores every case, and returns the error that should make the RUN
// errored — not the errors that make a CASE errored.
//
// THE DISTINCTION IS THE POINT, and it is deliverable 3's second requirement.
// A judge that answers prose, or a judge call that times out, is a CASE
// failure: the result row is stored with status `error` and the raw text, and
// the walk carries on. The run still finishes, and its scorecard shows which
// cases could not be scored. Only a failure that makes further work
// meaningless — the dataset cannot be read, the agent version does not exist,
// the LLM plane is not composed at all — stops the walk and errors the run.
func (o *Orchestrator) walk(ctx context.Context, ref RunRef, run Run) error {
	cases, err := o.repo.DatasetCases(ctx, ref.ProjectID, run.DatasetID)
	if err != nil {
		return fmt.Errorf("the dataset could not be read: %w", err)
	}
	if len(cases) == 0 {
		return errors.New("the dataset has no cases")
	}

	version := AgentVersion{}
	if run.ApplicationVersionID != nil {
		version, err = o.repo.AgentVersion(ctx, ref.ProjectID, *run.ApplicationVersionID)
		if err != nil {
			return fmt.Errorf("the agent version could not be read: %w", err)
		}
	}

	done := 0
	for _, testCase := range cases {
		if ctx.Err() != nil {
			return nil
		}

		output, agentErr := o.agentTurn(ctx, ref.ProjectID, actor(run), version, testCase)
		for _, binding := range run.Snapshot.Bindings {
			if ctx.Err() != nil {
				return nil
			}
			dimension, known := run.Snapshot.Dimensions[binding.DimensionID]
			if !known {
				// A binding with no dimension in the same snapshot is a
				// corrupt snapshot, not a case failure. It is stored as a
				// `skipped` result so the scorecard has a row to explain the
				// gap, rather than a missing cell the reader assumes is a bug.
				o.store(ctx, ref, RunResult{
					RunID: ref.RunID, DatasetCaseID: testCase.ID, DimensionID: binding.DimensionID,
					Status:  ResultStatusSkipped,
					Verdict: map[string]any{"error": "this run's snapshot has no dimension for the binding"},
				})
				continue
			}
			o.store(ctx, ref, o.scoreOne(ctx, ref, actor(run), version.ModelName, testCase, binding, dimension, output, agentErr))
		}

		done++
		if err := o.repo.Heartbeat(ctx, ref.ProjectID, ref.RunID, done); err != nil {
			// A heartbeat failure does not stop the run: the work is real and
			// the results are stored. It DOES mean the row may look orphaned,
			// so it is logged at error level rather than swallowed.
			o.logger.Error("evaluation: heartbeat failed",
				"project_id", ref.ProjectID, "run_id", ref.RunID, "error", err)
		}
	}
	return nil
}

// scoreOne produces exactly one result row and never panics on a nil judge.
func (o *Orchestrator) scoreOne(
	ctx context.Context,
	ref RunRef,
	userID string,
	model string,
	testCase DatasetCase,
	binding SnapshotBinding,
	dimension SnapshotDimension,
	output string,
	agentErr error,
) RunResult {
	result := RunResult{
		RunID:         ref.RunID,
		DatasetCaseID: testCase.ID,
		DimensionID:   binding.DimensionID,
		Evidence: map[string]any{
			"input":  testCase.Input,
			"output": output,
		},
	}
	if testCase.ExpectedOutput != nil {
		result.Evidence["expected_output"] = *testCase.ExpectedOutput
	}

	// The agent turn failing is a CASE error, and the evidence says so. It is
	// not scored 0: an agent that could not be reached has not answered badly.
	if agentErr != nil {
		result.Status = ResultStatusError
		result.Evidence["status"] = ResultStatusError
		result.Evidence["error"] = agentErr.Error()
		result.Verdict = map[string]any{"error": "the agent turn failed: " + agentErr.Error()}
		return result
	}

	// Only the `ai` engine exists in this slice. A `code` binding is REFUSED
	// with a named reason rather than scored: there is no sandbox in this
	// service, and a code validation that silently returned "passed" would be
	// the most dangerous fallback in the whole feature.
	if binding.Engine != EngineAI {
		result.Status = ResultStatusSkipped
		result.Verdict = map[string]any{
			"error": fmt.Sprintf(
				"the %q engine is not served by this release: there is no code sandbox in elitea-main, so a code validation cannot be executed and is not assumed to pass",
				binding.Engine),
		}
		return result
	}

	verdict, err := o.judgeCall(ctx, JudgeRequest{
		ProjectID: ref.ProjectID,
		// The judge runs on the AGENT VERSION's own model. This slice has no
		// separate judge-model setting — that belongs to the suite, which does
		// not exist here — and silently picking a different model would make
		// the score depend on a choice nobody made and nothing records.
		Model:          model,
		Dimension:      dimension,
		Input:          testCase.Input,
		Output:         output,
		ExpectedOutput: testCase.ExpectedOutput,
	})
	if err != nil {
		result.Status = ResultStatusError
		result.Verdict = map[string]any{"error": err.Error()}
		if verdict.Raw != "" {
			result.Verdict["raw"] = verdict.Raw
		}
		return result
	}

	native := verdict.Score
	normalized, ok := NormalizeScore(native, dimension.ScaleType, dimension.ScaleMin, dimension.ScaleMax, dimension.Polarity)
	if !ok {
		// A score that cannot be normalised is not a score. Storing the native
		// number with a NULL normalisation and status `ok` would be refused by
		// tenant/0132's CHECK anyway; the point of catching it here is that the
		// caller gets a readable reason instead of a 500.
		result.Status = ResultStatusError
		result.Verdict = map[string]any{
			"error": "the judge's score could not be normalised against this dimension's scale",
			"score": native,
			"raw":   verdict.Raw,
		}
		return result
	}

	result.Status = ResultStatusOK
	result.NativeScore = &native
	result.NormalizedScore = &normalized
	result.Verdict = map[string]any{"score": native, "reason": verdict.Reason}
	if binding.Target != nil && binding.TargetOperator != "" {
		if met, decidable := EvaluateTargetMet(native, binding.TargetOperator, *binding.Target); decidable {
			result.TargetMet = &met
		}
	}
	return result
}

func (o *Orchestrator) judgeCall(ctx context.Context, req JudgeRequest) (JudgeVerdict, error) {
	if o.judge == nil {
		return JudgeVerdict{}, ErrJudgeNotConfigured
	}
	return o.judge.Score(ctx, req)
}

// agentTurn produces the answer the judge grades.
//
// ONE BLOCKING COMPLETION, built from the version's instructions and the case
// input with the case's variables substituted. See the file header for why
// this is not the runtime plane, and run.go's ExecutionModePredictBlocking for
// where the limitation is recorded so a score cannot claim more than it
// measured.
func (o *Orchestrator) agentTurn(
	ctx context.Context,
	projectID string,
	userID string,
	version AgentVersion,
	testCase DatasetCase,
) (string, error) {
	if o.completer == nil {
		return "", ErrJudgeNotConfigured
	}
	messages := make([]predict.Message, 0, 2)
	if strings.TrimSpace(version.Instructions) != "" {
		messages = append(messages, predict.Message{Role: "system", Content: version.Instructions})
	}
	messages = append(messages, predict.Message{Role: "user", Content: substitute(testCase.Input, testCase.Variables)})

	output, err := o.completer.Complete(ctx, predict.CompletionRequest{
		ProjectID: projectID,
		UserID:    userID,
		Model:     version.ModelName,
		Messages:  messages,
	})
	if err != nil {
		return "", err
	}
	return output, nil
}

// substitute replaces `{{name}}` with the case's variable of that name.
//
// The reference's cases carry a `variables` map and its inputs are templates,
// so a case sent unsubstituted would ask the agent a question containing
// literal braces — the same shape as the i18n interpolation trap this
// repository already has a memory note about. Unknown placeholders are LEFT AS
// THEY ARE rather than emptied: an empty substitution silently changes the
// question, while a visible `{{name}}` in the evidence tells the author which
// variable they forgot.
func substitute(input string, variables map[string]any) string {
	if len(variables) == 0 {
		return input
	}
	out := input
	for name, value := range variables {
		out = strings.ReplaceAll(out, "{{"+name+"}}", fmt.Sprintf("%v", value))
	}
	return out
}

func (o *Orchestrator) store(ctx context.Context, ref RunRef, result RunResult) {
	if result.Verdict == nil {
		result.Verdict = map[string]any{}
	}
	if result.Evidence == nil {
		result.Evidence = map[string]any{}
	}
	// A stored result is written with the ORIGINAL context even when that
	// context is cancelled, because a score that was produced and then lost is
	// worse than one never produced: the case looks unscored and a re-queue
	// pays for it again. context.WithoutCancel keeps the deadline-free write.
	if err := o.repo.SaveResult(context.WithoutCancel(ctx), ref.ProjectID, result); err != nil {
		o.logger.Error("evaluation: could not store a result",
			"project_id", ref.ProjectID, "run_id", ref.RunID,
			"case_id", result.DatasetCaseID, "error", err)
	}
}

// headline averages the NORMALISED scores of the run's `ok` results, weighted
// by each binding's weight.
//
// Only `ok` rows count. An `error` row is not a zero — that is the whole
// reason the status exists — so a run where the judge failed on half the
// cases reports the average of the half it could score, and the scorecard
// shows the other half as unscored. Averaging errors as zeros would report a
// bad prompt as a bad agent.
//
// The second return value is false when nothing was scored at all, which keeps
// `headline_score` NULL rather than 0.
func (o *Orchestrator) headline(ctx context.Context, ref RunRef) (*float64, bool) {
	results, _, err := o.repo.ListResults(context.WithoutCancel(ctx), ref.ProjectID, ref.RunID,
		ResultPage{Limit: MaxResultPageLimit})
	if err != nil {
		o.logger.Error("evaluation: could not read results for the headline",
			"project_id", ref.ProjectID, "run_id", ref.RunID, "error", err)
		return nil, false
	}
	sum, count := 0.0, 0
	for _, result := range results {
		if result.Status != ResultStatusOK || result.NormalizedScore == nil {
			continue
		}
		sum += *result.NormalizedScore
		count++
	}
	if count == 0 {
		return nil, false
	}
	average := sum / float64(count)
	// The same 2dp rounding NormalizeScore applies, so the headline and the
	// cells it averages are printed on one scale.
	rounded := roundTo2(average)
	return &rounded, true
}

// actor is the run's stored `created_by`, as the identity headers want it.
//
// It is read from the ROW and never from a request context: by the time a case
// is scored the HTTP request that started the run has been over for minutes.
// An empty string is a run with no recorded person, which the signer omits
// rather than faking.
func actor(run Run) string {
	if run.CreatedBy == nil {
		return ""
	}
	return strconv.Itoa(*run.CreatedBy)
}

func roundTo2(value float64) float64 {
	return math.Round(value*100) / 100
}
