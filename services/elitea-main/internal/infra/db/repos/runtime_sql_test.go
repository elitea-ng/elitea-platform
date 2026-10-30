package repos

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type scriptedRow struct {
	values []any
	err    error
}

func (r scriptedRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return fmt.Errorf("scan destination count %d, want %d", len(dest), len(r.values))
	}
	for i := range dest {
		if err := assignScanValue(dest[i], r.values[i]); err != nil {
			return fmt.Errorf("scan value %d: %w", i, err)
		}
	}
	return nil
}

func assignScanValue(destination, value any) error {
	dest := reflect.ValueOf(destination)
	if dest.Kind() != reflect.Pointer || dest.IsNil() {
		return errors.New("destination is not a non-nil pointer")
	}
	if value == nil {
		dest.Elem().Set(reflect.Zero(dest.Elem().Type()))
		return nil
	}
	source := reflect.ValueOf(value)
	if source.Type().AssignableTo(dest.Elem().Type()) {
		dest.Elem().Set(source)
		return nil
	}
	if source.Type().ConvertibleTo(dest.Elem().Type()) {
		dest.Elem().Set(source.Convert(dest.Elem().Type()))
		return nil
	}
	return fmt.Errorf("cannot assign %T to %T", value, destination)
}

type scriptedRows struct {
	rows  []scriptedRow
	index int
	err   error
}

func (r *scriptedRows) Close()     {}
func (r *scriptedRows) Err() error { return r.err }
func (r *scriptedRows) Next() bool { return r.index < len(r.rows) }
func (r *scriptedRows) Scan(dest ...any) error {
	if !r.Next() {
		return pgx.ErrNoRows
	}
	row := r.rows[r.index]
	r.index++
	return row.Scan(dest...)
}

type queryCall struct {
	sql  string
	args []any
}

type scriptedExecutor struct {
	rowResults  []scriptedRow
	rowCalls    []queryCall
	rowsResult  *scriptedRows
	rowsResults []*scriptedRows
	queryCalls  []queryCall
	execTags    []pgconn.CommandTag
	execErrors  []error
	execCalls   []queryCall
}

// approvalLedgerless: a scripted executor carries no approval ledger
// (ADR-0041 decision 5); the PG integration tests cover the ledger.
func (*scriptedExecutor) approvalLedgerless() {}

func (e *scriptedExecutor) QueryRow(_ context.Context, sql string, args ...any) sqlRow {
	e.rowCalls = append(e.rowCalls, queryCall{sql: sql, args: append([]any(nil), args...)})
	if len(e.rowResults) == 0 {
		return scriptedRow{err: errors.New("unexpected QueryRow")}
	}
	result := e.rowResults[0]
	e.rowResults = e.rowResults[1:]
	return result
}

func (e *scriptedExecutor) Query(_ context.Context, sql string, args ...any) (sqlRows, error) {
	e.queryCalls = append(e.queryCalls, queryCall{sql: sql, args: append([]any(nil), args...)})
	if len(e.rowsResults) > 0 {
		result := e.rowsResults[0]
		e.rowsResults = e.rowsResults[1:]
		return result, nil
	}
	if e.rowsResult == nil {
		return nil, errors.New("unexpected Query")
	}
	return e.rowsResult, nil
}

func (e *scriptedExecutor) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	e.execCalls = append(e.execCalls, queryCall{sql: sql, args: append([]any(nil), args...)})
	var tag pgconn.CommandTag
	if len(e.execTags) > 0 {
		tag = e.execTags[0]
		e.execTags = e.execTags[1:]
	}
	var err error
	if len(e.execErrors) > 0 {
		err = e.execErrors[0]
		e.execErrors = e.execErrors[1:]
	}
	return tag, err
}

func (e *scriptedExecutor) InsertCurrentIndexTerminalNotification(
	ctx context.Context,
	arg sqlcgen.InsertCurrentIndexTerminalNotificationParams,
) (int64, error) {
	tag, err := e.Exec(
		ctx,
		"sqlc:InsertCurrentIndexTerminalNotification",
		arg.NotificationUuid,
		arg.ErrorMessage,
		arg.Indexed,
		arg.Updated,
		arg.TerminalState,
		arg.CompletionStatus,
		arg.ReindexPresent,
		arg.Reindex,
		arg.ExecutionID,
		arg.Generation,
	)
	return tag.RowsAffected(), err
}

type scriptedStore struct {
	*scriptedExecutor
	txCalls int
}

func (s *scriptedStore) WithinTx(ctx context.Context, _ pgx.TxOptions, fn func(sqlExecutor) error) error {
	s.txCalls++
	return fn(s.scriptedExecutor)
}

type scriptedProjectStore struct {
	projectID int64
	*scriptedExecutor
}

func (s *scriptedProjectStore) WithinProjectTx(ctx context.Context, projectID int64, _ pgx.TxOptions, fn func(sqlExecutor) error) error {
	s.projectID = projectID
	return fn(s.scriptedExecutor)
}
