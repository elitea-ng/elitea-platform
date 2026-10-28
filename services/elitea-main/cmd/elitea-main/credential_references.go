package main

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	v2credentialreferences "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/credentialreferences"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/epoch"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// credentialReferencesAdmin builds the credential reference admin API and
// the agent toolkit binding routes (#1124). Nil with the flag off, so the
// router mounts none of them. With the flag on every write is audited, and
// a retire signals the replicas' invalidation hub. Creating a reference from
// scratch (#1482) checks its locator against the same secret stores the
// broker reads, metadata only, under the active policy revision.
func credentialReferencesAdmin(admission profile.Admission, revision profile.PolicyRevision, secretStores secretStoresAtBoot, pool *pgxpool.Pool, invalidations *epoch.Hub) (*v2credentialreferences.Handler, error) {
	if !admission.Governed() {
		return nil, nil
	}
	if pool == nil {
		return nil, errors.New("the credential reference admin API needs the platform database")
	}
	audit, err := securityaudit.NewPostgresAppender("elitea-main")
	if err != nil {
		return nil, fmt.Errorf("construct credential reference security audit appender: %w", err)
	}
	store := repos.NewCredentialReferences(pool, audit)
	if invalidations != nil {
		store.WithNotifier(invalidations)
	}
	handler, err := v2credentialreferences.New(store)
	if err != nil {
		return nil, err
	}
	stores, err := secretStores()
	if err != nil {
		return nil, fmt.Errorf("credential reference admin %w", err)
	}
	locators, err := v2credentialreferences.NewSecretStoreLocators(stores, revision.ID)
	if err != nil {
		return nil, err
	}
	registry, err := credentialadapters.NewBuiltinRegistry()
	if err != nil {
		return nil, fmt.Errorf("credential reference admin provider registry: %w", err)
	}
	return handler.WithCreate(locators, registry), nil
}
