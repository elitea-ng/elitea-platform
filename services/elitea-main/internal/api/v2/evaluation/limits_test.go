package evaluation

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// markingRepo is the fake run repository that also records the limit a run
// reached (LimitMarker).
type markingRepo struct {
	*fakeRunRepo
	mu     sync.Mutex
	marked []string
}

func (m *markingRepo) MarkLimitReached(_ context.Context, _, _, limit string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.marked = append(m.marked, limit)
	return nil
}

// usageFunc is a UsageReader over a function, so a test states the spend as a
// function of what the run has stored so far.
type usageFunc func(caseID string) (Usage, error)

func (u usageFunc) EvaluationUsage(_ context.Context, _, _, caseID string) (Usage, error) {
	return u(caseID)
}

func intPtr(v int64) *int64 { return &v }

// limitedRun is twoCaseRun with limits frozen into its snapshot.
func limitedRun(limits RunLimits) (*markingRepo, *Run) {
	repo, run := twoCaseRun()
	run.Snapshot.Limits = &limits
	repo.seedRun(run)
	return &markingRepo{fakeRunRepo: repo}, &run
}

// spend returns a usage reader that charges 60 tokens and 0.5 USD per stored
// result, whole-run reads only; a case read charges caseTokens.
func spend(repo *fakeRunRepo, caseTokens int64) usageFunc {
	return func(caseID string) (Usage, error) {
		if caseID != "" {
			return Usage{Tokens: caseTokens}, nil
		}
		n := int64(len(repo.storedResults()))
		return Usage{Tokens: 60 * n, CostUSD: 0.5 * float64(n)}, nil
	}
}

func execute(repo RunRepository, usage UsageReader, judge Judge) {
	NewOrchestrator(repo, judge, &stubCompleter{answer: "an answer"}, quietLogger()).
		WithUsage(usage).
		Execute(context.Background(), RunRef{ProjectID: "1", RunID: "1"})
}

// THE RUN LIMIT, "stop". Before the second case the run has spent 60 tokens,
// over its 50 token limit: it ends errored with the reason, keeps the case it
// scored and its headline, and records which limit it reached.
func TestRunStopsWhenItsTokenLimitIsReached(t *testing.T) {
	t.Parallel()

	repo, _ := limitedRun(RunLimits{TokenLimitPerRun: intPtr(50), OnRunLimit: RunLimitActionStop})
	judge := &scriptedJudge{answers: []JudgeVerdict{{Score: 5}, {Score: 3}}}
	execute(repo, spend(repo.fakeRunRepo, 0), judge)

	run := repo.run("1")
	if run.Status != RunStatusErrored {
		t.Fatalf("status = %q, want errored", run.Status)
	}
	if !strings.Contains(run.Error, "token limit") || !strings.Contains(run.Error, "1 of 2 cases") {
		t.Errorf("error = %q, want the token limit and the progress", run.Error)
	}
	if got := len(repo.storedResults()); got != 1 {
		t.Errorf("stored %d results, want only the first case", got)
	}
	if run.HeadlineScore == nil || *run.HeadlineScore != 100 {
		t.Errorf("headline = %v, want the scored case's 100", run.HeadlineScore)
	}
	if len(repo.marked) != 1 || repo.marked[0] != LimitReachedTokens {
		t.Errorf("marked = %v, want [tokens]", repo.marked)
	}
}

// "Only report" lets the same run finish and still records the limit.
func TestRunOnlyReportsWhenItsLimitIsReachedAndTheActionIsReport(t *testing.T) {
	t.Parallel()

	repo, _ := limitedRun(RunLimits{TokenLimitPerRun: intPtr(50), OnRunLimit: RunLimitActionReport})
	judge := &scriptedJudge{answers: []JudgeVerdict{{Score: 5}, {Score: 3}}}
	execute(repo, spend(repo.fakeRunRepo, 0), judge)

	run := repo.run("1")
	if run.Status != RunStatusFinished {
		t.Fatalf("status = %q (%s), want finished", run.Status, run.Error)
	}
	if got := len(repo.storedResults()); got != 2 {
		t.Errorf("stored %d results, want both cases", got)
	}
	// Recorded once, not once per case that follows.
	if len(repo.marked) != 1 || repo.marked[0] != LimitReachedTokens {
		t.Errorf("marked = %v, want [tokens] once", repo.marked)
	}
}

func TestRunStopsWhenItsCostLimitIsReached(t *testing.T) {
	t.Parallel()

	cost := 0.4
	repo, _ := limitedRun(RunLimits{CostLimitPerRun: &cost, OnRunLimit: RunLimitActionStop})
	execute(repo, spend(repo.fakeRunRepo, 0), &scriptedJudge{})

	run := repo.run("1")
	if run.Status != RunStatusErrored || !strings.Contains(run.Error, "cost limit") {
		t.Fatalf("status = %q error = %q, want errored on the cost limit", run.Status, run.Error)
	}
	if len(repo.marked) != 1 || repo.marked[0] != LimitReachedCost {
		t.Errorf("marked = %v, want [cost]", repo.marked)
	}
}

