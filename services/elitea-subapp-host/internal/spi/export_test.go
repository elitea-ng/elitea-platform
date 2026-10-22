package spi

import "context"

// ReconcilePeriodic is the pass the store's supervisor runs every
// reconcileEvery, for the tests that tell it from the startup pass.
func (s *PostgresStore) ReconcilePeriodic(ctx context.Context) (int, error) {
	return s.reconcile(ctx, false)
}
