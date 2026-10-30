package requestlog

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/guardrail"
)

type recordingExecer struct {
	statements []string
	args       [][]any
	fail       map[string]error
}

func (r *recordingExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.statements = append(r.statements, sql)
	r.args = append(r.args, args)
	for prefix, err := range r.fail {
		if strings.Contains(sql, prefix) {
			return pgconn.CommandTag{}, err
		}
	}
	return pgconn.CommandTag{}, nil
}

func TestWriteBatchWritesGuardrailVerdictsAfterTheLogRows(t *testing.T) {
	db := &recordingExecer{}
	store := NewStore(db)
	err := store.WriteBatch(context.Background(), []Record{{
		ProjectID: "42", ExecutionID: "exec-1", Route: "/llm/v1/chat/completions",
		Guardrails: []guardrail.Verdict{{Stage: "output", Row: "r", Detector: "secrets", DetectorType: "secrets",
			Action: "mask", Outcome: "masked", FailMode: "closed", Labels: []string{"github-token"}, Findings: 1, MaxScore: 1, Runs: 2}},
	}, {ProjectID: "42"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(db.statements) != 2 || !strings.Contains(db.statements[1], "gateway.guardrail_verdicts") {
		t.Fatalf("statements = %v", db.statements)
	}
	args := db.args[1]
	if len(args) != verdictColumnsPerRow {
		t.Fatalf("bound %d args for %d columns", len(args), verdictColumnsPerRow)
	}
	if args[3] != "exec-1" || args[6] != "r" || args[10] != "masked" {
		t.Fatalf("args = %v", args)
	}
}

func TestWriteBatchWithoutVerdictsWritesNoVerdictStatement(t *testing.T) {
	db := &recordingExecer{}
	if err := NewStore(db).WriteBatch(context.Background(), []Record{{ProjectID: "1"}}); err != nil {
		t.Fatal(err)
	}
	if len(db.statements) != 1 {
		t.Fatalf("statements = %v", db.statements)
	}
}

func TestMissingVerdictTableLatchesAndKeepsTheLogRows(t *testing.T) {
	db := &recordingExecer{fail: map[string]error{"guardrail_verdicts": &pgconn.PgError{Code: "42P01"}}}
	store := NewStore(db)
	rec := []Record{{ProjectID: "1", Guardrails: []guardrail.Verdict{{Outcome: "pass"}}}}
	if err := store.WriteBatch(context.Background(), rec); err != nil {
		t.Fatalf("a missing verdict table failed the batch: %v", err)
	}
	if err := store.WriteBatch(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if len(db.statements) != 3 {
		t.Fatalf("the latch did not skip the second verdict insert: %v", db.statements)
	}
}

// pruneExecer reports a DELETE of `backlog` rows, at most the bound ($2) per
// statement, for each table.
type pruneExecer struct {
	backlog    map[string]int64
	statements map[string]int
	now        *time.Time
	step       time.Duration
}

func (p *pruneExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	table := "gateway.llm_request_logs"
	if strings.Contains(sql, "guardrail_verdicts") {
		table = "gateway.guardrail_verdicts"
	}
	p.statements[table]++
	if p.now != nil {
		*p.now = p.now.Add(p.step)
	}
	limit, _ := args[1].(int)
	n := min(p.backlog[table], int64(limit))
	p.backlog[table] -= n
	return pgconn.NewCommandTag("DELETE " + strconv.FormatInt(n, 10)), nil
}

// TestPruneDeletesEveryExpiredVerdictInBatches is the regression for a prune
// that removed at most 10000 verdicts per hour. A request writes several
// verdicts, so the table outgrew its retention window. The prune now runs
// bounded batches until nothing expired is left.
func TestPruneDeletesEveryExpiredVerdictInBatches(t *testing.T) {
	db := &pruneExecer{
		backlog:    map[string]int64{"gateway.llm_request_logs": 25_000, "gateway.guardrail_verdicts": 125_000},
		statements: map[string]int{},
	}
	removed, err := NewStore(db).Prune(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 150_000 {
		t.Fatalf("removed = %d, want 150000", removed)
	}
	if db.backlog["gateway.guardrail_verdicts"] != 0 || db.backlog["gateway.llm_request_logs"] != 0 {
		t.Fatalf("expired rows remain: %v", db.backlog)
	}
	// 125000 rows in batches of 10000: 12 full batches and one short one.
	if got := db.statements["gateway.guardrail_verdicts"]; got != 13 {
		t.Fatalf("verdict DELETE statements = %d, want 13", got)
	}
}

// TestPruneStopsAtItsCaps keeps one prune bounded: at most pruneMaxBatches
// statements, and no new statement after pruneMaxDuration.
func TestPruneStopsAtItsCaps(t *testing.T) {
	db := &pruneExecer{
		backlog:    map[string]int64{"gateway.guardrail_verdicts": 100 * pruneMaxBatches * pruneBatchRows},
		statements: map[string]int{},
	}
	store := NewStore(db)
	removed, err := store.pruneVerdicts(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if db.statements["gateway.guardrail_verdicts"] != pruneMaxBatches || removed != pruneMaxBatches*pruneBatchRows {
		t.Fatalf("statements = %d, removed = %d", db.statements["gateway.guardrail_verdicts"], removed)
	}

	clock := time.Unix(0, 0)
	timed := &pruneExecer{
		backlog:    map[string]int64{"gateway.guardrail_verdicts": 100 * pruneMaxBatches * pruneBatchRows},
		statements: map[string]int{},
		now:        &clock,
		step:       pruneMaxDuration / 4,
	}
	store = NewStore(timed)
	store.now = func() time.Time { return clock }
	if _, err := store.pruneVerdicts(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := timed.statements["gateway.guardrail_verdicts"]; got != 4 {
		t.Fatalf("statements after the time cap = %d, want 4", got)
	}
}

// TestVerdictWriteFailureDoesNotFailTheLogBatch is the regression for a
// WriteBatch that returned the verdict error after the request log rows were
// committed: the Recorder then counted the whole batch as failed. The batch
// now succeeds, and the dropped verdicts have their own counter.
func TestVerdictWriteFailureDoesNotFailTheLogBatch(t *testing.T) {
	db := &recordingExecer{fail: map[string]error{"guardrail_verdicts": errors.New("connection reset")}}
	before := VerdictWriteFailures()
	rec := []Record{{ProjectID: "1", Guardrails: []guardrail.Verdict{{Outcome: "pass"}, {Outcome: "masked"}}}}
	if err := NewStore(db).WriteBatch(context.Background(), rec); err != nil {
		t.Fatalf("a verdict write failure failed the request log batch: %v", err)
	}
	if got := VerdictWriteFailures() - before; got != 2 {
		t.Fatalf("verdict write failures = %d, want 2", got)
	}

	recorder := New(NewStore(db), nil)
	recorder.Record(rec[0])
	stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	recorder.Stop(stop)
	if recorder.Written() != 1 || recorder.Failed() != 0 {
		t.Fatalf("written = %d, failed = %d; the log row was written", recorder.Written(), recorder.Failed())
	}
}
