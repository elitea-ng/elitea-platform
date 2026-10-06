package legacyrbac

// NewTransactionResolver uses the existing permission rules on the caller's transaction.
// The caller must retain ownership of the transaction until authorization commits.
func NewTransactionResolver(transaction postgresStore) *PostgresResolver {
	return &PostgresResolver{store: transaction}
}
