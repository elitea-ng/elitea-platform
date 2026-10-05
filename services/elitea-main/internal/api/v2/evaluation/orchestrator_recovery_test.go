package evaluation

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/predict"
)

// Recovery of a run that a deployment interrupted (demo issue 4: a run sat at
// `running 2/4` after an elitea-main rollout until somebody cancelled it).

// A PROCESS SHUTDOWN hands the run back to `created`, so the next process
// resumes it at startup instead of after the stale TTL.
func TestOrchestratorShutdownReleasesTheRunForTheNextProcess(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	parent, stopProcess := context.WithCancel(context.Background())
	defer stopProcess()

	completer := &shutdownCompleter{stop: stopProcess}
	NewOrchestrator(repo, &scriptedJudge{}, completer, quietLogger()).
		Execute(parent, RunRef{ProjectID: "1", RunID: "1"})

	if got := repo.run("1"); got.Status != RunStatusCreated {
		t.Fatalf("status = %q, want created so the next process resumes the run", got.Status)
	}
}

// A user CANCEL is not a shutdown: the row stays `cancelled`, and the release
// does not put it back in the queue.
func TestOrchestratorCancelIsNotReleased(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	cancelling := &cancellingCompleter{repo: repo}
	orchestrator := NewOrchestrator(repo, &scriptedJudge{}, cancelling, quietLogger())
	cancelling.orchestrator = orchestrator
	orchestrator.Execute(context.Background(), RunRef{ProjectID: "1", RunID: "1"})

	if got := repo.run("1"); got.Status != RunStatusCancelled {
		t.Fatalf("status = %q, want cancelled", got.Status)
	}
}

// A RESUMED run skips the cases it already scored in full, so a rollout does
// not bill them twice, and its progress counts them.
func TestOrchestratorResumeSkipsScoredCases(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	native, normalized := 5.0, 100.0
	_ = repo.SaveResult(context.Background(), "1", RunResult{
		RunID: "1", DatasetCaseID: "11", DimensionID: "3", Status: ResultStatusOK,
		NativeScore: &native, NormalizedScore: &normalized,
	})
	completer := &stubCompleter{answer: "an answer"}
	judge := &scriptedJudge{answers: []JudgeVerdict{{Score: 3}}}

	NewOrchestrator(repo, judge, completer, quietLogger()).
		Execute(context.Background(), RunRef{ProjectID: "1", RunID: "1"})

	if len(completer.requests) != 1 {
		t.Fatalf("the agent was called %d times, want once: case 11 was already scored", len(completer.requests))
	}
	if asked := completer.requests[0].Messages[len(completer.requests[0].Messages)-1].Content; asked != "and Rust?" {
		t.Errorf("the agent was asked %q, want the unscored case", asked)
	}
	run := repo.run("1")
	if run.Status != RunStatusFinished || run.Progress.Done != 2 {
		t.Fatalf("run = %q done %d, want finished 2/2", run.Status, run.Progress.Done)
	}
	if run.HeadlineScore == nil || *run.HeadlineScore != 75 {
		t.Fatalf("headline = %v, want 75 over the kept and the new score", run.HeadlineScore)
	}
}

// An `error` result is NOT a scored case: an interruption mid-call stores one,
// and the resume tries the case again.
func TestOrchestratorResumeRetriesErroredCases(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	_ = repo.SaveResult(context.Background(), "1", RunResult{
		RunID: "1", DatasetCaseID: "11", DimensionID: "3", Status: ResultStatusError,
	})
	completer := &stubCompleter{answer: "an answer"}
	NewOrchestrator(repo, &scriptedJudge{}, completer, quietLogger()).
		Execute(context.Background(), RunRef{ProjectID: "1", RunID: "1"})

	if len(completer.requests) != 2 {
		t.Fatalf("the agent was called %d times, want 2", len(completer.requests))
	}
}

// THE LIVENESS TICK. A slow case keeps stamping the heartbeat, so the sweep
// in another replica cannot mistake it for an orphan.
func TestOrchestratorHeartbeatsDuringASlowCase(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	orchestrator := NewOrchestrator(repo, &scriptedJudge{}, &slowCompleter{delay: 60 * time.Millisecond}, quietLogger())
	orchestrator.beatEvery = 5 * time.Millisecond
	orchestrator.Execute(context.Background(), RunRef{ProjectID: "1", RunID: "1"})

	repo.mu.Lock()
	beats := append([]int(nil), repo.beats...)
	repo.mu.Unlock()
	// Two per-case stamps, plus the ticks during two 60 ms agent calls.
	if len(beats) < 4 {
		t.Fatalf("heartbeats = %v, want ticks during the slow case and not only one per case", beats)
	}
	for i := 1; i < len(beats); i++ {
		if beats[i] < beats[i-1] {
			t.Fatalf("heartbeats = %v: a tick moved the progress backwards", beats)
		}
	}
}

