package api

import (
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	v2core "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpoauth"
)

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
