package adminaccess

// The break-glass metrics (ADR-0059 decision 5, "Loud"; story R2.10a, issue
// 2524), in the Prometheus text format the main metrics listener serves
// (elitea-main has no metrics library; internal/application/breaker writes
// its exposition the same way, on the same listener).
//
//   - elitea_admin_elevations_active: the elevations active now, read from
//     elitea_auth.admin_elevations (shared 0269) at scrape time. It is a
//     query, not a count kept in memory, so every replica reports the same
//     value, and an elevation that expired with no reconciler pass already
//     reads as inactive: started, not ended, and its expiry after the
//     scrape's clock. Alert on max(), not sum().
//   - elitea_break_glass_activations_total{path}: the elevations this
//     replica started, by path (`approved`, `self`, `sealed`). StartElevation
//     counts it, so every start path counts without code of its own. It is
//     kept in process memory: it differs per replica and starts at 0 on a
//     restart, which `rate()` and `sum()` absorb. A start counts once its row
//     and its `started` event are written; a caller that then rolls its
//     transaction back over-counts by one.
//
// No series names a user, a session, a level by person or a credential: the
// one label is the path, a closed set.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// The metric names.
const (
	MetricElevationsActive      = "elitea_admin_elevations_active"
	MetricBreakGlassActivations = "elitea_break_glass_activations_total"
)

// metricsQueryTimeout bounds the scrape-time query: a slow database must not
// hang the scrape.
const metricsQueryTimeout = 2 * time.Second

// elevationPaths are the label values of the activation counter, in
// exposition order.
var elevationPaths = []string{ElevationPathApproved, ElevationPathSelf, ElevationPathSealed}

// activations counts the starts of this replica, by path. Every path is
// present from the start: a counter series that first appears at 1 makes
// increase() see no change.
var activations = struct {
	mu     sync.Mutex
	counts map[string]int64
}{counts: map[string]int64{ElevationPathApproved: 0, ElevationPathSelf: 0, ElevationPathSealed: 0}}

// countActivation counts one start on path. StartElevation calls it after the
// row and its event are written; a path outside the closed set (refused by
// Validate before) is not counted, so the label set stays bounded.
func countActivation(path string) {
	activations.mu.Lock()
	defer activations.mu.Unlock()
	if _, known := activations.counts[path]; known {
		activations.counts[path]++
	}
}

// BreakGlassActivations answers this replica's count of starts on path.
func BreakGlassActivations(path string) int64 {
	activations.mu.Lock()
	defer activations.mu.Unlock()
	return activations.counts[path]
}

// BreakGlassMetrics writes the two metrics. Build one per replica with
// NewBreakGlassMetrics; the zero value has no database and writes the gauge
// as 0.
type BreakGlassMetrics struct {
	db  Querier
	now func() time.Time

	mu         sync.Mutex
	lastActive int64
}

// NewBreakGlassMetrics returns the metrics over db, the platform database.
func NewBreakGlassMetrics(db Querier) *BreakGlassMetrics {
	return &BreakGlassMetrics{db: db, now: time.Now}
}

// WithClock replaces the clock the gauge reads "active" at (tests).
func (m *BreakGlassMetrics) WithClock(now func() time.Time) *BreakGlassMetrics {
	if now != nil {
		m.now = now
	}
	return m
}

// ActiveElevations answers the elevations active at the metrics' clock: started,
// not ended, and expiring after now. Bounded by metricsQueryTimeout.
func (m *BreakGlassMetrics) ActiveElevations(ctx context.Context) (int64, error) {
	if m == nil || m.db == nil {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(ctx, metricsQueryTimeout)
	defer cancel()
	var active int64
	if err := m.db.QueryRow(ctx, `
SELECT count(*) FROM elitea_auth.admin_elevations
WHERE started_at IS NOT NULL AND ended_at IS NULL AND expires_at > $1`, m.now().UTC()).Scan(&active); err != nil {
		return 0, fmt.Errorf("count the active elevations: %w", err)
	}
	return active, nil
}

// refreshActive reads the gauge. A failed or slow read keeps the last value:
// a database blip must not read as "every elevation ended".
func (m *BreakGlassMetrics) refreshActive(ctx context.Context) int64 {
	active, err := m.ActiveElevations(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		m.lastActive = active
	}
	return m.lastActive
}

// WriteTextContext writes the exposition, reading the gauge first.
func (m *BreakGlassMetrics) WriteTextContext(ctx context.Context, w io.Writer) error {
	if m == nil {
		return nil
	}
	active := m.refreshActive(ctx)
	var b strings.Builder
	b.WriteString("# HELP " + MetricElevationsActive +
		" Break-glass elevations active now, read from the database (the same on every replica; alert on max).\n")
	b.WriteString("# TYPE " + MetricElevationsActive + " gauge\n")
	fmt.Fprintf(&b, "%s %d\n", MetricElevationsActive, active)
	b.WriteString("# HELP " + MetricBreakGlassActivations + " Break-glass elevations this replica started, by path.\n")
	b.WriteString("# TYPE " + MetricBreakGlassActivations + " counter\n")
	activations.mu.Lock()
	for _, path := range elevationPaths {
		fmt.Fprintf(&b, "%s{path=%q} %d\n", MetricBreakGlassActivations, path, activations.counts[path])
	}
	activations.mu.Unlock()
	_, err := io.WriteString(w, b.String())
	return err
}
