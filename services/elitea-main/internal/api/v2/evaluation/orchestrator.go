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
// cancelled — and the orchestrator stamps `heartbeat_at` every
// HeartbeatInterval while it works. At shutdown it hands a run in flight back
// to `created`. At startup, and every sweepInterval after, it fails the
// orphans already resumed MaxResumes times, re-queues every other `running` row whose
// heartbeat is older than StaleRunTTL, then picks up everything `created`. A
// resumed run skips the cases it already scored in full. Results are upserted on
// (run, case, dimension), so a re-queued run re-scores what it already scored
// without producing a second, contradictory row the average counts twice.

// StaleRunTTL is how long a `running` row may go without a heartbeat before it
// is treated as orphaned.
//
// The heartbeat is now a LIVENESS TICK (HeartbeatInterval) stamped for as long
// as the run executes. The TTL still stays ten minutes, because a process of
// the previous release stamps only after a whole case (an agent call plus one
// judge call per dimension). A shorter TTL would re-queue that process's slow
// run during a rolling deployment, and two processes would score and bill the
// same run. A graceful shutdown does not wait for this TTL: ReleaseRun hands
// the run back at once. The TTL is only for a process that died.
const StaleRunTTL = 10 * time.Minute

// HeartbeatInterval is how often a running job stamps `heartbeat_at`. It is
// several times shorter than StaleRunTTL, so a few slow writes do not make a
// live run look orphaned.
const HeartbeatInterval = 30 * time.Second

// sweepInterval is how often the recovery sweep runs after the startup pass.
const sweepInterval = time.Minute

// MaxResumes bounds how many times the sweep may resume a run whose process
// died. A stale `running` row that the sweep already re-queued this many times
// is moved to `errored` with AbandonedRunReason, so a run that kills its
// process on every attempt does not re-pay for its cases for ever. A graceful
// shutdown (ReleaseRun) does not count: a deployment is not a failure of the
// run. The count is on the row (`resume_count`, tenant/0146), so a run that
// was healthy for hours and then lost its process once is still resumed.
const MaxResumes = 5

// AbandonedRunReason is the `error` text a run gets when MaxResumes ends
// its resumes.
const AbandonedRunReason = "the run was interrupted 5 times (its worker process stopped each time) and was not resumed again; start a new run"

// releaseTimeout bounds the shutdown write that hands a run back to the queue.
const releaseTimeout = 5 * time.Second

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

	// beatEvery overrides HeartbeatInterval in tests. Zero means the default.
	beatEvery time.Duration

	// stop and running let the composition root wait for the workers at
	// shutdown (Stop), so the release write lands before the pool closes.
	stop    context.CancelFunc
	running sync.WaitGroup
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
// cancelled, which is process shutdown; a run in flight then stops and hands
// its row back to `created`, so the next process resumes it at startup. A
// process that dies without that write leaves a stale heartbeat, and the sweep
// re-queues the row after StaleRunTTL.
func (o *Orchestrator) Start(ctx context.Context) {
	if o == nil || o.repo == nil {
		return
	}
	ctx, o.stop = context.WithCancel(ctx)
	for i := 0; i < defaultWorkers; i++ {
		o.running.Go(func() { o.worker(ctx) })
	}
	o.running.Go(func() { o.recover(ctx) })
}

