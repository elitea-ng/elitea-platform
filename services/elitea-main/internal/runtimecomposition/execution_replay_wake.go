package runtimecomposition

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	executionapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/executions"
	outputapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/output"
	"github.com/nats-io/nats.go"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/commandbus"
)

// executionReplayWakeSubject is core NATS, not JetStream: the wake-up is
// advisory, and a lost one costs latency only (PostgreSQL polling).
const executionReplayWakeSubject = commandbus.ReplayWakeSubject

const (
	executionReplayWakeQueueSize   = 1024
	executionReplayWakeHistorySize = 4096
	executionReplayWakeRetryMin    = 250 * time.Millisecond
	executionReplayWakeRetryMax    = 30 * time.Second
)

type executionReplayWake struct {
	ProjectID   string `json:"project_id"`
	ExecutionID string `json:"execution_id"`
	Cursor      uint64 `json:"cursor"`
}

func (w executionReplayWake) valid() bool {
	return w.Cursor > 0 && w.ProjectID != "" && len(w.ProjectID) <= 32 &&
		w.ExecutionID != "" && len(w.ExecutionID) <= 128 &&
		!strings.ContainsAny(w.ProjectID, "\x00\r\n") &&
		!strings.ContainsAny(w.ExecutionID, "\x00\r\n")
}

func (w executionReplayWake) key() string {
	return w.ProjectID + "\x00" + w.ExecutionID
}

// replayWakeTransport is the slice of NATS the wake bus uses; a fake stands in
// for it in unit tests.
type replayWakeTransport interface {
	Publish(subject string, data []byte) error
	Subscribe(subject string, deliver func([]byte)) (unsubscribe func() error, err error)
}

type natsReplayWakeTransport struct{ conn *nats.Conn }

func (t natsReplayWakeTransport) Publish(subject string, data []byte) error {
	return t.conn.Publish(subject, data)
}

func (t natsReplayWakeTransport) Subscribe(subject string, deliver func([]byte)) (func() error, error) {
	sub, err := t.conn.Subscribe(subject, func(msg *nats.Msg) { deliver(msg.Data) })
	if err != nil {
		return nil, err
	}
	if err := sub.SetPendingLimits(executionReplayWakeQueueSize, 1<<20); err != nil {
		_ = sub.Unsubscribe()
		return nil, err
	}
	return sub.Unsubscribe, nil
}

// natsExecutionReplayWakeBus carries only a tiny, advisory wake signal between
// elitea-main replicas, on core NATS (elitea.rt.v1.replay.wake, the
// elitea-main-runtime identity). The SSE handler always replays PostgreSQL
// after a wake, so a lost or duplicated message cannot lose, forge, or
// reorder execution output. One shared subscription serves every SSE stream
// in this elitea-main process.
type natsExecutionReplayWakeBus struct {
	transport replayWakeTransport
	logger    *slog.Logger

	notify chan executionReplayWake

	mu          sync.Mutex
	nextWaiter  uint64
	waiters     map[string]map[uint64]chan struct{}
	highWater   map[string]uint64
	historyKeys []string
	queueWarned atomic.Bool
}

func newNATSExecutionReplayWakeBus(conn *nats.Conn, logger *slog.Logger) (*natsExecutionReplayWakeBus, error) {
	if conn == nil {
		return nil, errors.New("execution replay wake NATS connection is required")
	}
	return newExecutionReplayWakeBus(natsReplayWakeTransport{conn: conn}, logger)
}

func newExecutionReplayWakeBus(transport replayWakeTransport, logger *slog.Logger) (*natsExecutionReplayWakeBus, error) {
	if transport == nil || logger == nil {
		return nil, errors.New("execution replay wake transport and logger are required")
	}
	return &natsExecutionReplayWakeBus{
		transport: transport,
		logger:    logger,
		notify:    make(chan executionReplayWake, executionReplayWakeQueueSize),
		waiters:   make(map[string]map[uint64]chan struct{}),
		highWater: make(map[string]uint64),
	}, nil
}

// Notify is deliberately non-blocking. Local streams wake immediately after
// the durable transaction commits; the bounded queue fans the same wake to
// other replicas over NATS. Queue pressure or NATS loss falls back to PostgreSQL
// polling and therefore affects latency only, never correctness.
func (b *natsExecutionReplayWakeBus) Notify(projectID, executionID string, cursor uint64) {
	if b == nil {
		return
	}
	wake := executionReplayWake{ProjectID: projectID, ExecutionID: executionID, Cursor: cursor}
	if !wake.valid() {
		return
	}
	b.dispatch(wake)
	select {
	case b.notify <- wake:
	default:
		if b.queueWarned.CompareAndSwap(false, true) {
			b.logger.Warn("execution replay wake queue is full; bounded polling remains active")
		}
	}
}

