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
//     never sends its last report; its latest snapshot is used).
//
// Main shutting down writes nothing: the turn stays in the store for the next
// sweep, here or on another Main replica.
//
// A report for a turn already written starts a new event (a resumed turn
// reuses the execution id with a new token).
//
// # Where the summaries live
//
// In a Store shared by every Main replica (the Postgres table
// elitea_runtime.egress_run_summaries, shared migration 0169), never in a
// replica's memory. Every report is an upsert, so which Main replica a proxy
// replica's connection lands on does not matter, and a Main restart loses
// nothing: the rows are still there and the next sweep, on any replica,
// writes the turn. Every Main replica sweeps; the store's ClaimDue hands a due
// turn's rows to exactly ONE of them (a single UPDATE ... RETURNING), so a
// turn yields exactly one audit event however many Main replicas run.
//
// # The worker's own reports
//
// The worker's HTTP clients (web_fetch, the SaaS toolkits, MCP servers) report
// the hosts a run reached and refused over RuntimeControlService
// ReportEgressSummary (ADR-0044 decision 2), under the run's live claim. Main
// stores that as the run's row with replica WorkerReplica, beside the proxy
// replicas' rows of the same execution, so one turn's event merges both. A
// worker row carries no token: it is non-final until the run settles
// (SettleWorker, from PrepareSettlement), and then written after Settle like a
// proxy replica's last report. Its token_expires_at is the last report plus
// WorkerHold, so a run that never settles (its worker died, and nothing took
// it over) is still written, partial, MaxHold after that.
//
// Delivery is at most once past the claim: a Main that dies between claiming
// a turn and recording its event loses that one event (the proxy's own log
// keeps the summary, and the recorder behind this package is itself an async
// best-effort queue). A claimed row is kept for Config.Retention and deleted
// by the sweep after it.
package egressaudit

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
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
	// Worker and Proxy say whose reports were merged: the worker's own HTTP
	// clients, the egress proxy's replicas, or both.
	Worker bool `json:"worker"`
	Proxy  bool `json:"proxy"`
}

// Row is one stored (execution, replica) snapshot.
type Row struct {
	Report
	FirstReport time.Time
	UpdatedAt   time.Time
}

// Store is the shared place the replicas' snapshots live in. Its
// implementations must make ClaimDue atomic across callers: a row it returns
// is returned to no other call, whatever the number of Main replicas
// (the Postgres store claims with one UPDATE ... RETURNING).
type Store interface {
	// Upsert replaces the (execution, replica) snapshot with report, stamped
	// now. A report for a row already claimed re-opens it as a new turn. A
	// non-final report never replaces a final one of the same open turn.
	Upsert(ctx context.Context, report Report, now time.Time) error
	// ClaimDue claims every open turn that is due at now and returns all of
	// its rows. A turn is due when every replica's row is final and the last
	// report is at least settle old, or when its latest token expiry is at
	// least maxHold past.
	ClaimDue(ctx context.Context, now time.Time, settle, maxHold time.Duration) ([]Row, error)
	// DeleteOlderThan deletes the rows last updated before cutoff, claimed or
	// not, and reports how many.
	DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
	// MarkFinal makes the open (execution, replica) row final, stamped now,
	// so the turn is due Settle later. No open row is not an error.
	MarkFinal(ctx context.Context, executionID, replica string, now time.Time) error
}

// WorkerReplica is the replica of the worker's own row. The egress audit
// service refuses it as a proxy replica id.
const WorkerReplica = "worker"

// WorkerHold is how long after a worker's last report its row counts as
// live; see "The worker's own reports".
const WorkerHold = 6 * time.Hour

// Config tunes the merger. Zero values take the defaults.
type Config struct {
	// Settle is how long the merger waits after the last report once every
	// replica it heard from is done. Default 30 s.
	Settle time.Duration
	// MaxHold is how long after the token's expiry a turn is written even
	// when a replica never sent its last report. Default 10 min (a proxy
	// replica sweeps every 5 s and logs the final summary within seconds).
	MaxHold time.Duration
	// Retention is how long the store keeps a row after its last report.
	// Default 30 days: the admin view of the hosts runs reached reads 30
	// days back by default.
	Retention time.Duration
	// Tick is the sweep interval of Start. Default 5 s.
	Tick time.Duration
}

// DefaultRetention is how long the rows are kept when Config.Retention is
// zero.
const DefaultRetention = 30 * 24 * time.Hour

func (c Config) withDefaults() Config {
	if c.Settle <= 0 {
		c.Settle = 30 * time.Second
	}
	if c.MaxHold <= 0 {
		c.MaxHold = 10 * time.Minute
	}
	if c.Retention <= 0 {
		c.Retention = DefaultRetention
	}
	if c.Tick <= 0 {
		c.Tick = 5 * time.Second
	}
	return c
}

// retentionEvery is how often the sweep deletes expired rows.
const retentionEvery = time.Minute

// storeTimeout bounds one store call of the background sweep.
const storeTimeout = 30 * time.Second

// Merger stores reports and writes one audit event per turn. It is safe for
// concurrent use, and any number of Mergers over one Store (one per Main
// replica) write each turn once between them.
type Merger struct {
	cfg      Config
	store    Store
	recorder audit.Recorder
	logger   *slog.Logger
	now      func() time.Time

	retentionMu   sync.Mutex
	lastRetention time.Time

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
	started  bool
	startMu  sync.Mutex
}

