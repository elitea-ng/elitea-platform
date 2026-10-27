// Package backends assembles the governed SecretStore set once, at the
// composition root, and only with the credential governance flag on.
//
// With the flag off nothing here is called: no environment variable is read,
// no Vault client is built and nothing is dialled (profile specification,
// flag-off contract item 4).
package backends

import (
	"errors"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore/vault"
)

// NewFromEnv returns the stores keyed by backend id and the readiness
// prerequisites to pass to profile.Boot.
//
//   - pg (the pgvault adapter) is always present: the PostgreSQL vault stays
//     the default backend.
//   - Vault is built only when at least one ELITEA_SECRETSTORE_VAULT_*
//     variable is set. A set but invalid configuration (http://, no role
//     prefix, a missing file) is an error, which refuses boot.
//   - Vault unconfigured is not an error here. The returned prerequisite
//     refuses boot later if the active revision requires Vault for a class.
//
// The map goes to the credential broker only.
func NewFromEnv(lookup func(string) (string, bool), pg secretstore.SecretStore) (map[string]secretstore.SecretStore, []profile.Prerequisite, error) {
	if lookup == nil {
		return nil, nil, errors.New("secretstore: environment lookup is required")
	}
	if pg == nil {
		return nil, nil, errors.New("secretstore: the pgvault adapter is required")
	}
	stores := map[string]secretstore.SecretStore{secretstore.BackendPGVault: pg}
	var external *vault.Store
	if vault.Configured(lookup) {
		cfg, err := vault.ConfigFromEnv(lookup)
		if err != nil {
			return nil, nil, err
		}
		external, err = vault.New(cfg)
		if err != nil {
			return nil, nil, err
		}
		stores[secretstore.BackendVault] = external
	}
	return stores, []profile.Prerequisite{vault.NewPrerequisite(external)}, nil
}
