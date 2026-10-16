// Package egressaudit turns the egress proxy's run summaries into one audit
// event per cloud turn (ADR-0034 decision 9, "Host allowlist: the egress
// proxy", Logging: "Main's audit log receives a per-turn summary of the
// hosts reached and the hosts refused").
//
// # What a report is
//
// Each proxy replica keeps its own summary of each run it serves and reports
// it over EgressAuditService (transport/runtimegrpc/egressaudit). A report is
// a CUMULATIVE snapshot of ONE replica's view of ONE run, so the merger keeps
// the latest snapshot per (execution, replica) and REPLACES it on every
// report: a repeated or retried report never counts twice.
//
// # When a turn is written
//
// A replica's last report for a run carries `final` (its token expired on
// that replica) or `replica_shutdown` (the replica is stopping). The merger
// writes the turn's event, merged across every replica it heard from, when:
//
//   - every replica it heard from has sent its last report and no report has
//     arrived for Settle (a replica that served the run late still gets in);
//   - or MaxHold has passed since the token expired (a replica that crashed
//     never sends its last report; its latest snapshot is used);
//   - or Main shuts down (Close), with what it has.
//
// A report for a turn already written starts a new event (a resumed turn
// reuses the execution id with a new token).
//
// # Main replicas
//
// The merge is in memory. With several Main replicas, proxy replicas whose
// connections landed on different Main replicas are merged on each, so one
// turn can produce one event per Main replica, each covering a disjoint set
// of proxy replicas (it says how many). The numbers never double count:
// snapshots are per proxy replica and each proxy replica reports to one Main
// connection at a time.
package egressaudit

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
)

// HostReached is one host a run reached through the proxy.
type HostReached struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Connections int64  `json:"connections"`
	BytesUp     int64  `json:"bytes_up"`
	BytesDown   int64  `json:"bytes_down"`
}

// HostRefused is one host the proxy refused for a run, with the reason.
type HostRefused struct {
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
}

// Report is one replica's cumulative summary of one run.
type Report struct {
	ReplicaID       string
	ExecutionID     string
	ProjectID       int64
	Final           bool
	ReplicaShutdown bool
	TokenExpiresAt  time.Time
	Connections     int64
	BytesUp         int64
	BytesDown       int64
	HostsReached    []HostReached
	HostsRefused    []HostRefused
	OtherHosts      int64
}

// Turn is the merged summary of one turn: what the audit event records.
type Turn struct {
	ExecutionID  string        `json:"execution_id"`
	ProjectID    int64         `json:"project_id"`
	Replicas     int           `json:"replicas"`
	Complete     bool          `json:"complete"`
	Connections  int64         `json:"connections"`
	BytesUp      int64         `json:"bytes_up"`
	BytesDown    int64         `json:"bytes_down"`
	HostsReached []HostReached `json:"hosts_reached"`
	HostsRefused []HostRefused `json:"hosts_refused"`
	OtherHosts   int64         `json:"other_hosts"`
	FirstReport  time.Time     `json:"first_report"`
	LastReport   time.Time     `json:"last_report"`
}

// Config tunes the merger. Zero values take the defaults.
type Config struct {
	// Settle is how long the merger waits after the last report once every
	// replica it heard from is done. Default 30 s.
	Settle time.Duration
	// MaxHold is how long after the token's expiry a turn is written even
	// when a replica never sent its last report. Default 10 min (a proxy
	// replica sweeps every 5 s and logs the final summary within seconds).
	MaxHold time.Duration
	// MaxPending bounds the turns held in memory; past it the oldest turn
	// is written early. Default 10000.
	MaxPending int
	// Tick is the sweep interval of Run. Default 5 s.
	Tick time.Duration
}

func (c Config) withDefaults() Config {
	if c.Settle <= 0 {
		c.Settle = 30 * time.Second
	}
	if c.MaxHold <= 0 {
		c.MaxHold = 10 * time.Minute
	}
	if c.MaxPending <= 0 {
		c.MaxPending = 10_000
	}
	if c.Tick <= 0 {
		c.Tick = 5 * time.Second
	}
	return c
}

type pending struct {
	projectID   int64
	expiresAt   time.Time
	replicas    map[string]Report
	done        map[string]bool
	firstReport time.Time
	lastReport  time.Time
}

