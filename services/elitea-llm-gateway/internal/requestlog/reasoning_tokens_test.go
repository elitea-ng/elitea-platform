package requestlog

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The reasoning count (ADR-0038 decision 5, shared migration 0225).

var reasoningTokensColumn = columnIndex(insertColumns, "reasoning_tokens")

// missingColumnExecer refuses every statement that names column with the
// 42703 Postgres raises, its message naming the column as Postgres does.
type missingColumnExecer struct {
	column     string
	statements []string
	args       [][]any
}

func (e *missingColumnExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	e.statements = append(e.statements, sql)
	e.args = append(e.args, args)
	if strings.Contains(sql, e.column) {
		return pgconn.CommandTag{}, &pgconn.PgError{
			Code:    "42703",
			Message: `column "` + e.column + `" of relation "llm_request_logs" does not exist`,
		}
	}
	return pgconn.CommandTag{}, nil
}

func TestWriteBatch_BindsTheReasoningTokens(t *testing.T) {
	db := &capturingExecer{}
	if err := NewStore(db).WriteBatch(context.Background(), []Record{{
		Route: "/llm/v1/chat/completions", Method: "POST", Status: 200,
		OutputToks: 900, ReasoningToks: 700,
	}}); err != nil {
		t.Fatal(err)
	}
	if len(db.args) != columnsPerRow {
		t.Fatalf("bound %d args for %d columns", len(db.args), columnsPerRow)
	}
	if got := db.args[reasoningTokensColumn]; got != int64(700) {
		t.Fatalf("reasoning_tokens bound as %v, want 700", got)
	}
}

func TestWriteBatch_ClampsANegativeReasoningCount(t *testing.T) {
	db := &capturingExecer{}
	if err := NewStore(db).WriteBatch(context.Background(), []Record{{ReasoningToks: -3}}); err != nil {
		t.Fatal(err)
	}
	if got := db.args[reasoningTokensColumn]; got != int64(0) {
		t.Fatalf("a negative reasoning count bound as %v, want 0", got)
	}
}

// A database before 0225 keeps every row and its 0139 columns: only the
// reasoning count is lost, and only until the latch expires.
func TestWriteBatch_FallsBackWhen0225IsAbsent(t *testing.T) {
	db := &missingColumnExecer{column: "reasoning_tokens"}
	store := NewStore(db)
	clock := time.Unix(1_700_000_000, 0)
	store.now = func() time.Time { return clock }

	if err := store.WriteBatch(context.Background(), []Record{{CacheReadToks: 5, ReasoningToks: 9}}); err != nil {
		t.Fatalf("a missing 0225 must not lose the batch: %v", err)
	}
	if len(db.statements) != 2 {
		t.Fatalf("statements = %d, want the refused one and one retry", len(db.statements))
	}
	retry := db.statements[1]
	if strings.Contains(retry, "reasoning_tokens") || !strings.Contains(retry, "cache_read_tokens") {
		t.Fatalf("the retry must keep the 0139 columns and drop only reasoning_tokens: %s", retry)
	}
	if len(db.args[1]) != pre0225ColumnsPerRow {
		t.Fatalf("the retry bound %d args, want %d", len(db.args[1]), pre0225ColumnsPerRow)
	}
	if store.legacyColumns() {
		t.Fatal("a missing 0225 column must not latch the pre-0139 list")
	}

	// Latched: the next batch does not name the column at all.
	if err := store.WriteBatch(context.Background(), []Record{{}}); err != nil {
		t.Fatal(err)
	}
	if len(db.statements) != 3 || strings.Contains(db.statements[2], "reasoning_tokens") {
		t.Fatalf("a latched store must skip the column: %q", db.statements[2:])
	}

	// Expired: the column is tried again.
	clock = clock.Add(legacyColumnsRetry + time.Second)
	_ = store.WriteBatch(context.Background(), []Record{{}})
	if !strings.Contains(db.statements[3], "reasoning_tokens") {
		t.Fatal("an expired latch must try the column again")
	}
}

// A refusal of a 0139 column is not taken for a missing 0225: the pre-0139
// latch owns it.
func TestWriteBatch_A0139RefusalKeepsItsOwnLatch(t *testing.T) {
	db := &missingColumnExecer{column: "credential_owner"}
	store := NewStore(db)
	if err := store.WriteBatch(context.Background(), []Record{{}}); err != nil {
		t.Fatal(err)
	}
	if !store.legacyColumns() || store.latched(&store.reasoningUntil) {
		t.Fatal("a missing 0139 column must latch the pre-0139 list and only it")
	}
}

func TestWriteBatch_AMeteredBatchCarriesTheReasoningCount(t *testing.T) {
	db := &meteredExecer{}
	if err := NewStore(db).WriteBatch(context.Background(), []Record{
		{Provider: "openai", Model: "o3", ReasoningToks: 40},
		{Provider: "tavily", Model: "web_search", Metered: &MeteredFields{Units: 1, Unit: "credit"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := db.args[0][reasoningTokensColumn]; got != int64(40) {
		t.Fatalf("the model row's reasoning_tokens bound as %v, want 40", got)
	}
}