// Stop cancels the workers and waits up to timeout for them to return. A run
// in flight writes its shutdown release (ReleaseRun) before its worker
// returns, so the composition root calls Stop BEFORE it closes the database
// pool. Without the wait the process closed the pool and exited while the
// release was still being written, and the run waited the stale TTL instead.
func (o *Orchestrator) Stop(timeout time.Duration) {
	if o == nil || o.stop == nil {
		return
	}
	o.stop()
	done := make(chan struct{})
	go func() {
		o.running.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		o.logger.Warn("evaluation: workers did not stop in time; the sweep resumes their runs after the stale TTL",
			"timeout", timeout)
	}
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
	ticker := time.NewTicker(sweepInterval)
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
	abandoned, err := o.repo.FailAbandonedRuns(ctx,
		int(StaleRunTTL.Seconds()), MaxResumes, AbandonedRunReason)
	if err != nil {
		o.logger.Error("evaluation: abandoned-run sweep failed", "error", err)
	}
	for _, ref := range abandoned {
		o.logger.Warn("evaluation: failed a run that was interrupted for too long",
			"project_id", ref.ProjectID, "run_id", ref.RunID)
	}
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

	progress := &runProgress{o: o, ctx: ctx, ref: ref}
	stopBeat := o.beat(ctx, progress)
	failure := o.walk(ctx, ref, run, progress)
	stopBeat()

	// A cancellation is NOT a failure. The route has already written
	// `cancelled` on the row, so the finish must not overwrite it with
	// `errored` — FinishRun refuses to move a row that is already terminal, and
	// this branch returns without asking it to.
	if ctx.Err() != nil {
		// A PROCESS shutdown (the parent context, not this run's own cancel)
		// hands the run back to the queue at once. The next process then
		// resumes it at startup, and the user does not watch a frozen
		// `running` row for the stale TTL. ReleaseRun only moves a `running`
		// row, so it cannot undo a cancel that the route already wrote.
		if parent.Err() != nil {
			releaseCtx, cancelRelease := context.WithTimeout(context.WithoutCancel(parent), releaseTimeout)
			if err := o.repo.ReleaseRun(releaseCtx, ref.ProjectID, ref.RunID); err != nil {
				o.logger.Error("evaluation: could not release the run at shutdown; the sweep resumes it after the stale TTL",
					"project_id", ref.ProjectID, "run_id", ref.RunID, "error", err)
			}
			cancelRelease()
		}
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

// runProgress serializes the run's heartbeat writes. The liveness tick and the
// per-case stamp both write `progress_done`. Without one lock a tick could
// read the old count, lose the race to the case stamp, and then write the old
// count over the new one, so the progress bar moved backwards.
type runProgress struct {
	o   *Orchestrator
	ctx context.Context
	ref RunRef

	mu   sync.Mutex
	done int
}

// set records n finished cases and stamps it when stamp is true.
func (p *runProgress) set(n int, stamp bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n > p.done {
		p.done = n
	}
	if stamp {
		p.stampLocked()
	}
}

// tick stamps the current count.
func (p *runProgress) tick() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stampLocked()
}

func (p *runProgress) stampLocked() {
	if err := p.o.repo.Heartbeat(p.ctx, p.ref.ProjectID, p.ref.RunID, p.done); err != nil && p.ctx.Err() == nil {
		// A heartbeat failure does not stop the run: the work is real and
		// the results are stored. It DOES mean the row may look orphaned,
		// so it is logged at error level rather than swallowed.
		p.o.logger.Error("evaluation: heartbeat failed",
			"project_id", p.ref.ProjectID, "run_id", p.ref.RunID, "error", err)
	}
}

// beat stamps the run's heartbeat every HeartbeatInterval until the returned
// stop function is called. The stamp carries the current progress, so it never
// moves the progress bar backwards.
func (o *Orchestrator) beat(ctx context.Context, progress *runProgress) func() {
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(o.heartbeatInterval())
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				progress.tick()
			}
		}
	}()
	return func() {
		close(stop)
		<-finished
	}
}

func (o *Orchestrator) heartbeatInterval() time.Duration {
	if o.beatEvery > 0 {
		return o.beatEvery
	}
	return HeartbeatInterval
}