// Merger merges reports per turn and writes one audit event per turn. It is
// safe for concurrent use.
type Merger struct {
	cfg      Config
	recorder audit.Recorder
	logger   *slog.Logger
	now      func() time.Time

	mu      sync.Mutex
	pending map[string]*pending
	closed  bool

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

// New builds a merger that writes to recorder. Start the sweep with Start.
func New(recorder audit.Recorder, logger *slog.Logger, cfg Config) *Merger {
	if logger == nil {
		logger = slog.Default()
	}
	return &Merger{
		cfg: cfg.withDefaults(), recorder: recorder, logger: logger, now: time.Now,
		pending: map[string]*pending{}, stop: make(chan struct{}), done: make(chan struct{}),
	}
}

// WithClock replaces time.Now, for tests.
func (m *Merger) WithClock(now func() time.Time) *Merger {
	m.now = now
	return m
}

// Report records one replica's snapshot. It never blocks on the audit write.
func (m *Merger) Report(report Report) {
	now := m.now()
	var flush []Turn
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.write([]Turn{merge(report.ExecutionID, &pending{
			projectID: report.ProjectID, replicas: map[string]Report{report.ReplicaID: report},
			done: map[string]bool{report.ReplicaID: report.Final || report.ReplicaShutdown}, firstReport: now, lastReport: now,
		})})
		return
	}
	p, ok := m.pending[report.ExecutionID]
	if !ok {
		if len(m.pending) >= m.cfg.MaxPending {
			flush = append(flush, m.evictOldestLocked())
		}
		p = &pending{projectID: report.ProjectID, replicas: map[string]Report{}, done: map[string]bool{}, firstReport: now}
		m.pending[report.ExecutionID] = p
	}
	p.replicas[report.ReplicaID] = report
	p.done[report.ReplicaID] = report.Final || report.ReplicaShutdown
	p.lastReport = now
	if report.TokenExpiresAt.After(p.expiresAt) {
		p.expiresAt = report.TokenExpiresAt
	}
	m.mu.Unlock()
	m.write(flush)
}

func (m *Merger) evictOldestLocked() Turn {
	var oldestID string
	var oldest *pending
	for id, p := range m.pending {
		if oldest == nil || p.firstReport.Before(oldest.firstReport) {
			oldestID, oldest = id, p
		}
	}
	delete(m.pending, oldestID)
	return merge(oldestID, oldest)
}

// Sweep writes every turn that is due at now.
func (m *Merger) Sweep() {
	now := m.now()
	var due []Turn
	m.mu.Lock()
	for id, p := range m.pending {
		allDone := true
		for _, done := range p.done {
			allDone = allDone && done
		}
		settled := allDone && now.Sub(p.lastReport) >= m.cfg.Settle
		expired := !p.expiresAt.IsZero() && now.Sub(p.expiresAt) >= m.cfg.MaxHold
		if settled || expired {
			due = append(due, merge(id, p))
			delete(m.pending, id)
		}
	}
	m.mu.Unlock()
	m.write(due)
}

// Start runs the sweep until Close.
func (m *Merger) Start() {
	go func() {
		defer close(m.done)
		ticker := time.NewTicker(m.cfg.Tick)
		defer ticker.Stop()
		for {
			select {
			case <-m.stop:
				return
			case <-ticker.C:
				m.Sweep()
			}
		}
	}()
}

// Close stops the sweep and writes every held turn with what it has. A
// report after Close is written at once, unmerged.
func (m *Merger) Close() {
	m.stopOnce.Do(func() {
		close(m.stop)
	})
	select {
	case <-m.done:
	case <-time.After(5 * time.Second):
	}
	m.mu.Lock()
	m.closed = true
	all := make([]Turn, 0, len(m.pending))
	for id, p := range m.pending {
		all = append(all, merge(id, p))
	}
	m.pending = map[string]*pending{}
	m.mu.Unlock()
	m.write(all)
}

// Pending is the number of turns held, for tests and metrics.
func (m *Merger) Pending() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.pending)
}

