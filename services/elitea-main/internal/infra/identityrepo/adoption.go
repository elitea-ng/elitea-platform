package identityrepo

// The ONE rule for when a first login may adopt an existing account by its
// email address.
//
// Two browser planes provision accounts on a first login: the SSO plane in
// internal/api/v2/auth (joinAccountByEmail) and the Form/pylon-parity plane
// composed in internal/authcomposition (resolveProvisioningUser here). Each
// used to carry its own copy of "may this address adopt that row", and the
// copies drifted: the second one adopted ANY row with a matching address,
// ignoring both the federated-link guard and the SCIM rule. The predicate is
// therefore defined here once, as SQL, and both planes embed it.
//
// A row is adoptable when BOTH hold:
//
//  1. No FEDERATED subject already holds it — no `auth_core__user_provider`
//     ref in a namespace listed by FederatedRefPatterns. A pylon bare ref or an
//     invite receipt does not count; see the SSO plane's FederatedRefPrefixes
//     comment for why those must stay adoptable.
//  2. It has no `elitea_auth.scim_users` record, OR the asserting provider is
//     configured to adopt SCIM-provisioned accounts (`adopt_scim_users`). A
//     directory-provisioned address is the directory's statement about a
//     person; a provider not paired with the directory (a GitHub connector
//     behind Dex, a local Form login) asserting it must not take the account.

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// The `auth_core__user_provider.provider_ref` namespaces the SSO plane writes.
// Every federated protocol MUST be listed; the guard is derived from this list
// rather than from literals, because a literal is what once let SAML through.
const (
	OIDCProviderRefPrefix = "oidc:"
	SAMLProviderRefPrefix = "saml:"
)

// FederatedRefPatterns is the federated namespaces as SQL LIKE patterns.
func FederatedRefPatterns() []string {
	return []string{OIDCProviderRefPrefix + "%", SAMLProviderRefPrefix + "%"}
}

// AdoptionGuard renders the adoption predicate for the account whose id is the
// SQL expression userID. patternsParam binds FederatedRefPatterns() and
// adoptParam binds the provider's adopt_scim_users boolean.
func AdoptionGuard(userID string, patternsParam, adoptParam int) string {
	return `NOT EXISTS (
		       SELECT 1 FROM auth_core__user_provider AS bound
		       WHERE bound.user_id = ` + userID + `
		         AND bound.provider_ref LIKE ANY ($` + strconv.Itoa(patternsParam) + `)
		   )
		   AND ($` + strconv.Itoa(adoptParam) + `::boolean OR NOT EXISTS (
		       SELECT 1 FROM elitea_auth.scim_users AS scim
		       WHERE scim.user_id = ` + userID + `
		   ))`
}

// accountAdoptable evaluates AdoptionGuard for one existing row.
func accountAdoptable(ctx context.Context, tx pgx.Tx, userID int32, adoptSCIMUsers bool) (bool, error) {
	var adoptable bool
	err := tx.QueryRow(ctx,
		`SELECT `+AdoptionGuard("$1::integer", 2, 3),
		userID, FederatedRefPatterns(), adoptSCIMUsers,
	).Scan(&adoptable)
	if err != nil {
		return false, fmt.Errorf("identityrepo: check account adoption: %w", err)
	}
	return adoptable, nil
}
