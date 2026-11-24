package repos

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/pipelinelimits"
)

type stubRow struct {
	enabled bool
	err     error
}

func (r stubRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if ptr, ok := dest[0].(*bool); ok {
		*ptr = r.enabled
	}
	return nil
}

type projectRuleKey struct {
	projectID int64
	name      string
}

type stubHTTPRuleQuerier struct {
	rules   map[projectRuleKey]stubRow
	queries int
}

func (q *stubHTTPRuleQuerier) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	q.queries++
	projectID, _ := args[0].(int64)
	name, _ := args[1].(string)
	if row, ok := q.rules[projectRuleKey{projectID: projectID, name: name}]; ok {
		return row
	}
	return stubRow{err: pgx.ErrNoRows}
}

func TestCheckHTTPActionOperations(t *testing.T) {
	const httpPipeline = `entry_point: fetch
nodes:
  - id: fetch
    type: http
    revision: 2
    operation: billing.list_invoices
    request: {}
    response:
      mode: json
    output: [invoices]
    transition: END
`
	t.Run("non-http pipeline makes zero queries", func(t *testing.T) {
		q := &stubHTTPRuleQuerier{}
		if err := CheckHTTPActionOperations(t.Context(), q, 42, "nodes:\n  - id: s\n    type: state_modifier\n"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if q.queries != 0 {
			t.Fatalf("queries = %d, want 0", q.queries)
		}
	})

	t.Run("enabled rule is admitted", func(t *testing.T) {
		q := &stubHTTPRuleQuerier{rules: map[projectRuleKey]stubRow{{projectID: 42, name: "billing.list_invoices"}: {enabled: true}}}
		if err := CheckHTTPActionOperations(t.Context(), q, 42, httpPipeline); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if q.queries != 1 {
			t.Fatalf("queries = %d, want 1", q.queries)
		}
	})

	t.Run("rule in another project is refused as unknown", func(t *testing.T) {
		q := &stubHTTPRuleQuerier{rules: map[projectRuleKey]stubRow{{projectID: 99, name: "billing.list_invoices"}: {enabled: true}}}
		err := CheckHTTPActionOperations(t.Context(), q, 42, httpPipeline)
		refusal := pipelinelimits.Refusal(err)
		if refusal == nil || refusal.Code != pipelinelimits.CodeHTTPNodeInvalid {
			t.Fatalf("got %v, want %s", err, pipelinelimits.CodeHTTPNodeInvalid)
		}
		if !strings.Contains(refusal.Message, `"fetch"`) || !strings.Contains(refusal.Message, "does not exist") {
			t.Fatalf("unexpected message: %s", refusal.Message)
		}
	})

	t.Run("unknown rule is refused with readable error", func(t *testing.T) {
		q := &stubHTTPRuleQuerier{rules: map[projectRuleKey]stubRow{}}
		err := CheckHTTPActionOperations(t.Context(), q, 42, httpPipeline)
		refusal := pipelinelimits.Refusal(err)
		if refusal == nil || refusal.Code != pipelinelimits.CodeHTTPNodeInvalid {
			t.Fatalf("got %v, want %s", err, pipelinelimits.CodeHTTPNodeInvalid)
		}
		if !strings.Contains(refusal.Message, `"fetch"`) || !strings.Contains(refusal.Message, `"billing.list_invoices"`) || !strings.Contains(refusal.Message, "does not exist") {
			t.Fatalf("unexpected message: %s", refusal.Message)
		}
	})

	t.Run("disabled rule is refused with readable error", func(t *testing.T) {
		q := &stubHTTPRuleQuerier{rules: map[projectRuleKey]stubRow{{projectID: 42, name: "billing.list_invoices"}: {enabled: false}}}
		err := CheckHTTPActionOperations(t.Context(), q, 42, httpPipeline)
		refusal := pipelinelimits.Refusal(err)
		if refusal == nil || refusal.Code != pipelinelimits.CodeHTTPNodeInvalid {
			t.Fatalf("got %v, want %s", err, pipelinelimits.CodeHTTPNodeInvalid)
		}
		if !strings.Contains(refusal.Message, `"fetch"`) || !strings.Contains(refusal.Message, `"billing.list_invoices"`) || !strings.Contains(refusal.Message, "disabled") {
			t.Fatalf("unexpected message: %s", refusal.Message)
		}
	})

	t.Run("database errors surface without masking", func(t *testing.T) {
		boom := errors.New("connection lost")
		q := &stubHTTPRuleQuerier{rules: map[projectRuleKey]stubRow{{projectID: 42, name: "billing.list_invoices"}: {err: boom}}}
		err := CheckHTTPActionOperations(t.Context(), q, 42, httpPipeline)
		if !errors.Is(err, boom) {
			t.Fatalf("got %v, want wrapped boom", err)
		}
	})
}
