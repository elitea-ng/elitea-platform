package auth

// First-login email matching ignores case, against a real PostgreSQL.
//
// The motivating deployment provisions people through SCIM from Microsoft
// Entra ID and signs them in through SAML. SCIM stores the userName in lower
// case (scimdirectory.NormalizeUserName). Entra can assert the same person's
// `mail` attribute as "Alice@Contoso.com". Before the fix, the upsert matched
// `email` exactly, missed the provisioned row, and inserted a SECOND account:
// the person signed in to an empty account, and SCIM group grants applied to
// an account nobody used.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// A SCIM-provisioned account (lower case, no federated link) is ADOPTED by a
// SAML first login that asserts the same address in mixed case.
func TestASAMLFirstLoginWithMixedCaseAdoptsTheSCIMProvisionedAccount(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx := context.Background()

	var provisioned int
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO auth_core__user (email, name) VALUES ('alice@contoso.com', 'Alice') RETURNING id`,
	).Scan(&provisioned))

	adopted, err := resolveOnce(t, pool, SAMLProviderRefPrefix+"entra-object-id", "  Alice@Contoso.COM ")
	require.NoError(t, err)
	require.Equal(t, provisioned, adopted, "a mixed-case assertion created a second account")

	var accounts int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM auth_core__user WHERE lower(email) = 'alice@contoso.com'`,
	).Scan(&accounts))
	require.Equal(t, 1, accounts)

	// The second login resolves by subject and keeps the same account.
	again, err := resolveOnce(t, pool, SAMLProviderRefPrefix+"entra-object-id", "ALICE@contoso.com")
	require.NoError(t, err)
	require.Equal(t, provisioned, again)
}

// The same rule on the OIDC path, against a row stored in MIXED case (as
// pylon stored what the identity provider sent). The stored spelling is kept.
func TestAnOIDCFirstLoginAdoptsAMixedCaseLegacyAccount(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx := context.Background()

	var legacy int
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO auth_core__user (email, name) VALUES ('Bob@Corp.com', 'Bob') RETURNING id`,
	).Scan(&legacy))

	adopted, err := resolveOnce(t, pool, OIDCProviderRefPrefix+"bob-sub", "bob@corp.com")
	require.NoError(t, err)
	require.Equal(t, legacy, adopted)

	var stored string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT email FROM auth_core__user WHERE id = $1`, legacy).Scan(&stored))
	require.Equal(t, "Bob@Corp.com", stored, "a case-only difference rewrote the stored address")
}

// A brand-new person is stored in lower case, so a later SCIM push of the
// same address finds the row.
func TestANewAccountIsStoredInLowerCase(t *testing.T) {
	pool := newProvisioningPool(t)
	ctx := context.Background()

	created, err := resolveOnce(t, pool, SAMLProviderRefPrefix+"carol-nameid", "Carol@Corp.com")
	require.NoError(t, err)

	var stored string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT email FROM auth_core__user WHERE id = $1`, created).Scan(&stored))
	require.Equal(t, "carol@corp.com", stored)
}

// The adoption guard still applies across case: an account another federated
// subject holds is not adopted because the address differs only in case.
func TestCaseFoldingDoesNotBypassTheAdoptionGuard(t *testing.T) {
	pool := newProvisioningPool(t)

	_, err := resolveOnce(t, pool, OIDCProviderRefPrefix+"dave-sub", "dave@corp.com")
	require.NoError(t, err)

	_, err = resolveOnce(t, pool, SAMLProviderRefPrefix+"mallory-nameid", "DAVE@corp.com")
	require.ErrorIs(t, err, errIdentityConflict)
}
