package repos

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

type startTxFake struct{ pgx.Tx }

type poolFake struct{ rowsQuerier }

// A local-turn start's recall reads on the start's own transaction, never a
// second pool connection (issue 1888): N concurrent starts on a pool of N
// would otherwise each hold one connection and wait for another.
func TestStartQuerierUsesTheStartTransaction(t *testing.T) {
	pool := poolFake{}
	if got := startQuerier(context.Background(), pool); got != rowsQuerier(pool) {
		t.Fatalf("without a start: got %T, want the pool", got)
	}
	tx := startTxFake{}
	if got := startQuerier(withStartTx(context.Background(), tx), pool); got != rowsQuerier(tx) {
		t.Fatalf("inside a start: got %T, want the start's transaction", got)
	}
}
