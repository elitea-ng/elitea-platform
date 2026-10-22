package spi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
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
	// Labels are facts a runner attaches to a running invocation so that
	// others can find it (Manager.StopMatching): the project it works for and
	// the wiki it builds, say. Guarded by the manager's lock.
	Labels map[string]string
	// HoldsSlot is true for an invocation that takes one of the limited
	// slots while it runs (SlotHolder): it is admitted against the slots
	// and waits for a local one before it runs.
	HoldsSlot bool
	Events    []map[string]any
	Result    map[string]any
}

// state is the part of the invocation a store persists, copied — the
// manager's lock held by the caller. The events and labels are not in it:
// the store guards the first and the manager the second.
func (i *Invocation) state() Invocation {
	return Invocation{
		ID: i.ID, Toolkit: i.Toolkit, Tool: i.Tool, Status: i.Status,
		CreatedAt: i.CreatedAt, FinishedAt: i.FinishedAt, StopRequest: i.StopRequest, Result: i.Result,
	}
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

// FleetLister is a store that lists the running invocations of EVERY
// replica, with their labels (PostgresStore). Manager.StopMatching then
// matches the whole fleet, not only this process.
type FleetLister interface {
	RunningInvocations(ctx context.Context) ([]RunningInvocation, error)
	// CountUnfinished counts the invocations among ids that have not
	// ended (an id with no row has ended: it was pruned).
	CountUnfinished(ctx context.Context, ids []string) (int, error)
}

// FleetCapacity is the fleet's slot report.
type FleetCapacity struct {
	// Total is the sum of the slots of the live replicas that admit work.
	Total int
	// Active is the number of unfinished slot-holding invocations.
	Active int
	// Replicas is the number of live replicas that admit work.
	Replicas int
}

// FleetSlotter is a store that admits slot-holding invocations against the
// fleet in Create (it returns ErrNoSlot when every slot is taken) and
// reports the fleet's slots (PostgresStore). With any other store the
// manager admits against the slots of this process.
type FleetSlotter interface {
	FleetSlots(ctx context.Context) (FleetCapacity, error)
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

// DetachedContext is the Context for work that is not an invocation (a
// platform route, platform.go): it has an id for the engine's registry, its
// progress goes nowhere, and nothing can stop it.
func DetachedContext(id string) *Context {
	return &Context{invocation: &Invocation{ID: id}, manager: NewManager(nil, 0, nil)}
}

// SetLabel attaches a fact to the running invocation (see Invocation.Labels).
func (c *Context) SetLabel(key, value string) {
	c.manager.mu.Lock()
	defer c.manager.mu.Unlock()
	if c.invocation.Labels == nil {
		c.invocation.Labels = map[string]string{}
	}
	c.invocation.Labels[key] = value
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

// The refusals of a slot-holding invocation. "[SERVICE_BUSY]" classifies
// each as service_busy (errors.go), the category the recorded busy refusal
// carries.
const (
	// noSlotMessage: every slot of the fleet is busy.
	noSlotMessage = "[SERVICE_BUSY] All %d generation slots are busy. Start it again later."
	// noLocalSlotMessage: every slot of the replica the call landed on is
	// busy. Another replica may have a free one, and the next call may
	// land there.
	noLocalSlotMessage = "[SERVICE_BUSY] All %d generation slots are busy on this replica. Start it again later."
	// drainingMessage: the replica is shutting down.
	drainingMessage = "[SERVICE_BUSY] This replica is shutting down and starts no new generation. Start it again later."
)

type Manager struct {
	store     Store
	logger    *slog.Logger
	retention time.Duration

	mu       sync.Mutex
	inFlight map[string]*inFlight
	wg       sync.WaitGroup
	stopOnce sync.Once
	stopped  chan struct{}
	// cancels fans a cancel out to the other replicas (Server's
	// WithCancelBus); nil reaches this process only.
	cancels CancelBus

	// slots is how many slot-holding invocations this process runs at once
	// (SetSlots: <PREFIX>MAX_PARALLEL_WORKERS); zero sets no limit. holders
	// counts the slot holders this process admitted that have not ended: a
	// local slot is taken at admission and given back when the run ends.
	// draining refuses every new slot holder (SetAdmitting(false)).
	slots    int
	holders  int
	draining bool

	// refusals counts the slot holders this process refused because every
	// slot of the fleet was busy: the demand that admission turned away,
	// which an autoscaler adds to the running load (MetricSlotRefusals).
	refusals atomic.Int64
}

// SetSlots sets how many slot-holding invocations this process runs at once
// (the server sets <PREFIX>MAX_PARALLEL_WORKERS). Call it before the first
// Submit; zero or less sets no limit.
func (m *Manager) SetSlots(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.slots = max(n, 0)
}

// SetAdmitting says whether this process admits new slot holders. The host
// calls SetAdmitting(false) when SIGTERM arrives, with the store's: a
// replica that drains refuses every new slot holder at once, so no full
// generation starts on a replica that is going away.
func (m *Manager) SetAdmitting(admitting bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.draining = !admitting
}

// LocalHolders is the number of slot-holding invocations this process has
// admitted and not ended.
func (m *Manager) LocalHolders() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.holders
}

// FleetSlots is the slot report of the fleet when the store keeps one
// (FleetSlotter), and false otherwise.
func (m *Manager) FleetSlots(ctx context.Context) (FleetCapacity, bool, error) {
	slotter, ok := m.store.(FleetSlotter)
	if !ok {
		return FleetCapacity{}, false, nil
	}
	capacity, err := slotter.FleetSlots(ctx)
	return capacity, true, err
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
	return m.SubmitLabelled(ctx, toolkit, tool, nil, call)
}

// SubmitLabelled is Submit with the invocation's labels (Invocation.Labels)
// set at registration: they are on the invocation BEFORE it is in flight,
// under the same lock StopMatching reads, so a StopMatching that runs at
// any moment after Submit returns sees them. A label the runner would set
// from inside the run (Context.SetLabel) is invisible until the run gets
// there, and a deletion that stopped the matching invocations in between
// would miss it. The server derives them from the runner (Labeler).
func (m *Manager) SubmitLabelled(ctx context.Context, toolkit, tool string, labels map[string]string, call Call) (*Invocation, error) {
	return m.SubmitWith(ctx, toolkit, tool, Submission{Labels: labels}, call)
}

// Submission is what the server knows about an invocation before it runs.
type Submission struct {
	// Labels are put on the invocation at registration (SubmitLabelled).
	Labels map[string]string
	// HoldsSlot makes the invocation take one of the limited slots
	// (SlotHolder).
	HoldsSlot bool
}

// SubmitWith is SubmitLabelled with the slot flag.
//
// A slot-holding invocation is ADMITTED before it is registered, in two
// steps, and never waits:
//
//  1. It takes a LOCAL slot, without blocking. When this process drains
//     (SetAdmitting(false)) or runs as many slot holders as it has slots,
//     it is refused at once. A refusal here, and not a wait, is what keeps
//     a call off a full replica while a sibling has a free slot, and keeps
//     a pending generation from starting on a replica after SIGTERM.
//  2. With a store that admits against the fleet (FleetSlotter), Create
//     checks the fleet. When the fleet is full, the local slot is given
//     back and the call is refused.
//
// A refused invocation is still accepted with an id, because the SPI is
// asynchronous unconditionally (Server.Accept). It is recorded at once as
// terminal, with holds_slot false, and a service_busy tool error that says
// why, so the first poll on any replica reads the refusal. Each refusal is
// counted (Refusals).
func (m *Manager) SubmitWith(ctx context.Context, toolkit, tool string, submission Submission, call Call) (*Invocation, error) {
	invocation := &Invocation{ID: newInvocationID(), Toolkit: toolkit, Tool: tool, Status: statusPending, CreatedAt: time.Now()}
	if len(submission.Labels) > 0 {
		invocation.Labels = make(map[string]string, len(submission.Labels))
		for k, v := range submission.Labels {
			invocation.Labels[k] = v
		}
	}
	holds := false
	if submission.HoldsSlot {
		m.mu.Lock()
		limited, draining, slots := m.slots > 0, m.draining, m.slots
		full := limited && m.holders >= m.slots
		holds = limited && !draining && !full
		if holds {
			m.holders++
		}
		m.mu.Unlock()
		switch {
		case limited && draining:
			return m.refuseNoSlot(ctx, invocation, Failf(KindRuntime, drainingMessage))
		case full:
			return m.refuseNoSlot(ctx, invocation, Failf(KindRuntime, noLocalSlotMessage, slots))
		}
	}
	invocation.HoldsSlot = holds
	if err := m.store.Create(ctx, invocation); err != nil {
		if holds {
			m.releaseHolder()
		}
		var noSlot *NoSlotError
		if errors.As(err, &noSlot) {
			return m.refuseNoSlot(ctx, invocation, Failf(KindRuntime, noSlotMessage, noSlot.Total))
		}
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
			if holds {
				m.holders--
			}
			m.mu.Unlock()
		}()
		m.run(runCtx, invocation, call)
	}()
	return invocation, nil
}

// releaseHolder gives back a local admission that did not become a run.
func (m *Manager) releaseHolder() {
	m.mu.Lock()
	m.holders--
	m.mu.Unlock()
}

// refuseNoSlot records the invocation as terminal at once with the refusal
// and returns it as accepted (SubmitWith). The row it writes holds no slot
// and is finished, so a refusal never counts against the fleet.
func (m *Manager) refuseNoSlot(ctx context.Context, invocation *Invocation, refusal error) (*Invocation, error) {
	m.refusals.Add(1)
	invocation.HoldsSlot = false
	invocation.Status = statusStopped
	invocation.FinishedAt = time.Now()
	invocation.Result = ToolError(m.logger, invocation.ID, invocation.Tool, refusal)
	if err := m.store.Create(ctx, invocation); err != nil {
		return nil, err
	}
	return invocation, nil
}

func (m *Manager) run(ctx context.Context, invocation *Invocation, call Call) {
	m.mu.Lock()
	invocation.Status = statusRunning
	snapshot := invocation.state()
	m.mu.Unlock()
	// A snapshot taken under the lock, not the live struct: a cancel fanned
	// in from another replica flips the live one's stop flag concurrently.
	_ = m.store.Update(ctx, &snapshot)
	body, err := call(ctx, &Context{invocation: invocation, manager: m})

	var result map[string]any
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
	snapshot = invocation.state()
	m.mu.Unlock()
	_ = m.store.Update(context.WithoutCancel(ctx), &snapshot)
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
// observes it at its next Checkpoint — Cancel only requests.
//
// The request reaches the run wherever it runs. The store records it (the
// row's stop flag), the live struct is flipped when THIS process owns the
// run, and otherwise the cancel bus (cancelbus.go) carries it to the replica
// that does: with N replicas a cancel lands on any one of them, and before
// #1359 one that landed on the wrong one was recorded and never acted on.
func (m *Manager) Cancel(ctx context.Context, toolkit, tool, id string) (bool, error) {
	invocation, err := m.store.Get(ctx, toolkit, tool, id)
	if err != nil || invocation == nil {
		return false, err
	}
	local := m.StopLocal(id)
	m.mu.Lock()
	invocation.StopRequest = true
	m.mu.Unlock()
	// Only the flag is written when the store can: a durable store's Update
	// writes the whole row as this replica read it, and the replica running
	// the invocation may have finished it in between — a stale "running"
	// written over its terminal body would make it poll InProgress for ever.
	if recorder, ok := m.store.(StopRecorder); ok {
		err = recorder.RequestStop(ctx, id)
	} else {
		err = m.store.Update(ctx, invocation)
	}
	if err != nil {
		return true, err
	}
	if !local && !invocation.Terminal() && m.cancels != nil {
		if err := m.cancels.Publish(ctx, id); err != nil {
			// The row records the request either way; the replica running
			// the invocation simply does not hear of it until it ends.
			m.logger.Warn("could not fan the cancel out to the other replicas", "invocation", id, "error", err)
		}
	}
	return true, nil
}

// StopLocal flips the stop flag of a run THIS process owns and reports
// whether it owned one. It is what a cancel fanned out by another replica
// calls (Server.Start subscribes it); an id this process is not running is
// none of its business, and the call is a no-op.
func (m *Manager) StopLocal(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	live, ok := m.inFlight[id]
	if ok {
		live.invocation.StopRequest = true
	}
	return ok
}

// StopWatcher is a store that hands this process the stop requests that
// other replicas recorded on the rows of its runs (PostgresStore polls them
// at each claim check). It is the path that does not depend on the cancel
// bus.
type StopWatcher interface {
	WatchStops(stop func(invocationID string))
}

// StopRecorder is a store that can record a stop request on its own,
// without writing the rest of the row (PostgresStore).
type StopRecorder interface {
	RequestStop(ctx context.Context, id string) error
}

// WaitIdle returns once no invocation is in flight, or with ctx's error when
// ctx ends first. It cancels nothing: it is the drain a terminating replica
// gives its runs before Stop cancels what is left.
func (m *Manager) WaitIdle(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for m.InFlight() > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return nil
}

// Refusals is how many slot holders this process refused because every slot
// was busy, since it started. It only increases.
func (m *Manager) Refusals() int64 { return m.refusals.Load() }

// InFlight is the number of invocations still running — what Slots reports
// as active.
func (m *Manager) InFlight() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.inFlight)
}

