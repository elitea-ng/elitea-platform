package platformconfig

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
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
