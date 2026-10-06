package execution

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type publisherListResponse struct {
	ids []string
	err error
}

type publisherOutboxStub struct {
	mu           sync.Mutex
	responses    []publisherListResponse
	limits       []int
	retireLimits []int
	retireCount  int
	retireErr    error
}

func (s *publisherOutboxStub) RetireNoAuthorityValidation(_ context.Context, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retireLimits = append(s.retireLimits, limit)
	return s.retireCount, s.retireErr
}

func (s *publisherOutboxStub) ListPendingValidationIDs(_ context.Context, limit int, _ time.Duration) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limits = append(s.limits, limit)
	if len(s.responses) == 0 {
		return nil, nil
	}
	response := s.responses[0]
	s.responses = s.responses[1:]
	return append([]string(nil), response.ids...), response.err
}

type validationDispatchFunc func(context.Context, string) error

func (f validationDispatchFunc) Dispatch(ctx context.Context, outboxID string) error {
	return f(ctx, outboxID)
}

type boundedDispatcherStub struct {
	release chan struct{}
	started chan string
	fail    map[string]error

	mu      sync.Mutex
	current int
	maximum int
	calls   []string
}

func (s *boundedDispatcherStub) Dispatch(ctx context.Context, outboxID string) error {
	s.mu.Lock()
	s.current++
	if s.current > s.maximum {
		s.maximum = s.current
	}
	s.calls = append(s.calls, outboxID)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.current--
		s.mu.Unlock()
	}()

	select {
	case s.started <- outboxID:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-s.release:
		return s.fail[outboxID]
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestOutboxPublisherRunOnceBoundsConcurrencyAndIsolatesItemFailure(t *testing.T) {
	dispatchFailure := errors.New("dependency unavailable")
	outboxIDs := []string{"outbox-1", "outbox-2", "outbox-3", "outbox-4", "outbox-5"}
	outbox := &publisherOutboxStub{responses: []publisherListResponse{{ids: outboxIDs}}}
	dispatcher := &boundedDispatcherStub{
		release: make(chan struct{}),
		started: make(chan string, len(outboxIDs)),
		fail:    map[string]error{"outbox-3": dispatchFailure},
	}
	publisher := newTestOutboxPublisher(t, outbox, dispatcher, OutboxPublisherConfig{
		PollInterval:      time.Hour,
		VisibilityTimeout: time.Minute,
		BatchSize:         len(outboxIDs),
		MaxConcurrent:     2,
		ReportFailure:     func(error) {},
	})

	done := make(chan error, 1)
	go func() {
		done <- publisher.RunOnce(context.Background())
	}()
	for range 2 {
		select {
		case <-dispatcher.started:
		case <-time.After(time.Second):
			t.Fatal("bounded workers did not start")
		}
	}
	close(dispatcher.release)

	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(time.Second):
		t.Fatal("bounded batch did not drain")
	}
	if !errors.Is(runErr, dispatchFailure) || !strings.Contains(runErr.Error(), "outbox-3") {
		t.Fatalf("expected identity-scoped item failure, got %v", runErr)
	}

	dispatcher.mu.Lock()
	maximum := dispatcher.maximum
	calls := append([]string(nil), dispatcher.calls...)
	dispatcher.mu.Unlock()
	if maximum != 2 {
		t.Fatalf("unexpected maximum concurrency: %d", maximum)
	}
	sort.Strings(calls)
	if !equalStrings(calls, outboxIDs) {
		t.Fatalf("one item failure prevented sibling attempts: %v", calls)
	}
	outbox.mu.Lock()
	limits := append([]int(nil), outbox.limits...)
	outbox.mu.Unlock()
	if len(limits) != 1 || limits[0] != len(outboxIDs) {
		t.Fatalf("publisher did not pass the configured query bound: %v", limits)
	}
}

func TestOutboxPublisherFullRetirementBatchDefersLiveDiscovery(t *testing.T) {
	outbox := &publisherOutboxStub{retireCount: 4}
	dispatchCalled := false
	publisher := newTestOutboxPublisher(t, outbox, validationDispatchFunc(func(context.Context, string) error {
		dispatchCalled = true
		return nil
	}), OutboxPublisherConfig{
		PollInterval:      time.Hour,
		VisibilityTimeout: time.Minute,
		BatchSize:         4,
		MaxConcurrent:     1,
		ReportFailure:     func(error) {},
	})
	if err := publisher.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	outbox.mu.Lock()
	defer outbox.mu.Unlock()
	if len(outbox.retireLimits) != 1 || outbox.retireLimits[0] != 4 || len(outbox.limits) != 0 || dispatchCalled {
		t.Fatalf("full retirement batch did not bound the cycle: retire=%v list=%v dispatch=%t", outbox.retireLimits, outbox.limits, dispatchCalled)
	}
}

