package spi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// The invocation registry: submit, poll, cancel, retention. Statuses are the
// shell's — pending / running / stopped internally, Started / InProgress on
// the wire, then the stored terminal body verbatim — and custom_events are
// read once: a poll returns what accumulated since the previous poll and
// clears it, which is what the facade's and the web app's poll loops rely on.

const (
	statusPending = "pending"
	statusRunning = "running"
	statusStopped = "stopped"

	wireStarted    = "Started"
	wireInProgress = "InProgress"

	invocationIDPrefix = "invocation_"
)

// ErrCancelled is what Context.Checkpoint returns once a stop was requested;
// a runner that returns it (or wraps it) is recorded as cancelled.
var ErrCancelled = errors.New("invocation cancelled")

// Invocation is one accepted tool call and everything the SPI reports on it.
type Invocation struct {
	ID          string
	Toolkit     string
	Tool        string
	Status      string
	CreatedAt   time.Time
	FinishedAt  time.Time
	StopRequest bool
	Events      []map[string]any
	Result      map[string]any
}

// Terminal reports whether the invocation has a result.
func (i *Invocation) Terminal() bool { return i.Status == statusStopped }

// Store keeps invocations. The in-memory implementation is the default and
// reports Durable false; a durable one reports true and survives a restart.
type Store interface {
	Durable() bool
	Create(ctx context.Context, invocation *Invocation) error
	Get(ctx context.Context, toolkit, tool, id string) (*Invocation, error)
	Update(ctx context.Context, invocation *Invocation) error
	AppendEvent(ctx context.Context, id, message string) error
	DrainEvents(ctx context.Context, invocation *Invocation) ([]map[string]any, error)
	Prune(ctx context.Context, olderThan time.Duration) (int, error)
}

// MemoryStore is the process-local store.
type MemoryStore struct {
	mu   sync.Mutex
	rows map[string]*Invocation // key: toolkit + "/" + tool + "/" + id
	byID map[string]*Invocation
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rows: map[string]*Invocation{}, byID: map[string]*Invocation{}}
}

func key(toolkit, tool, id string) string { return toolkit + "/" + tool + "/" + id }

func (s *MemoryStore) Durable() bool { return false }

func (s *MemoryStore) Create(_ context.Context, invocation *Invocation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[key(invocation.Toolkit, invocation.Tool, invocation.ID)] = invocation
	s.byID[invocation.ID] = invocation
	return nil
}

func (s *MemoryStore) Get(_ context.Context, toolkit, tool, id string) (*Invocation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows[key(toolkit, tool, id)], nil
}

func (s *MemoryStore) Update(context.Context, *Invocation) error { return nil }

func (s *MemoryStore) AppendEvent(_ context.Context, id, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if invocation := s.byID[id]; invocation != nil {
		invocation.Events = append(invocation.Events, map[string]any{"data": map[string]any{"message": message}})
	}
	return nil
}

func (s *MemoryStore) DrainEvents(_ context.Context, invocation *Invocation) ([]map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	events := invocation.Events
	invocation.Events = nil
	return events, nil
}

func (s *MemoryStore) Prune(_ context.Context, olderThan time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-olderThan)
	removed := 0
	for k, invocation := range s.rows {
		if invocation.Terminal() && invocation.FinishedAt.Before(cutoff) {
			delete(s.rows, k)
			delete(s.byID, invocation.ID)
			removed++
		}
	}
	return removed, nil
}

// Count is the number of invocations held, terminal ones included.
func (s *MemoryStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows)
}

// Context is what a running tool is handed: progress, cancellation.
type Context struct {
	invocation *Invocation
	manager    *Manager
}

// InvocationID names the invocation.
func (c *Context) InvocationID() string { return c.invocation.ID }

// Toolkit and Tool name what is running.
func (c *Context) Toolkit() string { return c.invocation.Toolkit }
func (c *Context) Tool() string    { return c.invocation.Tool }

// Thinking emits one progress event (the legacy invocation_thinking).
func (c *Context) Thinking(ctx context.Context, message string) error {
	return c.manager.store.AppendEvent(ctx, c.invocation.ID, message)
}

