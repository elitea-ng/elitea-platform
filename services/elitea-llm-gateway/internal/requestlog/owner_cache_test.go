package requestlog

// The credential owner and the cache counts on the write path (shared
// migration 0139, legacy issue 6709), and the fallback for a database that has
// not run 0139 yet.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The column positions the tests read, derived from the column list itself so
// a reordered list fails here rather than binding into the wrong column.
var (
	executionIDColumn     = columnIndex(insertColumns, "execution_id")
	credentialOwnerColumn = columnIndex(insertColumns, "credential_owner")
	cacheReadColumn       = columnIndex(insertColumns, "cache_read_tokens")
	cacheWriteColumn      = columnIndex(insertColumns, "cache_write_tokens")
)

func columnIndex(list, name string) int {
	for index, column := range splitColumns(list) {
		if column == name {
			return index
		}
	}
	return -1
}

func splitColumns(list string) []string {
	trimmed := strings.Trim(strings.TrimSpace(list), "()")
	parts := strings.Split(trimmed, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		out = append(out, strings.TrimSpace(part))
	}
	return out
}

func TestColumnCountsMatchTheColumnLists(t *testing.T) {
	if got := len(splitColumns(insertColumns)); got != columnsPerRow {
		t.Fatalf("insertColumns names %d columns, columnsPerRow is %d", got, columnsPerRow)
	}
	if got := len(splitColumns(legacyInsertColumns)); got != legacyColumnsPerRow {
		t.Fatalf("legacyInsertColumns names %d columns, legacyColumnsPerRow is %d", got, legacyColumnsPerRow)
	}
	// The legacy list is the full list minus the 0139 columns, in the same
	// order, so a row binds the same values into the same leading columns.
	full := splitColumns(insertColumns)
	for index, column := range splitColumns(legacyInsertColumns) {
		if full[index] != column {
			t.Fatalf("column %d: legacy %q, full %q", index, column, full[index])
		}
	}
}

func TestWriteBatch_BindsTheCredentialOwnerAndCacheTokens(t *testing.T) {
	db := &capturingExecer{}
	store := NewStore(db)

	if err := store.WriteBatch(context.Background(), []Record{{
		ProjectID: "42", Route: "/llm/v1/chat/completions", Method: "POST", Status: 200,
		PromptToks: 1200, CredentialOwner: CredentialOwnerPlatform,
		CacheReadToks: 1000, CacheWriteToks: 150,
	}}); err != nil {
		t.Fatal(err)
	}

	if len(db.args) != columnsPerRow {
		t.Fatalf("bound %d args for %d columns", len(db.args), columnsPerRow)
	}
	if got := db.args[credentialOwnerColumn]; got != CredentialOwnerPlatform {
		t.Errorf("credential_owner bound as %v, want %q", got, CredentialOwnerPlatform)
	}
	if got := db.args[cacheReadColumn]; got != int64(1000) {
		t.Errorf("cache_read_tokens bound as %v, want 1000", got)
	}
	if got := db.args[cacheWriteColumn]; got != int64(150) {
		t.Errorf("cache_write_tokens bound as %v, want 150", got)
	}
}

func TestWriteBatch_ClampsANegativeCacheCount(t *testing.T) {
	db := &capturingExecer{}
	if err := NewStore(db).WriteBatch(context.Background(), []Record{{CacheReadToks: -5, CacheWriteToks: -1}}); err != nil {
		t.Fatal(err)
	}
	if db.args[cacheReadColumn] != int64(0) || db.args[cacheWriteColumn] != int64(0) {
		t.Errorf("negative cache counts bound as (%v, %v), want (0, 0)", db.args[cacheReadColumn], db.args[cacheWriteColumn])
	}
}

// schemaBehindExecer refuses any statement that names a 0139 column, the way a
// database that has not run 0139 does.
type schemaBehindExecer struct {
	statements []string
	argCounts  []int
	migrated   bool
}

func (e *schemaBehindExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	e.statements = append(e.statements, sql)
	e.argCounts = append(e.argCounts, len(args))
	if !e.migrated && strings.Contains(sql, "credential_owner") {
		return pgconn.CommandTag{}, &pgconn.PgError{Code: "42703", Message: `column "credential_owner" does not exist`}
	}
	return pgconn.CommandTag{}, nil
}

