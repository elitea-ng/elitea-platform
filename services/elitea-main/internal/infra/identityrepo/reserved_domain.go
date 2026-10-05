package identityrepo

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxReservedDomainAccountsReported bounds the boot-time report; the count is
// exact, the listed references are the first ones by account id.
const MaxReservedDomainAccountsReported = 20

// ReservedDomainAccounts is what ListReservedDomainSignInAccounts found.
type ReservedDomainAccounts struct {
	Count              int64
	ProviderReferences []string
}

// ListReservedDomainSignInAccounts finds PERSON accounts — accounts that a
// sign-in created, so they carry a provider link — whose address is in the
// reserved system-identity domain `@centry.user`.
//
// Until the Form plane stopped synthesizing addresses, a Form user configured
// without an email signed in as `<login>@centry.user`. Those rows still exist
// on upgraded deployments, and every surface that filters
// `email LIKE '%@centry.user'` (the Users page, analytics, budgets, SCIM)
// treats them as platform accounts. This reads them so the boot can WARN; it
// changes nothing. Rewriting a person's address is an operator decision (see
// docs/UPGRADING.md), never a silent migration.
//
// Platform accounts (system@centry.user, system_user_<n>@centry.user) have no
// provider link, and they are excluded by name as well.
func ListReservedDomainSignInAccounts(ctx context.Context, pool *pgxpool.Pool) (ReservedDomainAccounts, error) {
	if pool == nil {
		return ReservedDomainAccounts{}, errors.New("identityrepo: PostgreSQL pool is unavailable")
	}
	const predicate = `
FROM public.auth_core__user_provider AS provider
JOIN public.auth_core__user AS owner ON owner.id = provider.user_id
WHERE lower(owner.email) LIKE '%@centry.user'
  AND lower(owner.email) <> 'system@centry.user'
  AND lower(owner.email) NOT LIKE 'system\_user\_%@centry.user'`

	var result ReservedDomainAccounts
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT owner.id)`+predicate).Scan(&result.Count); err != nil {
		return ReservedDomainAccounts{}, fmt.Errorf("identityrepo: count reserved-domain sign-in accounts: %w", err)
	}
	if result.Count == 0 {
		return result, nil
	}
	rows, err := pool.Query(ctx, `SELECT provider.provider_ref`+predicate+`
ORDER BY owner.id, provider.provider_ref
LIMIT $1`, MaxReservedDomainAccountsReported)
	if err != nil {
		return ReservedDomainAccounts{}, fmt.Errorf("identityrepo: list reserved-domain sign-in accounts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var reference string
		if err := rows.Scan(&reference); err != nil {
			return ReservedDomainAccounts{}, fmt.Errorf("identityrepo: read reserved-domain sign-in account: %w", err)
		}
		result.ProviderReferences = append(result.ProviderReferences, reference)
	}
	if err := rows.Err(); err != nil {
		return ReservedDomainAccounts{}, fmt.Errorf("identityrepo: read reserved-domain sign-in accounts: %w", err)
	}
	return result, nil
}
