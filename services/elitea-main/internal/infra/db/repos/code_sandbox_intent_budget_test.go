package repos

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
)

// Per With*Intent transaction: the access lock runs once at the start and once as
// the end-of-transaction fence re-check. Each execution transfers the request bytes.
const (
	codeIntentTxAccessQueryBudget = 2
	codeIntentTxStatementBudget   = 10
)

// Counting-fake budget (the scripted executor records every statement). The
// duplicate inner loop used to make this four access queries per transaction.
func TestCodeIntentTxStatementBudget(t *testing.T) {
	f := newOriginalCodeFixture(t)
	selector, _ := code.Canonical([]any{f.intent.CompiledBindingBase64URL, f.intent.SelectedDescriptorSHA256})
	rows := append(originalCodeAccessRows(f.access), scriptedRow{values: []any{f.visitWire, []byte(f.ref.DigestSHA256)}}, scriptedRow{values: []any{f.bindingWire, selector}})
	rows = append(rows, originalCodeAccessRows(f.access)...)
	e := &scriptedExecutor{rowResults: rows, execTags: []pgconn.CommandTag{pgconn.NewCommandTag("INSERT 0 1")}}
	s := &recoveryTxStore{scriptedExecutor: e}
	repo := originalCodeRepo(t, s, f)
	if _, err := repo.RegisterOriginalCodeIntent(t.Context(), f.claim, f.intent); err != nil || !s.committed {
		t.Fatal(err, s.committed)
	}
	access := 0
	for _, call := range e.rowCalls {
		if strings.Contains(call.sql, "FROM elitea_runtime.execution_jobs j") {
			access++
		}
	}
	if access != codeIntentTxAccessQueryBudget {
		t.Fatalf("access queries = %d, want %d (start lock + end fence re-check)", access, codeIntentTxAccessQueryBudget)
	}
	if total := len(e.rowCalls) + len(e.queryCalls) + len(e.execCalls); total > codeIntentTxStatementBudget {
		t.Fatalf("statements = %d, budget %d", total, codeIntentTxStatementBudget)
	}
}