// A gateway that rolls out before elitea-migrate must keep logging. Without the
// fallback every batch fails on 42703, and the log loses ALL traffic.
func TestWriteBatch_FallsBackToTheLegacyColumnsOnUndefinedColumn(t *testing.T) {
	db := &schemaBehindExecer{}
	store := NewStore(db)
	now := time.Unix(1_700_000_000, 0)
	store.now = func() time.Time { return now }

	records := []Record{{ProjectID: "42", Route: "/llm/v1/chat/completions", CredentialOwner: CredentialOwnerProject}}
	if err := store.WriteBatch(context.Background(), records); err != nil {
		t.Fatalf("WriteBatch on a pre-0139 database: %v", err)
	}
	if len(db.statements) != 2 {
		t.Fatalf("statements = %d, want 2 (refused full list, then legacy list)", len(db.statements))
	}
	if strings.Contains(db.statements[1], "credential_owner") || db.argCounts[1] != legacyColumnsPerRow {
		t.Fatalf("retry used %d args and %q, want the %d-column legacy list", db.argCounts[1], db.statements[1], legacyColumnsPerRow)
	}

	// The latch holds: the next batch goes straight to the legacy list.
	if err := store.WriteBatch(context.Background(), records); err != nil {
		t.Fatal(err)
	}
	if len(db.statements) != 3 || strings.Contains(db.statements[2], "credential_owner") {
		t.Fatalf("a latched store re-tried the full list: %v", db.statements)
	}

	// The latch expires, and a migrated database gets the full list again.
	db.migrated = true
	now = now.Add(legacyColumnsRetry + time.Second)
	if err := store.WriteBatch(context.Background(), records); err != nil {
		t.Fatal(err)
	}
	if last := db.statements[len(db.statements)-1]; !strings.Contains(last, "credential_owner") {
		t.Fatalf("after the latch expired the store still wrote the legacy list: %s", last)
	}
}

// Any other failure is returned as it was, and does not latch.
func TestWriteBatch_OtherErrorsDoNotLatch(t *testing.T) {
	failing := &failingExecer{err: errors.New("connection refused")}
	store := NewStore(failing)
	if err := store.WriteBatch(context.Background(), []Record{{}}); err == nil {
		t.Fatal("WriteBatch swallowed a write failure")
	}
	if store.legacyColumns() {
		t.Fatal("a non-schema error latched the legacy column list")
	}
	if failing.calls != 1 {
		t.Fatalf("calls = %d, want 1: a non-schema failure is not retried", failing.calls)
	}
}

type failingExecer struct {
	err   error
	calls int
}

func (f *failingExecer) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	f.calls++
	return pgconn.CommandTag{}, f.err
}

func TestMiddleware_RecordsTheEnrichedOwnerAndCacheTokens(t *testing.T) {
	sink := &captureSink{}
	recorder := New(sink, nil)
	t.Cleanup(func() { recorder.Stop(context.Background()) })

	handler := Middleware(recorder)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enrichment := FromContext(r.Context())
		enrichment.SetCredentialOwner(CredentialOwnerProject)
		// A later call that knows nothing must not erase the owner.
		enrichment.SetCredentialOwner("")
		enrichment.SetCacheTokens(300, 20)
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/llm/v1/chat/completions", nil))
	recorder.Stop(context.Background())

	records := sink.records()
	if len(records) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(records))
	}
	got := records[0]
	if got.CredentialOwner != CredentialOwnerProject {
		t.Errorf("CredentialOwner = %q, want %q", got.CredentialOwner, CredentialOwnerProject)
	}
	if got.CacheReadToks != 300 || got.CacheWriteToks != 20 {
		t.Errorf("cache tokens = (%d, %d), want (300, 20)", got.CacheReadToks, got.CacheWriteToks)
	}
}

// The analytics "Last 90d" preset reads this table, so the log must keep at
// least 90 days (legacy issue 6879). A shorter window shows one month under a
// three-month label.
func TestRetentionCoversTheNinetyDayAnalyticsPreset(t *testing.T) {
	if RetentionWindow < 90*24*time.Hour {
		t.Fatalf("RetentionWindow = %s, want at least 90 days", RetentionWindow)
	}
}

// The values are the ones shared migration 0139 documents on the column.
func TestCredentialOwnerValuesFitTheColumn(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z]{1,16}$`)
	for _, owner := range []string{CredentialOwnerProject, CredentialOwnerPlatform} {
		if !valid.MatchString(owner) {
			t.Errorf("owner %q does not fit credential_owner VARCHAR(16)", owner)
		}
	}
}
