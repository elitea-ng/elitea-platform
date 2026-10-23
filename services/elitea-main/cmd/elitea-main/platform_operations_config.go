package main

// The application engines' platform services (#1243, #1244): where each engine's
// PlatformOperations listener is, read beside the facade whose material the
// client reuses.
//
// ONE VARIABLE PER APPLICATION, ELITEA_<APP>_PLATFORM_GRPC_TARGET, a host:port
// (compose: elitea-deepwiki:9443; the chart derives <service>:<platformGrpc
// port>). Since #1359 it is the FACADE's target too — the host serves the
// provider SPI and the platform operations on one listener, behind one TLS
// configuration and one allowlist — so an enabled facade always has it, and
// the data deletion is always wired where the facade is. Everything else the
// hop needs — the client certificate, its key, the CA, the server name — is
// the facade's. A target set beside a disabled facade is a boot error rather
// than a silently ignored setting: an operator who set it meant it, and a
// process that reads it as nothing would delete projects for ever while the
// engine kept their data.

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api"
	v2deepwiki "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/deepwiki"
	v2inventory "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/inventory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/facade"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/platformops"
)

var (
	deepwikiPlatformTargetEnv  = v2deepwiki.TargetEnv
	inventoryPlatformTargetEnv = v2inventory.EnvNames.Target
)

// platformOperationsConfigs builds one client Config per enabled facade, from
// the facade's own target, and refuses a target set beside a disabled one.
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
	if target := deepwiki.Target; deepwiki.Enabled {
		configs = append(configs, platformops.Config{
			App: platformops.AppDeepWiki, Target: target,
			ClientCertFile: deepwiki.ClientCertFile, ClientKeyFile: deepwiki.ClientKeyFile,
			CAFile: deepwiki.CAFile, ServerName: deepwiki.ServerName,
			CallTimeout: platformCallTimeout(deepwiki.Timeout),
		})
	} else if get(deepwikiPlatformTargetEnv) != "" {
		return nil, fmt.Errorf("%s is set but %s is off: the platform gRPC client presents the facade's client certificate (%s, %s, %s), so it cannot exist without the facade. Turn the facade on, or clear the target",
			deepwikiPlatformTargetEnv, v2deepwiki.EnabledEnv, v2deepwiki.ClientCertEnv, v2deepwiki.ClientKeyEnv, v2deepwiki.CAFileEnv)
	}
	if target := inventory.Target; inventory.Enabled {
		configs = append(configs, platformops.Config{
			App: platformops.AppInventory, Target: target,
			ClientCertFile: inventory.ClientCertFile, ClientKeyFile: inventory.ClientKeyFile,
			CAFile: inventory.CAFile, ServerName: inventory.ServerName,
			CallTimeout: platformCallTimeout(inventory.Timeout),
		})
	} else if get(inventoryPlatformTargetEnv) != "" {
		return nil, fmt.Errorf("%s is set but %s is off: the platform gRPC client presents the facade's client certificate (%s, %s, %s), so it cannot exist without the facade. Turn the facade on, or clear the target",
			inventoryPlatformTargetEnv, v2inventory.EnvNames.Enabled,
			v2inventory.EnvNames.ClientCertFile, v2inventory.EnvNames.ClientKeyFile, v2inventory.EnvNames.CAFile)
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

// composePlatformOperations builds the clients the two cleanup journals (the
// project provisioner's and the toolkit one) share, one per enabled facade.
// The returned close releases the connections.
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
		logger.Info("application data deletion is wired: the engine's data goes with its project and toolkits",
			"app", cfg.App, "target", cfg.Target)
	}
	closeAll := func() {
		for _, closeClient := range closers {
			_ = closeClient()
		}
	}
	return clients, closeAll, nil
}
