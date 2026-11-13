package runevidence

import (
	"context"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/retention"
)

type memoryPolicies struct{ maxDays int }

func (p memoryPolicies) PlatformPolicy(_ context.Context, class retention.DataClass) (retention.Policy, error) {
	return retention.Policy{Class: class, MaxAgeDays: &p.maxDays}, nil
}

func (memoryPolicies) ProjectOverrides(context.Context, retention.DataClass) (map[int64]retention.Policy, error) {
	return nil, nil
}

// emptyRunStore has no runs: only the methods the step_record pass calls.
type emptyRunStore struct{ RetentionStore }

func (emptyRunStore) ClaimPurges(context.Context, int) ([]Purge, error) { return nil, nil }
func (emptyRunStore) AnchorPending(context.Context, int) (int, error)   { return 0, nil }
func (emptyRunStore) EvidenceProjects(context.Context) ([]int64, error) {
	return []int64{4}, nil
}
func (emptyRunStore) HeldRuns(context.Context, int64, time.Time) (int64, error) { return 0, nil }
func (emptyRunStore) ExpiredRuns(context.Context, int64, time.Time, []string, int) ([]string, error) {
	return nil, nil
}

type memoryStoreSpy struct {
	project int64
	cutoff  time.Time
	calls   int
	// backfills counts the full-scan backfills; backfillRecorded is what each
	// one answers.
	backfills        int
	backfillRecorded int
}

func (s *memoryStoreSpy) BackfillDeletedAgents(context.Context, int64, int) (int, error) {
	s.backfills++
	return s.backfillRecorded, nil
}

func (s *memoryStoreSpy) PurgeDeletedAgents(_ context.Context, project int64, cutoff time.Time, _ int) (int, error) {
	s.project, s.cutoff = project, cutoff
	s.calls++
	return 2, nil
}

// Issue 1658: the step_record pass purges a deleted agent's memory with the
// project's step_record cutoff.
func TestRecordPassPurgesDeletedAgentsMemoryAtTheStepRecordCutoff(t *testing.T) {
	spy := &memoryStoreSpy{}
	sweeper, err := NewSweeper(memoryPolicies{maxDays: 30}, emptyRunStore{}, nil,
		func(context.Context) (bool, error) { return false, nil }, RetentionConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	sweeper.now = func() time.Time { return now }
	sweeper.WithAgentMemory(spy)

	stats, err := sweeper.SweepRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if spy.calls != 1 || spy.project != 4 || !spy.cutoff.Equal(now.Add(-30*24*time.Hour)) || stats.MemoryAgents != 2 {
		t.Fatalf("calls %d project %d cutoff %s agents %d", spy.calls, spy.project, spy.cutoff, stats.MemoryAgents)
	}
}

// Issue 1658: the hourly purge is driven by the orphan rows; the full-scan
// backfill runs once per day per project, not on every pass, and a full batch
// is repeated on the next pass.
func TestRecordPassBackfillsDeletedAgentsOncePerDay(t *testing.T) {
	spy := &memoryStoreSpy{}
	sweeper, err := NewSweeper(memoryPolicies{maxDays: 30}, emptyRunStore{}, nil,
		func(context.Context) (bool, error) { return false, nil }, RetentionConfig{BatchSize: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	sweeper.now = func() time.Time { return now }
	sweeper.WithAgentMemory(spy)
	pass := func() {
		if _, err := sweeper.SweepRecords(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	pass()
	pass()
	if spy.backfills != 1 || spy.calls != 2 {
		t.Fatalf("two passes in a day: backfills %d purges %d, want 1 and 2", spy.backfills, spy.calls)
	}
	now = now.Add(25 * time.Hour)
	spy.backfillRecorded = 10 // a full batch: more may remain
	pass()
	pass()
	if spy.backfills != 3 {
		t.Fatalf("a full batch repeats on the next pass: backfills %d, want 3", spy.backfills)
	}
}

// fullRunStore always has expired runs, so the record pass spends its whole
// budget and truncates.
type fullRunStore struct{ emptyRunStore }

func (fullRunStore) ExpiredRuns(_ context.Context, _ int64, _ time.Time, _ []string, limit int) ([]string, error) {
	runs := make([]string, limit)
	for i := range runs {
		runs[i] = "run"
	}
	return runs, nil
}
func (fullRunStore) ExpireRunContent(context.Context, string, string) error { return nil }
func (fullRunStore) DeleteRun(context.Context, string, time.Time, []string) (bool, error) {
	return true, nil
}

// A record pass that truncates on its budget still purges the memory.
func TestTruncatedRecordPassStillPurgesDeletedAgentsMemory(t *testing.T) {
	spy := &memoryStoreSpy{}
	sweeper, err := NewSweeper(memoryPolicies{maxDays: 30}, fullRunStore{}, nil,
		func(context.Context) (bool, error) { return false, nil }, RetentionConfig{BatchSize: 2, MaxPerPass: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sweeper.WithAgentMemory(spy)
	stats, err := sweeper.SweepRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Truncated || spy.calls != 1 || stats.MemoryAgents != 2 {
		t.Fatalf("truncated %v purges %d agents %d", stats.Truncated, spy.calls, stats.MemoryAgents)
	}
}