// A case limit keeps that case from being judged, and only that case: the run
// carries on and finishes.
func TestCaseLimitSkipsTheJudgeForThatCase(t *testing.T) {
	t.Parallel()

	repo, _ := limitedRun(RunLimits{TokenLimitPerCase: intPtr(10), OnRunLimit: RunLimitActionStop})
	judge := &scriptedJudge{}
	execute(repo, spend(repo.fakeRunRepo, 25), judge)

	if judge.calls != 0 {
		t.Errorf("the judge was called %d times for cases over their limit", judge.calls)
	}
	run := repo.run("1")
	if run.Status != RunStatusFinished {
		t.Fatalf("status = %q, want finished: a case limit does not stop the run", run.Status)
	}
	for _, result := range repo.storedResults() {
		if result.Status != ResultStatusSkipped {
			t.Errorf("case %s: status %q, want skipped", result.DatasetCaseID, result.Status)
		}
		if reason, _ := result.Verdict["error"].(string); !strings.Contains(reason, "case's token limit") {
			t.Errorf("case %s: verdict %v does not say the case limit was reached", result.DatasetCaseID, result.Verdict)
		}
		if result.Evidence["output"] != "an answer" {
			t.Errorf("case %s: the answer is not kept in the evidence", result.DatasetCaseID)
		}
	}
	if run.HeadlineScore != nil {
		t.Errorf("headline = %v, want none: nothing was scored", *run.HeadlineScore)
	}
}

// A run with no limits never reads usage.
func TestRunWithoutLimitsNeverReadsUsage(t *testing.T) {
	t.Parallel()

	repo, _ := twoCaseRun()
	calls := 0
	execute(repo, usageFunc(func(string) (Usage, error) { calls++; return Usage{}, nil }), &scriptedJudge{})

	if calls != 0 {
		t.Errorf("usage was read %d times for a run with no limits", calls)
	}
	if repo.run("1").Status != RunStatusFinished {
		t.Error("the run did not finish")
	}
}

// A usage read that fails does not fail the run: the limit is not enforced
// while it cannot be read, and the run finishes.
func TestRunKeepsGoingWhenUsageCannotBeRead(t *testing.T) {
	t.Parallel()

	repo, _ := limitedRun(RunLimits{TokenLimitPerRun: intPtr(1), TokenLimitPerCase: intPtr(1), OnRunLimit: RunLimitActionStop})
	execute(repo, usageFunc(func(string) (Usage, error) { return Usage{}, errors.New("no request log") }), &scriptedJudge{})

	if run := repo.run("1"); run.Status != RunStatusFinished {
		t.Fatalf("status = %q (%s), want finished", run.Status, run.Error)
	}
}

func TestRunLimitReachedNamesTheLimit(t *testing.T) {
	t.Parallel()

	cost := 2.0
	limits := RunLimits{TokenLimitPerRun: intPtr(100), CostLimitPerRun: &cost}
	if which, _ := runLimitReached(limits, Usage{Tokens: 99, CostUSD: 1.99}); which != "" {
		t.Errorf("under both limits reported %q", which)
	}
	if which, _ := runLimitReached(limits, Usage{Tokens: 100}); which != LimitReachedTokens {
		t.Errorf("at the token limit reported %q", which)
	}
	if which, _ := runLimitReached(limits, Usage{Tokens: 1, CostUSD: 2}); which != LimitReachedCost {
		t.Errorf("at the cost limit reported %q", which)
	}
}

// The last case can cross the run limit. It is checked once more when every
// case is done, so the limit is recorded; nothing is left to stop, so a "stop"
// run finishes.
func TestRunLimitCrossedByTheLastCaseIsRecordedAndTheRunFinishes(t *testing.T) {
	t.Parallel()

	repo, _ := limitedRun(RunLimits{TokenLimitPerRun: intPtr(100), OnRunLimit: RunLimitActionStop})
	// 60 tokens per stored result: 0 before case 1, 60 before case 2 (under
	// 100), 120 after the last one.
	execute(repo, spend(repo.fakeRunRepo, 0), &scriptedJudge{})

	run := repo.run("1")
	if run.Status != RunStatusFinished {
		t.Fatalf("status = %q (%s), want finished", run.Status, run.Error)
	}
	if got := len(repo.storedResults()); got != 2 {
		t.Fatalf("stored %d results, want both cases", got)
	}
	if len(repo.marked) != 1 || repo.marked[0] != LimitReachedTokens {
		t.Errorf("marked = %v, want [tokens] from the final check", repo.marked)
	}
}