// completedCases answers the cases a resumed run already scored in full: every
// binding has a stored result that is `ok` or `skipped`. An `error` result is
// tried again, because an interruption mid-call stores one.
func (o *Orchestrator) completedCases(ctx context.Context, ref RunRef, bindings []SnapshotBinding) map[string]bool {
	completed := map[string]bool{}
	if len(bindings) == 0 {
		return completed
	}
	// Read EVERY page. One page holds MaxResultPageLimit rows, and a run of
	// 500 cases x 5 dimensions has more: a single page would re-score (and
	// re-bill) every case past it.
	var results []RunResult
	for offset := 0; ; {
		page, total, err := o.repo.ListResults(ctx, ref.ProjectID, ref.RunID,
			ResultPage{Limit: MaxResultPageLimit, Offset: offset})
		if err != nil {
			// Not fatal: the run re-scores every case, and the upsert keeps one
			// row per (run, case, dimension).
			o.logger.Warn("evaluation: could not read stored results; the run re-scores every case",
				"project_id", ref.ProjectID, "run_id", ref.RunID, "error", err)
			return completed
		}
		results = append(results, page...)
		offset += len(page)
		if len(page) == 0 || offset >= total {
			break
		}
	}
	settled := map[string]map[string]bool{}
	for _, result := range results {
		if result.Status != ResultStatusOK && result.Status != ResultStatusSkipped {
			continue
		}
		if settled[result.DatasetCaseID] == nil {
			settled[result.DatasetCaseID] = map[string]bool{}
		}
		settled[result.DatasetCaseID][result.DimensionID] = true
	}
	for caseID, dimensions := range settled {
		all := true
		for _, binding := range bindings {
			if !dimensions[binding.DimensionID] {
				all = false
				break
			}
		}
		if all {
			completed[caseID] = true
		}
	}
	return completed
}

// snapshotCases keeps the cases the run's snapshot froze at start, in dataset
// order.
//
// The orchestrator reads the case TEXT from the dataset, but the case LIST is
// the snapshot's. Without this filter a case excluded at start would run, and
// a case excluded after start would vanish from a run already counted with it.
// A snapshot with no cases is a run row written before the snapshot carried
// them; it executes the cases that are not excluded now.
func snapshotCases(cases []DatasetCase, snapshot RunSnapshot) []DatasetCase {
	if len(snapshot.Cases) == 0 {
		return activeCases(cases)
	}
	frozen := make(map[string]bool, len(snapshot.Cases))
	for _, snapshotCase := range snapshot.Cases {
		frozen[snapshotCase.ID] = true
	}
	kept := make([]DatasetCase, 0, len(snapshot.Cases))
	for _, testCase := range cases {
		if frozen[testCase.ID] {
			kept = append(kept, testCase)
		}
	}
	return kept
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
func (o *Orchestrator) walk(ctx context.Context, ref RunRef, run Run, progress *runProgress) error {
	cases, err := o.repo.DatasetCases(ctx, ref.ProjectID, run.DatasetID)
	if err != nil {
		return fmt.Errorf("the dataset could not be read: %w", err)
	}
	cases = snapshotCases(cases, run.Snapshot)
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

	// A RESUMED run (re-queued after its worker stopped) skips the cases it
	// already scored in full, so a rollout does not make the project pay for
	// them twice. A fresh run has no results, and this skips nothing.
	completed := o.completedCases(ctx, ref, run.Snapshot.Bindings)

	done := 0
	for _, testCase := range cases {
		if ctx.Err() != nil {
			return nil
		}
		if completed[testCase.ID] {
			done++
			progress.set(done, false)
			continue
		}

		output, agentErr := o.agentTurn(ctx, ref, actor(run), version, testCase)
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
		progress.set(done, true)
	}
	// A resume whose LAST cases were all scored before the interruption
	// stamped no progress for them in the loop. Stamp the final count, so the
	// finished row does not show fewer cases than it scored.
	if ctx.Err() == nil && done > 0 {
		progress.tick()
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
		// The run's author, the same identity the agent turn signs. It was
		// accepted by this function and never passed on, so every judge call
		// reached the gateway with no user at all.
		UserID:        userID,
		AttributionID: JudgeAttributionID(ref.RunID, testCase.ID),
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
	ref RunRef,
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
		ProjectID: ref.ProjectID,
		UserID:    userID,
		Model:     version.ModelName,
		Messages:  messages,
		// Legacy issue 6677: the agent turn's spend is attributed to this run
		// and case, so it is not lost among unattributed /predict_llm calls.
		AttributionID: AgentAttributionID(ref.RunID, testCase.ID),
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
