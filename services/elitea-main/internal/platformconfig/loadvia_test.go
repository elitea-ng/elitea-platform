package platformconfig

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentlifecycle"
)

// fakeRows is a query result of (key, raw JSON value) pairs.
type fakeRows struct {
	rows   [][2]string
	next   int
	closed bool
}

func (r *fakeRows) Close()     { r.closed = true }
func (r *fakeRows) Err() error { return nil }
func (r *fakeRows) Next() bool {
	r.next++
	return r.next <= len(r.rows)
}

func (r *fakeRows) Scan(dest ...any) error {
	row := r.rows[r.next-1]
	*dest[0].(*string) = row[0]
	*dest[1].(*[]byte) = []byte(row[1])
	return nil
}

func TestLoadViaReadsOneSectionThroughTheCallersQuery(t *testing.T) {
	rows := &fakeRows{rows: [][2]string{
		{KeyApprovalsUnattendedDefault, `"skip"`},
		{KeyApprovalsUnattendedWaitHours, `6`},
	}}
	var gotSection any
	values, err := LoadVia(t.Context(), func(_ context.Context, _ string, args ...any) (Rows, error) {
		gotSection = args[0]
		return rows, nil
	}, SectionApprovals)
	if err != nil {
		t.Fatal(err)
	}
	if gotSection != SectionApprovals || !rows.closed {
		t.Fatalf("section = %v, closed = %v", gotSection, rows.closed)
	}
	settings := ApprovalsSettingsFromValues(values)
	if settings.UnattendedDefault != OnApprovalRequiredSkip || settings.UnattendedWait.Hours() != 6 {
		t.Fatalf("settings = %+v", settings)
	}

	refused := errors.New("connection lost")
	if _, err := LoadVia(t.Context(), func(context.Context, string, ...any) (Rows, error) {
		return nil, refused
	}, SectionApprovals); !errors.Is(err, refused) {
		t.Fatalf("query error = %v", err)
	}
}

// boolRows is the one-row, one-column answer of the to_regclass probe.
type boolRows struct {
	value bool
	read  bool
}

func (r *boolRows) Close()     {}
func (r *boolRows) Err() error { return nil }
func (r *boolRows) Next() bool {
	if r.read {
		return false
	}
	r.read = true
	return true
}

func (r *boolRows) Scan(dest ...any) error {
	*dest[0].(*bool) = r.value
	return nil
}

// TestLoadAgentLifecyclePolicyViaReadsOnTheCallersQuery: every read of the
// tier table goes through the query it is handed (the lifecycle store's
// transaction, never a second pool connection), and a database without the
// table answers the defaults without the query that would fail and abort
// that transaction.
func TestLoadAgentLifecyclePolicyViaReadsOnTheCallersQuery(t *testing.T) {
	var statements []string
	query := func(exists bool, section Rows) QueryFunc {
		return func(_ context.Context, sql string, _ ...any) (Rows, error) {
			statements = append(statements, sql)
			if len(statements) == 1 {
				return &boolRows{value: exists}, nil
			}
			return section, nil
		}
	}

	section := &fakeRows{rows: [][2]string{{"low_approvals", `1`}}}
	policy, err := LoadAgentLifecyclePolicyVia(t.Context(), query(true, section))
	if err != nil {
		t.Fatal(err)
	}
	if len(statements) != 2 || !section.closed {
		t.Fatalf("statements = %q, closed = %v", statements, section.closed)
	}
	if got := policy.Requirement(agentlifecycle.TierLow).Approvals; got != 1 {
		t.Fatalf("low approvals = %d, want the saved 1", got)
	}

	statements = nil
	policy, err = LoadAgentLifecyclePolicyVia(t.Context(), query(false, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(statements) != 1 {
		t.Fatalf("a missing table ran %d statements, want only the probe: %q", len(statements), statements)
	}
	if got, want := policy.Requirement(agentlifecycle.TierCritical).Approvals,
		agentlifecycle.DefaultPolicy().Tiers[agentlifecycle.TierCritical].Approvals; got != want {
		t.Fatalf("critical approvals = %d, want the default %d", got, want)
	}

	refused := errors.New("connection lost")
	if _, err := LoadAgentLifecyclePolicyVia(t.Context(), func(context.Context, string, ...any) (Rows, error) {
		return nil, refused
	}); !errors.Is(err, refused) {
		t.Fatalf("probe error = %v", err)
	}
}

func TestUndefinedTableIsSQLState42P01Only(t *testing.T) {
	if !UndefinedTable(fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "42P01"})) {
		t.Fatal("42P01 is an undefined table")
	}
	for _, err := range []error{nil, errors.New("42P01"), &pgconn.PgError{Code: "3F000"}} {
		if UndefinedTable(err) {
			t.Fatalf("%v read as an undefined table", err)
		}
	}
}