// The sweep fails the long-abandoned orphans BEFORE it re-queues the rest, and
// it gives the repository the reason a person reads on the row.
func TestOrchestratorSweepFailsAbandonedRuns(t *testing.T) {
	t.Parallel()

	repo := &sweepRepo{fakeRunRepo: newFakeRunRepo()}
	NewOrchestrator(repo, &scriptedJudge{}, &stubCompleter{}, quietLogger()).sweep(context.Background())

	if len(repo.abandonCalls) != 1 {
		t.Fatalf("FailAbandonedRuns called %d times, want 1", len(repo.abandonCalls))
	}
	got := repo.abandonCalls[0]
	if got[0] != int(StaleRunTTL.Seconds()) || got[1] != MaxResumes {
		t.Errorf("FailAbandonedRuns(%d, %d), want (%d, %d)", got[0], got[1],
			int(StaleRunTTL.Seconds()), MaxResumes)
	}
	if repo.abandonReason != AbandonedRunReason {
		t.Errorf("reason = %q", repo.abandonReason)
	}
	if HeartbeatInterval*4 > StaleRunTTL {
		t.Errorf("HeartbeatInterval %v is too close to StaleRunTTL %v", HeartbeatInterval, StaleRunTTL)
	}
}

type shutdownCompleter struct {
	stop func()
}

func (s *shutdownCompleter) Complete(ctx context.Context, _ predict.CompletionRequest) (string, error) {
	s.stop()
	<-ctx.Done()
	return "", ctx.Err()
}

type slowCompleter struct {
	mu    sync.Mutex
	delay time.Duration
}

func (s *slowCompleter) Complete(ctx context.Context, _ predict.CompletionRequest) (string, error) {
	s.mu.Lock()
	delay := s.delay
	s.mu.Unlock()
	select {
	case <-time.After(delay):
		return "an answer", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// onePageRepo serves stored results one row per page, so a resume must page
// through all of them to see every scored case.
type onePageRepo struct {
	*fakeRunRepo
	mu    sync.Mutex
	pages int
}

func (r *onePageRepo) ListResults(ctx context.Context, projectID, runID string, page ResultPage) ([]RunResult, int, error) {
	all, total, err := r.fakeRunRepo.ListResults(ctx, projectID, runID, ResultPage{})
	if err != nil {
		return nil, 0, err
	}
	r.mu.Lock()
	r.pages++
	r.mu.Unlock()
	if page.Offset >= len(all) {
		return []RunResult{}, total, nil
	}
	return all[page.Offset : page.Offset+1], total, nil
}

// A resume reads EVERY page of stored results. A single page skipped only the
// cases on it and re-scored (and re-billed) the rest.
func TestOrchestratorResumeReadsEveryResultPage(t *testing.T) {
	t.Parallel()

	base, _ := twoCaseRun()
	native, normalized := 5.0, 100.0
	for _, caseID := range []string{"11", "12"} {
		_ = base.SaveResult(context.Background(), "1", RunResult{
			RunID: "1", DatasetCaseID: caseID, DimensionID: "3", Status: ResultStatusOK,
			NativeScore: &native, NormalizedScore: &normalized,
		})
	}
	repo := &onePageRepo{fakeRunRepo: base}
	completer := &stubCompleter{answer: "an answer"}

	NewOrchestrator(repo, &scriptedJudge{}, completer, quietLogger()).
		Execute(context.Background(), RunRef{ProjectID: "1", RunID: "1"})

	if len(completer.requests) != 0 {
		t.Fatalf("the agent was called %d times, want none: both cases were scored", len(completer.requests))
	}
	if repo.pages < 2 {
		t.Fatalf("read %d result pages, want every page", repo.pages)
	}
	if run := base.run("1"); run.Status != RunStatusFinished || run.Progress.Done != 2 {
		t.Fatalf("run = %q done %d, want finished 2/2", run.Status, run.Progress.Done)
	}
}

// Stop cancels the workers and WAITS for them, so the shutdown release is
// written before the composition root closes the database pool.
func TestOrchestratorStopWaitsForTheShutdownRelease(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	completer := &blockingCompleter{started: make(chan struct{})}
	orchestrator := NewOrchestrator(repo, &scriptedJudge{}, completer, quietLogger())
	orchestrator.Start(context.Background())
	orchestrator.Enqueue(RunRef{ProjectID: "1", RunID: "1"})
	select {
	case <-completer.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not start")
	}

	orchestrator.Stop(5 * time.Second)

	if got := repo.run("1"); got.Status != RunStatusCreated {
		t.Fatalf("status after Stop = %q, want created (released before Stop returned)", got.Status)
	}
}

// blockingCompleter blocks the agent call until its context ends.
type blockingCompleter struct {
	started chan struct{}
	once    sync.Once
}

func (c *blockingCompleter) Complete(ctx context.Context, _ predict.CompletionRequest) (string, error) {
	c.once.Do(func() { close(c.started) })
	<-ctx.Done()
	return "", ctx.Err()
}
