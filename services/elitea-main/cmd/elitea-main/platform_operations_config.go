package main

// The application engines' platform services (#1243, #1244): where each engine's
// PlatformOperations listener is, read beside the facade whose material the
// client reuses.
//
// ONE VARIABLE PER APPLICATION, ELITEA_<APP>_PLATFORM_GRPC_TARGET, a host:port
// (compose: elitea-deepwiki:9443; the chart derives <service>:<platformGrpc
// port>). Everything else the hop needs — the client certificate, its key, the
// CA, the server name — is the facade's: the engine serves both listeners
// behind one TLS configuration, and this process presents the same identity on
// both. So the target is read only where the facade is ENABLED, and a target
// set beside a disabled facade is a boot error rather than a silently ignored
// setting: an operator who set it meant it, and a process that reads it as
// nothing would delete projects for ever while the engine kept their data.
//
// A facade that is enabled WITHOUT a target is allowed, logged at Warn at boot:
// the engine's service is off until its allowlist is set (ELITEA_<APP>_
// PLATFORM_CLIENTS), and a deployment is entitled to run the facade alone.
// What that deployment then has is the leak the two issues describe, and the
// log line says so.

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api"
	v2deepwiki "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/deepwiki"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/facade"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/platformops"
)

const (
	deepwikiPlatformTargetEnv  = "ELITEA_DEEPWIKI_PLATFORM_GRPC_TARGET"
	inventoryPlatformTargetEnv = "ELITEA_INVENTORY_PLATFORM_GRPC_TARGET"
)

// platformOperationsConfigs reads the two targets against the two facades. It
// returns one client Config per target set, and an error for a target set
// beside a disabled facade.
func platformOperationsConfigs(
	deepwiki v2deepwiki.Config,
	inventory facade.Config,
	lookup func(string) (string, bool),
) ([]platformops.Config, error) {
	get := func(key string) string {
		value, _ := lookup(key)
		return strings.TrimSpace(value)
	}
	var configs []platformops.Config
	if target := get(deepwikiPlatformTargetEnv); target != "" {
		if !deepwiki.Enabled {
			return nil, fmt.Errorf("%s is set but %s is off: the platform gRPC client presents the facade's client certificate (%s, %s, %s), so it cannot exist without the facade. Turn the facade on, or clear the target",
				deepwikiPlatformTargetEnv, v2deepwiki.EnabledEnv, v2deepwiki.ClientCertEnv, v2deepwiki.ClientKeyEnv, v2deepwiki.CAFileEnv)
		}
		configs = append(configs, platformops.Config{
			App: platformops.AppDeepWiki, Target: target,
			ClientCertFile: deepwiki.ClientCertFile, ClientKeyFile: deepwiki.ClientKeyFile,
			CAFile: deepwiki.CAFile, ServerName: deepwiki.ServerName,
			CallTimeout: platformCallTimeout(deepwiki.Timeout),
		})
	}
	if target := get(inventoryPlatformTargetEnv); target != "" {
		if !inventory.Enabled {
			return nil, fmt.Errorf("%s is set but %s is off: the platform gRPC client presents the facade's client certificate (%s, %s, %s), so it cannot exist without the facade. Turn the facade on, or clear the target",
				inventoryPlatformTargetEnv, "ELITEA_INVENTORY_ENABLED",
				"ELITEA_INVENTORY_CLIENT_CERT_FILE", "ELITEA_INVENTORY_CLIENT_KEY_FILE", "ELITEA_INVENTORY_CA_FILE")
		}
		configs = append(configs, platformops.Config{
			App: platformops.AppInventory, Target: target,
			ClientCertFile: inventory.ClientCertFile, ClientKeyFile: inventory.ClientKeyFile,
			CAFile: inventory.CAFile, ServerName: inventory.ServerName,
			CallTimeout: platformCallTimeout(inventory.Timeout),
		})
	}
	for _, cfg := range configs {
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
	}
	return configs, nil
}

// platformCallTimeout is the bound of one deletion call. The facade's timeout
// is for a user-facing proxy hop; a deletion first stops the engine's running
// generations or ingests and waits for them (30s on the host), so the bound is
// never below the package default.
func platformCallTimeout(facadeTimeout time.Duration) time.Duration {
	return max(facadeTimeout, platformops.DefaultCallTimeout)
}

// composePlatformOperations builds the clients the router and the provisioner
// share, and logs, per facade, whether the engine's data is deleted with its
// project and toolkits. The returned close releases the connections.
func composePlatformOperations(
	logger *slog.Logger,
	deepwiki v2deepwiki.Config,
	inventory facade.Config,
	lookup func(string) (string, bool),
) ([]api.ApplicationDataClient, func(), error) {
	configs, err := platformOperationsConfigs(deepwiki, inventory, lookup)
	if err != nil {
		return nil, nil, err
	}
	clients := make([]api.ApplicationDataClient, 0, len(configs))
	closers := make([]func() error, 0, len(configs))
	wired := map[string]bool{}
	for _, cfg := range configs {
		client, err := platformops.New(cfg, logger)
		if err != nil {
			for _, closeClient := range closers {
				_ = closeClient()
			}
			return nil, nil, fmt.Errorf("compose %s platform gRPC client: %w", cfg.App, err)
		}
		clients = append(clients, client)
		closers = append(closers, client.Close)
		wired[cfg.App] = true
		logger.Info("application data deletion is wired: the engine's data goes with its project and toolkits",
			"app", cfg.App, "target", cfg.Target)
	}
	for _, app := range []struct {
		name    string
		enabled bool
		env     string
	}{
		{platformops.AppDeepWiki, deepwiki.Enabled, deepwikiPlatformTargetEnv},
		{platformops.AppInventory, inventory.Enabled, inventoryPlatformTargetEnv},
	} {
		if app.enabled && !wired[app.name] {
			logger.Warn("application data deletion is NOT wired: deleting a project (or a toolkit) leaves its data in this engine for ever",
				"app", app.name, "set", app.env)
		}
	}
	closeAll := func() {
		for _, closeClient := range closers {
			_ = closeClient()
		}
	}
	return clients, closeAll, nil
}