func merge(executionID string, p *pending) Turn {
	turn := Turn{ExecutionID: executionID, ProjectID: p.projectID, Replicas: len(p.replicas),
		FirstReport: p.firstReport, LastReport: p.lastReport, Complete: true}
	type hp struct {
		host string
		port int
	}
	type hr struct {
		hp
		reason string
	}
	reached := map[hp]*HostReached{}
	refused := map[hr]*HostRefused{}
	for replica, report := range p.replicas {
		turn.Complete = turn.Complete && p.done[replica]
		turn.Connections += report.Connections
		turn.BytesUp += report.BytesUp
		turn.BytesDown += report.BytesDown
		turn.OtherHosts += report.OtherHosts
		for _, h := range report.HostsReached {
			key := hp{h.Host, h.Port}
			if r, ok := reached[key]; ok {
				r.Connections += h.Connections
				r.BytesUp += h.BytesUp
				r.BytesDown += h.BytesDown
				continue
			}
			copied := h
			reached[key] = &copied
		}
		for _, h := range report.HostsRefused {
			key := hr{hp{h.Host, h.Port}, h.Reason}
			if r, ok := refused[key]; ok {
				r.Count += h.Count
				continue
			}
			copied := h
			refused[key] = &copied
		}
	}
	for _, r := range reached {
		turn.HostsReached = append(turn.HostsReached, *r)
	}
	for _, r := range refused {
		turn.HostsRefused = append(turn.HostsRefused, *r)
	}
	sort.Slice(turn.HostsReached, func(i, j int) bool {
		a, b := turn.HostsReached[i], turn.HostsReached[j]
		return a.Host < b.Host || (a.Host == b.Host && a.Port < b.Port)
	})
	sort.Slice(turn.HostsRefused, func(i, j int) bool {
		a, b := turn.HostsRefused[i], turn.HostsRefused[j]
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		return a.Reason < b.Reason
	})
	return turn
}

// The audit event's vocabulary: the SPA's "user" tab knows `tool`
// (internal/audit package doc), and the entity is the execution.
const (
	EventType  = "tool"
	EntityType = "execution"
	// LogEvent is the structured log line that carries the whole merged
	// summary; the audit row's action holds a bounded rendering of it.
	LogEvent = "egress_audit.turn_summary"
)

func (m *Merger) write(turns []Turn) {
	for _, turn := range turns {
		project := turn.ProjectID
		duration := float64(turn.LastReport.Sub(turn.FirstReport).Milliseconds())
		if duration < 0 {
			duration = 0
		}
		event := audit.Event{
			Timestamp:  turn.LastReport,
			ProjectID:  &project,
			EventType:  EventType,
			Action:     Action(turn),
			DurationMS: &duration,
			EntityType: EntityType,
			EntityName: turn.ExecutionID,
		}
		if m.recorder != nil {
			m.recorder.Record(context.Background(), event)
		}
		m.logger.Info(LogEvent,
			"execution_id", turn.ExecutionID, "project_id", turn.ProjectID, "replicas", turn.Replicas,
			"complete", turn.Complete, "connections", turn.Connections, "bytes_up", turn.BytesUp,
			"bytes_down", turn.BytesDown, "hosts_reached", turn.HostsReached, "hosts_refused", turn.HostsRefused,
			"other_hosts", turn.OtherHosts)
	}
}

// maxAction is the audit_events.action column width.
const maxAction = 512

// Action renders a turn for the audit trail's Action column, at most 512
// bytes: the hosts reached and refused, most significant first.
func Action(turn Turn) string {
	var b strings.Builder
	b.WriteString("egress proxy: ")
	if len(turn.HostsReached) == 0 {
		b.WriteString("reached no host")
	} else {
		b.WriteString("reached ")
		for i, h := range turn.HostsReached {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s:%d", h.Host, h.Port)
		}
	}
	if len(turn.HostsRefused) > 0 {
		b.WriteString("; refused ")
		for i, h := range turn.HostsRefused {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s:%d (%s x%d)", h.Host, h.Port, h.Reason, h.Count)
		}
	}
	if turn.OtherHosts > 0 {
		fmt.Fprintf(&b, "; %d more", turn.OtherHosts)
	}
	if !turn.Complete {
		b.WriteString("; partial")
	}
	text := b.String()
	if len(text) > maxAction {
		cut := maxAction - len("...")
		for cut > 0 && !utf8Start(text[cut]) {
			cut--
		}
		text = text[:cut] + "..."
	}
	return text
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
