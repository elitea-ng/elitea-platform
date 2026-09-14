package runtimecomposition

import (
	v2secrets "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/mcpoauth"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
)

// Missing key material disables delegated references; admission refuses such requests.
func currentToolkitMCPTokenStore(pool *pgxpool.Pool) *mcpoauth.Tokens {
	key, err := v2secrets.MasterKeyFromEnv(os.Getenv)
	if err != nil {
		return nil
	}
	defer clear(key)
	tokens, err := mcpoauth.NewTokens(pool, key)
	if err != nil {
		return nil
	}
	return tokens
}
