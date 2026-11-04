package budgetwriteback

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// reasoningColumnSQL reports whether shared migration 0225 added
// gateway.llm_usage_events.reasoning_tokens (ADR-0038 decision 5).
const reasoningColumnSQL = `SELECT EXISTS (SELECT 1 FROM information_schema.columns
	WHERE table_schema = 'gateway' AND table_name = 'llm_usage_events'
	  AND column_name = 'reasoning_tokens')`

// reasoningColumnRecheck is how long an absent column is believed absent: the
// migration can land while the consumer runs.
const reasoningColumnRecheck = 5 * time.Minute

// ColumnQuerier runs one statement on the pool, outside any transaction.
// PoolDB implements it. A DB that does not is never asked about the column,
// and the ledger row is written without the count.
type ColumnQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) Row
}

// reasoningColumn remembers the column. Present is final; absent is read
// again after reasoningColumnRecheck; a failed read answers false, so the row
// is written without the count rather than refused.
//
// The probe runs on the pool, never in the money transaction: in PostgreSQL a
// failed statement aborts the transaction it runs in, so a probe there could
// roll back the budget write it only decorates.
type reasoningColumn struct {
	mu        sync.Mutex
	known     bool
	checkedAt time.Time
}

func (c *reasoningColumn) present(ctx context.Context, db ColumnQuerier) bool {
	if db == nil {
		return false
	}
	c.mu.Lock()
	if c.known || (!c.checkedAt.IsZero() && time.Since(c.checkedAt) < reasoningColumnRecheck) {
		known := c.known
		c.mu.Unlock()
		return known
	}
	// Claim this recheck so concurrent callers answer from the cache while
	// the probe runs, without holding the lock across the query.
	c.checkedAt = time.Now()
	c.mu.Unlock()

	var present bool
	if err := db.QueryRow(ctx, reasoningColumnSQL).Scan(&present); err != nil {
		return false
	}
	if present {
		c.mu.Lock()
		c.known = true
		c.mu.Unlock()
	}
	return present
}

// withReasoningTokens adds the reasoning_tokens column to a ledger insert, as
// the next placeholder after the statement's last one.
func withReasoningTokens(statement string) string {
	columns := strings.Count(statement, "$")
	statement = strings.Replace(statement, ")\n\tVALUES (", ", reasoning_tokens)\n\tVALUES (", 1)
	return strings.Replace(statement, ")\n\tON CONFLICT", fmt.Sprintf(", $%d)\n\tON CONFLICT", columns+1), 1)
}