// Token emits one fragment of the ANSWER (issue #701).
//
// It travels down the same read-once event list as the progress, so the two
// keep the order the tool produced them in, but it is wrapped in the
// structured `{event, data}` envelope the event text already carries for
// todo_update and tool_start. That is what lets a consumer tell an answer
// fragment from a progress line without guessing: a reader that does not
// know `llm_chunk` shows it as one more step, and a reader that does
// appends it to the answer as it is written.
//
// The envelope is frozen with the rest of the SPI's event vocabulary in
// conformance/provider/fixtures/deepwiki/stream/token_events.json.
//
// An empty fragment is DROPPED. The poll drain discards an event whose
// message is empty, but this message would not be empty — it would be a
// well-formed envelope around nothing — so it would survive the drain and
// reach the browser as a token that adds no text.
func (c *Context) Token(ctx context.Context, text string) error {
	if text == "" {
		return nil
	}
	return c.manager.store.AppendEvent(ctx, c.invocation.ID, TokenEvent(text))
}

// tokenEvent is the envelope one answer fragment travels in. A STRUCT and
// not a map, so the keys come out in the declared order and the text on the
// wire is the text the golden fixture spells out; a map would order them
// alphabetically and the two would only be comparable after parsing.
type tokenEvent struct {
	Event string         `json:"event"`
	Data  tokenEventText `json:"data"`
}

type tokenEventText struct {
	Text string `json:"text"`
}

// TokenEvent is one answer fragment in the event text that carries it.
//
// Exported because the fixture runner and the tests build the same string,
// and a second spelling of it would be a second thing to keep in step with
// the browser's adapter.
func TokenEvent(text string) string {
	event, err := json.Marshal(tokenEvent{Event: "llm_chunk", Data: tokenEventText{Text: text}})
	if err != nil {
		// A string always marshals. Returning the raw text rather than an
		// empty event keeps the fragment visible in the thinking log if
		// this ever becomes reachable.
		return text
	}
	return string(event)
}

// Checkpoint is the cooperative cancellation point: nil to go on,
// ErrCancelled once a stop was requested.
func (c *Context) Checkpoint() error {
	c.manager.mu.Lock()
	stop := c.invocation.StopRequest
	c.manager.mu.Unlock()
	if stop {
		return ErrCancelled
	}
	return nil
}

// StopRequested reports whether a cancel arrived.
func (c *Context) StopRequested() bool { return c.Checkpoint() != nil }

// Call runs one tool and returns its terminal body — at minimum
// invocation_id, status (Completed or Error), result (a JSON string of the
// result-object list) and result_type. An error is the normal failure path
// and is classified into the error contract.
type Call func(ctx context.Context, tc *Context) (map[string]any, error)

// Manager owns the registry and the goroutines behind it.
// inFlight is one run this process owns: how to cancel its context, and
// the struct its Context checkpoints on.
type inFlight struct {
	cancel     context.CancelFunc
	invocation *Invocation
}

type Manager struct {
	store     Store
	logger    *slog.Logger
	retention time.Duration

	mu       sync.Mutex
	inFlight map[string]*inFlight
	wg       sync.WaitGroup
	stopOnce sync.Once
	stopped  chan struct{}
}

// NewManager wraps a store; nil means an in-memory one.
func NewManager(store Store, retention time.Duration, logger *slog.Logger) *Manager {
	if store == nil {
		store = NewMemoryStore()
	}
	if logger == nil {
		logger = slog.Default()
	}
	if retention <= 0 {
		retention = time.Hour
	}
	return &Manager{store: store, logger: logger, retention: retention, inFlight: map[string]*inFlight{}, stopped: make(chan struct{})}
}

// Store exposes the registry's store (for /health's durable flag).
func (m *Manager) Store() Store { return m.store }

// Start begins housekeeping: terminal invocations older than the retention
// are pruned once a minute.
func (m *Manager) Start(ctx context.Context) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-m.stopped:
				return
			case <-ticker.C:
				if removed, err := m.store.Prune(ctx, m.retention); err != nil {
					m.logger.Error("invocation housekeeping failed", "error", err)
				} else if removed > 0 {
					m.logger.Info("pruned terminal invocations", "count", removed)
				}
			}
		}
	}()
}

// Stop cancels every in-flight invocation and waits for them to record a
// terminal body ("Service stopped while the invocation was running").
func (m *Manager) Stop() {
	m.stopOnce.Do(func() { close(m.stopped) })
	m.mu.Lock()
	for _, live := range m.inFlight {
		live.cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
}

func newInvocationID() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return invocationIDPrefix + hex.EncodeToString(raw[:])
}