// New builds a merger over store that writes to recorder. Start the sweep
// with Start.
func New(store Store, recorder audit.Recorder, logger *slog.Logger, cfg Config) *Merger {
	if logger == nil {
		logger = slog.Default()
	}
	return &Merger{
		cfg: cfg.withDefaults(), store: store, recorder: recorder, logger: logger, now: time.Now,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
}

// WithClock replaces time.Now, for tests.
func (m *Merger) WithClock(now func() time.Time) *Merger {
	m.now = now
	return m
}

// Report stores one replica's snapshot. An error means it was NOT stored; the
// proxy retries it.
func (m *Merger) Report(ctx context.Context, report Report) error {
	return m.store.Upsert(ctx, report, m.now())
}

// WorkerSummary is the worker's cumulative view of one claimed run.
type WorkerSummary struct {
	ExecutionID  string
	ProjectID    int64
	HostsReached []HostReached
	HostsRefused []HostRefused
	OtherHosts   int64
}

// ReportWorker stores the worker's snapshot of a run as its WorkerReplica
// row, replacing the previous one. It stays non-final until SettleWorker.
func (m *Merger) ReportWorker(ctx context.Context, summary WorkerSummary) error {
	report := Report{
		ReplicaID: WorkerReplica, ExecutionID: summary.ExecutionID, ProjectID: summary.ProjectID,
		TokenExpiresAt: m.now().Add(WorkerHold),
		HostsReached:   summary.HostsReached, HostsRefused: summary.HostsRefused, OtherHosts: summary.OtherHosts,
	}
	for _, host := range summary.HostsReached {
		report.Connections += host.Connections
		report.BytesUp += host.BytesUp
		report.BytesDown += host.BytesDown
	}
	return m.store.Upsert(ctx, report, m.now())
}

// SettleWorker makes the run's worker row final: the run settled, so no
// further report can come for this turn.
func (m *Merger) SettleWorker(ctx context.Context, executionID string) error {
	return m.store.MarkFinal(ctx, executionID, WorkerReplica, m.now())
}

// Sweep claims every turn that is due at now, writes one audit event for
// each, and (at most once a minute) deletes the rows past retention.
func (m *Merger) Sweep(ctx context.Context) error {
	now := m.now()
	rows, err := m.store.ClaimDue(ctx, now, m.cfg.Settle, m.cfg.MaxHold)
	if err != nil {
		return fmt.Errorf("claim due egress turns: %w", err)
	}
	m.write(mergeRows(rows))
	return m.expire(ctx, now)
}

func (m *Merger) expire(ctx context.Context, now time.Time) error {
	m.retentionMu.Lock()
	if !m.lastRetention.IsZero() && now.Sub(m.lastRetention) < retentionEvery {
		m.retentionMu.Unlock()
		return nil
	}
	m.lastRetention = now
	m.retentionMu.Unlock()
	if _, err := m.store.DeleteOlderThan(ctx, now.Add(-m.cfg.Retention)); err != nil {
		return fmt.Errorf("delete expired egress summaries: %w", err)
	}
	return nil
}

// Start runs the sweep until Close.
func (m *Merger) Start() {
	m.startMu.Lock()
	defer m.startMu.Unlock()
	if m.started {
		return
	}
	m.started = true
	go func() {
		defer close(m.done)
		ticker := time.NewTicker(m.cfg.Tick)
		defer ticker.Stop()
		for {
			select {
			case <-m.stop:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
				if err := m.Sweep(ctx); err != nil {
					m.logger.Warn("egress_audit.sweep_failed", "error", err.Error())
				}
				cancel()
			}
		}
	}()
}

// Close stops the sweep. It writes nothing: a turn not yet written stays in
// the store for the next sweep, on this Main replica after a restart or on
// another.
func (m *Merger) Close() {
	m.stopOnce.Do(func() {
		close(m.stop)
	})
	m.startMu.Lock()
	started := m.started
	m.startMu.Unlock()
	if !started {
		return
	}
	select {
	case <-m.done:
	case <-time.After(5 * time.Second):
	}
}

// mergeRows groups the claimed rows by execution and merges each group.
func mergeRows(rows []Row) []Turn {
	groups := map[string][]Row{}
	var order []string
	for _, row := range rows {
		if _, ok := groups[row.ExecutionID]; !ok {
			order = append(order, row.ExecutionID)
		}
		groups[row.ExecutionID] = append(groups[row.ExecutionID], row)
	}
	sort.Strings(order)
	turns := make([]Turn, 0, len(order))
	for _, id := range order {
		turns = append(turns, merge(id, groups[id]))
	}
	return turns
}

func merge(executionID string, rows []Row) Turn {
	turn := Turn{ExecutionID: executionID, Replicas: len(rows), Complete: true}
	for _, row := range rows {
		if row.ReplicaID == WorkerReplica {
			turn.Worker = true
		} else {
			turn.Proxy = true
		}
	}
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
	for i, row := range rows {
		if i == 0 || row.FirstReport.Before(turn.FirstReport) {
			turn.FirstReport = row.FirstReport
		}
		if row.UpdatedAt.After(turn.LastReport) {
			turn.LastReport = row.UpdatedAt
		}
		if turn.ProjectID == 0 {
			turn.ProjectID = row.ProjectID
		}
		turn.Complete = turn.Complete && (row.Final || row.ReplicaShutdown)
		turn.Connections += row.Connections
		turn.BytesUp += row.BytesUp
		turn.BytesDown += row.BytesDown
		turn.OtherHosts += row.OtherHosts
		for _, h := range row.HostsReached {
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
		for _, h := range row.HostsRefused {
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
	switch {
	case turn.Worker && turn.Proxy:
		b.WriteString("egress (worker and proxy): ")
	case turn.Worker:
		b.WriteString("worker egress: ")
	default:
		b.WriteString("egress proxy: ")
	}
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
