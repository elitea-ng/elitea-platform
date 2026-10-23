package runtimecomposition

import (
	"context"
	"errors"
	"testing"
	"time"

	indexscheduleapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/indexschedule"
	schedulingapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/scheduling"
)

func TestCurrentIndexScheduleDueWorkAdmitsExactFencedOccurrence(t *testing.T) {
	dueAt := time.Date(2026, 7, 30, 18, 4, 0, 0, time.FixedZone("EEST", 3*60*60))
	scanner := &currentIndexScheduleScannerStub{
		result: indexscheduleapp.TickResult{Admitted: 2, Idempotent: 1},
	}
	work, err := newCurrentIndexScheduleDueWork(scanner)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := work.Execute(
		context.Background(),
		currentIndexScheduledOccurrence(dueAt),
	)
	if err != nil ||
		outcome != schedulingapp.OutcomeDurablyAdmitted ||
		scanner.calls != 1 ||
		!scanner.occurrence.Equal(dueAt) {
		t.Fatalf(
			"outcome=%q error=%v calls=%d occurrence=%s",
			outcome,
			err,
			scanner.calls,
			scanner.occurrence,
		)
	}
}

func TestCurrentIndexScheduleDueWorkDoesNotAcknowledgeIncompleteScan(t *testing.T) {
	for _, test := range []struct {
		name   string
		result indexscheduleapp.TickResult
	}{
		{
			name: "partial dependency failure",
			result: indexscheduleapp.TickResult{
				Admitted: 1, DependencyErrors: 1,
			},
		},
		{
			name:   "local overlap",
			result: indexscheduleapp.TickResult{SkippedOverlap: true},
		},
		{
			name: "scheduler unavailable",
			result: indexscheduleapp.TickResult{
				SkippedUnavailable: true,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			work, err := newCurrentIndexScheduleDueWork(
				&currentIndexScheduleScannerStub{result: test.result},
			)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := work.Execute(
				context.Background(),
				currentIndexScheduledOccurrence(time.Now()),
			)
			if outcome != "" ||
				!errors.Is(err, indexscheduleapp.ErrScheduleDependency) {
				t.Fatalf("outcome=%q error=%v", outcome, err)
			}
		})
	}
}

func TestCurrentIndexScheduleDueWorkPropagatesCancellation(t *testing.T) {
	scanner := &currentIndexScheduleScannerStub{err: context.Canceled}
	work, err := newCurrentIndexScheduleDueWork(scanner)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := work.Execute(
		context.Background(),
		currentIndexScheduledOccurrence(time.Now()),
	)
	if outcome != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("outcome=%q error=%v", outcome, err)
	}
}

func TestCurrentIndexScheduleDueWorkRejectsWrongRevisionBeforeScan(t *testing.T) {
	scanner := &currentIndexScheduleScannerStub{}
	work, err := newCurrentIndexScheduleDueWork(scanner)
	if err != nil {
		t.Fatal(err)
	}
	occurrence := currentIndexScheduledOccurrence(time.Now())
	occurrence.ScheduleRevision = "changed"
	if _, err := work.Execute(context.Background(), occurrence); !errors.Is(
		err,
		indexscheduleapp.ErrInvalidRequest,
	) {
		t.Fatalf("error=%v", err)
	}
	if scanner.calls != 0 {
		t.Fatalf("scanner calls=%d", scanner.calls)
	}
}

type currentIndexScheduleScannerStub struct {
	calls      int
	occurrence time.Time
	result     indexscheduleapp.TickResult
	err        error
}

func (stub *currentIndexScheduleScannerStub) RunDue(
	_ context.Context,
	occurrence time.Time,
) (indexscheduleapp.TickResult, error) {
	stub.calls++
	stub.occurrence = occurrence
	return stub.result, stub.err
}

func currentIndexScheduledOccurrence(
	dueAt time.Time,
) schedulingapp.Occurrence {
	return schedulingapp.Occurrence{
		InvocationID:     "fenced-occurrence-1",
		JobID:            currentIndexScheduleCapability,
		ScheduleRevision: currentIndexScheduleRevision,
		DueAt:            dueAt,
		LeaseEpoch:       3,
		ClaimFence:       "0123456789abcdef",
	}
}
