package adminaccess_test

// The break-glass metrics without a database (ADR-0059 R2.10a, issue 2524):
// the exposition carries no user label, and a start counts its path. The
// gauge on PostgreSQL is metrics_postgres_integration_test.go.

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// countQuerier answers the gauge's count, or an error.
type countQuerier struct {
	count int64
	err   error
}

func (q countQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("not used")
}

func (q countQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	return scriptedRow{values: []any{q.count}, err: q.err}
}

// scriptedRow scans fixed values into the destinations, in order.
type scriptedRow struct {
	values []any
	err    error
}

func (r scriptedRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return errors.New("scriptedRow: the scan does not match the script")
	}
	for i, value := range r.values {
		reflect.ValueOf(dest[i]).Elem().Set(reflect.ValueOf(value))
	}
	return nil
}

func exposition(t *testing.T, metrics *adminaccess.BreakGlassMetrics) string {
	t.Helper()
	var b strings.Builder
	if err := metrics.WriteTextContext(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Every sample's labels are a closed set: none on the gauge, `path` with a
// known value on the counter. No user, session, level or credential.
func TestTheBreakGlassMetricsHaveNoUserLabel(t *testing.T) {
	text := exposition(t, adminaccess.NewBreakGlassMetrics(countQuerier{count: 2}))
	sample := regexp.MustCompile(`^([a-z_]+)(\{[^}]*\})? -?[0-9]+$`)
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		match := sample.FindStringSubmatch(line)
		if match == nil {
			t.Fatalf("not a sample line: %q", line)
		}
		seen[match[1]] = true
		switch match[1] {
		case adminaccess.MetricElevationsActive, adminaccess.MetricElevationsReadOK:
			if match[2] != "" {
				t.Errorf("the gauge has labels: %q", line)
			}
		case adminaccess.MetricBreakGlassActivations:
			if !regexp.MustCompile(`^\{path="(approved|self|sealed)"\}$`).MatchString(match[2]) {
				t.Errorf("the counter has a label other than a known path: %q", line)
			}
		default:
			t.Errorf("unexpected series %q", line)
		}
		for _, forbidden := range []string{"user", "session", "level", "credential", "email", "id="} {
			if strings.Contains(match[2], forbidden) {
				t.Errorf("a sample carries %q: %q", forbidden, line)
			}
		}
	}
	if !seen[adminaccess.MetricElevationsActive] || !seen[adminaccess.MetricBreakGlassActivations] {
		t.Fatalf("missing a series:\n%s", text)
	}
	if !strings.Contains(text, adminaccess.MetricElevationsActive+" 2\n") {
		t.Errorf("the gauge does not report the database's count:\n%s", text)
	}
	for _, path := range []string{"approved", "self", "sealed"} {
		if !strings.Contains(text, adminaccess.MetricBreakGlassActivations+`{path="`+path+`"} `) {
			t.Errorf("the counter has no %s series before any start:\n%s", path, text)
		}
	}
}

// A failed read keeps the last value: a database blip is not "every
// elevation ended".
// A failed read writes no gauge sample (neither the last value nor 0, both
// of which read as correct) and read-ok 0; no database is the same.
func TestAFailedReadWritesNoGaugeSample(t *testing.T) {
	querier := &switchingQuerier{count: 3}
	metrics := adminaccess.NewBreakGlassMetrics(querier)
	text := exposition(t, metrics)
	if !strings.Contains(text, adminaccess.MetricElevationsActive+" 3\n") ||
		!strings.Contains(text, adminaccess.MetricElevationsReadOK+" 1\n") {
		t.Fatalf("first scrape:\n%s", text)
	}
	querier.err = errors.New("the database is gone")
	for name, m := range map[string]*adminaccess.BreakGlassMetrics{
		"a failed read": metrics,
		"no database":   adminaccess.NewBreakGlassMetrics(nil),
	} {
		text := exposition(t, m)
		if strings.Contains(text, "\n"+adminaccess.MetricElevationsActive+" ") {
			t.Errorf("%s wrote a gauge sample:\n%s", name, text)
		}
		if !strings.Contains(text, adminaccess.MetricElevationsReadOK+" 0\n") {
			t.Errorf("%s did not say the read failed:\n%s", name, text)
		}
	}
}

type switchingQuerier struct {
	count int64
	err   error
}

func (q *switchingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("not used")
}

func (q *switchingQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	return scriptedRow{values: []any{q.count}, err: q.err}
}

// A start counts its path, once, and only a start that was written.
func TestAStartCountsItsPathInTheActivationCounter(t *testing.T) {
	start := validElevationStart()
	start.Path, start.ApproverUserID = adminaccess.ElevationPathSealed, 0
	before := adminaccess.BreakGlassActivations(adminaccess.ElevationPathSealed)
	approvedBefore := adminaccess.BreakGlassActivations(adminaccess.ElevationPathApproved)
	if _, err := adminaccess.StartElevation(t.Context(), newStartTx(start), okAppender{}, start); err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := adminaccess.BreakGlassActivations(adminaccess.ElevationPathSealed); got != before+1 {
		t.Fatalf("sealed activations %d after a start, want %d", got, before+1)
	}
	if got := adminaccess.BreakGlassActivations(adminaccess.ElevationPathApproved); got != approvedBefore {
		t.Fatalf("a sealed start counted on approved: %d, want %d", got, approvedBefore)
	}
	text := exposition(t, adminaccess.NewBreakGlassMetrics(countQuerier{}))
	if !strings.Contains(text, adminaccess.MetricBreakGlassActivations+`{path="sealed"} `) {
		t.Fatalf("no sealed series:\n%s", text)
	}

	// A start whose event is not written leaves no row and counts nothing.
	if _, err := adminaccess.StartElevation(t.Context(), newStartTx(start), securityaudit.FailingAppender{Err: errors.New("down")}, start); err == nil {
		t.Fatal("a start with a failing appender succeeded")
	}
	if got := adminaccess.BreakGlassActivations(adminaccess.ElevationPathSealed); got != before+1 {
		t.Fatalf("a failed start counted: %d, want %d", got, before+1)
	}
}

type okAppender struct{}

func (okAppender) Append(context.Context, pgx.Tx, securityaudit.SecurityEvent) error { return nil }

// startTx is the transaction StartElevation sees: the lock, the checks (a
// live person and session, no active elevation) and the INSERT, answered from
// the start itself.
type startTx struct {
	pgx.Tx
	start   adminaccess.ElevationStart
	queries int
}

func newStartTx(start adminaccess.ElevationStart) *startTx { return &startTx{start: start} }

func (tx *startTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.NewCommandTag("SELECT 1"), nil
}

func (tx *startTx) Begin(context.Context) (pgx.Tx, error) { return tx, nil }
func (tx *startTx) Commit(context.Context) error          { return nil }
func (tx *startTx) Rollback(context.Context) error        { return nil }

func (tx *startTx) QueryRow(context.Context, string, ...any) pgx.Row {
	tx.queries++
	if tx.queries == 1 {
		// person, session, approver level, already elevated
		return scriptedRow{values: []any{true, true, true, false}}
	}
	s := tx.start
	at := s.At.UTC()
	expires := at.Add(s.Duration)
	var none *time.Time
	return scriptedRow{values: []any{
		uuid.New(), s.UserID, s.BrowserSessionRef, s.Level, s.Path, s.ReasonCode, s.Reason,
		s.Ticket, s.UserID, s.ApproverUserID, at,
		"approved", &at, &at, &expires, none,
		"", "",
	}}
}
