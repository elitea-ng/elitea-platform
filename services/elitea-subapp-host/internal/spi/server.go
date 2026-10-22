package spi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// App is a sub-application as the host sees it: a name, a descriptor, an
// admission table and a runner. Everything else on the wire is the host's.
type App struct {
	// Name is the provider's name, reported as `plugin` by Health.
	Name string
	// Version is reported as providerVersion.
	Version string
	// Descriptor returns the provider's self-description for the service
	// location the host is configured with. The document is the
	// application's; the host injects nothing but the URL it is given.
	Descriptor func(serviceLocationURL string) any
	Toolkits   Toolkits
	Runner     Runner
}

// Validate refuses an application the host could not serve honestly.
func (a App) Validate() error {
	if a.Name == "" || a.Version == "" || a.Descriptor == nil {
		return fmt.Errorf("%w: an application needs a name, a version and a descriptor", ErrConfig)
	}
	if a.Runner == nil {
		return fmt.Errorf("%w: application %q has no runner (use UnavailableRunner to serve without one)", ErrConfig, a.Name)
	}
	return a.Toolkits.Validate()
}

// Server serves the SPI for one application.
type Server struct {
	settings  Settings
	app       App
	manager   *Manager
	logger    *slog.Logger
	startedAt time.Time
	ready     func(context.Context) map[string]bool
	cancels   CancelBus
}

// Option adjusts a Server.
type Option func(*Server)

// WithStore selects the invocation store (default: in memory).
func WithStore(store Store) Option {
	return func(s *Server) {
		s.manager = NewManager(store, time.Duration(s.settings.InvocationRetentionSeconds)*time.Second, s.logger)
	}
}

// WithCancelBus fans a cancel out to the replica running the invocation
// (cancelbus.go). Without one a cancel reaches only this process's runs; the
// store still records the request.
func WithCancelBus(bus CancelBus) Option {
	return func(s *Server) { s.cancels = bus }
}

// WithReadiness adds checks Health reports; every check must pass for ready.
func WithReadiness(check func(context.Context) map[string]bool) Option {
	return func(s *Server) { s.ready = check }
}

// NewServer composes the host.
func NewServer(settings Settings, app App, logger *slog.Logger, options ...Option) (*Server, error) {
	if err := app.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{settings: settings, app: app, logger: logger, startedAt: time.Now()}
	s.manager = NewManager(nil, time.Duration(settings.InvocationRetentionSeconds)*time.Second, logger)
	for _, option := range options {
		option(s)
	}
	// A runner that wants the registry (to find and stop its own running
	// invocations) is handed it once, after the options have chosen the store.
	if aware, ok := app.Runner.(ManagerAware); ok {
		aware.AttachManager(s.manager)
	}
	s.manager.cancels = s.cancels
	// Only a runner with slot holders (SlotHolder) has a slot limit; for any
	// other runner every invocation runs at once, as before.
	if _, ok := app.Runner.(SlotHolder); ok {
		s.manager.SetSlots(settings.MaxParallelWorkers)
	}
	return s, nil
}

// Manager exposes the registry (for tests and for an embedding main).
func (s *Server) Manager() *Manager { return s.manager }

// Start begins housekeeping and listens for the cancels other replicas fan
// out; Stop ends every in-flight invocation.
func (s *Server) Start(ctx context.Context) error {
	// A stop a sibling recorded on a row reaches the run here too, at the
	// store's next claim check, when the bus is absent or lost the message.
	// The bus and the poll take the same path (StopLocal), which only raises
	// the run's stop flag: a run stopped twice ends once.
	if watcher, ok := s.manager.Store().(StopWatcher); ok {
		watcher.WatchStops(func(id string) { s.manager.StopLocal(id) })
	}
	if s.cancels != nil {
		if err := s.cancels.Subscribe(ctx, func(id string) { s.manager.StopLocal(id) }); err != nil {
			return fmt.Errorf("listen for cancellations: %w", err)
		}
	}
	s.manager.Start(ctx)
	if starter, ok := s.app.Runner.(Starter); ok {
		starter.Start(ctx)
	}
	return nil
}
func (s *Server) Stop() { s.manager.Stop() }

