package main

import (
	"errors"
	"log/slog"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/publicorigin"
)

// publicOriginFromEnv resolves the deployment's public origin once, at boot,
// from DEPLOYMENT_URL (ADR-0025 decision 9). The discovery document and the
// brand pack JSON make their URLs absolute against it; WP2's native
// authorization server stamps the same value into `iss`.
//
// Empty is a valid answer: the anonymous documents then derive the origin
// from each request's Host and mark the response Vary. A DEPLOYMENT_URL that
// is set but cannot be an origin is logged and treated as empty rather than
// refusing boot: the value predates ADR-0025 (the mailer and the
// configurations self-reference guard read it leniently), and refusing to
// start an existing deployment over a field nothing native depends on yet
// would be a regression. WP2 makes it required once a native client is
// registered, which is where refusing is right.
func publicOriginFromEnv(lookup func(string) (string, bool), logger *slog.Logger) (string, error) {
	if lookup == nil {
		return "", errors.New("public origin environment lookup is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	raw, _ := lookup(publicorigin.Env)
	origin, droppedPath, err := publicorigin.Normalize(raw)
	if err != nil {
		logger.Warn("DEPLOYMENT_URL is not a usable public origin; discovery and pack.json derive the origin from each request",
			"reason", err.Error())
		return "", nil
	}
	if origin == "" {
		logger.Info("DEPLOYMENT_URL not set; discovery and pack.json derive the origin from each request (Vary: Host)")
		return "", nil
	}
	if droppedPath {
		logger.Warn("DEPLOYMENT_URL carries a path; discovery and pack.json use its origin only",
			"origin", origin)
	}
	return origin, nil
}