func (b *natsExecutionReplayWakeBus) Wait(ctx context.Context, projectID, executionID string, afterCursor uint64) (bool, error) {
	if b == nil || ctx == nil || projectID == "" || executionID == "" ||
		len(projectID) > 32 || len(executionID) > 128 {
		return false, errors.New("execution replay wake wait is invalid")
	}
	key := executionReplayWake{ProjectID: projectID, ExecutionID: executionID}.key()
	wake := make(chan struct{}, 1)

	b.mu.Lock()
	if b.highWater[key] > afterCursor {
		b.mu.Unlock()
		return false, nil
	}
	b.nextWaiter++
	waiterID := b.nextWaiter
	if b.waiters[key] == nil {
		b.waiters[key] = make(map[uint64]chan struct{})
	}
	b.waiters[key][waiterID] = wake
	b.mu.Unlock()

	defer b.removeWaiter(key, waiterID)
	timer := time.NewTimer(phaseOneReplayPollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-wake:
		return false, nil
	case <-timer.C:
		return true, nil
	}
}

// Run subscribes once (retrying with backoff until the first subscribe
// succeeds; nats.go re-subscribes by itself after a reconnect) and publishes
// queued wakes until ctx ends.
func (b *natsExecutionReplayWakeBus) Run(ctx context.Context) error {
	if b == nil || b.transport == nil || ctx == nil {
		return errors.New("execution replay wake lifecycle is incomplete")
	}
	go b.runPublisher(ctx)
	retryDelay := executionReplayWakeRetryMin
	for {
		unsubscribe, err := b.transport.Subscribe(executionReplayWakeSubject, b.receive)
		if err == nil {
			<-ctx.Done()
			_ = unsubscribe()
			return ctx.Err()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		b.logger.Warn("execution replay wake subscription failed; bounded polling remains active", "err", err)
		if err := waitForReplayWakeRetry(ctx, retryDelay); err != nil {
			return err
		}
		retryDelay = min(retryDelay*2, executionReplayWakeRetryMax)
	}
}

func (b *natsExecutionReplayWakeBus) receive(payload []byte) {
	var wake executionReplayWake
	if json.Unmarshal(payload, &wake) != nil || !wake.valid() {
		return
	}
	b.dispatch(wake)
}

func (b *natsExecutionReplayWakeBus) runPublisher(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case wake := <-b.notify:
			b.queueWarned.Store(false)
			encoded, err := json.Marshal(wake)
			if err != nil {
				continue
			}
			if err := b.transport.Publish(executionReplayWakeSubject, encoded); err != nil && ctx.Err() == nil {
				b.logger.Warn("execution replay wake publish failed; bounded polling remains active", "err", err)
			}
		}
	}
}

func waitForReplayWakeRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (b *natsExecutionReplayWakeBus) dispatch(wake executionReplayWake) {
	key := wake.key()
	b.mu.Lock()
	if wake.Cursor > b.highWater[key] {
		if _, exists := b.highWater[key]; !exists {
			if len(b.historyKeys) == executionReplayWakeHistorySize {
				delete(b.highWater, b.historyKeys[0])
				copy(b.historyKeys, b.historyKeys[1:])
				b.historyKeys = b.historyKeys[:len(b.historyKeys)-1]
			}
			b.historyKeys = append(b.historyKeys, key)
		}
		b.highWater[key] = wake.Cursor
	}
	for _, waiter := range b.waiters[key] {
		select {
		case waiter <- struct{}{}:
		default:
		}
	}
	b.mu.Unlock()
}

func (b *natsExecutionReplayWakeBus) removeWaiter(key string, waiterID uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.waiters[key], waiterID)
	if len(b.waiters[key]) == 0 {
		delete(b.waiters, key)
	}
}

type wakingNodeEventIngestor struct {
	next outputNodeEventIngestor
	wake *natsExecutionReplayWakeBus
}

type outputNodeEventIngestor interface {
	IngestNodeEvent(context.Context, outputapp.NodeEventFrame) (outputapp.ProjectionOutcome, error)
}

func (i wakingNodeEventIngestor) IngestNodeEvent(ctx context.Context, frame outputapp.NodeEventFrame) (outputapp.ProjectionOutcome, error) {
	outcome, err := i.next.IngestNodeEvent(ctx, frame)
	if err == nil && !agentTerminalNodeEvent(frame.BrowserData) {
		i.wake.Notify(frame.ProjectionProjectID, frame.Fence.ExecutionID, outcome.Cursor)
	}
	return outcome, err
}

func agentTerminalNodeEvent(data []byte) bool {
	var event struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &event) != nil {
		return false
	}
	switch event.Type {
	case "full_message", "agent_hitl_interrupt", "mcp_authorization_required":
		return true
	default:
		return false
	}
}

type wakingAgentExecutionIngestor struct {
	next outputAgentExecutionIngestor
	wake *natsExecutionReplayWakeBus
}

type outputAgentExecutionIngestor interface {
	IngestAgent(context.Context, outputapp.AgentExecutionFrame) (outputapp.ProjectionOutcome, error)
}

func (i wakingAgentExecutionIngestor) IngestAgent(ctx context.Context, frame outputapp.AgentExecutionFrame) (outputapp.ProjectionOutcome, error) {
	outcome, err := i.next.IngestAgent(ctx, frame)
	if err == nil {
		i.wake.Notify(frame.ProjectionProjectID, frame.Fence.ExecutionID, outcome.Cursor)
	}
	return outcome, err
}

var (
	_ executionapi.ReplayWaiter = (*natsExecutionReplayWakeBus)(nil)
	_ publisherRunner           = (*natsExecutionReplayWakeBus)(nil)
)
