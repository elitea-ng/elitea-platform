package maintenance

// The maintenance switch the retention sweepers share.
//
// What is worth pinning is not that a boolean is read, but which way it fails
// and that it reads the row the admin surface writes.

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

// switchStore answers the maintenance point read with a chosen value.
type switchStore struct {
	// value is the JSON stored in centry.platform_config, or nil for "no row".
	value []byte
	// queryErr makes the read fail, for the permissive-failure case.
	queryErr error
	queries  int
}

func (s *switchStore) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	s.queries++
	if s.queryErr != nil {
		return nil, s.queryErr
	}
	if !strings.Contains(sql, "centry.platform_config") {
		return nil, errors.New("the switch read a table other than centry.platform_config")
	}
	return &switchRows{value: s.value}, nil
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
		if got := New(testCase.store).Active(context.Background()); got != testCase.want {
			t.Errorf("%s: MaintenanceActive = %v, want %v", name, got, testCase.want)
		}
	}
}

// TestEveryFailureModeKeepsWorking is the direction that matters, stated on
// its own because it is the opposite of the HTTP gate's.
//
// An unreadable switch must not halt every background job on the platform. That
// would be an outage this daemon caused rather than one an operator asked for,
// and it would be indistinguishable from a maintenance window nobody opened.
func TestEveryFailureModeKeepsWorking(t *testing.T) {
	t.Parallel()

	for name, store := range map[string]*switchStore{
		"query error":     {queryErr: errors.New("connection refused")},
		"wrong type":      {value: jsonValue(t, 1)},
		"corrupt json":    {value: []byte("not json")},
		"absent row":      {value: nil},
		"explicit false":  {value: jsonValue(t, false)},
		"json null value": {value: []byte("null")},
	} {
		if New(store).Active(context.Background()) {
			t.Errorf("%s: the sweepers would stop on a switch they could not trust", name)
		}
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
// A drift would be silent and total: the sweepers would read a row nobody
// writes, find nothing, and delete straight through every maintenance window
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
			t.Errorf("elitea-main does not declare %s = %q; this daemon reads a row the admin "+
				"surface does not write, so every maintenance window would be ignored here",
				constant, want)
		}
	}
}
