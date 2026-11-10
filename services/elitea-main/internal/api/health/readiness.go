package health

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Check states in the readiness body. The body carries only these words,
// never an error text.
const (
	StateOK           = "ok"
	StateUnavailable  = "unavailable"
	StateReconnecting = "reconnecting"
	StateDegraded     = "degraded"
)

// readinessTimeout bounds one readiness evaluation. It stays below the
// shortest probe timeout that calls /readyz (Helm 3 s, compose 5 s), so a
// probe gets an answer instead of its own timeout.
const readinessTimeout = 2 * time.Second

// Check is one readiness dependency (FU-275).
type Check struct {
	// Name is the key in the readiness body.
	Name string
	// Critical makes a failing check answer 503 not_ready: Main can serve
	// nothing without it (the database). A non-critical check that is
	// reconnecting or degraded answers 200 degraded: requests still succeed
	// (NATS: admitted commands wait in the outbox; claimed runs renew their
	// leases over gRPC). Any check that reports StateUnavailable answers 503:
	// unavailable means it will not come back on its own (a closed NATS
	// connection).
	Critical bool
	// Probe returns the check's state and, when it is not ok, a short cause
	// for the log. It must honour ctx and must not open connections outside
	// an existing pool.
	Probe func(ctx context.Context) (state string, cause string)
	// Lost and Recovered are the sentences the Monitor logs once per outage.
	// Leave both empty for a check whose events are logged elsewhere.
	Lost      string
	Recovered string
}

// Evaluation is the outcome of one readiness evaluation.
type Evaluation struct {
	Status  string
	Checks  map[string]string
	causes  map[string]string
	started time.Time
}

// Evaluate runs every check concurrently under one bounded context and
// classifies the result: not_ready when a critical check fails, degraded when
// only non-critical checks fail, ready otherwise.
func Evaluate(ctx context.Context, checks []Check) Evaluation {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()
	type outcome struct {
		state string
		cause string
	}
	outcomes := make([]outcome, len(checks))
	var wg sync.WaitGroup
	for i, check := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			state, cause := check.Probe(ctx)
			outcomes[i] = outcome{state: state, cause: cause}
		}()
	}
	wg.Wait()

	evaluation := Evaluation{Status: "ready", Checks: make(map[string]string, len(checks)), causes: make(map[string]string), started: started}
	for i, check := range checks {
		state := outcomes[i].state
		if state == "" {
			state = StateUnavailable
		}
		evaluation.Checks[check.Name] = state
		if state == StateOK {
			continue
		}
		evaluation.causes[check.Name] = outcomes[i].cause
		if check.Critical || state == StateUnavailable {
			evaluation.Status = "not_ready"
		} else if evaluation.Status == "ready" {
			evaluation.Status = StateDegraded
		}
	}
	return evaluation
}

// Monitor evaluates the checks on an interval and logs each dependency's
// transitions once per outage: one WARN when it is lost and one INFO with the
// outage length when it is back, however many runners or requests failed in
// between (FU-275 C4). Readiness evaluations made by probes are observed too.
type Monitor struct {
	checks   []Check
	logger   *slog.Logger
	interval time.Duration
	now      func() time.Time

	mu          sync.Mutex
	since       map[string]time.Time // set while the check is failing
	lastStarted time.Time            // start of the newest evaluation applied
}

// NewMonitor returns a Monitor over checks. interval must be positive.
func NewMonitor(checks []Check, logger *slog.Logger, interval time.Duration) (*Monitor, error) {
	if logger == nil || interval <= 0 {
		return nil, fmt.Errorf("readiness monitor needs a logger and a positive interval")
	}
	return &Monitor{checks: checks, logger: logger, interval: interval, now: time.Now, since: make(map[string]time.Time)}, nil
}

// Checks returns the monitored checks.
func (m *Monitor) Checks() []Check { return m.checks }

// Run evaluates every interval until ctx ends.
func (m *Monitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		evaluation := Evaluate(ctx, m.checks)
		if ctx.Err() != nil {
			return // a check cut short by shutdown is not an outage
		}
		m.Observe(evaluation)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Observe records one evaluation and logs the transitions it shows. Probe and
// monitor evaluations run concurrently and can finish out of order, so an
// evaluation that started before the newest one already applied is ignored.
func (m *Monitor) Observe(evaluation Evaluation) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if evaluation.started.Before(m.lastStarted) {
		return
	}
	m.lastStarted = evaluation.started
	for _, check := range m.checks {
		if check.Lost == "" && check.Recovered == "" {
			continue
		}
		state, evaluated := evaluation.Checks[check.Name]
		if !evaluated {
			continue
		}
		since, failing := m.since[check.Name]
		switch {
		case state != StateOK && !failing:
			m.since[check.Name] = now
			m.logger.Warn(check.Lost, "dependency", check.Name, "state", state, "cause", evaluation.causes[check.Name])
		case state == StateOK && failing:
			delete(m.since, check.Name)
			outage := now.Sub(since).Round(time.Second)
			m.logger.Info(fmt.Sprintf("%s after %s", check.Recovered, outage), "dependency", check.Name, "outage", outage.String())
		}
	}
}

func readinessFromChecks(checks []Check, monitor *Monitor) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		evaluation := Evaluate(r.Context(), checks)
		// A probe that went away mid-check (its own timeout, or shutdown) cut
		// the pings short: that is not an outage, so the monitor never sees it.
		if monitor != nil && r.Context().Err() == nil {
			monitor.Observe(evaluation)
		}
		status := Status{Status: evaluation.Status, Checks: evaluation.Checks}
		if evaluation.Status == "not_ready" {
			writeJSON(w, http.StatusServiceUnavailable, status)
			return
		}
		writeJSON(w, http.StatusOK, status)
	}
}
