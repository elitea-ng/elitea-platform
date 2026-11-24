package main

import (
	"fmt"

	"github.com/elitea-ng/elitea-platform/libs/go/secretstrength"
	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// productionAuthVars mark a deployment with a real authentication
// configuration. Such a deployment is not a developer machine, so the
// development identity flag is refused next to any of them (the pattern of
// elitea-main's developmentFlagsFromEnv).
var productionAuthVars = []string{"OIDC_ISSUER_URL", "ELITEA_AUTH_CONFIG_FILE"}

// identityStartupCheck decides whether the host may start with the identity
// secret it was given (#2958, #2959). The secret signs the X-Elitea-*
// identity the facade in elitea-main sends; without it the host cannot tell a
// tenant's call from a forged one.
//
//   - a strong <PREFIX>IDENTITY_SECRET: start;
//   - a weak one (blank, short, a placeholder, a published value): refuse;
//   - none: refuse, unless ELITEA_DEV_INSECURE_IDENTITY=true is set for a
//     local stack. That flag is itself refused on a host with a mutual-TLS
//     listener, and next to a production authentication configuration.
//
// No error carries the value.
func identityStartupCheck(settings spi.Settings, lookup spi.Lookup) error {
	name := settings.Prefix + "IDENTITY_SECRET"
	if settings.IdentitySecret == "" && settings.InsecureIdentity {
		// The host's own signal of a deployment: a mutual-TLS listener. The
		// chart and every compose stack give the host a client CA; a
		// developer's plaintext host has none. The two authentication
		// variables are elitea-main's and usually absent here, so they are a
		// second guard, not the only one.
		if settings.TLSCAFile != "" {
			return fmt.Errorf("%w: %s=true is a local-development flag and must not be set on a host with a mutual-TLS listener (%sTLS_CA_FILE); set %s and remove the flag",
				spi.ErrConfig, spi.InsecureIdentityEnvVar, settings.Prefix, name)
		}
		for _, configured := range productionAuthVars {
			if value, _ := lookup(configured); value != "" {
				return fmt.Errorf("%w: %s=true is a local-development flag and must not be set when %s is configured; set %s and remove the flag",
					spi.ErrConfig, spi.InsecureIdentityEnvVar, configured, name)
			}
		}
		return nil
	}
	if err := secretstrength.Check(name, settings.IdentitySecret); err != nil {
		return fmt.Errorf("%w: %w. It must hold the SAME value as elitea-main's %s; for a throwaway local stack only, leave it unset and set %s=true",
			spi.ErrConfig, err, name, spi.InsecureIdentityEnvVar)
	}
	return nil
}
