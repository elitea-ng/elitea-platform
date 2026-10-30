package requestlog

import (
	"context"
	"strings"
	"testing"

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