// ManagerAware is a runner that wants the invocation registry.
type ManagerAware interface{ AttachManager(*Manager) }

// Labeler is a runner that names, from the call alone, the labels an
// invocation must carry from the moment it is registered (the project and
// toolkit an ingest writes, say), so that a deletion's StopMatching finds it
// whether or not the run has begun. The server asks before Submit
// (Manager.SubmitLabelled); nil or empty means no labels.
type Labeler interface {
	Labels(call Invoke) map[string]string
}

// SlotHolder is a runner some of whose invocations take one of the limited
// slots (<PREFIX>MAX_PARALLEL_WORKERS on each replica) while they run: a
// DeepWiki wiki generation, say. The server asks before Submit, from the
// call alone. A slot holder is admitted only while this process has a free
// slot and admits work, and (with a durable store) the fleet has a free
// slot; otherwise it is refused at once, never queued (Manager.SubmitWith).
// A runner that does not implement it has no slot limit.
type SlotHolder interface {
	HoldsSlot(call Invoke) bool
}

// Starter is a runner with background work (refreshing what the engine can
// do, say) that runs until the context ends.
type Starter interface{ Start(ctx context.Context) }

// Settings are the host's settings, for the code that starts its listeners.
func (s *Server) Settings() Settings { return s.settings }

// PlatformOps is the application's platform operations, when its runner has
// any (platform.go).
func (s *Server) PlatformOps() (PlatformOps, bool) {
	ops, ok := s.app.Runner.(PlatformOps)
	return ops, ok
}

// The documents the provider service (grpc.go) answers with. They are the
// bodies the SPI's HTTP surface wrote before #1359, byte for byte, because
// the facade hands them on to the browser unchanged and the conformance
// fixtures pin them.

// DescriptorJSON is the provider's self-description for the configured
// service location.
//
// It is encoded WITHOUT Go's HTML escaping, unlike every other body: it is a
// document the application supplies verbatim, and the golden fixtures were
// recorded from Python's json.dumps, which leaves <, > and & alone. Escaping
// them is JSON-equivalent but not byte-equal, and the descriptor is pinned
// byte for byte — Inventory's recorded text says "Jira ticket -> GitHub PR".
func (s *Server) DescriptorJSON() ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(s.app.Descriptor(s.settings.ServiceLocationURL)); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buffer.Bytes(), "\n"), nil
}

// HealthDocument is what the host reports about itself.
func (s *Server) HealthDocument() map[string]any {
	hostname := os.Getenv("HOSTNAME")
	if hostname == "" {
		hostname = os.Getenv("POD_NAME")
	}
	if hostname == "" {
		hostname = "unknown"
	}
	podIP := os.Getenv("POD_IP")
	if podIP == "" {
		podIP = "unknown"
	}
	return map[string]any{
		"status":          "UP",
		"providerVersion": s.app.Version,
		"uptime":          int(time.Since(s.startedAt).Seconds()),
		"timestamp":       time.Now().UTC().Format("2006-01-02T15:04:05+00:00"),
		"plugin":          s.app.Name,
		"configuration": map[string]any{
			"scratch_path":         s.settings.ScratchPath,
			"service_location_url": s.settings.ServiceLocationURL,
		},
		"extra_info": map[string]any{
			"hostname":            hostname,
			"pod_ip":              podIP,
			"durable_invocations": s.manager.Store().Durable(),
			"cancel_fanout":       s.cancels != nil,
			"runner":              s.app.Runner.Name(),
			"mtls_required":       true,
			"identity_verified":   s.settings.IdentitySecret != "",
			"git_egress":          ParseEgressPolicy(s.settings.GitAllowlist).Describe(),
		},
	}
}

