package proxy

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/EliteaAI/elitea-platform/libs/go/egresslib/runtoken"
)

// SummaryEvent is the stable message of a run summary log line. The same
// summary goes to Main's audit log through the Runs sink (SetSink); do not
// rename it.
const SummaryEvent = "egress_proxy.run_summary"

// ConnectionEvent is the stable message of a per-connection log line.
const ConnectionEvent = "egress_proxy.connection"

// maxHostsPerRun bounds the distinct hosts one run's summary holds. Further
// hosts are counted in other_hosts, so a run cannot grow the proxy's memory
// without bound by probing names.
const maxHostsPerRun = 256

type hostPort struct {
	host string
	port int
}

type refusalKey struct {
	hostPort
	reason string
}

// run is one execution's state on this replica.
type run struct {
	executionID string
	projectID   int64
	expiresAt   time.Time

	bytes atomic.Int64 // up and down, against the run's byte limit

	// Guarded by Runs.mu.
	active      int
	connections int64
	bytesUp     int64
	bytesDown   int64
	reached     map[hostPort]*reachStat
	refused     map[refusalKey]int64
	otherHosts  int64
	dirty       bool
	lastEmitted time.Time
}

type reachStat struct {
	connections int64
	bytesUp     int64
	bytesDown   int64
}

// HostReached is one entry of a summary's hosts_reached.
type HostReached struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Connections int64  `json:"connections"`
	BytesUp     int64  `json:"bytes_up"`
	BytesDown   int64  `json:"bytes_down"`
}

// HostRefused is one entry of a summary's hosts_refused.
type HostRefused struct {
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
}

// Runs tracks the runs this replica serves: their limits and their summaries.
type Runs struct {
	maxConnections int
	maxBytes       int64
	logger         *slog.Logger

	mu   sync.Mutex
	runs map[string]*run

	// sink receives every summary that is logged (elitea-main's audit
	// reporter). It must not block. Set before Run starts.
	sink func(Summary)
}

// Summary is one run's summary on this replica: cumulative since the run's
// first connection here.
type Summary struct {
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

// SetSink sets the function that receives every summary. Call it before Run.
func (rs *Runs) SetSink(sink func(Summary)) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.sink = sink
}

// NewRuns builds the run registry.
func NewRuns(maxConnections int, maxBytes int64, logger *slog.Logger) *Runs {
	return &Runs{maxConnections: maxConnections, maxBytes: maxBytes, logger: logger, runs: map[string]*run{}}
}

// Active is the number of runs this replica holds state for.
func (rs *Runs) Active() int64 {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return int64(len(rs.runs))
}

// lookup returns the run of these claims, creating it. Caller holds rs.mu.
func (rs *Runs) lookup(c runtoken.Claims) *run {
	r, ok := rs.runs[c.ExecutionID]
	if !ok {
		r = &run{
			executionID: c.ExecutionID,
			projectID:   c.ProjectID,
			expiresAt:   c.ExpiresAt,
			reached:     map[hostPort]*reachStat{},
			refused:     map[refusalKey]int64{},
		}
		rs.runs[c.ExecutionID] = r
	}
	// A later token for the same run (a reclaim) extends what this replica
	// keeps; the limits stay per execution.
	if c.ExpiresAt.After(r.expiresAt) {
		r.expiresAt = c.ExpiresAt
	}
	return r
}

// acquire takes one connection slot of the run. It returns the refusal reason
// when the run is at its connection limit or has spent its byte budget.
func (rs *Runs) acquire(c runtoken.Claims) (*run, string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	r := rs.lookup(c)
	if r.bytes.Load() >= rs.maxBytes {
		return nil, ReasonRunByteLimit
	}
	if r.active >= rs.maxConnections {
		return nil, ReasonRunConnectionLimit
	}
	r.active++
	r.dirty = true
	return r, ""
}

// release returns the slot and records what the connection moved.
func (rs *Runs) release(r *run, target hostPort, up, down int64, reached bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	r.active--
	r.bytesUp += up
	r.bytesDown += down
	r.dirty = true
	if !reached {
		return
	}
	r.connections++
	st, ok := r.reached[target]
	if !ok {
		if len(r.reached)+len(r.refused) >= maxHostsPerRun {
			r.otherHosts++
			return
		}
		st = &reachStat{}
		r.reached[target] = st
	}
	st.connections++
	st.bytesUp += up
	st.bytesDown += down
}

