package execution

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestOutboxPublisherFailureDelayIsBoundedAndJittered(t *testing.T) {
	maximumJitter := func(maximum time.Duration) time.Duration { return maximum }
	zeroJitter := func(time.Duration) time.Duration { return 0 }
	invalidJitter := func(maximum time.Duration) time.Duration { return maximum + 1 }

	tests := []struct {
		name     string
		poll     time.Duration
		failures int
		jitter   func(time.Duration) time.Duration
		want     time.Duration
	}{
		{name: "first failure retains floor and adds bounded jitter", poll: 250 * time.Millisecond, failures: 1, jitter: maximumJitter, want: 375 * time.Millisecond},
		{name: "second failure doubles floor", poll: 250 * time.Millisecond, failures: 2, jitter: maximumJitter, want: 750 * time.Millisecond},
		{name: "seventh failure remains below ceiling", poll: 250 * time.Millisecond, failures: 7, jitter: maximumJitter, want: 24 * time.Second},
		{name: "steady state reaches declared ceiling", poll: 250 * time.Millisecond, failures: 100, jitter: maximumJitter, want: 30 * time.Second},
		{name: "steady state floor is twenty seconds", poll: 250 * time.Millisecond, failures: 100, jitter: zeroJitter, want: 20 * time.Second},
		{name: "long configured poll is never shortened", poll: time.Hour, failures: 100, jitter: maximumJitter, want: time.Hour},
		{name: "poll between floor and ceiling is never shortened", poll: 25 * time.Second, failures: 1, jitter: maximumJitter, want: 30 * time.Second},
		{name: "invalid jitter fails to the exponential floor", poll: 250 * time.Millisecond, failures: 2, jitter: invalidJitter, want: 500 * time.Millisecond},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := outboxPublisherFailureDelay(test.poll, test.failures, test.jitter); got != test.want {
				t.Fatalf("failure delay=%s, want %s", got, test.want)
			}
		})
	}
}

func TestOutboxPublisherFailureBackoffResetsAfterSuccessAndCancels(t *testing.T) {
	dependencyFailure := errors.New("command bus unavailable")
	outbox := &publisherOutboxStub{responses: []publisherListResponse{
		{err: dependencyFailure},
		{},
		{err: dependencyFailure},
	}}
	publisher := newTestOutboxPublisher(t, outbox, validationDispatchFunc(func(context.Context, string) error {
		return nil
	}), OutboxPublisherConfig{
		PollInterval:      250 * time.Millisecond,
		VisibilityTimeout: time.Minute,
		BatchSize:         1,
		MaxConcurrent:     1,
		ReportFailure:     func(error) {},
	})
	publisher.jitter = func(maximum time.Duration) time.Duration { return maximum }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	delays := make([]time.Duration, 0, 3)
	publisher.wait = func(ctx context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		if len(delays) == 3 {
			cancel()
			return ctx.Err()
		}
		return nil
	}

	if err := publisher.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("publisher cancellation identity=%v", err)
	}
	want := []time.Duration{375 * time.Millisecond, 250 * time.Millisecond, 375 * time.Millisecond}
	if len(delays) != len(want) {
		t.Fatalf("wait calls=%v, want %v", delays, want)
	}
	for index := range want {
		if delays[index] != want[index] {
			t.Fatalf("delay[%d]=%s, want %s; success did not reset backoff", index, delays[index], want[index])
		}
	}
}

type sustainedFailureDispatcher struct {
	failure error
	current atomic.Int64
	maximum atomic.Int64
	calls   atomic.Int64
}

func (d *sustainedFailureDispatcher) Dispatch(context.Context, string) error {
	current := d.current.Add(1)
	defer d.current.Add(-1)
	d.calls.Add(1)
	for {
		maximum := d.maximum.Load()
		if current <= maximum || d.maximum.CompareAndSwap(maximum, current) {
			break
		}
	}
	runtime.Gosched()
	return d.failure
}

func TestOutboxPublisherSustainedFailureHasConstantPerCycleWorkAndConcurrency(t *testing.T) {
	const (
		cycles        = 64
		batchSize     = 64
		maxConcurrent = 8
	)
	dependencyFailure := errors.New("command bus unavailable")
	ids := make([]string, batchSize)
	for index := range ids {
		ids[index] = fmt.Sprintf("outbox-%03d", index)
	}
	responses := make([]publisherListResponse, cycles)
	for index := range responses {
		responses[index].ids = ids
	}
	outbox := &publisherOutboxStub{responses: responses}
	dispatcher := &sustainedFailureDispatcher{failure: dependencyFailure}
	publisher := newTestOutboxPublisher(t, outbox, dispatcher, OutboxPublisherConfig{
		PollInterval:      250 * time.Millisecond,
		VisibilityTimeout: time.Minute,
		BatchSize:         batchSize,
		MaxConcurrent:     maxConcurrent,
		ReportFailure:     func(error) {},
	})

	for cycle := range cycles {
		if err := publisher.RunOnce(context.Background()); !errors.Is(err, dependencyFailure) {
			t.Fatalf("cycle %d failure identity=%v", cycle, err)
		}
		if active := dispatcher.current.Load(); active != 0 {
			t.Fatalf("cycle %d left %d publisher workers active", cycle, active)
		}
	}
	if calls := dispatcher.calls.Load(); calls != cycles*batchSize {
		t.Fatalf("dispatch attempts=%d, want exactly one bounded attempt per row per cycle (%d)", calls, cycles*batchSize)
	}
	if maximum := dispatcher.maximum.Load(); maximum <= 0 || maximum > maxConcurrent {
		t.Fatalf("maximum concurrent dispatches=%d, configured bound=%d", maximum, maxConcurrent)
	}
	outbox.mu.Lock()
	defer outbox.mu.Unlock()
	if len(outbox.limits) != cycles || len(outbox.retireLimits) != cycles {
		t.Fatalf("database scans were not one-per-cycle: list=%d retire=%d cycles=%d", len(outbox.limits), len(outbox.retireLimits), cycles)
	}
	for index := range cycles {
		if outbox.limits[index] != batchSize || outbox.retireLimits[index] != batchSize {
			t.Fatalf("cycle %d exceeded database scan bound: list=%d retire=%d", index, outbox.limits[index], outbox.retireLimits[index])
		}
	}
}
