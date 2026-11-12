package repos

// Issue 1947: one rule for execution_jobs.idle_compaction. Idle compaction
// admission, the breaker and the run_finished read name the column
// unguarded, so the analytics read does not probe for it either.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// recordingProbe answers every probe true and records its SQL.
type recordingProbe struct {
	queries *[]string
}

func (r recordingProbe) Query(context.Context, string, ...any) (pgx.Rows, error) {
	panic("readOriginSources must not call Query")
}

func (r recordingProbe) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	*r.queries = append(*r.queries, sql+fmt.Sprint(args...))
	return presentRow{}
}

type presentRow struct{}

func (presentRow) Scan(dest ...any) error {
	if p, ok := dest[0].(*bool); ok {
		*p = true
	}
	return nil
}

func TestReadOriginSourcesDoesNotProbeForTheIdleMarkerColumn(t *testing.T) {
	t.Parallel()

	var queries []string
	sources, err := readOriginSources(context.Background(), recordingProbe{queries: &queries})
	if err != nil {
		t.Fatalf("readOriginSources: %v", err)
	}
	if !sources.origins || !sources.idle {
		t.Fatalf("sources = %+v, want origins and idle", sources)
	}
	for _, q := range queries {
		if strings.Contains(q, "execution_jobs idle_compaction") {
			t.Fatalf("a probe for the idle_compaction column ran: %s", q)
		}
	}
	// The column is named directly, as the other paths name it.
	if got := idleRunJob(sources, "j"); !strings.Contains(got, "j.idle_compaction") {
		t.Fatalf("idleRunJob does not read the marker column: %s", got)
	}
}
