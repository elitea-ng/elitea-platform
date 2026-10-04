package api

import (
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	v2core "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/webhook"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpoauth"
)

// mcpAuthorizationEgressGuard returns the guard the MCP OAuth and DCR proxies
// dial through. A nil config field gets a guard with an empty allowlist, so a
// composition that forgets the field refuses private destinations instead of
// reaching them.
func mcpAuthorizationEgressGuard(cfg RouterConfig) *webhook.DestinationGuard {
	if cfg.MCPAuthorizationEgressGuard != nil {
		return cfg.MCPAuthorizationEgressGuard
	}
	return webhook.NewDestinationGuard(nil)
}

// Public DCR remains available without a master key. Confidential DCR fails closed.
func mcpOAuthClientStore(pool *pgxpool.Pool) v2core.MCPDCRClients {
	key, err := v2secrets.MasterKeyFromEnv(os.Getenv)
	if err != nil {
		return nil
	}
	defer clear(key)
	store, err := mcpoauth.NewClients(pool, key)
	if err != nil {
		return nil
	}
	return store
}

func mcpOAuthTokenStore(pool *pgxpool.Pool) v2core.MCPDelegatedTokens {
	key, err := v2secrets.MasterKeyFromEnv(os.Getenv)
	if err != nil {
		return nil
	}
	defer clear(key)
	store, err := mcpoauth.NewTokens(pool, key)
	if err != nil {
		return nil
	}
	return store
}
