package runevidence

import (
	"context"
	"testing"
	"time"
)

type sandboxSpy struct {
	project int64
	cutoff  time.Time
	limits  []int
	answers []int
}

func (s *sandboxSpy) PurgeSettledRuns(_ context.Context, project int64, cutoff time.Time, limit int) (int, error) {
	s.project, s.cutoff = project, cutoff
	s.limits = append(s.limits, limit)
	n := s.answers[0]
	s.answers = s.answers[1:]
	return n, nil
}

// #2241: the step_record pass clears the worker's sandbox rows at the
// project's step_record cutoff, repeating a full batch within the same pass.
func TestRecordPassClearsSandboxRunStateAtTheStepRecordCutoff(t *testing.T) {
	spy := &sandboxSpy{answers: []int{10, 3}}
	sweeper, err := NewSweeper(memoryPolicies{maxDays: 30}, emptyRunStore{}, nil,
		func(context.Context) (bool, error) { return false, nil }, RetentionConfig{BatchSize: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	sweeper.now = func() time.Time { return now }
	sweeper.WithSandboxState(spy)

	stats, err := sweeper.SweepRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if spy.project != 4 || !spy.cutoff.Equal(now.Add(-30*24*time.Hour)) || len(spy.limits) != 2 || stats.SandboxRuns != 13 {
		t.Fatalf("project %d cutoff %s limits %v runs %d", spy.project, spy.cutoff, spy.limits, stats.SandboxRuns)
	}
}
