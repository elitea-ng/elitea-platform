package runtimecomposition

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"sync"

	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
)

// fakeWakeBroker is an in-memory core-NATS stand-in: every subscriber on a
// subject receives every publish. The real-server path is
// TestRuntimeNATSPlaneOnTheChartsPermissions (nats_secured_test.go).
type fakeWakeBroker struct {
	mu          sync.Mutex
	subscribers map[int]func([]byte)
	next        int
	failSub     bool
}

func (b *fakeWakeBroker) Publish(_ string, data []byte) error {
	b.mu.Lock()
	subscribers := make([]func([]byte), 0, len(b.subscribers))
	for _, deliver := range b.subscribers {
		subscribers = append(subscribers, deliver)
	}
	b.mu.Unlock()
	for _, deliver := range subscribers {
		deliver(append([]byte(nil), data...))
	}
	return nil
}

func (b *fakeWakeBroker) Subscribe(_ string, deliver func([]byte)) (func() error, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failSub {
		return nil, errors.New("subscription unavailable")
	}
	if b.subscribers == nil {
		b.subscribers = map[int]func([]byte){}
	}
	b.next++
	id := b.next
	b.subscribers[id] = deliver
	return func() error {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.subscribers, id)
		return nil
	}, nil
}

func (b *fakeWakeBroker) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribers)
}

type nodeEventIngestorStub struct {
	outcome outputapp.ProjectionOutcome
	err     error
}

func (s nodeEventIngestorStub) IngestNodeEvent(context.Context, outputapp.NodeEventFrame) (outputapp.ProjectionOutcome, error) {
	return s.outcome, s.err
}

type agentExecutionIngestorStub struct {
	outcome outputapp.ProjectionOutcome
	err     error
}

func (s agentExecutionIngestorStub) IngestAgent(context.Context, outputapp.AgentExecutionFrame) (outputapp.ProjectionOutcome, error) {
	return s.outcome, s.err
}