// refused records a refusal for a verified run.
func (rs *Runs) refused(c runtoken.Claims, target hostPort, reason string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	r := rs.lookup(c)
	r.dirty = true
	key := refusalKey{hostPort: target, reason: reason}
	if _, ok := r.refused[key]; !ok && len(r.reached)+len(r.refused) >= maxHostsPerRun {
		r.otherHosts++
		return
	}
	r.refused[key]++
}

// spend adds n bytes to the run's budget and reports whether it still fits.
func (r *run) spend(n int, limit int64) bool {
	return r.bytes.Add(int64(n)) <= limit
}

// Sweep logs a summary for every run that changed since its last summary at
// least interval ago, and a final summary for every run whose token expired
// and that has no open connection. A run with a final summary is dropped.
// With shutdown set, it logs every run that changed, marked replica_shutdown.
func (rs *Runs) Sweep(now time.Time, interval time.Duration, shutdown bool) {
	var out []Summary
	rs.mu.Lock()
	for id, r := range rs.runs {
		expired := !now.Before(r.expiresAt) && r.active == 0
		due := r.dirty && (shutdown || now.Sub(r.lastEmitted) >= interval)
		if !expired && !due {
			continue
		}
		out = append(out, r.summary(expired, shutdown))
		r.dirty = false
		r.lastEmitted = now
		if expired {
			delete(rs.runs, id)
		}
	}
	sink := rs.sink
	rs.mu.Unlock()
	for _, summary := range out {
		rs.logger.LogAttrs(context.Background(), slog.LevelInfo, SummaryEvent, summary.attrs()...)
		if sink != nil {
			sink(summary)
		}
	}
}

// Run sweeps until ctx ends. It ticks often enough that a final summary
// follows a token's expiry within a few seconds.
func (rs *Runs) Run(ctx context.Context, interval time.Duration, now func() time.Time) {
	tick := interval
	if tick > 5*time.Second {
		tick = 5 * time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rs.Sweep(now(), interval, false)
		}
	}
}

// summary builds the summary of one run. Caller holds rs.mu.
func (r *run) summary(final, shutdown bool) Summary {
	reached := make([]HostReached, 0, len(r.reached))
	for hp, st := range r.reached {
		reached = append(reached, HostReached{Host: hp.host, Port: hp.port, Connections: st.connections, BytesUp: st.bytesUp, BytesDown: st.bytesDown})
	}
	sort.Slice(reached, func(i, j int) bool {
		if reached[i].Host != reached[j].Host {
			return reached[i].Host < reached[j].Host
		}
		return reached[i].Port < reached[j].Port
	})
	refused := make([]HostRefused, 0, len(r.refused))
	for k, n := range r.refused {
		refused = append(refused, HostRefused{Host: k.host, Port: k.port, Reason: k.reason, Count: n})
	}
	sort.Slice(refused, func(i, j int) bool {
		if refused[i].Host != refused[j].Host {
			return refused[i].Host < refused[j].Host
		}
		if refused[i].Port != refused[j].Port {
			return refused[i].Port < refused[j].Port
		}
		return refused[i].Reason < refused[j].Reason
	})
	return Summary{
		ExecutionID: r.executionID, ProjectID: r.projectID, Final: final, ReplicaShutdown: shutdown,
		TokenExpiresAt: r.expiresAt, Connections: r.connections, BytesUp: r.bytesUp, BytesDown: r.bytesDown,
		HostsReached: reached, HostsRefused: refused, OtherHosts: r.otherHosts,
	}
}

func (s Summary) attrs() []slog.Attr {
	return []slog.Attr{
		slog.String("execution_id", s.ExecutionID),
		slog.Int64("project_id", s.ProjectID),
		slog.Bool("final", s.Final),
		slog.Bool("replica_shutdown", s.ReplicaShutdown),
		slog.Time("token_expires_at", s.TokenExpiresAt),
		slog.Int64("connections", s.Connections),
		slog.Int64("bytes_up", s.BytesUp),
		slog.Int64("bytes_down", s.BytesDown),
		slog.Any("hosts_reached", s.HostsReached),
		slog.Any("hosts_refused", s.HostsRefused),
		slog.Int64("other_hosts", s.OtherHosts),
	}
}
