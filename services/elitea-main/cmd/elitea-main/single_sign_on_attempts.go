package main

import (
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	scimapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/scim"
	v2auth "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/authcomposition"
)

// composeSingleSignOnAttempts puts the OIDC begin/callback and SAML begin/ACS
// behind the shared PostgreSQL attempt limiter (#1078; see
// internal/api/v2/auth/signin_attempts.go).
//
// The limiter keys per client, so it needs the real client address. Without
// trusted proxy CIDRs (ELITEA_TRUSTED_PROXY_CIDRS or the Form document's
// trusted_proxy_cidrs) every request behind an ingress looks like the
// ingress, and one window would hold every user: the planes then run
// unlimited and the boot says so. A limiter that cannot be built stops the
// boot.
func composeSingleSignOnAttempts(
	pool *pgxpool.Pool,
	applicationSecretKey string,
	addresses *scimapi.ClientAddressResolver,
	oidc *v2auth.OIDCHandler,
	saml *v2auth.SAMLHandler,
	logger *slog.Logger,
) error {
	if pool == nil {
		logger.Warn("single sign-on runs without a sign-in attempt limiter: no PostgreSQL pool")
		return nil
	}
	if !addresses.Configured() {
		logger.Warn("single sign-on runs without a sign-in attempt limiter: no trusted proxy CIDRs, "+
			"so a client cannot be told from the ingress",
			"env", trustedProxyCIDRsEnv)
		return nil
	}
	admitter, err := authcomposition.NewSingleSignOnAttemptAdmitter(pool, applicationSecretKey)
	if err != nil {
		return fmt.Errorf("compose the single sign-on attempt limiter: %w", err)
	}
	oidc.WithAttemptLimiter(admitter, addresses)
	saml.WithAttemptLimiter(admitter, addresses)
	return nil
}
