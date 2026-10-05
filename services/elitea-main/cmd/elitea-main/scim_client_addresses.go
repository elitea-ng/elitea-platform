package main

import (
	"fmt"
	"log/slog"

	scimapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/scim"
)

// trustedProxyCIDRsEnv names the deployment's trusted proxy CIDRs for the
// planes that have no Form authentication document (OIDC-only, SAML-only).
// A comma-separated list, for example "10.0.0.0/8,fd00::/8".
const trustedProxyCIDRsEnv = "ELITEA_TRUSTED_PROXY_CIDRS"

// scimClientAddressesFromConfig builds the caller-address resolver the SCIM
// token endpoint's failure limiter keys on.
//
// It is built on EVERY authentication plane. The CIDRs are the union of
// ELITEA_TRUSTED_PROXY_CIDRS and the Form authentication document's
// `trusted_proxy_cidrs` (empty when there is no document). Before this the
// limiter read the Form plane's resolver only, so an OIDC-only deployment fell
// back to the socket peer — the ingress — and every caller shared one key.
//
// An invalid CIDR stops the boot. No CIDR at all is valid but logged as a
// WARN: the token endpoint then cannot know a caller's address, so it verifies
// every secret before it throttles (a correct secret is never refused).
func scimClientAddressesFromConfig(
	getenv func(string) string, authDocumentCIDRs []string, logger *slog.Logger,
) (*scimapi.ClientAddressResolver, error) {
	cidrs := append(splitEnvList(getenv(trustedProxyCIDRsEnv)), authDocumentCIDRs...)
	resolver, err := scimapi.NewClientAddressResolver(cidrs)
	if err != nil {
		return nil, fmt.Errorf("%s / trusted_proxy_cidrs: %w", trustedProxyCIDRsEnv, err)
	}
	if !resolver.Configured() && logger != nil {
		logger.Warn("SCIM token rate limiting runs without trusted proxy CIDRs; "+
			"failed client authentications are throttled per client id only, after the secret check",
			"env", trustedProxyCIDRsEnv)
	}
	return resolver, nil
}
