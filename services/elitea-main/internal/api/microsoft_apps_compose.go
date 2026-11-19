package api

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	v2secrets "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftapps"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// microsoftAppRegistry composes the Microsoft Default app registry (ADR-0057
// decision 5): the store (the only code that seals or opens a client secret) and
// the resolver the credential forms, the sign-in and the refresh read.
//
// Both are nil without a database or without the vault master key: the registry
// seals its secrets under a key derived from it, and a registry that accepted a
// secret it could not seal would leave a credential pointing at an app with no
// secret. Every route then answers 503, and an empty registry is not an error.
func microsoftAppRegistry(pool *pgxpool.Pool) (*microsoftapps.Store, *microsoftapps.Resolver) {
	if pool == nil {
		return nil, nil
	}
	key, err := v2secrets.MasterKeyFromEnv(os.Getenv)
	if err != nil || len(key) == 0 {
		return nil, nil
	}
	defer clear(key)
	audit, err := securityaudit.NewPostgresAppender("elitea-main")
	if err != nil {
		slog.Error("microsoft apps: the security audit appender", "error", err)
		return nil, nil
	}
	store, err := microsoftapps.NewStore(pool, key, audit)
	if err != nil {
		slog.Error("microsoft apps: compose the registry", "error", err)
		return nil, nil
	}
	return store, microsoftapps.NewResolver(store)
}

// microsoftMetadataClient is the client a Test of a Microsoft app reads Entra's
// metadata with: the MCP authorization egress guard's transport, and no redirects
// (a metadata document never redirects; following one would read from a host the
// guard was never asked about).
func microsoftMetadataClient(cfg RouterConfig) microsoftapps.HTTPDoer {
	guard := mcpAuthorizationEgressGuard(cfg)
	return &http.Client{
		Transport:     guard.Transport(),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