func TestExecutionReplayWakeTargetsExactExecutionAndRetainsHighWater(t *testing.T) {
	bus := newTestReplayWakeBus(t, &fakeWakeBroker{})

	matching := make(chan error, 1)
	go func() {
		_, err := bus.Wait(context.Background(), "42", "execution-1", 6)
		matching <- err
	}()
	nonMatchingContext, cancelNonMatching := context.WithCancel(context.Background())
	nonMatching := make(chan error, 1)
	go func() {
		_, err := bus.Wait(nonMatchingContext, "42", "execution-2", 6)
		nonMatching <- err
	}()

	waitForReplayWaiters(t, bus, 2)
	bus.Notify("42", "execution-1", 7)
	select {
	case err := <-matching:
		if err != nil {
			t.Fatalf("matching waiter: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("matching execution was not woken")
	}
	select {
	case err := <-nonMatching:
		t.Fatalf("unrelated execution was woken: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	cancelNonMatching()
	if err := <-nonMatching; !errors.Is(err, context.Canceled) {
		t.Fatalf("unrelated waiter cancellation = %v", err)
	}

	started := time.Now()
	if heartbeat, err := bus.Wait(context.Background(), "42", "execution-1", 6); err != nil || heartbeat {
		t.Fatalf("retained wake = heartbeat %t, err %v", heartbeat, err)
	}
	if time.Since(started) > 25*time.Millisecond {
		t.Fatal("retained wake did not release immediately")
	}
}

func TestExecutionReplayWakeCrossReplicaSignal(t *testing.T) {
	broker := &fakeWakeBroker{}
	first := newTestReplayWakeBus(t, broker)
	second := newTestReplayWakeBus(t, broker)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	results := make(chan error, 2)
	go func() { results <- first.Run(ctx) }()
	go func() { results <- second.Run(ctx) }()
	waitForReplaySubscribers(t, broker, 2)

	woken := make(chan error, 1)
	go func() {
		_, err := second.Wait(context.Background(), "42", "execution-1", 6)
		woken <- err
	}()
	waitForReplayWaiters(t, second, 1)
	first.Notify("42", "execution-1", 7)
	select {
	case err := <-woken:
		if err != nil {
			t.Fatalf("cross-replica waiter: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cross-replica wake was not delivered")
	}
	cancel()
	for range 2 {
		if err := <-results; !errors.Is(err, context.Canceled) {
			t.Fatalf("wake lifecycle shutdown = %v", err)
		}
	}
}

func TestExecutionReplayWakePublisherDrainsWhenSubscriptionIsUnavailable(t *testing.T) {
	bus := newTestReplayWakeBus(t, &fakeWakeBroker{failSub: true})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go bus.runPublisher(ctx)

	for cursor := uint64(1); cursor <= executionReplayWakeQueueSize; cursor++ {
		bus.notify <- executionReplayWake{
			ProjectID:   "42",
			ExecutionID: "execution-1",
			Cursor:      cursor,
		}
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(bus.notify) < executionReplayWakeQueueSize {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("publisher did not drain the advisory wake queue while subscription was unavailable")
}

func TestWakingNodeEventIngestorDefersTerminalUntilAgentProjection(t *testing.T) {
	bus := newTestReplayWakeBus(t, &fakeWakeBroker{})
	ingestor := wakingNodeEventIngestor{
		next: nodeEventIngestorStub{outcome: outputapp.ProjectionOutcome{Cursor: 7}},
		wake: bus,
	}
	frame := outputapp.NodeEventFrame{
		ProjectionProjectID: "42",
		BrowserData:         []byte(`{"type":"full_message"}`),
	}
	frame.Fence.ExecutionID = "execution-1"
	if _, err := ingestor.IngestNodeEvent(context.Background(), frame); err != nil {
		t.Fatal(err)
	}
	if got := replayWakeHighWater(bus, "42", "execution-1"); got != 0 {
		t.Fatalf("terminal node event woke SSE before projection: %d", got)
	}

	frame.BrowserData = []byte(`{"type":"next_input_suggestion_ready"}`)
	if _, err := ingestor.IngestNodeEvent(context.Background(), frame); err != nil {
		t.Fatal(err)
	}
	if got := replayWakeHighWater(bus, "42", "execution-1"); got != 7 {
		t.Fatalf("nonterminal node event wake = %d", got)
	}
}

func TestWakingAgentExecutionIngestorSignalsOnlyCommittedProjection(t *testing.T) {
	bus := newTestReplayWakeBus(t, &fakeWakeBroker{})
	frame := outputapp.AgentExecutionFrame{ProjectionProjectID: "42"}
	frame.Fence.ExecutionID = "execution-1"

	success := wakingAgentExecutionIngestor{
		next: agentExecutionIngestorStub{outcome: outputapp.ProjectionOutcome{Cursor: 9}},
		wake: bus,
	}
	if _, err := success.IngestAgent(context.Background(), frame); err != nil {
		t.Fatal(err)
	}
	if got := replayWakeHighWater(bus, "42", "execution-1"); got != 9 {
		t.Fatalf("committed terminal projection wake = %d", got)
	}

	failure := wakingAgentExecutionIngestor{
		next: agentExecutionIngestorStub{err: errors.New("projection failed")},
		wake: bus,
	}
	frame.Fence.ExecutionID = "execution-2"
	if _, err := failure.IngestAgent(context.Background(), frame); err == nil {
		t.Fatal("failed projection unexpectedly succeeded")
	}
	if got := replayWakeHighWater(bus, "42", "execution-2"); got != 0 {
		t.Fatalf("failed projection woke SSE: %d", got)
	}
}

func newTestReplayWakeBus(t *testing.T, transport replayWakeTransport) *natsExecutionReplayWakeBus {
	t.Helper()
	bus, err := newExecutionReplayWakeBus(
		transport,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	return bus
}

func waitForReplayWaiters(t *testing.T, bus *natsExecutionReplayWakeBus, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		bus.mu.Lock()
		waiters := 0
		for _, keyed := range bus.waiters {
			waiters += len(keyed)
		}
		bus.mu.Unlock()
		if waiters == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waiter count did not reach %d", count)
}

func waitForReplaySubscribers(t *testing.T, broker *fakeWakeBroker, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if broker.count() == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("subscriber count did not reach %d", count)
}

func replayWakeHighWater(bus *natsExecutionReplayWakeBus, projectID, executionID string) uint64 {
	key := executionReplayWake{ProjectID: projectID, ExecutionID: executionID}.key()
	bus.mu.Lock()
	defer bus.mu.Unlock()
	return bus.highWater[key]
}
