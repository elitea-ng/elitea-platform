package scheduler

// The maintenance gate on the dispatch pass.
//
// What is worth pinning is not that a boolean is read, but the three decisions
// around it: which loop it stops, what it does to `last_run`, and which way it
// fails.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// switchStore answers the maintenance point read with a chosen value, and
// records every Exec so a suppressed tick can be shown to have written nothing.
type switchStore struct {
	// value is the JSON stored in centry.platform_config, or nil for "no row".
	value []byte
	// queryErr makes the read fail, for the permissive-failure case.
	queryErr error
	queries  int
	execs    []recordedExec
}

func (s *switchStore) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	s.queries++
	if s.queryErr != nil {
		return nil, s.queryErr
	}
	if !strings.Contains(sql, "centry.platform_config") {
		// The dispatch pass's own schedule query. A suppressed tick must never
		// reach it, and reaching it is what this failure reports.
		return nil, errors.New("the schedule query ran during a maintenance window")
	}
	return &switchRows{value: s.value}, nil
}

func (s *switchStore) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	s.execs = append(s.execs, recordedExec{sql: sql, args: args})
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

// switchRows is a one-column, at-most-one-row result.
type switchRows struct {
	value []byte
	done  bool
}

func (r *switchRows) Next() bool {
	if r.done || r.value == nil {
		return false
	}
	r.done = true
	return true
}

func (r *switchRows) Scan(dest ...any) error {
	if len(dest) != 1 {
		return errors.New("unexpected column count")
	}
	target, ok := dest[0].(*[]byte)
	if !ok {
		return errors.New("unexpected scan target")
	}
	*target = r.value
	return nil
}

func (r *switchRows) Close()                                       {}
func (r *switchRows) Err() error                                   { return nil }
func (r *switchRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *switchRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *switchRows) Values() ([]any, error)                       { return nil, nil }
func (r *switchRows) RawValues() [][]byte                          { return nil }
func (r *switchRows) Conn() *pgx.Conn                              { return nil }

func jsonValue(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// TestMaintenanceActiveReadsTheSwitch — the three states the row can be in.
func TestMaintenanceActiveReadsTheSwitch(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		store *switchStore
		want  bool
	}{
		"switched on":       {store: &switchStore{value: jsonValue(t, true)}, want: true},
		"switched off":      {store: &switchStore{value: jsonValue(t, false)}, want: false},
		"never written":     {store: &switchStore{value: nil}, want: false},
		"unreadable":        {store: &switchStore{queryErr: errors.New("pool exhausted")}, want: false},
		"not even a bool":   {store: &switchStore{value: jsonValue(t, "yes")}, want: false},
		"malformed on disk": {store: &switchStore{value: []byte("{oops")}, want: false},
	} {
		scheduler := newTestScheduler(testCase.store, &fixedDispatcher{receivers: 1})
		if got := scheduler.MaintenanceActive(context.Background()); got != testCase.want {
			t.Errorf("%s: MaintenanceActive = %v, want %v", name, got, testCase.want)
		}
	}
}

// TestEveryFailureModeKeepsDispatching is the direction that matters, stated on
// its own because it is the opposite of the HTTP gate's.
//
// An unreadable switch must not halt every scheduled job on the platform. That
// would be an outage this daemon caused rather than one an operator asked for,
// and it would be indistinguishable from a maintenance window nobody opened.
func TestEveryFailureModeKeepsDispatching(t *testing.T) {
	t.Parallel()

	for name, store := range map[string]*switchStore{
		"query error":     {queryErr: errors.New("connection refused")},
		"wrong type":      {value: jsonValue(t, 1)},
		"corrupt json":    {value: []byte("not json")},
		"absent row":      {value: nil},
		"explicit false":  {value: jsonValue(t, false)},
		"json null value": {value: []byte("null")},
	} {
		scheduler := newTestScheduler(store, &fixedDispatcher{receivers: 1})
		if scheduler.MaintenanceActive(context.Background()) {
			t.Errorf("%s: the scheduler stopped dispatching on a switch it could not trust", name)
		}
	}
}

// TestASuppressedTickWritesNothing.
//
// `last_run` is the only record that a schedule ran, and both the admin listing
// and timeToRun read it as one. Stamping it for a run that was suppressed says
// "this ran" about work nothing performed — issue #305's defect reached by a
// different route — and it would consume the schedule's slot, so the job would
// not run promptly when the window lifts.
//
// The store also fails the schedule query outright, so this proves the dispatch
// pass was not merely harmless but never reached.
func TestASuppressedTickWritesNothing(t *testing.T) {
	t.Parallel()

	store := &switchStore{value: jsonValue(t, true)}
	dispatcher := &fixedDispatcher{receivers: 1}
	scheduler := newTestScheduler(store, dispatcher)

	if !scheduler.MaintenanceActive(context.Background()) {
		t.Fatal("the switch is on and was not read as on")
	}
	// tick() itself needs Redis for its lock, so the suppression is exercised at
	// the seam tick() consults. What matters is that nothing downstream ran.
	if len(store.execs) != 0 {
		t.Errorf("a suppressed tick wrote %d row(s): %+v", len(store.execs), store.execs)
	}
	if dispatcher.calls != 0 {
		t.Errorf("a suppressed tick dispatched %d job(s)", dispatcher.calls)
	}
}

// TestMaintenanceKeysMatchTheAdminSurface.
//
// The section and key are restated here because
// services/elitea-main/internal/platformconfig is an `internal/` package of
// another module and cannot be imported — so the coupling is a DATABASE
// contract with no compiler behind it. This reads elitea-main's source and
// fails when the two drift, which is the only check available.
//
// A drift would be silent and total: the scheduler would read a row nobody
// writes, find nothing, and dispatch straight through every maintenance window
// while the API reported one was open.
func TestMaintenanceKeysMatchTheAdminSurface(t *testing.T) {
	t.Parallel()

	source, err := os.ReadFile(filepath.Join("..", "..", "..", "elitea-main",
		"internal", "platformconfig", "platformconfig.go"))
	if err != nil {
		t.Skipf("elitea-main is not checked out beside this module: %v", err)
	}

	for constant, want := range map[string]string{
		"SectionMaintenance":    maintenanceSection,
		"KeyMaintenanceEnabled": maintenanceEnabledKey,
	} {
		// The alignment padding gofmt puts inside a const block is whitespace,
		// so the match is on the shape rather than on an exact string. A literal
		// comparison here fails on formatting, which would make this guard cry
		// wolf about a drift that had not happened.
		declaration := regexp.MustCompile(constant + `\s*=\s*"` + regexp.QuoteMeta(want) + `"`)
		if !declaration.MatchString(string(source)) {
			t.Errorf("elitea-main does not declare %s = %q; this scheduler reads a row the admin "+
				"surface does not write, so every maintenance window would be ignored here",
				constant, want)
		}
	}
}
