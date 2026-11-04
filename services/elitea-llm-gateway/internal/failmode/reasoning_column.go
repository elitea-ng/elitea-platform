package failmode

import (
	"context"
	"sync"
	"time"
)

// reasoningColumnSQL reports whether shared migration 0225 added
// gateway.llm_usage_events.reasoning_tokens.
const reasoningColumnSQL = `SELECT EXISTS (SELECT 1 FROM information_schema.columns
	WHERE table_schema = 'gateway' AND table_name = 'llm_usage_events'
	  AND column_name = 'reasoning_tokens')`

// reasoningColumnRecheck is how long an absent column is believed absent
// before it is read again: the migration can land while the gateway runs.
const reasoningColumnRecheck = 5 * time.Minute

// ReasoningColumn remembers whether the ledger has the reasoning_tokens
// column (ADR-0038 decision 5). Present is final; absent is read again after
// reasoningColumnRecheck. The zero value is ready to use.
type ReasoningColumn struct {
	mu        sync.Mutex
	present   bool
	checkedAt time.Time
}

// Present reports the column, reading it through query when the remembered
// answer is absent and stale. A failed read answers false: the row is then
// written without the count, never refused.
func (c *ReasoningColumn) Present(ctx context.Context, query func(context.Context, string) (bool, error)) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.present || (!c.checkedAt.IsZero() && time.Since(c.checkedAt) < reasoningColumnRecheck) {
		return c.present
	}
	present, err := query(ctx, reasoningColumnSQL)
	c.checkedAt = time.Now()
	if err != nil {
		return false
	}
	c.present = present
	return present
}

// reasoningColumn reports the column for d's ledger row. A row with no
// reasoning count never reads it.
func (s *Store) reasoningColumn(ctx context.Context, d OutageDelta) bool {
	if d.Usage == nil || d.Usage.ReasoningTokens <= 0 {
		return false
	}
	return s.reasoning.Present(ctx, func(ctx context.Context, sql string) (bool, error) {
		var present bool
		err := s.db.QueryRow(ctx, sql).Scan(&present)
		return present, err
	})
}
