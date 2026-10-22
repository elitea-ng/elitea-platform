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
	// Active is the number of unfinished slot-holding invocations that
	// count as load (PostgresStore.fleetQuery says which).
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
	// noLocalSlotMessage: every slot of this process is busy, and the store
	// has no fleet (an in-memory store: one replica only).
	noLocalSlotMessage = "[SERVICE_BUSY] All %d generation slots are busy on this replica. Start it again later."
	// drainingMessage: the replica is shutting down.
	drainingMessage = "[SERVICE_BUSY] This replica is shutting down and starts no new generation. Start it again later."
	// fencedMessage: a deletion of what the invocation works on is in
	// progress (StopMatching). It is not service_busy: it is not demand
	// for more slots.
	fencedMessage = "A deletion of what this invocation works on is in progress, so it was not started. Start it again when the deletion ends."
)

// The reasons of a busy refusal: the `reason` label of MetricSlotRefusals,
// and the value of BusyReasonKey in the refused invocation's stored result.
const (
	// RefusalFleetFull: every slot of the fleet was busy. This is the only
	// reason that is demand for one more replica.
	RefusalFleetFull = "fleet_full"
	// RefusalDraining: this replica was shutting down.
	RefusalDraining = "draining"
	// RefusalLocalFull: every slot of this process was busy and the store
	// has no fleet (in-memory store). A host with a durable store never
	// refuses for this reason.
	RefusalLocalFull = "local_full"
)

// RefusalReasons are the busy refusal reasons, in the order /metrics writes
// them.
var RefusalReasons = []string{RefusalFleetFull, RefusalDraining, RefusalLocalFull}

