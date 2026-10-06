package budgetwriteback

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// The write-back consumer's states, as Status reports them.
const (
	// StateDisabled: BUDGET_WRITEBACK_ENABLED is off or GATEWAY_NATS_URL is
	// unset. A declared posture, not a fault.
	StateDisabled = "disabled"
	// StateMisconfigured: the NATS settings are refused (half a client
	// identity, a URL that disagrees with it). No retry can fix that.
	StateMisconfigured = "misconfigured"
	// StateWaiting: configured, not attached yet — NATS unreachable, or the
	// nats-bootstrap Job has not created the consumer. Deltas wait in
	// GATEWAY_BUDGET_DELTAS meanwhile, for up to its MaxAge (72h by default).
	StateWaiting = "waiting"
	// StateAttached: bound to the consumer and draining.
	StateAttached = "attached"
)

// Status is the write-back consumer's state, served as JSON on the
// scheduler's health server (/readyz/budget-writeback): 200 when attached or
// disabled, 503 while waiting or misconfigured. It is deliberately NOT the
// pod's readiness probe — the scheduler runs other jobs a NATS outage does
// not touch — but it is what an operator or an alert reads to tell "draining"
// from "silently not draining".
type Status struct {
	mu        sync.Mutex
	state     string
	since     time.Time
	attempts  int
	lastError string
	now       func() time.Time
}

// NewStatus returns a Status in StateDisabled.
func NewStatus() *Status {
	return &Status{state: StateDisabled, since: time.Now(), now: time.Now}
}

// Snapshot is a point-in-time copy of a Status.
type Snapshot struct {
	State     string    `json:"state"`
	Since     time.Time `json:"since"`
	Attempts  int       `json:"attempts,omitempty"`
	LastError string    `json:"last_error,omitempty"`
}

// Snapshot returns the current state.
func (s *Status) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Snapshot{State: s.state, Since: s.since, Attempts: s.attempts, LastError: s.lastError}
}

func (s *Status) set(state string, attempts int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state != s.state {
		s.since = s.now()
	}
	s.state, s.attempts, s.lastError = state, attempts, ""
	if err != nil {
		s.lastError = err.Error()
	}
}

// SetMisconfigured records a configuration the scheduler refused.
func (s *Status) SetMisconfigured(err error) { s.set(StateMisconfigured, 0, err) }

// ServeHTTP reports the state.
func (s *Status) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	snap := s.Snapshot()
	w.Header().Set("Content-Type", "application/json")
	if snap.State == StateWaiting || snap.State == StateMisconfigured {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(snap)
}

// BindFunc binds to the consumer once (Bind, closed over its arguments).
type BindFunc func(ctx context.Context) (*Consumer, error)

// Supervisor keeps the write-back consumer attached for the life of the
// process.
//
// A single Bind at boot is not enough: the scheduler can start before the
// nats-bootstrap Job has created the consumer, or while NATS is down, and a
// bind that gives up there leaves deltas accumulating unread until they age
// out of GATEWAY_BUDGET_DELTAS — spend that never reaches the accumulator
// tier, with nothing failing loudly. The Supervisor retries with capped
// exponential backoff, logs one WARN per outage (and one INFO when it
// attaches again), and re-attaches when a running consumer is lost (deleted
// and re-created by a bootstrap re-run, or a JetStream store lost with its
// node).
type Supervisor struct {
	Bind   BindFunc
	Status *Status
	Logger *slog.Logger

	// MinBackoff and MaxBackoff bound the retry delay (defaults 1s, 30s).
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// BindTimeout bounds each attempt (default 5s).
	BindTimeout time.Duration

	// sleep waits d or until ctx ends; tests replace it.
	sleep func(ctx context.Context, d time.Duration) bool
	now   func() time.Time
}

func (s *Supervisor) defaults() {
	if s.Status == nil {
		s.Status = NewStatus()
	}
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	if s.MinBackoff <= 0 {
		s.MinBackoff = time.Second
	}
	if s.MaxBackoff < s.MinBackoff {
		s.MaxBackoff = 30 * time.Second
		if s.MaxBackoff < s.MinBackoff {
			s.MaxBackoff = s.MinBackoff
		}
	}
	if s.BindTimeout <= 0 {
		s.BindTimeout = 5 * time.Second
	}
	if s.sleep == nil {
		s.sleep = func(ctx context.Context, d time.Duration) bool {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return false
			case <-t.C:
				return true
			}
		}
	}
	if s.now == nil {
		s.now = time.Now
	}
}

// Run attaches, drains until the consumer is lost or ctx ends, and attaches
// again. It returns when ctx ends.
func (s *Supervisor) Run(ctx context.Context) {
	s.defaults()
	for {
		c, ok := s.attach(ctx)
		if !ok {
			return
		}
		c.Run(ctx)
		if ctx.Err() != nil {
			return
		}
		s.Logger.Warn("budget write-back: the consumer was lost; re-attaching",
			"stream", DeltasStream, "consumer", DurableName)
	}
}

// attach binds, retrying with backoff, until it succeeds or ctx ends.
func (s *Supervisor) attach(ctx context.Context) (*Consumer, bool) {
	delay := s.MinBackoff
	started := s.now()
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return nil, false
		}
		bctx, cancel := context.WithTimeout(ctx, s.BindTimeout)
		c, err := s.Bind(bctx)
		cancel()
		if err == nil {
			s.Status.set(StateAttached, attempt, nil)
			if attempt == 1 {
				s.Logger.Info("budget write-back: attached to the consumer", "stream", DeltasStream, "consumer", DurableName)
			} else {
				s.Logger.Info("budget write-back: attached to the consumer after an outage",
					"stream", DeltasStream, "consumer", DurableName,
					"attempts", attempt, "waited", s.now().Sub(started).Round(time.Second).String())
			}
			return c, true
		}
		if ctx.Err() != nil {
			return nil, false
		}
		s.Status.set(StateWaiting, attempt, err)
		if attempt == 1 {
			// Once per outage: the retries below log at DEBUG only, and the
			// attach that ends the outage logs at INFO.
			s.Logger.Warn("budget write-back: cannot attach to the consumer; retrying with backoff. "+
				"Deltas wait in the stream meanwhile, for up to its MaxAge (72h by default); "+
				"/readyz/budget-writeback reports the state",
				"stream", DeltasStream, "consumer", DurableName, "err", err)
		} else {
			s.Logger.Debug("budget write-back: attach retry failed", "attempt", attempt, "err", err)
		}
		if !s.sleep(ctx, delay) {
			return nil, false
		}
		delay *= 2
		if delay > s.MaxBackoff {
			delay = s.MaxBackoff
		}
	}
}

// consumerLost reports a fetch error that no retry of the same consumer
// handle can fix: the consumer (or its stream) is gone, so the Supervisor
// must bind again — after the nats-bootstrap Job has re-created it.
func consumerLost(err error) bool {
	return errors.Is(err, jetstream.ErrConsumerDeleted) ||
		errors.Is(err, jetstream.ErrConsumerNotFound) ||
		errors.Is(err, jetstream.ErrStreamNotFound) ||
		errors.Is(err, nats.ErrNoResponders)
}
