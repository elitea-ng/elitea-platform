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

// reasoningColumn remembers the column. Present is final; absent is read
// again after reasoningColumnRecheck; a failed read answers false, so the row
// is written without the count rather than refused.
type reasoningColumn struct {
	mu        sync.Mutex
	known     bool
	checkedAt time.Time
}

func (c *reasoningColumn) present(ctx context.Context, tx Tx) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.known || (!c.checkedAt.IsZero() && time.Since(c.checkedAt) < reasoningColumnRecheck) {
		return c.known
	}
	var present bool
	err := tx.QueryRow(ctx, reasoningColumnSQL).Scan(&present)
	c.checkedAt = time.Now()
	if err != nil {
		return false
	}
	c.known = present
	return present
}

// withReasoningTokens adds the reasoning_tokens column to a ledger insert, as
// the next placeholder after the statement's last one.
func withReasoningTokens(statement string) string {
	columns := strings.Count(statement, "$")
	statement = strings.Replace(statement, ")\n\tVALUES (", ", reasoning_tokens)\n\tVALUES (", 1)
	return strings.Replace(statement, ")\n\tON CONFLICT", fmt.Sprintf(", $%d)\n\tON CONFLICT", columns+1), 1)
}