// BusyReasonKey is the key of the busy refusal reason in the stored result
// of a refused invocation (result->>'busy_reason' in SQL). Only a busy
// refusal has it.
const BusyReasonKey = "busy_reason"

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

	// fences are the stop fences of the deletions in progress on this
	// process (StopMatching), by id. A new invocation that a fence matches
	// is refused when it registers. Guarded by mu.
	fences map[string]StopFence

	// refusals counts the slot holders this process refused, by reason
	// (RefusalReasons, in that order). The fleet_full count is the demand
	// that admission turned away, which an autoscaler adds to the running
	// load (MetricSlotRefusals).
	refusals [3]atomic.Int64
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
	return &Manager{store: store, logger: logger, retention: retention, inFlight: map[string]*inFlight{},
		fences: map[string]StopFence{}, stopped: make(chan struct{})}
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
// A slot-holding invocation is ADMITTED before it is registered, and never
// waits:
//
//   - When this process drains (SetAdmitting(false)), it is refused at once
//     (reason draining), so no generation starts on a replica after SIGTERM.
//   - With a store that admits against the fleet (FleetSlotter), the FLEET
//     decides, in Create: the call is admitted while the fleet's running
//     slot holders are fewer than the slots of the live replicas that admit
//     work, else refused (reason fleet_full). The slots of THIS process do
//     not refuse it. /slots reports the fleet, and a call that a load
//     balancer sends to a busy replica while a sibling is idle must run, or
//     can_start would lie and the refusal would count as demand. So a
//     replica can run more slot holders than its own slot count while the
//     fleet has room; the fleet total stays the bound.
//   - With any other store (in memory: one replica), the slots of this
//     process are the bound (reason local_full).
//
// A refused invocation is still accepted with an id, because the SPI is
// asynchronous unconditionally (Server.Accept). It is recorded at once as
// terminal, with holds_slot false, and a service_busy tool error that says
// why, with the reason under BusyReasonKey, so the first poll on any
// replica reads the refusal. Each refusal is counted by reason
// (RefusalsFor).
//
// An invocation that a stop fence of a deletion in progress matches
// (StopMatching) is refused too, with a runtime error that is not
// service_busy and is not counted as a busy refusal.
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
		_, fleet := m.store.(FleetSlotter)
		m.mu.Lock()
		limited, draining, slots := m.slots > 0, m.draining, m.slots
		full := limited && m.holders >= m.slots
		holds = limited && !draining && (fleet || !full)
		if holds {
			m.holders++
		}
		m.mu.Unlock()
		switch {
		case limited && draining:
			return m.refuse(ctx, invocation, Failf(KindRuntime, drainingMessage), RefusalDraining)
		case full && !fleet:
			return m.refuse(ctx, invocation, Failf(KindRuntime, noLocalSlotMessage, slots), RefusalLocalFull)
		}
	}
	invocation.HoldsSlot = holds
	if err := m.store.Create(ctx, invocation); err != nil {
		if holds {
			m.releaseHolder()
		}
		var noSlot *NoSlotError
		switch {
		case errors.As(err, &noSlot):
			return m.refuse(ctx, invocation, Failf(KindRuntime, noSlotMessage, noSlot.Total), RefusalFleetFull)
		case errors.Is(err, ErrFenced):
			return m.refuse(ctx, invocation, Failf(KindRuntime, fencedMessage), "")
		}
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	m.mu.Lock()
	// The local fences are read under the same lock hold that registers
	// the run, and StopMatching adds a fence and reads the runs under one
	// lock hold too. So a run of this process is either in StopMatching's
	// list or refused here.
	if m.fencedLocked(invocation) {
		if holds {
			m.holders--
		}
		invocation.HoldsSlot = false
		invocation.Status = statusStopped
		invocation.FinishedAt = time.Now()
		invocation.Result = ToolError(m.logger, invocation.ID, invocation.Tool, Failf(KindRuntime, fencedMessage))
		snapshot := invocation.state()
		m.mu.Unlock()
		cancel()
		if err := m.store.Update(ctx, &snapshot); err != nil {
			return nil, err
		}
		return invocation, nil
	}
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

// refuse records the invocation as terminal at once with the refusal and
// returns it as accepted (SubmitWith). The row it writes holds no slot and
// is finished, so a refusal never counts against the fleet. A busy refusal
// (reason not empty) is counted by reason, and its result carries the
// reason under BusyReasonKey.
func (m *Manager) refuse(ctx context.Context, invocation *Invocation, refusal error, reason string) (*Invocation, error) {
	invocation.HoldsSlot = false
	invocation.Status = statusStopped
	invocation.FinishedAt = time.Now()
	invocation.Result = ToolError(m.logger, invocation.ID, invocation.Tool, refusal)
	if reason != "" {
		if counter := m.refusalCounter(reason); counter != nil {
			counter.Add(1)
		}
		invocation.Result[BusyReasonKey] = reason
	}
	if err := m.store.Create(ctx, invocation); err != nil {
		return nil, err
	}
	return invocation, nil
}

// refusalCounter is the counter of one refusal reason; nil for a reason
// that is not in RefusalReasons.
func (m *Manager) refusalCounter(reason string) *atomic.Int64 {
	for i, known := range RefusalReasons {
		if known == reason {
			return &m.refusals[i]
		}
	}
	return nil
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

// Refusals is how many slot holders this process refused as busy, for all
// reasons, since it started. It only increases.
func (m *Manager) Refusals() int64 {
	var total int64
	for i := range m.refusals {
		total += m.refusals[i].Load()
	}
	return total
}

// RefusalsFor is how many slot holders this process refused for one reason
// (RefusalReasons) since it started; zero for an unknown reason.
func (m *Manager) RefusalsFor(reason string) int64 {
	if counter := m.refusalCounter(reason); counter != nil {
		return counter.Load()
	}
	return 0
}

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

// StopFence names the runs a deletion stops (StopMatching), and the new
// invocations it holds back while the deletion is in progress.
//
// A run or an invocation matches when its tool is Tool and, for each label
// of the fence with a value, its own label of that name is empty or equal.
// So an empty fence value matches every value ("every wiki of the
// project"), and a run with no value for a label matches every fence value
// of that label ("a generation whose wiki the host could not name"). The
// second rule also makes a row with no labels at all match: the row of a
// replica from before the fleet labels (a mixed fleet in a rolling
// upgrade). Stopping one run too many is the safe side of a deletion.
type StopFence struct {
	Tool   string
	Labels map[string]string
}

// Matches reports whether the fence selects a run of tool with labels.
func (f StopFence) Matches(tool string, labels map[string]string) bool {
	if tool != f.Tool {
		return false
	}
	for key, want := range f.Labels {
		if want == "" {
			continue
		}
		if got := labels[key]; got != "" && got != want {
			return false
		}
	}
	return true
}

// StopFencer is a store that keeps stop fences for the whole fleet
// (PostgresStore). Fence records the fence for ttl at most, and returns
// its id. From the moment Fence returns, Create refuses (ErrFenced) a new
// invocation that the fence matches, on every replica: the fence write
// and the admission take the same transaction lock. Unfence removes it.
type StopFencer interface {
	Fence(ctx context.Context, fence StopFence, ttl time.Duration) (string, error)
	Unfence(ctx context.Context, id string) error
}

// stopFenceMargin is how much longer than the stop's wait a fence lives at
// most. It is a safety net for a process that stops before it removes its
// fence; a deletion that ends removes it at once.
const stopFenceMargin = 10 * time.Minute

// fencedLocked reports whether a local fence matches the invocation. The
// caller holds m.mu.
func (m *Manager) fencedLocked(invocation *Invocation) bool {
	for _, fence := range m.fences {
		if fence.Matches(invocation.Tool, invocation.Labels) {
			return true
		}
	}
	return false
}

// LocalRunning lists the runs of this process with their labels, as
// StopMatching matches them. It changes nothing.
func (m *Manager) LocalRunning() []RunningInvocation {
	m.mu.Lock()
	defer m.mu.Unlock()
	running := make([]RunningInvocation, 0, len(m.inFlight))
	for _, info := range m.localRunningLocked() {
		running = append(running, info)
	}
	return running
}

// localRunningLocked copies the runs of this process, by id. The caller
// holds m.mu.
func (m *Manager) localRunningLocked() map[string]RunningInvocation {
	local := make(map[string]RunningInvocation, len(m.inFlight))
	for id, live := range m.inFlight {
		info := RunningInvocation{ID: id, Toolkit: live.invocation.Toolkit, Tool: live.invocation.Tool, Labels: map[string]string{}}
		for k, v := range live.invocation.Labels {
			info.Labels[k] = v
		}
		local[id] = info
	}
	return local
}

// StopMatching asks every running invocation that the fence selects to
// stop, then waits up to `wait` for them to end. It returns how many are
// still running afterwards (0 means they all stopped).
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
//
// THE FENCE. The list of runs is read one time. Without more, a replica
// could admit a new matching run right after that read, and the run would
// publish into what the deletion removes. So StopMatching first records the
// fence, and only then reads the runs:
//
//   - on this process, in memory, under the same lock hold that reads this
//     process's runs. SubmitWith reads the fences under the lock hold that
//     registers a run, so a run of this process is in the list or refused;
//   - with a StopFencer store, in the database (host_stop_fences), under a
//     transaction lock that Create also takes for the tool. A Create that
//     committed before the fence is in the list that is read after it; a
//     Create after the fence finds the fence and is refused (ErrFenced).
//
// The fence stays while the deletion runs: it is removed when ctx ends, so
// a caller passes the context of the deletion and cancels it when the
// deletion ends. When runs are still there after the wait, the deletion is
// refused, and the fence is removed at once. A process that stops before it
// removes its database fence leaves it for wait plus stopFenceMargin at
// most. When the fence cannot be written, nothing on the other replicas can
// be held back, so the answer is "still running" and the caller deletes
// nothing.
func (m *Manager) StopMatching(ctx context.Context, fence StopFence, wait time.Duration) int {
	localID := newFenceID()
	m.mu.Lock()
	m.fences[localID] = fence
	local := m.localRunningLocked()
	m.mu.Unlock()

	var storeID string
	release := func() {
		m.mu.Lock()
		delete(m.fences, localID)
		m.mu.Unlock()
		if storeID == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.store.(StopFencer).Unfence(ctx, storeID); err != nil {
			m.logger.Warn("could not remove a stop fence; it expires by itself", "fence", storeID, "error", err)
		}
	}
	fenceFailed := false
	if fencer, ok := m.store.(StopFencer); ok {
		id, err := fencer.Fence(ctx, fence, wait+stopFenceMargin)
		if err != nil {
			m.logger.Warn("could not write the stop fence; only this replica's runs are stopped", "tool", fence.Tool, "error", err)
			fenceFailed = true
		}
		storeID = id
	}

	var remaining int
	lister, fleet := m.store.(FleetLister)
	switch {
	case fenceFailed || !fleet:
		remaining = m.stopTargets(ctx, nil, matching(local, fence), wait)
		if fenceFailed {
			remaining = max(1, remaining)
		}
	default:
		remaining = m.stopMatchingFleet(ctx, lister, fence, local, wait)
	}
	if remaining > 0 {
		release()
		return remaining
	}
	context.AfterFunc(ctx, release)
	return 0
}

// matching selects the runs the fence matches.
func matching(runs map[string]RunningInvocation, fence StopFence) []RunningInvocation {
	var targets []RunningInvocation
	for _, info := range runs {
		if fence.Matches(info.Tool, info.Labels) {
			targets = append(targets, info)
		}
	}
	return targets
}

func newFenceID() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return "fence_" + hex.EncodeToString(raw[:])
}

// stopMatchingFleet is StopMatching over the fleet's rows (FleetLister).
// local is this process's runs, read when the local fence was set.
func (m *Manager) stopMatchingFleet(ctx context.Context, lister FleetLister, fence StopFence, local map[string]RunningInvocation, wait time.Duration) int {
	running, err := lister.RunningInvocations(ctx)
	if err != nil {
		// Without the list nobody can say what runs on the other replicas.
		// This process's runs are stopped all the same, and the answer is
		// "still running", so the caller deletes nothing and tries again.
		m.logger.Warn("could not list the fleet's running invocations; only this replica's are stopped", "error", err)
		return max(1, m.stopTargets(ctx, nil, matching(local, fence), wait))
	}
	// A run of THIS process may carry labels it set from inside the run
	// (Context.SetLabel) on top of the ones its row was written with, and a
	// run this process registered is in flight before anything else can
	// list it. Both are matched with the labels in memory.
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
		if fence.Matches(info.Tool, info.Labels) {
			targets = append(targets, info)
		}
	}
	for id, info := range local {
		if !seen[id] && fence.Matches(info.Tool, info.Labels) {
			targets = append(targets, info)
		}
	}
	return m.stopTargets(ctx, lister, targets, wait)
}

// stopTargets cancels each target and waits up to `wait` for them to end.
// With a lister it reads the rows (one statement a tick); without one it
// reads this process's runs.
func (m *Manager) stopTargets(ctx context.Context, lister FleetLister, targets []RunningInvocation, wait time.Duration) int {
	if len(targets) == 0 {
		return 0
	}
	for _, target := range targets {
		if _, err := m.Cancel(ctx, target.Toolkit, target.Tool, target.ID); err != nil {
			m.logger.Warn("could not request a stop", "invocation", target.ID, "error", err)
		}
	}
	ids := make([]string, len(targets))
	for i, target := range targets {
		ids[i] = target.ID
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		var remaining int
		if lister != nil {
			// A failed read counts every target as still running.
			n, err := lister.CountUnfinished(ctx, ids)
			if err != nil {
				n = len(ids)
			}
			remaining = n
		} else {
			m.mu.Lock()
			for _, id := range ids {
				if _, running := m.inFlight[id]; running {
					remaining++
				}
			}
			m.mu.Unlock()
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
