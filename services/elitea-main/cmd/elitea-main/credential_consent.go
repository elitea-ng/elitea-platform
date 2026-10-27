package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	v2credentialconsent "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/credentialconsent"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/refresh"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/provideroauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// providerOAuth is the provider delegated OAuth composition (#1121): the
// consent routes and the token source the credential broker reads. Both
// nil with the flag off or with no provider OAuth application configured.
type providerOAuth struct {
	Consent *v2credentialconsent.Handler
	Tokens  credentialbroker.ProviderTokenSource
}

// providerOAuthAtBoot builds provider OAuth custody, consent and refresh.
// Flag off: nothing is read or built, so no consent route mounts. Flag on:
// the ELITEA_PROVIDER_OAUTH_* applications are read; none configured
// builds nothing (a provider-OAuth use is then BACKEND_UNAVAILABLE). An
// application configured without the vault master key, the key the Entra
// delegation custody also derives from, refuses boot.
func providerOAuthAtBoot(
	admission profile.Admission,
	lookup func(string) (string, bool),
	pool *pgxpool.Pool,
	masterKey func() ([]byte, error),
	logger *slog.Logger,
) (providerOAuth, error) {
	if !admission.Governed() {
		return providerOAuth{}, nil
	}
	configs, err := provideroauth.ConfigFromEnv(lookup)
	if err != nil {
		return providerOAuth{}, err
	}
	if len(configs) == 0 {
		return providerOAuth{}, nil
	}
	if pool == nil || masterKey == nil {
		return providerOAuth{}, errors.New("provider OAuth needs the platform database and the vault master key")
	}
	key, err := masterKey()
	if err != nil {
		return providerOAuth{}, fmt.Errorf("provider OAuth custody needs the vault master key: %w", err)
	}
	defer clear(key)
	if len(key) == 0 {
		// An unset key reads as no key, not as an error.
		return providerOAuth{}, errors.New("provider OAuth custody needs the vault master key (SECRETS_MASTER_KEY)")
	}
	audit, err := securityaudit.NewPostgresAppender("elitea-main")
	if err != nil {
		return providerOAuth{}, fmt.Errorf("construct provider OAuth security audit appender: %w", err)
	}
	grants, err := provideroauth.NewGrants(pool, key, audit)
	if err != nil {
		return providerOAuth{}, fmt.Errorf("construct provider OAuth custody: %w", err)
	}
	clients := make([]*provideroauth.Client, 0, len(configs))
	providers := make([]string, 0, len(configs))
	for _, cfg := range configs {
		client, err := provideroauth.NewClient(cfg, nil)
		if err != nil {
			return providerOAuth{}, err
		}
		clients = append(clients, client)
		providers = append(providers, cfg.Provider)
	}
	leases, err := refresh.NewLeases(pool)
	if err != nil {
		return providerOAuth{}, err
	}
	coordinator, err := refresh.NewCoordinator(leases, refresh.Config{Holder: refreshHolder()})
	if err != nil {
		return providerOAuth{}, err
	}
	tokens, err := provideroauth.NewTokenSource(provideroauth.TokenSourceConfig{
		Grants: grants, Leases: leases, Coordinator: coordinator, Clients: clients,
	})
	if err != nil {
		return providerOAuth{}, err
	}
	references := repos.NewCredentialGrants(pool, audit)
	consent, err := provideroauth.NewConsent(provideroauth.ConsentConfig{
		Grants: grants, Clients: clients, Targets: references, Bindings: repos.NewAgentIdentityBindings(pool, audit),
	})
	if err != nil {
		return providerOAuth{}, err
	}
	handler, err := v2credentialconsent.New(consent, logger)
	if err != nil {
		return providerOAuth{}, err
	}
	logger.Info("provider OAuth consent enabled", "providers", providers)
	return providerOAuth{Consent: handler, Tokens: tokens}, nil
}

// refreshHolder names this replica in a refresh lease row: the host name
// (the pod name in Kubernetes) and a random suffix per process.
func refreshHolder() string {
	host, _ := os.Hostname()
	if len(host) > 200 {
		host = host[:200]
	}
	suffix := make([]byte, 8)
	_, _ = rand.Read(suffix)
	return host + "/" + hex.EncodeToString(suffix)
}