// Readiness runs the readiness checks; ready when every one passed.
func (s *Server) Readiness(ctx context.Context) (bool, map[string]bool) {
	checks := map[string]bool{"invocations": true}
	if s.ready != nil {
		for name, ok := range s.ready(ctx) {
			checks[name] = ok
		}
	}
	ready := true
	for _, ok := range checks {
		if !ok {
			ready = false
		}
	}
	return ready, checks
}

// SlotsDocument is the provider's capacity report.
//
// For a runner with slot holders (SlotHolder) over a store that keeps the
// fleet (FleetSlotter) it reports the FLEET: the slots of the live replicas
// that admit work and the slot holders that run on any of them. That is the
// same rule admission applies, so a can_start the UI reads is a start the
// fleet admits. With an in-memory store it reports this process's slot
// holders. For any other runner it reports this process's invocations, as
// before (the conformance fixtures pin that body).
func (s *Server) SlotsDocument(ctx context.Context) map[string]any {
	if _, ok := s.app.Runner.(SlotHolder); !ok {
		return Slots(s.settings, s.manager.InFlight())
	}
	capacity, fleet, err := s.manager.FleetSlots(ctx)
	if !fleet || s.settings.JobsEnabled {
		return Slots(s.settings, s.manager.LocalHolders())
	}
	if err != nil {
		s.logger.Warn("could not read the fleet's slots; this replica reports its own", "error", err)
		return Slots(s.settings, s.manager.LocalHolders())
	}
	return FleetSlots(capacity)
}

// MaxInvokeBytes bounds a request body: large enough for an expanded
// toolkit configuration, small enough that a caller cannot hold a goroutine
// on a body that never ends.
const MaxInvokeBytes = 4 << 20

// Accept registers one tool call and starts it, returning the accepted
// invocation. identity is the caller the transport verified (empty when the
// hop carried none).
//
// Admission runs INSIDE the invocation: an unknown toolkit or tool is
// accepted with an id and terminates as resource_not_found on the first
// read, which is what the recording shows and what the facade's and the
// browser's poll loops expect. Refusing at the door would be tidier and would
// break both.
func (s *Server) Accept(ctx context.Context, identity Identity, toolkit, tool string, request map[string]any) (*Invocation, error) {
	call := func(ctx context.Context, tc *Context) (map[string]any, error) {
		family, err := s.app.Toolkits.Resolve(toolkit)
		if err != nil {
			return nil, err
		}
		if err := s.app.Toolkits.Admit(family, tool); err != nil {
			return nil, err
		}
		return s.app.Runner.Invoke(ctx, Invoke{Family: family, Toolkit: toolkit, Tool: tool, Request: request, Identity: identity}, tc)
	}
	// The labels a deletion finds the invocation by are set at
	// registration, not by the run: a run that has not reached its
	// SetLabel yet would be invisible to a StopMatching in between
	// (Manager.SubmitLabelled). An unknown toolkit has no family and so no
	// labels; the run refuses it.
	var submission Submission
	if family, err := s.app.Toolkits.Resolve(toolkit); err == nil {
		invoke := Invoke{Family: family, Toolkit: toolkit, Tool: tool, Request: request, Identity: identity}
		if labeler, ok := s.app.Runner.(Labeler); ok {
			submission.Labels = labeler.Labels(invoke)
		}
		// Only a call the runner names a slot holder takes a slot. An
		// unknown toolkit takes none: the run refuses it at once.
		if holder, ok := s.app.Runner.(SlotHolder); ok {
			submission.HoldsSlot = holder.HoldsSlot(invoke)
		}
	}
	return s.manager.SubmitWith(ctx, toolkit, tool, submission, call)
}

// AcceptedBody is the body of an accepted invocation.
func AcceptedBody(invocation *Invocation) map[string]any {
	return map[string]any{"invocation_id": invocation.ID, "status": wireStarted}
}

// TerminalBody reports whether a poll body ends its invocation.
func TerminalBody(body map[string]any) bool {
	status, _ := body["status"].(string)
	return status != wireStarted && status != wireInProgress
}