// RunningInvocation describes a running invocation: one of this process,
// or, from a FleetLister, one of any replica.
type RunningInvocation struct {
	ID      string
	Toolkit string
	Tool    string
	Labels  map[string]string
}

// StopMatching asks every running invocation that `match` selects to stop,
// then waits up to `wait` for them to end. It returns how many are still
// running afterwards (0 means they all stopped).
//
// The stop is the same cooperative one Cancel sends: the tool observes it at
// its next checkpoint, and the engine call is stopped by the host's stop
// watcher.
//
// With a store that lists the fleet (FleetLister) the match runs over the
// unfinished rows of EVERY replica, with the labels each row was written
// with. Cancel records the stop on each row and fans it out on the cancel
// bus to the replica that runs it, and the wait reads the rows until each is
// terminal. With any other store only this process's runs are matched,
// because the labels live in memory.
func (m *Manager) StopMatching(ctx context.Context, match func(RunningInvocation) bool, wait time.Duration) int {
	if lister, ok := m.store.(FleetLister); ok {
		return m.stopMatchingFleet(ctx, lister, match, wait)
	}
	return m.stopMatchingLocal(ctx, match, wait)
}

// stopMatchingLocal is StopMatching over this process's runs.
func (m *Manager) stopMatchingLocal(ctx context.Context, match func(RunningInvocation) bool, wait time.Duration) int {
	m.mu.Lock()
	var targets []RunningInvocation
	for id, live := range m.inFlight {
		info := RunningInvocation{ID: id, Toolkit: live.invocation.Toolkit, Tool: live.invocation.Tool, Labels: map[string]string{}}
		for k, v := range live.invocation.Labels {
			info.Labels[k] = v
		}
		if match(info) {
			targets = append(targets, info)
		}
	}
	m.mu.Unlock()
	if len(targets) == 0 {
		return 0
	}
	for _, target := range targets {
		if _, err := m.Cancel(ctx, target.Toolkit, target.Tool, target.ID); err != nil {
			m.logger.Warn("could not request a stop", "invocation", target.ID, "error", err)
		}
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		m.mu.Lock()
		remaining := 0
		for _, target := range targets {
			if _, running := m.inFlight[target.ID]; running {
				remaining++
			}
		}
		m.mu.Unlock()
		if remaining == 0 {
			return 0
		}
		select {
		case <-ctx.Done():
			return remaining
		case <-deadline.C:
			return remaining
		case <-tick.C:
		}
	}
}

