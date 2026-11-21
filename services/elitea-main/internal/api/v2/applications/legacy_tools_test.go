package applications

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// fakeLegacyReader answers the to_regclass probe with table (nil: absent) or
// probeErr, and the read with rows or queryErr.
type fakeLegacyReader struct {
	table    *string
	probeErr error
	rows     [][]any
	queryErr error
	queried  bool
}

func (f *fakeLegacyReader) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if !strings.Contains(sql, "to_regclass") {
		panic("unexpected QueryRow: " + sql)
	}
	return fakeRow{value: f.table, err: f.probeErr}
}

func (f *fakeLegacyReader) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	f.queried = true
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return &fakeRows{rows: f.rows, at: -1}, nil
}

type fakeRow struct {
	value *string
	err   error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(**string)) = r.value
	return nil
}

// fakeRows implements the pgx.Rows methods legacyApplicationTools calls; the
// embedded interface panics on any other.
type fakeRows struct {
	pgx.Rows
	rows [][]any
	at   int
}

func (r *fakeRows) Next() bool { r.at++; return r.at < len(r.rows) }
func (r *fakeRows) Close()     {}
func (r *fakeRows) Err() error { return nil }
func (r *fakeRows) Scan(dest ...any) error {
	row := r.rows[r.at]
	*(dest[0].(*int)) = row[0].(int)
	*(dest[1].(*string)) = row[1].(string)
	*(dest[2].(*string)) = row[2].(string)
	*(dest[3].(*string)) = row[3].(string)
	return nil
}

func TestLegacyApplicationToolsSkipsATenantWithoutTheTable(t *testing.T) {
	reader := &fakeLegacyReader{}
	tools, err := legacyApplicationTools(context.Background(), reader, `"p_1"`, "7", nil)
	if err != nil || tools != nil {
		t.Fatalf("absent table: got %v, %v; want no rows and no error", tools, err)
	}
	if reader.queried {
		t.Fatal("absent table: the relation was queried; a fresh tenant must not be")
	}
}

func TestLegacyApplicationToolsFailedReadIsAnError(t *testing.T) {
	table := `"p_1".application_tools`
	reader := &fakeLegacyReader{table: &table, queryErr: errors.New("permission denied")}
	if _, err := legacyApplicationTools(context.Background(), reader, `"p_1"`, "7", nil); err == nil {
		t.Fatal("a failed read on a legacy tenant returned no error; it would shorten the tools list silently")
	}
	probe := &fakeLegacyReader{probeErr: errors.New("connection reset")}
	if _, err := legacyApplicationTools(context.Background(), probe, `"p_1"`, "7", nil); err == nil {
		t.Fatal("a failed probe returned no error")
	}
}

func TestLegacyApplicationToolsReadsAndStampsTheRows(t *testing.T) {
	table := `"p_1".application_tools`
	reader := &fakeLegacyReader{table: &table, rows: [][]any{{3, "child", "application", `{"application_id": 9}`}}}
	tools, err := legacyApplicationTools(context.Background(), reader, `"p_1"`, "7", map[string]any{"author_id": "5", "project_id": 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}
	tool := tools[0]
	settings, _ := tool["settings"].(map[string]any)
	if tool["id"] != 3 || tool["name"] != "child" || tool["type"] != "application" || tool["author_id"] != "5" || tool["project_id"] != 1 || settings["application_id"] != float64(9) {
		t.Fatalf("tool = %#v", tool)
	}
}
