package requestlog

// The execution id on the write path: read off the transport by the middleware,
// bound as NULL-or-value by the store.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

type capturingExecer struct {
	sql  string
	args []any
}

func (c *capturingExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c.sql, c.args = sql, args
	return pgconn.CommandTag{}, nil
}

// TestWriteBatch_BindsTheExecutionID checks the column list and the argument
// order together.
//
// They are checked together on purpose: a column added to the list without a
// matching argument does not fail loudly, it SHIFTS every later binding by one
// — which for this table means a status landing in duration_ms and every
// latency reading as nonsense. columnsPerRow is the invariant that stops it and
// this is the test that holds it.
func TestWriteBatch_BindsTheExecutionID(t *testing.T) {
	db := &capturingExecer{}
	store := NewStore(db)

	if err := store.WriteBatch(context.Background(), []Record{{
		ProjectID: "42", UserID: "7", Route: "/llm/v1/chat/completions",
		Method: "POST", Status: 200, ExecutionID: "exec-9",
	}}); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(db.sql, "execution_id") {
		t.Fatalf("the insert names no execution_id column: %s", db.sql)
	}
	if len(db.args) != columnsPerRow {
		t.Fatalf("bound %d args for %d columns; a mismatch shifts every later column", len(db.args), columnsPerRow)
	}
	// The position the column list gives it.
	if got := db.args[executionIDColumn]; got != "exec-9" {
		t.Fatalf("execution_id bound as %v, want exec-9", got)
	}
}

// TestWriteBatch_AbsentExecutionIDIsNULL.
//
// NOT the empty string. The agent breakdown GROUPs on this column, so ” would
// collapse into a single nameless agent carrying every request in the project
// that was not made from an execution — which is most of them. NULL is filtered
// out instead, which is the only reading that is true.
func TestWriteBatch_AbsentExecutionIDIsNULL(t *testing.T) {
	db := &capturingExecer{}
	store := NewStore(db)

	if err := store.WriteBatch(context.Background(), []Record{{
		ProjectID: "42", Route: "/llm/v1/chat/completions", Method: "POST", Status: 200,
	}}); err != nil {
		t.Fatal(err)
	}
	if got := db.args[executionIDColumn]; got != nil {
		t.Fatalf("an absent execution id bound as %#v, want SQL NULL", got)
	}
}

// TestMiddleware_RecordsTheExecutionIDFromTheTransport.
//
// It is read by the MIDDLEWARE rather than enriched by a handler, so a request
// that 404s or is refused before dispatch still carries it. Those are exactly
// the agent runs an operator goes looking for when a run produced nothing.
func TestMiddleware_RecordsTheExecutionIDFromTheTransport(t *testing.T) {
	sink := &captureSink{}
	recorder := New(sink, nil)
	t.Cleanup(func() { recorder.Stop(context.Background()) })

	handler := Middleware(recorder)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	request := httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", nil)
	request.Header.Set(headerProjectID, "42")
	request.Header.Set(headerExecutionID, "exec-9")
	handler.ServeHTTP(httptest.NewRecorder(), request)

	recorder.Stop(context.Background())

	records := sink.records()
	if len(records) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(records))
	}
	if got := records[0].ExecutionID; got != "exec-9" {
		t.Fatalf("ExecutionID = %q, want exec-9 (a refused agent run must still be attributable)", got)
	}
	if records[0].Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", records[0].Status)
	}
}