func TestOutboxPublisherRunCancellationPropagatesAndDrainsActiveWorker(t *testing.T) {
	outbox := &publisherOutboxStub{responses: []publisherListResponse{{ids: []string{"outbox-1"}}}}
	started := make(chan struct{})
	exited := make(chan struct{})
	dispatcher := validationDispatchFunc(func(ctx context.Context, _ string) error {
		close(started)
		<-ctx.Done()
		close(exited)
		return ctx.Err()
	})
	reported := make(chan error, 1)
	publisher := newTestOutboxPublisher(t, outbox, dispatcher, OutboxPublisherConfig{
		PollInterval:      time.Hour,
		VisibilityTimeout: time.Minute,
		BatchSize:         1,
		MaxConcurrent:     1,
		ReportFailure:     func(err error) { reported <- err },
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- publisher.Run(ctx)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("publisher did not start the active dispatch")
	}
	cancel()

	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("active dispatch did not observe cancellation")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation identity, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run returned before neither cancellation nor drain")
	}
	select {
	case err := <-reported:
		t.Fatalf("lifecycle cancellation was reported as a retry failure: %v", err)
	default:
	}
}

func TestOutboxPublisherRunReportsFailedCycleAndContinues(t *testing.T) {
	listFailure := errors.New("postgres unavailable")
	outbox := &publisherOutboxStub{responses: []publisherListResponse{
		{err: listFailure},
		{ids: []string{"outbox-1"}},
	}}
	dispatched := make(chan struct{})
	dispatcher := validationDispatchFunc(func(_ context.Context, _ string) error {
		close(dispatched)
		return nil
	})
	reported := make(chan error, 1)
	publisher := newTestOutboxPublisher(t, outbox, dispatcher, OutboxPublisherConfig{
		PollInterval:      time.Millisecond,
		VisibilityTimeout: time.Minute,
		BatchSize:         1,
		MaxConcurrent:     1,
		ReportFailure:     func(err error) { reported <- err },
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- publisher.Run(ctx)
	}()
	select {
	case err := <-reported:
		if !errors.Is(err, listFailure) {
			t.Fatalf("unexpected reported failure: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("failed cycle was not reported")
	}
	select {
	case <-dispatched:
	case <-time.After(time.Second):
		t.Fatal("publisher did not continue after the failed cycle")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation identity, got %v", err)
	}
}

func TestOutboxPublisherRejectsInvalidBoundsAndStoreOverflow(t *testing.T) {
	valid := OutboxPublisherConfig{
		PollInterval:      time.Second,
		VisibilityTimeout: time.Minute,
		BatchSize:         2,
		MaxConcurrent:     1,
		ReportFailure:     func(error) {},
	}
	outbox := &publisherOutboxStub{}
	dispatcher := validationDispatchFunc(func(context.Context, string) error { return nil })
	tests := []OutboxPublisherConfig{
		{},
		withPublisherVisibilityTimeout(valid, MinOutboxVisibilityTimeout-time.Nanosecond),
		withPublisherVisibilityTimeout(valid, MaxOutboxVisibilityTimeout+time.Nanosecond),
		withPublisherBatchSize(valid, 0),
		withPublisherBatchSize(valid, MaxOutboxPublisherBatchSize+1),
		withPublisherConcurrency(valid, 0),
		withPublisherConcurrency(valid, MaxOutboxPublisherConcurrency+1),
		withPublisherConcurrency(valid, valid.BatchSize+1),
		withPublisherReporter(valid, nil),
	}
	for _, config := range tests {
		if _, err := NewOutboxPublisher(outbox, dispatcher, config); !errors.Is(err, ErrInvalidOutboxPublisherConfig) {
			t.Fatalf("expected invalid configuration for %+v, got %v", config, err)
		}
	}

	overflow := &publisherOutboxStub{responses: []publisherListResponse{{ids: []string{"outbox-1", "outbox-2", "outbox-3"}}}}
	publisher := newTestOutboxPublisher(t, overflow, dispatcher, valid)
	if err := publisher.RunOnce(context.Background()); !errors.Is(err, ErrPendingOutboxBatchLimitExceeded) {
		t.Fatalf("expected store overflow rejection, got %v", err)
	}
}

func newTestOutboxPublisher(t *testing.T, outbox PendingValidationOutbox, dispatcher ValidationDispatchExecutor, config OutboxPublisherConfig) *OutboxPublisher {
	t.Helper()
	publisher, err := NewOutboxPublisher(outbox, dispatcher, config)
	if err != nil {
		t.Fatal(err)
	}
	return publisher
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func withPublisherBatchSize(config OutboxPublisherConfig, value int) OutboxPublisherConfig {
	config.BatchSize = value
	return config
}

func withPublisherVisibilityTimeout(config OutboxPublisherConfig, value time.Duration) OutboxPublisherConfig {
	config.VisibilityTimeout = value
	return config
}

func withPublisherConcurrency(config OutboxPublisherConfig, value int) OutboxPublisherConfig {
	config.MaxConcurrent = value
	return config
}

func withPublisherReporter(config OutboxPublisherConfig, reporter func(error)) OutboxPublisherConfig {
	config.ReportFailure = reporter
	return config
}