// Submit accepts an invocation and starts it. It returns immediately: the
// SPI is asynchronous unconditionally, even for a tool whose descriptor
// advertises sync_invocation_supported — the facade's poll loop depends on
// that, and so does an invocation whose toolkit is unknown, which is refused
// INSIDE the run so the caller learns of it by polling, as the fixtures
// record.
func (m *Manager) Submit(ctx context.Context, toolkit, tool string, call Call) (*Invocation, error) {
	invocation := &Invocation{ID: newInvocationID(), Toolkit: toolkit, Tool: tool, Status: statusPending, CreatedAt: time.Now()}
	if err := m.store.Create(ctx, invocation); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	m.mu.Lock()
	m.inFlight[invocation.ID] = &inFlight{cancel: cancel, invocation: invocation}
	m.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			cancel()
			m.mu.Lock()
			delete(m.inFlight, invocation.ID)
			m.mu.Unlock()
		}()
		m.run(runCtx, invocation, call)
	}()
	return invocation, nil
}

func (m *Manager) run(ctx context.Context, invocation *Invocation, call Call) {
	m.mu.Lock()
	invocation.Status = statusRunning
	m.mu.Unlock()
	_ = m.store.Update(ctx, invocation)

	var result map[string]any
	body, err := call(ctx, &Context{invocation: invocation, manager: m})
	switch {
	case err == nil:
		result = body
	case errors.Is(err, ErrCancelled):
		result = ToolError(m.logger, invocation.ID, invocation.Tool, Failf(KindRuntime, "Invocation cancelled"))
	case ctx.Err() != nil:
		result = ToolError(m.logger, invocation.ID, invocation.Tool, Failf(KindRuntime, "Service stopped while the invocation was running"))
	default:
		result = ToolError(m.logger, invocation.ID, invocation.Tool, err)
	}
	if result == nil {
		result = ToolError(m.logger, invocation.ID, invocation.Tool, Failf(KindRuntime, "the tool returned no body"))
	}
	m.mu.Lock()
	invocation.Status = statusStopped
	invocation.FinishedAt = time.Now()
	invocation.Result = result
	m.mu.Unlock()
	_ = m.store.Update(context.WithoutCancel(ctx), invocation)
}

// Poll projects an invocation for the wire: nil when unknown; Started or
// InProgress with the drained events while it runs; the terminal body, with
// any events that arrived since the last read, once it stopped.
func (m *Manager) Poll(ctx context.Context, toolkit, tool, id string) (map[string]any, error) {
	invocation, err := m.store.Get(ctx, toolkit, tool, id)
	if err != nil || invocation == nil {
		return nil, err
	}
	events, err := m.store.DrainEvents(ctx, invocation)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	status, result := invocation.Status, invocation.Result
	m.mu.Unlock()
	body := map[string]any{}
	if status == statusStopped && result != nil {
		for k, v := range result {
			body[k] = v
		}
	} else {
		body["invocation_id"] = id
		body["status"] = wireInProgress
		if status == statusPending {
			body["status"] = wireStarted
		}
	}
	if len(events) > 0 {
		body["custom_events"] = events
	}
	return body, nil
}

// Cancel requests a stop; false when the invocation is unknown. The tool
// observes it at its next Checkpoint — DELETE is 204 and only a request.
func (m *Manager) Cancel(ctx context.Context, toolkit, tool, id string) (bool, error) {
	invocation, err := m.store.Get(ctx, toolkit, tool, id)
	if err != nil || invocation == nil {
		return false, err
	}
	// The flag must reach the struct the RUNNING call checkpoints on. The
	// in-memory store hands back that very struct; a durable store hands
	// back a fresh row, so the live one is flipped too when this process
	// owns the run. (A run owned by another replica cannot be stopped from
	// here — the same limit the Python store had; its row still records
	// the request.)
	m.mu.Lock()
	invocation.StopRequest = true
	if live, ok := m.inFlight[id]; ok {
		live.invocation.StopRequest = true
	}
	m.mu.Unlock()
	return true, m.store.Update(ctx, invocation)
}

// InFlight is the number of invocations still running — what /slots reports
// as active.
func (m *Manager) InFlight() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.inFlight)
}
