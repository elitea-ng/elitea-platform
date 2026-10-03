package api

import (
	"log/slog"
	"os"
	"strings"

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

// mcpEgressGuard is the dial-time SSRF guard for the MCP requests Main makes
// for a tenant (Load Tools, OAuth and DCR proxies, metadata reads). It uses
// the webhook guard's grammar and split: ELITEA_MCP_EGRESS_ALLOWLIST naming a
// private destination permits private and loopback MCP servers; link-local
// and multicast stay refused. A malformed value fails closed to an empty
// allowlist, which refuses every internal destination.
func mcpEgressGuard() *webhook.DestinationGuard {
	raw := strings.FieldsFunc(os.Getenv("ELITEA_MCP_EGRESS_ALLOWLIST"), func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	allowlist, err := webhook.ParseDestinationAllowlist(raw)
	if err != nil {
		slog.Error("ELITEA_MCP_EGRESS_ALLOWLIST is malformed; private MCP destinations are refused", "err", err)
		allowlist = nil
	}
	return webhook.NewDestinationGuard(allowlist)
}
