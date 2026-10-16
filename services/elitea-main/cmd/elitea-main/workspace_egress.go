package main

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/egresstoken"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
)

// The workspace egress proxy's configuration (ADR-0034 decision 9, "Host
// allowlist: the egress proxy"). The chart sets all three when
// egressProxy.enabled is true (templates/main/deployment.yaml).
const (
	// envEgressProxyURL is the proxy as the sandbox reaches it,
	// http://host:port, without credentials.
	envEgressProxyURL = "ELITEA_EGRESS_PROXY_URL"
	// envEgressDefaultTurnWall is the run-token lifetime base when the
	// cloud policy leaves the turn's wall time to the worker's default (a Go
	// duration). Empty: workspaceruntime.DefaultEgressTurnWall.
	envEgressDefaultTurnWall = "ELITEA_EGRESS_PROXY_DEFAULT_TURN_WALL"
)

// loadWorkspaceEgress builds the egress seam from the environment. The
// signing key is loaded only when its file is configured; a configured file
// that does not hold an Ed25519 key stops the start, as does a malformed
// proxy URL. Half a configuration is not an error: allowlist turns are then
// refused (egress mode `unavailable`), and the log says why at start.
func loadWorkspaceEgress(getenv func(string) string, logger *slog.Logger) (workspaceruntime.Egress, error) {
	if logger == nil {
		logger = slog.Default()
	}
	egress := workspaceruntime.Egress{ProxyURL: strings.TrimSpace(getenv(envEgressProxyURL))}
	if raw := strings.TrimSpace(getenv(envEgressDefaultTurnWall)); raw != "" {
		wall, err := time.ParseDuration(raw)
		if err != nil || wall <= 0 {
			return workspaceruntime.Egress{}, fmt.Errorf("%s must be a positive duration", envEgressDefaultTurnWall)
		}
		egress.DefaultTurnWall = wall
	}
	if err := egress.Validate(); err != nil {
		return workspaceruntime.Egress{}, fmt.Errorf("%s: %w", envEgressProxyURL, err)
	}
	minter, err := egresstoken.Load(egresstoken.ConfigFromEnv(getenv))
	switch {
	case errors.Is(err, egresstoken.ErrNotConfigured):
		minter = nil
	case err != nil:
		return workspaceruntime.Egress{}, err
	}
	if minter != nil {
		egress.Minter = minter
	}
	switch {
	case egress.ProxyURL != "" && minter != nil:
		logger.Info("workspace egress proxy enabled: host allowlists are enforced through the proxy",
			"proxy_url", egress.ProxyURL, "key_id", minter.KeyID())
	case egress.ProxyURL != "":
		logger.Warn("workspace egress proxy is configured but no run-token signing key is; cloud turns with a host allowlist are refused",
			"proxy_url", egress.ProxyURL, "missing", egresstoken.EnvSigningKeyFile)
	case minter != nil:
		logger.Warn("a run-token signing key is configured but no egress proxy is; cloud turns with a host allowlist are refused",
			"key_id", minter.KeyID(), "missing", envEgressProxyURL)
	}
	return egress, nil
}
