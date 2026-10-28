package main

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	v2credentialreferences "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/credentialreferences"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/epoch"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// credentialReferencesAdmin builds the credential reference admin API and
// the agent toolkit binding routes (#1124). Nil with the flag off, so the
// router mounts none of them. With the flag on every write is audited, and
// a retire signals the replicas' invalidation hub.
func credentialReferencesAdmin(admission profile.Admission, pool *pgxpool.Pool, invalidations *epoch.Hub) (*v2credentialreferences.Handler, error) {
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
	return v2credentialreferences.New(store)
}