// stopMatchingFleet is StopMatching over the fleet's rows (FleetLister).
func (m *Manager) stopMatchingFleet(ctx context.Context, lister FleetLister, match func(RunningInvocation) bool, wait time.Duration) int {
	running, err := lister.RunningInvocations(ctx)
	if err != nil {
		// Without the list nobody can say what runs on the other replicas.
		// This process's runs are stopped all the same, and the answer is
		// "still running", so the caller deletes nothing and tries again.
		m.logger.Warn("could not list the fleet's running invocations; only this replica's are stopped", "error", err)
		return max(1, m.stopMatchingLocal(ctx, match, wait))
	}
	// A run of THIS process may carry labels it set from inside the run
	// (Context.SetLabel) on top of the ones its row was written with, and a
	// run this process registered is in flight before anything else can
	// list it. Both are matched with the labels in memory.
	m.mu.Lock()
	local := make(map[string]RunningInvocation, len(m.inFlight))
	for id, live := range m.inFlight {
		info := RunningInvocation{ID: id, Toolkit: live.invocation.Toolkit, Tool: live.invocation.Tool, Labels: map[string]string{}}
		for k, v := range live.invocation.Labels {
			info.Labels[k] = v
		}
		local[id] = info
	}
	m.mu.Unlock()
	var targets []RunningInvocation
	seen := map[string]bool{}
	for _, info := range running {
		seen[info.ID] = true
		if mine, ok := local[info.ID]; ok {
			for k, v := range info.Labels {
				if _, set := mine.Labels[k]; !set {
					mine.Labels[k] = v
				}
			}
			info = mine
		}
		if info.Labels == nil {
			info.Labels = map[string]string{}
		}
		if match(info) {
			targets = append(targets, info)
		}
	}
	for id, info := range local {
		if !seen[id] && match(info) {
			targets = append(targets, info)
		}
	}
	if len(targets) == 0 {
		return 0
	}
	for _, target := range targets {
		if _, err := m.Cancel(ctx, target.Toolkit, target.Tool, target.ID); err != nil {
			m.logger.Warn("could not request a stop", "invocation", target.ID, "error", err)
		}
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	ids := make([]string, len(targets))
	for i, target := range targets {
		ids[i] = target.ID
	}
	for {
		// One statement a tick, whatever the number of targets. A failed
		// read counts every target as still running.
		remaining, err := lister.CountUnfinished(ctx, ids)
		if err != nil {
			remaining = len(ids)
		}
		if remaining == 0 {
			return 0
		}
		select {
		case <-ctx.Done():
			return remaining
		case <-deadline.C:
			return remaining
		case <-tick.C:
		}
	}
}
